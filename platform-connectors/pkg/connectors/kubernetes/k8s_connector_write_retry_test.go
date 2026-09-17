// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package kubernetes

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	clientretry "k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// writeFailureTransport fails selected requests before forwarding successful writes
// to envtest. This exercises the real API, Event counts, and production retry layers.
type writeFailureTransport struct {
	base          http.RoundTripper
	mu            sync.Mutex
	calls         map[string]int
	transientNode string
	permanentNode string
}

func (f *writeFailureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodPost && req.Method != http.MethodPut {
		return f.base.RoundTrip(req)
	}

	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read test request: %w", err)
	}
	if err := req.Body.Close(); err != nil {
		return nil, fmt.Errorf("close test request: %w", err)
	}
	req.Body = io.NopCloser(bytes.NewReader(body))

	var obj struct {
		Metadata       metav1.ObjectMeta      `json:"metadata"`
		InvolvedObject corev1.ObjectReference `json:"involvedObject"`
	}
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil, fmt.Errorf("decode test request: %w", err)
	}
	node := obj.Metadata.Name
	if strings.HasSuffix(req.URL.Path, "/events") {
		node = obj.InvolvedObject.Name
	}

	f.mu.Lock()
	f.calls[node]++
	attempt := f.calls[node]
	f.mu.Unlock()

	var failure *apierrors.StatusError
	switch {
	case node == f.permanentNode:
		failure = &apierrors.StatusError{ErrStatus: metav1.Status{
			Status: metav1.StatusFailure, Reason: metav1.StatusReasonForbidden, Code: http.StatusForbidden,
			Message: "permanent write failure",
		}}
	case node == f.transientNode && attempt <= clientretry.DefaultRetry.Steps:
		failure = apierrors.NewServiceUnavailable("temporary write failure")
	default:
		return f.base.RoundTrip(req)
	}

	data, err := json.Marshal(failure.ErrStatus)
	if err != nil {
		return nil, fmt.Errorf("encode test failure: %w", err)
	}
	return &http.Response{StatusCode: int(failure.ErrStatus.Code),
		Header: http.Header{"Content-Type": []string{"application/json"}},
		Body:   io.NopCloser(bytes.NewReader(data)), Request: req}, nil
}

// TestProcessHealthEventsWithRetry_PartialWrites_PreserveCountsAndRetryIndependentErrors
// proves both reviewer examples against persisted Kubernetes objects in both error orders.
func TestProcessHealthEventsWithRetry_PartialWrites_PreserveCountsAndRetryIndependentErrors(t *testing.T) {
	testEnv, cli := setupEnvtest(t)
	t.Cleanup(func() { require.NoError(t, testEnv.Stop()) })

	for i, test := range []struct {
		name      string
		permanent bool
		reverse   bool
		condition bool
	}{
		{name: "successful event is not counted twice"},
		{name: "permanent before transient", permanent: true},
		{name: "transient before permanent", permanent: true, reverse: true},
		{name: "successful condition is not written twice", condition: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := t.Context()
			nodeA, nodeB := fmt.Sprintf("partial-%d-a", i), fmt.Sprintf("partial-%d-b", i)
			_, err := cli.CoreV1().Nodes().Create(ctx, &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeA}}, metav1.CreateOptions{})
			require.NoError(t, err)
			transport := &writeFailureTransport{calls: make(map[string]int), transientNode: nodeB}
			if test.permanent {
				transport.permanentNode = nodeA
			}
			cfg := rest.CopyConfig(testEnv.Config)
			cfg.ContentType = "application/json"
			cfg.AcceptContentTypes = "application/json"
			cfg.WrapTransport = func(base http.RoundTripper) http.RoundTripper { transport.base = base; return transport }
			client, err := kubernetes.NewForConfig(cfg)
			require.NoError(t, err)
			connector := NewK8sConnector(client, nil, nil, ctx, defaultConnectorConfig)
			connector.retryBaseDelay, connector.retryMaxDelay = time.Nanosecond, time.Nanosecond
			events := []*protos.HealthEvent{
				{NodeName: nodeA, Agent: "retry-test", CheckName: "GPUWarning", IsFatal: test.condition,
					GeneratedTimestamp: timestamppb.Now()},
				{NodeName: nodeB, Agent: "retry-test", CheckName: "GPUWarning", GeneratedTimestamp: timestamppb.Now()},
			}
			if test.reverse {
				events[0], events[1] = events[1], events[0]
			}

			retries, err := connector.processHealthEventsWithRetry(ctx, &protos.HealthEvents{Events: events})
			if test.permanent {
				require.True(t, apierrors.IsForbidden(err), "permanent error must remain visible: %v", err)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, 1, retries)
			require.Equal(t, 1, transport.calls[nodeA], "completed or permanent writes must not be replayed")
			require.Equal(t, clientretry.DefaultRetry.Steps+1, transport.calls[nodeB])
			stored, err := cli.CoreV1().Events(DefaultNamespace).List(ctx, metav1.ListOptions{})
			require.NoError(t, err)
			counts := make(map[string]int32)
			for _, event := range stored.Items {
				counts[event.InvolvedObject.Name] += event.Count
			}
			require.EqualValues(t, 1, counts[nodeB])
			if !test.condition && !test.permanent {
				require.EqualValues(t, 1, counts[nodeA], "the successful occurrence must remain Count=1")
			}
			if test.condition {
				node, err := cli.CoreV1().Nodes().Get(ctx, nodeA, metav1.GetOptions{})
				require.NoError(t, err)
				condition, _, found := findNodeCondition(node, "GPUWarning")
				require.True(t, found)
				require.Equal(t, corev1.ConditionTrue, condition.Status)
			}
		})
	}
}

// TestProcessHealthEventsWithRetry_MixedDrops_CountsWritesAndBatches checks bounded
// metric labels and that multiple failures of one reason count a batch only once.
func TestProcessHealthEventsWithRetry_MixedDrops_CountsWritesAndBatches(t *testing.T) {
	droppedWritesCounter.Reset()
	droppedBatchesCounter.Reset()
	connector := retryTestConnector(1, nil)
	permanent := fmt.Errorf("invalid event")
	transient := apierrors.NewServiceUnavailable("unavailable")
	connector.prepareWrites = func(context.Context, *protos.HealthEvents) []kubernetesWrite {
		return []kubernetesWrite{
			{operation: "node_event", run: func(context.Context) error { return permanent }},
			{operation: "node_event", run: func(context.Context) error { return permanent }},
			{operation: "node_condition", run: func(context.Context) error { return transient }},
			{operation: "node_condition", run: func(context.Context) error { return nil }},
		}
	}
	_, err := connector.processHealthEventsWithRetry(t.Context(), &protos.HealthEvents{})
	require.ErrorIs(t, err, permanent)
	require.ErrorIs(t, err, transient)
	require.Equal(t, 2.0, testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_event", "permanent_error")))
	require.Equal(t, 1.0, testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_condition", "retry_exhausted")))
	require.Equal(t, 1.0, testutil.ToFloat64(droppedBatchesCounter.WithLabelValues("permanent_error")))
	require.Equal(t, 1.0, testutil.ToFloat64(droppedBatchesCounter.WithLabelValues("retry_exhausted")))
}

// TestProcessHealthEventsWithRetry_Deadline_CoversCallsAndLaterWrites verifies API
// time consumes the shared batch budget and unattempted writes are observable.
func TestProcessHealthEventsWithRetry_Deadline_CoversCallsAndLaterWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		droppedWritesCounter.Reset()
		droppedBatchesCounter.Reset()
		connector := retryTestConnector(100, nil)
		connector.config.MaxRetryDuration = time.Minute
		secondCalls := 0
		connector.prepareWrites = func(context.Context, *protos.HealthEvents) []kubernetesWrite {
			return []kubernetesWrite{
				{operation: "node_condition", run: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }},
				{operation: "node_event", run: func(context.Context) error { secondCalls++; return nil }},
			}
		}
		start := time.Now()
		_, err := connector.processHealthEventsWithRetry(t.Context(), &protos.HealthEvents{})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, time.Minute, time.Since(start))
		require.Zero(t, secondCalls)
		require.Equal(t, 1.0, testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_condition", "retry_timeout")))
		require.Equal(t, 1.0, testutil.ToFloat64(droppedWritesCounter.WithLabelValues("node_event", "retry_timeout")))
		require.Equal(t, 1.0, testutil.ToFloat64(droppedBatchesCounter.WithLabelValues("retry_timeout")))
	})
}

// TestProcessHealthEventsWithRetry_StopDuringCall_CancelsImmediately checks that
// stopCh propagates into an in-flight operation before the root context is canceled.
func TestProcessHealthEventsWithRetry_StopDuringCall_CancelsImmediately(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stopCh := make(chan struct{})
		connector := retryTestConnector(100, func(ctx context.Context, _ *protos.HealthEvents) error {
			<-ctx.Done()
			return ctx.Err()
		})
		connector.stopCh = stopCh
		result := make(chan error, 1)
		start := time.Now()
		go func() {
			_, err := connector.processHealthEventsWithRetry(t.Context(), &protos.HealthEvents{})
			result <- err
		}()
		synctest.Wait()
		close(stopCh)
		synctest.Wait()
		require.Len(t, result, 1)
		require.ErrorIs(t, <-result, context.Canceled)
		require.Zero(t, time.Since(start))
	})
}

// processHealthEvents executes one pass for existing focused condition/Event tests.
// The production queue path and its retries are exercised by the retry regressions.
func (r *K8sConnector) processHealthEvents(ctx context.Context, events *protos.HealthEvents) error {
	var failures []error
	for _, write := range r.prepareHealthEventWrites(ctx, events) {
		if err := write.run(ctx); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// TestProcessHealthEventsWithRetry_LongOutage_HoldsCurrentBatchUntilRecovery
// checks that an outage beyond the former 3.5-second window retains the current
// batch. The queue ordering tests separately exercise the ring buffer consumer.
func TestProcessHealthEventsWithRetry_LongOutage_HoldsCurrentBatchUntilRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		connector := NewK8sConnector(nil, nil, nil, t.Context(), K8sConnectorConfig{})
		available := make(chan struct{})
		completed := make(chan error, 1)
		var calls atomic.Int32
		connector.prepareWrites = retryTestWrites(func(_ context.Context, _ *protos.HealthEvents) error {
			calls.Add(1)
			select {
			case <-available:
				return nil
			default:
				return apierrors.NewServiceUnavailable("API is restarting")
			}
		})
		start := time.Now()
		go func() {
			_, err := connector.processHealthEventsWithRetry(t.Context(), &protos.HealthEvents{})
			completed <- err
		}()
		synctest.Wait()
		<-time.After(30 * time.Second)
		synctest.Wait()
		require.Greater(t, calls.Load(), int32(4))
		require.Empty(t, completed, "the current batch must still be pending")
		close(available)
		require.NoError(t, <-completed)
		require.GreaterOrEqual(t, time.Since(start), 30*time.Second)
		require.Less(t, time.Since(start), DefaultMaxRetryDuration)
	})
}

// TestRetryKubernetesAPIWrite_DeadlineDuringInnerBackoff_StopsAtDeadline ensures
// the client's short retries cannot outlive the whole-batch budget.
func TestRetryKubernetesAPIWrite_DeadlineDuringInnerBackoff_StopsAtDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Millisecond)
		defer cancel()
		calls := 0
		start := time.Now()
		err := retryKubernetesAPIWrite(ctx, isKubernetesConnectorRetryableError, func() error {
			calls++
			return apierrors.NewServiceUnavailable("unavailable")
		})
		require.ErrorIs(t, err, context.DeadlineExceeded)
		require.Equal(t, 1, calls)
		require.Equal(t, time.Millisecond, time.Since(start))
	})
}

// TestInitializeK8sConnector_InvalidRetryDuration_ReturnsError checks both boundaries before client initialization.
func TestInitializeK8sConnector_InvalidRetryDuration_ReturnsError(t *testing.T) {
	for _, duration := range []time.Duration{-time.Nanosecond, MaxAllowedRetryDuration + time.Nanosecond} {
		t.Run(duration.String(), func(t *testing.T) {
			cfg := defaultConnectorConfig
			cfg.MaxRetryDuration = duration
			_, _, err := InitializeK8sConnector(t.Context(), nil, 1, 1, nil, cfg, "")
			require.ErrorContains(t, err, "maxRetryDuration must be between")
		})
	}
}
