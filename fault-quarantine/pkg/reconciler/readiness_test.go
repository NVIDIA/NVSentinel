// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package reconciler

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/commons/pkg/server"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/breaker"
)

type fakeLagWatcher struct {
	lastEmptyBatch time.Time
	lastEventRead  time.Time
}

func (f fakeLagWatcher) LagState() (lastEmptyBatch, lastEventRead time.Time) {
	return f.lastEmptyBatch, f.lastEventRead
}

func (fakeLagWatcher) IsReady() bool { return true }

type trippedStartupBreaker struct {
	entered chan struct{}
	release chan struct{}
}

func (trippedStartupBreaker) AddCordonEvent(string) {}
func (b trippedStartupBreaker) IsTripped(ctx context.Context) (bool, error) {
	if b.entered != nil {
		close(b.entered)
	}
	if b.release != nil {
		select {
		case <-b.release:
		case <-ctx.Done():
		}
	}

	return true, nil
}
func (trippedStartupBreaker) ForceState(context.Context, breaker.State) error { return nil }
func (trippedStartupBreaker) CurrentState() breaker.State                     { return breaker.StateTripped }
func (trippedStartupBreaker) GetCursorMode(context.Context) (breaker.CursorMode, error) {
	return breaker.CursorModeResume, nil
}
func (trippedStartupBreaker) SetCursorMode(context.Context, breaker.CursorMode) error { return nil }

func TestReconciler_ReadinessChecker(t *testing.T) {
	r := &Reconciler{}
	checker := server.NewDatastoreReadinessChecker(prometheus.NewRegistry())
	r.SetReadinessChecker(checker)

	ctx := context.Background()

	// Initial: returns error
	err := checker.Ready(ctx)
	require.Error(t, err)

	// Simulating setupChangeStreamWatcher completing with a connected watcher
	r.readinessChecker.SetWatcher(fakeLagWatcher{})

	err = checker.Ready(ctx)
	require.NoError(t, err)
}

func TestReadiness_DatastoreCanBeReadyWhileTrippedStartupHaltsRollout(t *testing.T) {
	checker := server.NewDatastoreReadinessChecker(prometheus.NewRegistry())
	checker.SetWatcher(fakeLagWatcher{})
	cb := trippedStartupBreaker{entered: make(chan struct{}), release: make(chan struct{})}
	r := &Reconciler{
		config: ReconcilerConfig{CircuitBreakerEnabled: true},
		cb:     cb,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startupDone := make(chan error, 1)
	go func() { startupDone <- r.checkCircuitBreakerAtStartup(ctx) }()

	select {
	case <-cb.entered:
	case <-time.After(time.Second):
		t.Fatal("startup did not inspect the TRIPPED circuit breaker")
	}
	// Readiness reports datastore capability, not whether Fault Quarantine is dequeuing events.
	require.NoError(t, checker.Ready(ctx))
	close(cb.release)
	select {
	case err := <-startupDone:
		t.Fatalf("TRIPPED startup should halt until cancellation, got %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	select {
	case err := <-startupDone:
		require.ErrorContains(t, err, "circuit breaker is TRIPPED at startup")
	case <-time.After(time.Second):
		t.Fatal("TRIPPED startup did not stop after cancellation")
	}
}
