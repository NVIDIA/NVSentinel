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

package ringbuffer

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/trace"
	"k8s.io/client-go/util/workqueue"

	"github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

const (
	// Default retry configuration for production use
	// Retry 1: 500ms, Retry 2: 1.5s, Retry 3: 3s (total ~5s to 4th attempt)
	DefaultBaseDelay = 500 * time.Millisecond
	DefaultMaxDelay  = 3 * time.Second
)

// QueuedHealthEvents carries health events and the trace context from the gRPC handler
// so store and K8s connectors can continue the same trace.
type QueuedHealthEvents struct {
	Events            *protos.HealthEvents
	ParentSpanContext trace.SpanContext
}

func NewQueuedHealthEvents(events *protos.HealthEvents) *QueuedHealthEvents {
	return &QueuedHealthEvents{Events: events, ParentSpanContext: trace.SpanContext{}}
}

type RingBuffer struct {
	ringBufferIdentifier string
	healthMetricQueue    workqueue.TypedRateLimitingInterface[*QueuedHealthEvents]
	ctx                  context.Context
}

type Option func(*config)

type config struct {
	baseDelay time.Duration
	maxDelay  time.Duration
}

func WithRetryConfig(baseDelay, maxDelay time.Duration) Option {
	return func(c *config) {
		c.baseDelay = baseDelay
		c.maxDelay = maxDelay
	}
}

func NewRingBuffer(ringBufferName string, ctx context.Context, opts ...Option) *RingBuffer {
	cfg := &config{
		baseDelay: DefaultBaseDelay,
		maxDelay:  DefaultMaxDelay,
	}
	for _, opt := range opts {
		opt(cfg)
	}

	workqueue.SetProvider(prometheusMetricsProvider{})

	rateLimiter := workqueue.NewTypedItemExponentialFailureRateLimiter[*QueuedHealthEvents](
		cfg.baseDelay,
		cfg.maxDelay,
	)

	queue := workqueue.NewTypedRateLimitingQueueWithConfig(
		rateLimiter,
		workqueue.TypedRateLimitingQueueConfig[*QueuedHealthEvents]{
			Name: ringBufferName,
		},
	)

	return &RingBuffer{
		ringBufferIdentifier: ringBufferName,
		healthMetricQueue:    queue,
		ctx:                  ctx,
	}
}

func (rb *RingBuffer) Enqueue(item *QueuedHealthEvents) {
	rb.healthMetricQueue.Add(item)
}

func (rb *RingBuffer) Dequeue() (*QueuedHealthEvents, bool) {
	healthEvents, quit := rb.healthMetricQueue.Get()
	if quit {
		slog.InfoContext(rb.ctx, "Queue signaled shutdown")
		return nil, true
	}

	slog.InfoContext(rb.ctx, "Successfully got item", "healthEvents", healthEvents.Events)

	if errors.Is(rb.ctx.Err(), context.Canceled) {
		slog.InfoContext(rb.ctx, "Context cancelled, signaling quit")
		rb.healthMetricQueue.Forget(healthEvents)
		rb.healthMetricQueue.Done(healthEvents)

		return nil, true
	}

	return healthEvents, false
}

func (rb *RingBuffer) HealthMetricEleProcessingCompleted(data *QueuedHealthEvents) {
	rb.healthMetricQueue.Forget(data)
	rb.healthMetricQueue.Done(data)
}

func (rb *RingBuffer) HealthMetricEleProcessingFailed(data *QueuedHealthEvents) {
	rb.healthMetricQueue.Forget(data)
	rb.healthMetricQueue.Done(data)
}

func (rb *RingBuffer) AddRateLimited(data *QueuedHealthEvents) {
	rb.healthMetricQueue.AddRateLimited(data)
	rb.healthMetricQueue.Done(data)
}

func (rb *RingBuffer) NumRequeues(data *QueuedHealthEvents) int {
	return rb.healthMetricQueue.NumRequeues(data)
}

func (rb *RingBuffer) ShutDownHealthMetricQueue() {
	rb.healthMetricQueue.ShutDownWithDrain()
}

func (rb *RingBuffer) CurrentLength() int {
	return rb.healthMetricQueue.Len()
}
