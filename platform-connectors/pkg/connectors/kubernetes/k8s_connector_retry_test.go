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

package kubernetes

import (
	"context"
	"fmt"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	clientretry "k8s.io/client-go/util/retry"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

// retryTestConnector uses short delays for retry-policy tests without API calls.
func retryTestConnector(
	maxRetries int,
	process func(context.Context, *protos.HealthEvents) error,
) *K8sConnector {
	return &K8sConnector{
		config:         K8sConnectorConfig{MaxRetries: maxRetries},
		processEvents:  process,
		retryBaseDelay: time.Nanosecond,
		retryMaxDelay:  time.Nanosecond,
	}
}

// TestNewK8sConnector_RetryConfiguration_DefaultsOrPreserves verifies constructor retry defaults and overrides.
func TestNewK8sConnector_RetryConfiguration_DefaultsOrPreserves(t *testing.T) {
	for _, test := range []struct {
		name       string
		configured int
		want       int
	}{
		{name: "zero selects default", want: DefaultMaxRetries},
		{name: "positive override", configured: 20, want: 20},
	} {
		t.Run(test.name, func(t *testing.T) {
			connector := NewK8sConnector(nil, nil, nil, context.Background(), K8sConnectorConfig{MaxRetries: test.configured})
			require.Equal(t, test.want, connector.config.MaxRetries)
		})
	}
}

// TestInitializeK8sConnector_NegativeMaxRetries_ReturnsError verifies invalid retry limits fail initialization.
func TestInitializeK8sConnector_NegativeMaxRetries_ReturnsError(t *testing.T) {
	_, _, err := InitializeK8sConnector(
		context.Background(), nil, 1, 1, nil,
		K8sConnectorConfig{
			MaxNodeConditionMessageLength: 1,
			CompactedHealthEventMsgLen:    1,
			MaxRetries:                    -1,
		},
		"",
	)

	require.EqualError(t, err, "maxRetries must not be negative, got -1")
}

// TestProcessHealthEventsWithRetry_RetryScenarios_EnforcePolicy verifies retry bounds, error classification, and cancellation.
func TestProcessHealthEventsWithRetry_RetryScenarios_EnforcePolicy(t *testing.T) {
	unavailable := apierrors.NewServiceUnavailable("temporarily unavailable")
	notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "node-a")
	conflict := apierrors.NewConflict(schema.GroupResource{Resource: "nodes"}, "node-a", fmt.Errorf("stale version"))
	tests := []struct {
		name          string
		maxRetries    int
		attemptErrors []error
		canceled      bool
		wantRetries   int
		wantCalls     int
		wantErr       error
	}{
		{name: "initial success", maxRetries: 3, attemptErrors: []error{nil}, wantCalls: 1},
		{name: "transient failure succeeds", maxRetries: 3, attemptErrors: []error{unavailable, nil},
			wantRetries: 1, wantCalls: 2},
		{name: "conflict succeeds", maxRetries: 3, attemptErrors: []error{conflict, nil},
			wantRetries: 1, wantCalls: 2},
		{name: "permanent failure is not retried", maxRetries: 3,
			attemptErrors: []error{fmt.Errorf("update node status: %w", notFound)}, wantCalls: 1, wantErr: notFound},
		{name: "transient failure stops at the retry bound", maxRetries: 2, attemptErrors: []error{unavailable},
			wantRetries: 2, wantCalls: 3, wantErr: unavailable},
		{name: "canceled context stops retries", maxRetries: 3, attemptErrors: []error{unavailable},
			canceled: true, wantCalls: 1, wantErr: context.Canceled},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if test.canceled {
				cancel()
			}

			calls := 0
			connector := retryTestConnector(test.maxRetries, func(context.Context, *protos.HealthEvents) error {
				calls++
				return test.attemptErrors[min(calls-1, len(test.attemptErrors)-1)]
			})

			retries, err := connector.processHealthEventsWithRetry(ctx, &protos.HealthEvents{})
			if test.wantErr == nil {
				require.NoError(t, err)
			} else {
				require.ErrorIs(t, err, test.wantErr)
			}
			require.Equal(t, test.wantRetries, retries)
			require.Equal(t, test.wantCalls, calls)
		})
	}
}

// TestProcessHealthEventsWithRetry_InterruptionDuringBackoff_ReturnsCanceled verifies both shutdown signals
// interrupt a pending timer without waiting for the retry delay or starting another attempt.
func TestProcessHealthEventsWithRetry_InterruptionDuringBackoff_ReturnsCanceled(t *testing.T) {
	tests := []struct {
		name      string
		interrupt func(context.CancelFunc, chan struct{})
	}{
		{name: "context cancellation", interrupt: func(cancel context.CancelFunc, _ chan struct{}) { cancel() }},
		{name: "connector shutdown", interrupt: func(_ context.CancelFunc, stopCh chan struct{}) { close(stopCh) }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				stopCh := make(chan struct{})
				defer func() {
					select {
					case <-stopCh:
					default:
						close(stopCh)
					}
				}()

				calls := 0
				connector := retryTestConnector(3, func(context.Context, *protos.HealthEvents) error {
					calls++
					return apierrors.NewServiceUnavailable("unavailable during shutdown")
				})
				connector.stopCh = stopCh
				connector.retryBaseDelay = time.Minute
				connector.retryMaxDelay = time.Minute

				type result struct {
					retries int
					err     error
				}
				resultCh := make(chan result, 1)
				start := time.Now()
				go func() {
					retries, err := connector.processHealthEventsWithRetry(ctx, &protos.HealthEvents{})
					resultCh <- result{retries: retries, err: err}
				}()

				// Wait for the worker to block on the real retry timer before interrupting it.
				synctest.Wait()
				require.Equal(t, 1, calls)
				require.Empty(t, resultCh)
				test.interrupt(cancel, stopCh)
				synctest.Wait()

				require.Len(t, resultCh, 1, "retry backoff did not stop promptly")
				got := <-resultCh
				require.ErrorIs(t, got.err, context.Canceled)
				require.Equal(t, 1, got.retries)
				require.Equal(t, 1, calls)
				require.Zero(t, time.Since(start), "shutdown must not wait for the retry timer")
			})
		})
	}
}

// TestProcessHealthEventsWithRetry_ProductionDelays_MatchDocumentedHorizon verifies the default and
// operator-configured retry budgets with the actual exponential backoff and delay cap.
func TestProcessHealthEventsWithRetry_ProductionDelays_MatchDocumentedHorizon(t *testing.T) {
	for _, test := range []struct {
		name        string
		maxRetries  int
		wantRetries int
		wantDelay   time.Duration
	}{
		{name: "default", wantRetries: 3, wantDelay: 3500 * time.Millisecond},
		{name: "longer control plane outage", maxRetries: 20, wantRetries: 20, wantDelay: 54500 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				connector := NewK8sConnector(nil, nil, nil, context.Background(), K8sConnectorConfig{MaxRetries: test.maxRetries})
				unavailable := apierrors.NewServiceUnavailable("control plane unavailable")
				calls := 0
				connector.processEvents = func(context.Context, *protos.HealthEvents) error {
					calls++
					return unavailable
				}

				start := time.Now()
				retries, err := connector.processHealthEventsWithRetry(context.Background(), &protos.HealthEvents{})
				require.ErrorIs(t, err, unavailable)
				require.Equal(t, test.wantRetries, retries)
				require.Equal(t, test.wantRetries+1, calls)
				require.Equal(t, test.wantDelay, time.Since(start))
			})
		})
	}
}

// TestFetchAndProcessHealthMetric_TransientFaultFailure_PreservesFaultRecoveryOrder verifies retries retain queue order.
func TestFetchAndProcessHealthMetric_TransientFaultFailure_PreservesFaultRecoveryOrder(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	buffer := ringbuffer.NewRingBuffer("kubernetes-ordered-retry", ctx)
	stopCh := make(chan struct{})
	connector := NewK8sConnector(nil, buffer, stopCh, ctx, K8sConnectorConfig{MaxRetries: 1})
	connector.retryBaseDelay = time.Nanosecond
	connector.retryMaxDelay = time.Nanosecond

	attempts := make([]string, 0, 3)
	faultFailed := false
	recoveryProcessed := make(chan struct{})
	connector.processEvents = func(_ context.Context, events *protos.HealthEvents) error {
		state := "fault"
		if events.GetEvents()[0].GetIsHealthy() {
			state = "recovery"
		}
		attempts = append(attempts, state)

		if state == "fault" && !faultFailed {
			faultFailed = true
			return apierrors.NewServiceUnavailable("retry the fault first")
		}

		if state == "recovery" {
			close(recoveryProcessed)
		}

		return nil
	}

	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{Events: []*protos.HealthEvent{{
		CheckName:          "DerivedCondition",
		IsFatal:            true,
		ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
	}}}))
	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{Events: []*protos.HealthEvent{{
		CheckName:          "DerivedCondition",
		IsHealthy:          true,
		ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
	}}}))

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		connector.FetchAndProcessHealthMetric(ctx)
	}()

	select {
	case <-recoveryProcessed:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for recovery processing")
	}

	buffer.ShutDownHealthMetricQueue()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connector shutdown")
	}

	require.Equal(t, []string{"fault", "fault", "recovery"}, attempts)
}

// TestFetchAndProcessHealthMetric_ClientGoRetryExhaustion_PersistsRecovery verifies outer retries persist a recovery.
func TestFetchAndProcessHealthMetric_ClientGoRetryExhaustion_PersistsRecovery(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	const (
		nodeName      = "retry-recovery-node"
		conditionType = corev1.NodeConditionType("DerivedCondition")
	)

	client := fake.NewSimpleClientset(&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: nodeName}})
	recoveryFailures := 0
	client.Fake.PrependReactor("update", "nodes", func(action k8stesting.Action) (bool, runtime.Object, error) {
		updatedNode := action.(k8stesting.UpdateAction).GetObject().(*corev1.Node)
		condition, _, found := findNodeCondition(updatedNode, conditionType)
		if found && condition.Status == corev1.ConditionFalse && recoveryFailures < clientretry.DefaultRetry.Steps {
			recoveryFailures++
			return true, nil, apierrors.NewServiceUnavailable("recovery write unavailable")
		}

		return false, nil, nil
	})

	buffer := ringbuffer.NewRingBuffer("kubernetes-client-recovery-retry", ctx)
	connector := NewK8sConnector(
		client,
		buffer,
		make(chan struct{}),
		ctx,
		K8sConnectorConfig{
			MaxNodeConditionMessageLength: 1024,
			CompactedHealthEventMsgLen:    72,
			MaxRetries:                    1,
		},
	)
	connector.retryBaseDelay = time.Nanosecond
	connector.retryMaxDelay = time.Nanosecond
	faultTime := timestamppb.Now()
	recoveryTime := timestamppb.New(faultTime.AsTime().Add(time.Second))

	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{Events: []*protos.HealthEvent{{
		Agent:              "health-events-analyzer",
		CheckName:          string(conditionType),
		NodeName:           nodeName,
		IsFatal:            true,
		GeneratedTimestamp: faultTime,
		ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
	}}}))
	buffer.Enqueue(ringbuffer.NewQueuedHealthEvents(&protos.HealthEvents{Events: []*protos.HealthEvent{{
		Agent:              "health-events-analyzer",
		CheckName:          string(conditionType),
		NodeName:           nodeName,
		IsHealthy:          true,
		GeneratedTimestamp: recoveryTime,
		ProcessingStrategy: protos.ProcessingStrategy_EXECUTE_REMEDIATION,
	}}}))

	exited := make(chan struct{})
	go func() {
		defer close(exited)
		connector.FetchAndProcessHealthMetric(ctx)
	}()

	require.Eventually(t, func() bool {
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			return false
		}

		condition, _, found := findNodeCondition(node, conditionType)
		return found && condition.Status == corev1.ConditionFalse
	}, 5*time.Second, 10*time.Millisecond)

	buffer.ShutDownHealthMetricQueue()
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for connector shutdown")
	}

	require.Equal(t, clientretry.DefaultRetry.Steps, recoveryFailures)
}
