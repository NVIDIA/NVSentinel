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

package kubernetes

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"go.opentelemetry.io/otel/attribute"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/kubernetes"

	"github.com/nvidia/nvsentinel/commons/pkg/auditlogger"
	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/kubeconfig"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
)

/*
In the code coverage report, this file is contributing only 4%. Reason is most of the code in this part is
initializing the k8sClientset from kubernetes config   and since in unit tests, it is there is no k8s cluster,
hence it is complex to test this. Hence, ignoring this initilization part for now as part of unit testing
Hence, ignoring this file as part of unit testing for now.
*/

// K8sConnectorConfig holds tunable parameters for the K8sConnector.
type K8sConnectorConfig struct {
	MaxNodeConditionMessageLength int64
	CompactedHealthEventMsgLen    int64
	MaxRetries                    int
}

// DefaultMaxRetries is the number of ordered outer retries after Kubernetes
// client-go's short in-process retry window is exhausted.
const DefaultMaxRetries = 3

type K8sConnector struct {
	clientset  kubernetes.Interface
	ringBuffer *ringbuffer.RingBuffer
	stopCh     <-chan struct{}
	ctx        context.Context
	config     K8sConnectorConfig

	processEvents  func(context.Context, *protos.HealthEvents) error
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration

	// nodeEventNames caches the last written event name per dedupe key;
	// see writeNodeEvent. nodeEventMu guards only the lazy init.
	nodeEventMu    sync.Mutex
	nodeEventNames *expirable.LRU[string, string]
}

// NewK8sConnector creates a K8sConnector with the given Kubernetes client, ring buffer, and configuration.
func NewK8sConnector(
	client kubernetes.Interface,
	ringBuffer *ringbuffer.RingBuffer,
	stopCh <-chan struct{}, ctx context.Context,
	cfg K8sConnectorConfig) *K8sConnector {
	if cfg.MaxRetries == 0 {
		cfg.MaxRetries = DefaultMaxRetries
	}

	connector := &K8sConnector{
		clientset:  client,
		ringBuffer: ringBuffer,
		stopCh:     stopCh,
		ctx:        ctx,
		config:     cfg,

		retryBaseDelay: ringbuffer.DefaultBaseDelay,
		retryMaxDelay:  ringbuffer.DefaultMaxDelay,
	}
	connector.processEvents = connector.processHealthEvents

	return connector
}

func InitializeK8sConnector(ctx context.Context, ringbuffer *ringbuffer.RingBuffer,
	qps float32, burst int, stopCh <-chan struct{}, cfg K8sConnectorConfig,
	kubeconfigPath string,
) (*K8sConnector, kubernetes.Interface, error) {
	if cfg.MaxNodeConditionMessageLength <= 0 {
		return nil, nil, fmt.Errorf("maxNodeConditionMessageLength must be greater than 0, got %d",
			cfg.MaxNodeConditionMessageLength)
	}

	if cfg.CompactedHealthEventMsgLen <= 0 {
		return nil, nil, fmt.Errorf("CompactedHealthEventMsgLen must be greater than 0, got %d",
			cfg.CompactedHealthEventMsgLen)
	}

	if cfg.MaxRetries < 0 {
		return nil, nil, fmt.Errorf("maxRetries must not be negative, got %d", cfg.MaxRetries)
	}

	config, err := kubeconfig.Load(kubeconfigPath)
	if err != nil {
		return nil, nil, err
	}

	config.Burst = burst
	config.QPS = qps

	config.Wrap(func(rt http.RoundTripper) http.RoundTripper {
		return auditlogger.NewAuditingRoundTripper(rt)
	})

	clientSet, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	kubernetesConnector := NewK8sConnector(clientSet, ringbuffer, stopCh, ctx, cfg)

	return kubernetesConnector, clientSet, nil
}

func (r *K8sConnector) FetchAndProcessHealthMetric(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "Context canceled, exiting Kubernetes connector processing loop")
			return
		case <-r.stopCh:
			slog.InfoContext(r.ctx, "k8sConnector queue received stop signal")
			return
		default:
			queuedHealthEvents, quit := r.ringBuffer.Dequeue()
			if quit {
				slog.InfoContext(ctx, "Queue signaled shutdown, exiting processing loop")
				return
			}

			r.processQueuedHealthEvents(ctx, queuedHealthEvents)
		}
	}
}

func (r *K8sConnector) processQueuedHealthEvents(
	ctx context.Context,
	queuedHealthEvents *ringbuffer.QueuedHealthEvents,
) {
	healthEvents := queuedHealthEvents.Events
	if healthEvents == nil || len(healthEvents.GetEvents()) == 0 {
		r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
		return
	}

	batchCtx, span := tracing.StartSpanWithLinkFromSpanContext(
		ctx, queuedHealthEvents.ParentSpanContext, "platform_connector.k8s.fetch_and_process_health_metric")
	defer span.End()

	retryCount, err := r.processHealthEventsWithRetry(batchCtx, healthEvents)
	if err == nil {
		r.ringBuffer.HealthMetricEleProcessingCompleted(queuedHealthEvents)
		return
	}

	tracing.RecordError(span, err)
	span.SetAttributes(
		attribute.String("platform_connector.k8s.error.type", "not_able_to_process_health_event"),
		attribute.String("platform_connector.k8s.error.message", err.Error()),
		attribute.Int("platform_connector.k8s.retry_count", retryCount),
		attribute.Int("platform_connector.k8s.max_retries", r.config.MaxRetries),
	)
	r.logTerminalProcessingFailure(batchCtx, healthEvents, retryCount, err)
	r.ringBuffer.HealthMetricEleProcessingFailed(queuedHealthEvents)
}

func (r *K8sConnector) logTerminalProcessingFailure(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
	retryCount int,
	err error,
) {
	switch {
	case ctx.Err() != nil:
		slog.InfoContext(ctx, "Kubernetes health event processing stopped with context cancellation",
			"error", err,
			"eventCount", len(healthEvents.GetEvents()))
	case isKubernetesConnectorRetryableError(err) && retryCount >= r.config.MaxRetries:
		slog.ErrorContext(ctx, "Max retries exceeded, dropping Kubernetes health events permanently",
			"error", err,
			"retryCount", retryCount,
			"maxRetries", r.config.MaxRetries,
			"eventCount", len(healthEvents.GetEvents()))
	default:
		slog.ErrorContext(ctx, "Non-retryable Kubernetes health event failure, dropping permanently",
			"error", err,
			"eventCount", len(healthEvents.GetEvents()))
	}
}

// processHealthEventsWithRetry holds the current batch while retrying so a newer
// fault or recovery cannot overtake it in the queue and reverse condition state.
func (r *K8sConnector) processHealthEventsWithRetry(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
) (int, error) {
	processEvents := r.processEvents
	if processEvents == nil {
		processEvents = r.processHealthEvents
	}

	retryCount := 0

	retryDelay := r.retryBaseDelay
	if retryDelay <= 0 {
		retryDelay = ringbuffer.DefaultBaseDelay
	}

	maxRetryDelay := r.retryMaxDelay

	if maxRetryDelay <= 0 {
		maxRetryDelay = ringbuffer.DefaultMaxDelay
	}

	for {
		err := processEvents(ctx, healthEvents)
		if err == nil {
			return retryCount, nil
		}

		if ctx.Err() != nil {
			return retryCount, ctx.Err()
		}

		if !isKubernetesConnectorRetryableError(err) || retryCount >= r.config.MaxRetries {
			return retryCount, err
		}

		retryCount++
		slog.WarnContext(ctx, "Kubernetes health event processing failed; retrying in order",
			"error", err,
			"retryCount", retryCount,
			"maxRetries", r.config.MaxRetries,
			"retryDelay", retryDelay)

		if err := waitForKubernetesRetry(ctx, retryDelay); err != nil {
			return retryCount, err
		}

		retryDelay = min(retryDelay*2, maxRetryDelay)
	}
}

func isKubernetesConnectorRetryableError(err error) bool {
	return apierrors.IsConflict(err) || isTemporaryError(err)
}

func waitForKubernetesRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
