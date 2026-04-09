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
	"fmt"
	"log/slog"
	"sync"

	"github.com/nvidia/nvsentinel/event-exporter/pkg/metrics"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

// workItem is dispatched from the consumer to a worker for processing.
type workItem struct {
	seq         uint64
	event       client.Event
	resumeToken []byte
}

// workResult is sent from a worker back to the token writer after processing.
type workResult struct {
	seq         uint64
	resumeToken []byte
	err         error
}

// processFunc is called by workers to handle an event (unmarshal, skip, or publish).
// Returning nil means the event was handled (published or intentionally skipped);
// returning a non-nil error is fatal and stops the pool.
type processFunc func(ctx context.Context, event client.Event) error

// workerPool manages concurrent event processing with ordered resume token advancement.
//
// Architecture:
//
//	Consumer (single goroutine)
//	    reads from change stream, assigns sequence numbers, dispatches to workers
//	         │
//	         ▼
//	    dispatchCh (buffered channel)
//	         │
//	    ┌────┴────┐
//	    ▼    ▼    ▼
//	   W-1  W-2 W-3  (N goroutines, process concurrently)
//	    │    │    │
//	    └────┬────┘
//	         ▼
//	    resultCh (buffered channel)
//	         │
//	         ▼
//	Token Writer (single goroutine)
//	    uses sequenceTracker to advance resume token only for contiguous completions
type workerPool struct {
	numWorkers int
	process    processFunc
	source     client.ChangeStreamWatcher
	cancel     context.CancelFunc

	dispatchCh chan workItem
	resultCh   chan workResult
}

func newWorkerPool(
	numWorkers int,
	process processFunc,
	source client.ChangeStreamWatcher,
	cancel context.CancelFunc,
) *workerPool {
	bufSize := numWorkers * 2

	return &workerPool{
		numWorkers: numWorkers,
		process:    process,
		source:     source,
		cancel:     cancel,
		dispatchCh: make(chan workItem, bufSize),
		resultCh:   make(chan workResult, bufSize),
	}
}

// run starts the worker pool and blocks until all work is done or the context is cancelled.
// The caller must close dispatchCh when no more items will be sent (i.e., when the event
// channel is closed). This method returns the first fatal error, or nil.
func (wp *workerPool) run(ctx context.Context) error {
	var workersWg sync.WaitGroup

	for range wp.numWorkers {
		workersWg.Go(func() {
			wp.worker(ctx)
		})
	}

	// Close resultCh after all workers finish so tokenWriter exits its range loop
	go func() {
		workersWg.Wait()
		close(wp.resultCh)
	}()

	return wp.tokenWriter(ctx)
}

// dispatch sends an event to the worker pool for processing.
// Returns false if the context was cancelled.
func (wp *workerPool) dispatch(ctx context.Context, item workItem) bool {
	select {
	case wp.dispatchCh <- item:
		return true
	case <-ctx.Done():
		return false
	}
}

// closeDispatch signals that no more items will be dispatched.
func (wp *workerPool) closeDispatch() {
	close(wp.dispatchCh)
}

// worker picks items from dispatchCh and processes them.
func (wp *workerPool) worker(ctx context.Context) {
	for item := range wp.dispatchCh {
		if ctx.Err() != nil {
			return
		}

		err := wp.process(ctx, item.event)
		wp.resultCh <- workResult{
			seq:         item.seq,
			resumeToken: item.resumeToken,
			err:         err,
		}
	}
}

// tokenWriter collects results from workers and advances the resume token in order.
// Returns the first fatal error (a worker failed to process after all retries).
func (wp *workerPool) tokenWriter(ctx context.Context) error {
	tracker := newSequenceTracker()

	for result := range wp.resultCh {
		if result.err != nil {
			wp.cancel()
			return fmt.Errorf("process failed for seq %d: %w", result.seq, result.err)
		}

		token := tracker.markCompleted(result.seq, result.resumeToken)
		if token == nil {
			continue
		}

		if err := wp.source.MarkProcessed(ctx, token); err != nil {
			slog.ErrorContext(ctx, "Failed to mark processed", "error", err)
			wp.cancel()

			return fmt.Errorf("mark processed: %w", err)
		}

		metrics.ResumeTokenUpdateTimestamp.SetToCurrentTime()

		slog.DebugContext(ctx, "Resume token advanced",
			"seq", result.seq,
			"pendingOutOfOrder", tracker.pending())
	}

	return nil
}
