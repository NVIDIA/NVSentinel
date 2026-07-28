// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package exporter

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

// mockSource records MarkProcessed calls for verification.
// Only MarkProcessed is exercised by workerPool; other methods are no-ops.
type mockSource struct {
	mu      sync.Mutex
	tokens  [][]byte
	markErr error
}

func (m *mockSource) Start(context.Context)              {}
func (m *mockSource) Events() <-chan client.Event         { return nil }
func (m *mockSource) Close(context.Context) error         { return nil }

func (m *mockSource) MarkProcessed(_ context.Context, token []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.markErr != nil {
		return m.markErr
	}

	cp := make([]byte, len(token))
	copy(cp, token)
	m.tokens = append(m.tokens, cp)

	return nil
}

func (m *mockSource) getTokens() [][]byte {
	m.mu.Lock()
	defer m.mu.Unlock()

	out := make([][]byte, len(m.tokens))
	copy(out, m.tokens)

	return out
}

// TestWorkerPool_SingleWorker is a deterministic end-to-end test: with one
// worker, events are processed in FIFO order, so every sequence is contiguous
// and every token is advanced via MarkProcessed.
func TestWorkerPool_SingleWorker(t *testing.T) {
	src := &mockSource{}

	var processCount atomic.Int32

	process := func(_ context.Context, _ client.Event) error {
		processCount.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newWorkerPool(1, process, src, cancel)

	const numEvents = 5

	go func() {
		for i := uint64(1); i <= numEvents; i++ {
			pool.dispatch(ctx, workItem{
				seq:         i,
				resumeToken: []byte(fmt.Sprintf("tok-%d", i)),
			})
		}

		pool.closeDispatch()
	}()

	if err := pool.run(ctx); err != nil {
		t.Fatalf("run() returned error: %v", err)
	}

	if int(processCount.Load()) != numEvents {
		t.Fatalf("process called %d times, want %d", processCount.Load(), numEvents)
	}

	tokens := src.getTokens()
	if len(tokens) != numEvents {
		t.Fatalf("MarkProcessed called %d times, want %d", len(tokens), numEvents)
	}

	for i, tok := range tokens {
		want := fmt.Sprintf("tok-%d", i+1)
		if string(tok) != want {
			t.Errorf("token[%d] = %q, want %q", i, tok, want)
		}
	}
}

// TestWorkerPool_MultipleWorkers verifies the concurrent invariant: with N
// workers, all events must be processed and the final MarkProcessed token
// must correspond to the last sequence (because the sequenceTracker waits
// for contiguous completion before advancing).
func TestWorkerPool_MultipleWorkers(t *testing.T) {
	src := &mockSource{}

	var processCount atomic.Int32

	process := func(_ context.Context, _ client.Event) error {
		processCount.Add(1)
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newWorkerPool(4, process, src, cancel)

	const numEvents = 50

	go func() {
		for i := uint64(1); i <= numEvents; i++ {
			pool.dispatch(ctx, workItem{
				seq:         i,
				resumeToken: []byte(fmt.Sprintf("tok-%d", i)),
			})
		}

		pool.closeDispatch()
	}()

	if err := pool.run(ctx); err != nil {
		t.Fatalf("run() returned error: %v", err)
	}

	if int(processCount.Load()) != numEvents {
		t.Fatalf("process called %d times, want %d", processCount.Load(), numEvents)
	}

	// The final token must be tok-N regardless of worker completion order,
	// because the tracker only advances past contiguous sequences.
	tokens := src.getTokens()
	if len(tokens) == 0 {
		t.Fatal("MarkProcessed was never called")
	}

	lastToken := string(tokens[len(tokens)-1])
	if lastToken != fmt.Sprintf("tok-%d", numEvents) {
		t.Fatalf("last token = %q, want %q", lastToken, fmt.Sprintf("tok-%d", numEvents))
	}
}

// TestWorkerPool_ProcessError verifies that a worker's process failure
// propagates as a fatal error from run(), stopping the pool.
func TestWorkerPool_ProcessError(t *testing.T) {
	src := &mockSource{}
	errBoom := errors.New("process exploded")

	process := func(_ context.Context, _ client.Event) error {
		return errBoom
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newWorkerPool(1, process, src, cancel)

	go func() {
		pool.dispatch(ctx, workItem{
			seq:         1,
			resumeToken: []byte("tok-1"),
		})

		pool.closeDispatch()
	}()

	err := pool.run(ctx)
	if err == nil {
		t.Fatal("expected error from run(), got nil")
	}

	if !errors.Is(err, errBoom) {
		t.Fatalf("expected wrapped errBoom, got: %v", err)
	}
}

// TestWorkerPool_MarkProcessedError verifies that a store failure during
// resume token advancement surfaces from run() so the exporter can restart
// from the last committed position.
func TestWorkerPool_MarkProcessedError(t *testing.T) {
	errStore := errors.New("store unavailable")
	src := &mockSource{markErr: errStore}

	process := func(_ context.Context, _ client.Event) error {
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newWorkerPool(1, process, src, cancel)

	go func() {
		pool.dispatch(ctx, workItem{
			seq:         1,
			resumeToken: []byte("tok-1"),
		})

		pool.closeDispatch()
	}()

	err := pool.run(ctx)
	if err == nil {
		t.Fatal("expected error from run(), got nil")
	}

	if !errors.Is(err, errStore) {
		t.Fatalf("expected wrapped errStore, got: %v", err)
	}
}

// TestWorkerPool_CancelEmitsResultsForDequeuedItems verifies that workers report
// a result for every dequeued sequence after cancellation instead of silently
// dropping in-flight work (which would advance resume incorrectly / hang).
func TestWorkerPool_CancelEmitsResultsForDequeuedItems(t *testing.T) {
	src := &mockSource{}

	started := make(chan struct{})
	release := make(chan struct{})

	var processCount atomic.Int32

	process := func(ctx context.Context, _ client.Event) error {
		if processCount.Add(1) == 1 {
			close(started)
			<-release

			return errors.New("fatal worker failure")
		}

		<-ctx.Done()

		return ctx.Err()
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	pool := newWorkerPool(2, process, src, cancel)

	go func() {
		for i := uint64(1); i <= 4; i++ {
			pool.dispatch(ctx, workItem{
				seq:         i,
				resumeToken: []byte(fmt.Sprintf("tok-%d", i)),
			})
		}

		pool.closeDispatch()
	}()

	done := make(chan error, 1)

	go func() {
		done <- pool.run(ctx)
	}()

	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first worker to start")
	}

	close(release)

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected fatal error from run(), got nil")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("run() hung after cancel; dequeued items likely dropped without results")
	}
}
