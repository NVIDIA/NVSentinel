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
	"time"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

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

func TestNewK8sConnectorDefaultsMaxRetries(t *testing.T) {
	connector := NewK8sConnector(nil, nil, nil, context.Background(), K8sConnectorConfig{})

	require.Equal(t, DefaultMaxRetries, connector.config.MaxRetries)
}

func TestInitializeK8sConnectorRejectsNegativeMaxRetries(t *testing.T) {
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

func TestProcessHealthEventsWithRetry(t *testing.T) {
	t.Run("transient failure succeeds", func(t *testing.T) {
		calls := 0
		connector := retryTestConnector(3, func(context.Context, *protos.HealthEvents) error {
			calls++
			if calls == 1 {
				return apierrors.NewServiceUnavailable("temporarily unavailable")
			}

			return nil
		})

		retries, err := connector.processHealthEventsWithRetry(context.Background(), &protos.HealthEvents{})
		require.NoError(t, err)
		require.Equal(t, 1, retries)
		require.Equal(t, 2, calls)
	})

	t.Run("permanent failure is not retried", func(t *testing.T) {
		calls := 0
		notFound := apierrors.NewNotFound(schema.GroupResource{Resource: "nodes"}, "node-a")
		connector := retryTestConnector(3, func(context.Context, *protos.HealthEvents) error {
			calls++
			return fmt.Errorf("update node status: %w", notFound)
		})

		retries, err := connector.processHealthEventsWithRetry(context.Background(), &protos.HealthEvents{})
		require.ErrorIs(t, err, notFound)
		require.Equal(t, 0, retries)
		require.Equal(t, 1, calls)
	})

	t.Run("transient failure stops at the retry bound", func(t *testing.T) {
		calls := 0
		connector := retryTestConnector(2, func(context.Context, *protos.HealthEvents) error {
			calls++
			return apierrors.NewServiceUnavailable("still unavailable")
		})

		retries, err := connector.processHealthEventsWithRetry(context.Background(), &protos.HealthEvents{})
		require.Error(t, err)
		require.Equal(t, 2, retries)
		require.Equal(t, 3, calls)
	})

	t.Run("context cancellation stops retries", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		calls := 0
		connector := retryTestConnector(3, func(context.Context, *protos.HealthEvents) error {
			calls++
			return apierrors.NewServiceUnavailable("unavailable")
		})

		retries, err := connector.processHealthEventsWithRetry(ctx, &protos.HealthEvents{})
		require.ErrorIs(t, err, context.Canceled)
		require.Equal(t, 0, retries)
		require.Equal(t, 1, calls)
	})
}

func TestFetchAndProcessHealthMetricRetriesBeforeDequeuingRecovery(t *testing.T) {
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
