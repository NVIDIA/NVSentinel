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
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
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
	// MaxRetries limits retries per write. Zero selects DefaultMaxRetries.
	MaxRetries int
	// MaxRetryDuration bounds processing of the whole batch, including API calls.
	MaxRetryDuration time.Duration
}

// DefaultMaxRetries allows retries throughout the default one-minute batch window.
const DefaultMaxRetries = 25

// DefaultMaxRetryDuration bounds the time a batch holds the Kubernetes queue.
const DefaultMaxRetryDuration = time.Minute

// MaxAllowedRetryDuration limits operator-configured batch windows to five minutes.
const MaxAllowedRetryDuration = 5 * time.Minute

// K8sConnector writes health events to the cluster as node conditions and
// Kubernetes Events. A batch costs API calls only when it changes what the
// cluster shows: the node status update is skipped when every condition would
// keep its status, reason and message, and the Event write is skipped for a
// fault whose Event was written less than nodeEventRefreshInterval ago. So a
// monitor that reports every cycle, or a resent batch, costs nothing until
// something changes; the condition's heartbeat time moves with those changes.
type K8sConnector struct {
	clientset  kubernetes.Interface
	ringBuffer *ringbuffer.RingBuffer
	stopCh     <-chan struct{}
	ctx        context.Context
	config     K8sConnectorConfig

	prepareWrites  func(context.Context, *protos.HealthEvents) []kubernetesWrite
	retryBaseDelay time.Duration
	retryMaxDelay  time.Duration

	// nodeEvents remembers, per node and check, the Kubernetes Events written
	// for its faults (message to Event name and write time); see
	// writeNodeEvent. nodeEventMu guards it, including the maps it holds.
	nodeEventMu sync.Mutex
	nodeEvents  *expirable.LRU[string, map[string]rememberedEvent]
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

	if cfg.MaxRetryDuration == 0 {
		cfg.MaxRetryDuration = DefaultMaxRetryDuration
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
	connector.prepareWrites = connector.prepareHealthEventWrites

	return connector
}

// InitializeK8sConnector validates configuration and constructs a connector with
// a Kubernetes client. Zero retries uses the default; negative values are rejected.
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

	if cfg.MaxRetryDuration < 0 || cfg.MaxRetryDuration > MaxAllowedRetryDuration {
		return nil, nil, fmt.Errorf("maxRetryDuration must be between 0 and %s, got %s",
			MaxAllowedRetryDuration, cfg.MaxRetryDuration)
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

// ProcessBatch applies one batch to the cluster: node conditions and
// Kubernetes Events for every processable event. It is the entry point for
// callers that hold no queue (the deployment platform connector) and returns
// failures to the caller for retry. The queued path retries in place.
func (r *K8sConnector) ProcessBatch(ctx context.Context, healthEvents *protos.HealthEvents) error {
	return r.processHealthEvents(ctx, healthEvents)
}

// FetchAndProcessHealthMetric processes batches sequentially, completing all
// retry attempts for the current batch before consuming another one.
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

// processQueuedHealthEvents records the terminal outcome and releases the batch.
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
		attribute.String("platform_connector.k8s.max_retry_duration", r.config.MaxRetryDuration.String()),
	)

	level := slog.LevelError
	if ctx.Err() != nil || errors.Is(err, context.Canceled) {
		level = slog.LevelInfo
	}

	slog.Log(batchCtx, level, "Kubernetes batch finished with unsuccessful writes",
		"error", err, "retryCount", retryCount, "eventCount", len(healthEvents.GetEvents()))
	// Discard releases the item; it never schedules a retry. Write outcomes were
	// already recorded individually before completing this batch.
	r.ringBuffer.Discard(queuedHealthEvents)
}

// processHealthEventsWithRetry holds the batch until its individual writes finish.
// Successful and permanent writes are never retried because another write fails.
func (r *K8sConnector) processHealthEventsWithRetry(
	ctx context.Context,
	healthEvents *protos.HealthEvents,
) (int, error) {
	prepareWrites := r.prepareWrites
	if prepareWrites == nil {
		prepareWrites = r.prepareHealthEventWrites
	}

	writes := prepareWrites(ctx, healthEvents)

	duration := r.config.MaxRetryDuration
	if duration == 0 {
		duration = DefaultMaxRetryDuration
	}

	batchCtx, cancel := context.WithTimeout(ctx, duration)
	defer cancel()
	// Shutdown must also interrupt an in-flight API call, not only backoff.
	go func() {
		select {
		case <-r.stopCh:
			cancel()
		case <-batchCtx.Done():
		}
	}()

	var failures []error

	dropReasons := make(map[string]bool)
	totalRetries := 0

	for _, write := range writes {
		retries, err := r.processWriteWithRetry(batchCtx, write.run)
		totalRetries += retries

		if err == nil {
			continue
		}

		reason := writeDropReason(ctx, err)
		droppedWritesCounter.WithLabelValues(write.operation, reason, strconv.FormatBool(write.isHealthy)).Inc()
		dropReasons[reason] = true
		failures = append(failures, fmt.Errorf("%s write for node %s (%s): %w",
			write.operation, write.nodeName, reason, err))

		level := slog.LevelWarn
		if reason == "shutdown" {
			level = slog.LevelInfo
		}

		slog.Log(ctx, level, "Discarding unsuccessful Kubernetes write", "operation", write.operation,
			"node", write.nodeName, "reason", reason, "isHealthy", write.isHealthy,
			"retryCount", retries, "error", err)
	}

	for reason := range dropReasons {
		droppedBatchesCounter.WithLabelValues(reason).Inc()
	}

	return totalRetries, errors.Join(failures...)
}

// processWriteWithRetry retries one operation without repeating completed writes.
func (r *K8sConnector) processWriteWithRetry(ctx context.Context, run func(context.Context) error) (int, error) {
	retryDelay, maxRetryDelay := r.retryDelays()

	for retries := 0; ; retries++ {
		select {
		case <-ctx.Done():
			return retries, ctx.Err()
		case <-r.stopCh:
			return retries, context.Canceled
		default:
		}

		err := run(ctx)
		if err == nil {
			return retries, nil
		}

		if ctx.Err() != nil {
			return retries, ctx.Err()
		}

		if !isKubernetesConnectorRetryableError(err) || retries >= r.config.MaxRetries {
			return retries, err
		}

		slog.WarnContext(ctx, "Retrying unsuccessful Kubernetes write in place",
			"error", err, "retryCount", retries+1, "retryDelay", retryDelay)

		if err := waitForKubernetesRetry(ctx, r.stopCh, retryDelay); err != nil {
			return retries + 1, err
		}

		retryDelay = min(retryDelay*2, maxRetryDelay)
	}
}

// retryDelays supplies production defaults for connectors constructed without NewK8sConnector.
func (r *K8sConnector) retryDelays() (time.Duration, time.Duration) {
	retryDelay := r.retryBaseDelay
	if retryDelay <= 0 {
		retryDelay = ringbuffer.DefaultBaseDelay
	}

	maxRetryDelay := r.retryMaxDelay
	if maxRetryDelay <= 0 {
		maxRetryDelay = ringbuffer.DefaultMaxDelay
	}

	return retryDelay, maxRetryDelay
}

// writeDropReason provides bounded labels for alertable terminal outcomes.
func writeDropReason(parent context.Context, err error) string {
	switch {
	case parent.Err() != nil || errors.Is(err, context.Canceled):
		return "shutdown"
	case errors.Is(err, context.DeadlineExceeded):
		return "retry_timeout"
	case isKubernetesConnectorRetryableError(err):
		return "retry_exhausted"
	default:
		return "permanent_error"
	}
}

// isKubernetesConnectorRetryableError includes conflicts and transient API or transport failures.
func isKubernetesConnectorRetryableError(err error) bool {
	return apierrors.IsConflict(err) || isTemporaryError(err)
}

// waitForKubernetesRetry waits for backoff unless context cancellation or connector shutdown interrupts it.
func waitForKubernetesRetry(ctx context.Context, stopCh <-chan struct{}, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-stopCh:
		return context.Canceled
	case <-timer.C:
		return nil
	}
}
