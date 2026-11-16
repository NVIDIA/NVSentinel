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
	dto "github.com/prometheus/client_model/go"
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

func TestReadiness_DatastoreStaysReadyWhileTrippedBreakerHaltsProcessing(t *testing.T) {
	checker := server.NewDatastoreReadinessChecker(prometheus.NewRegistry())
	checker.SetWatcher(fakeLagWatcher{})
	cb := trippedStartupBreaker{entered: make(chan struct{}), release: make(chan struct{})}
	r := &Reconciler{
		config: ReconcilerConfig{CircuitBreakerEnabled: true},
		cb:     cb,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type haltResult struct {
		halted bool
		err    error
	}
	processingDone := make(chan haltResult, 1)
	go func() {
		halted, err := r.checkCircuitBreakerAndHalt(ctx)
		processingDone <- haltResult{halted: halted, err: err}
	}()

	select {
	case <-cb.entered:
	case <-time.After(time.Second):
		t.Fatal("event processing did not inspect the TRIPPED circuit breaker")
	}
	// The datastore is healthy even while the breaker prevents event processing.
	require.NoError(t, checker.Ready(ctx))
	requireDatastoreConnected(t, checker, 1)
	close(cb.release)
	select {
	case result := <-processingDone:
		t.Fatalf("TRIPPED breaker should halt processing until cancellation, got %+v", result)
	case <-time.After(20 * time.Millisecond):
	}

	cancel()
	select {
	case result := <-processingDone:
		require.True(t, result.halted)
		require.NoError(t, result.err)
		require.NoError(t, checker.Ready(context.Background()))
		requireDatastoreConnected(t, checker, 1)
	case <-time.After(time.Second):
		t.Fatal("TRIPPED breaker did not stop event processing after cancellation")
	}
}

func requireDatastoreConnected(t *testing.T, checker *server.DatastoreReadinessChecker, want float64) {
	t.Helper()

	metrics := make(chan prometheus.Metric, 1)
	checker.Collect(metrics)
	metric := <-metrics
	var collected dto.Metric
	require.NoError(t, metric.Write(&collected))
	require.Equal(t, want, collected.GetGauge().GetValue())
}
