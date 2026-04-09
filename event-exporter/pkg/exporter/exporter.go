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
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/nvidia/nvsentinel/commons/pkg/tracing"
	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/config"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/metrics"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/sink"
	"github.com/nvidia/nvsentinel/event-exporter/pkg/transformer"
	"github.com/nvidia/nvsentinel/store-client/pkg/client"
)

type HealthEventsExporter struct {
	cfg            *config.Config
	dbClient       client.DatabaseClient
	source         client.ChangeStreamWatcher
	transformer    transformer.EventTransformer
	sink           sink.EventSink
	hasResumeToken bool
	workers        int
}

func New(
	cfg *config.Config,
	dbClient client.DatabaseClient,
	source client.ChangeStreamWatcher,
	transformer transformer.EventTransformer,
	sink sink.EventSink,
	hasResumeToken bool,
	workers int,
) *HealthEventsExporter {
	return &HealthEventsExporter{
		cfg:            cfg,
		dbClient:       dbClient,
		source:         source,
		transformer:    transformer,
		sink:           sink,
		hasResumeToken: hasResumeToken,
		workers:        workers,
	}
}

func (e *HealthEventsExporter) Run(ctx context.Context) error {
	switch {
	case e.cfg.Exporter.Backfill.Enabled && !e.hasResumeToken:
		slog.InfoContext(ctx, "Backfill enabled and no resume token found - running historical backfill",
			"maxAge", e.cfg.Exporter.Backfill.GetMaxAge(),
			"maxEvents", e.cfg.Exporter.Backfill.MaxEvents)

		if err := e.runBackfill(ctx); err != nil {
			slog.ErrorContext(ctx, "Failed to run backfill", "error", err)
			return fmt.Errorf("backfill: %w", err)
		}

		slog.InfoContext(ctx, "Backfill complete")
	case e.cfg.Exporter.Backfill.Enabled && e.hasResumeToken:
		slog.InfoContext(ctx, "Backfill enabled but resume token exists - skipping backfill (already initialized)")
	default:
		slog.InfoContext(ctx, "Backfill disabled in configuration")
	}

	slog.InfoContext(ctx, "Starting event stream")

	e.source.Start(ctx)

	return e.streamEvents(ctx)
}

func (e *HealthEventsExporter) runBackfill(ctx context.Context) error {
	ctx, span := tracing.StartSpan(ctx, "event_exporter.backfill")
	defer span.End()

	startTime := time.Now().UTC()
	backfillStart := startTime.Add(-e.cfg.Exporter.Backfill.GetMaxAge())

	slog.InfoContext(ctx, "Starting backfill",
		"backfillStart", backfillStart, "maxAge", e.cfg.Exporter.Backfill.GetMaxAge())

	metrics.BackfillInProgress.Set(1)
	defer metrics.BackfillInProgress.Set(0)

	cursor, err := e.queryBackfillEvents(ctx, backfillStart, startTime)
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("event_exporter.backfill.status", "failed"),
			attribute.String("event_exporter.error.type", "backfill_query_error"),
			attribute.String("event_exporter.error.message", err.Error()),
		)
		slog.ErrorContext(ctx, "Failed to query backfill events", "error", err)

		return fmt.Errorf("query backfill events: %w", err)
	}
	defer cursor.Close(ctx)

	count, err := e.processBackfillCursor(ctx, cursor)
	if err != nil {
		span.SetAttributes(
			attribute.String("event_exporter.backfill.status", "partial_success"),
			attribute.Int("event_exporter.backfill.events_processed", count),
			attribute.String("event_exporter.error.type", "backfill_process_error"),
			attribute.String("event_exporter.error.message", err.Error()),
		)
		tracing.RecordError(span, err)
		slog.ErrorContext(ctx, "Failed to process backfill cursor", "error", err)

		return fmt.Errorf("process backfill cursor: %w", err)
	}

	metrics.BackfillDuration.Observe(time.Since(startTime).Seconds())
	slog.InfoContext(ctx, "Backfill complete", "events", count)

	return nil
}

func (e *HealthEventsExporter) queryBackfillEvents(ctx context.Context, start, end time.Time) (client.Cursor, error) {
	ctx, span := tracing.StartSpan(ctx, "event_exporter.backfill.query")
	defer span.End()

	if e.cfg.Exporter.Backfill.MaxEvents <= 0 {
		err := fmt.Errorf("invalid MaxEvents value %d: must be positive", e.cfg.Exporter.Backfill.MaxEvents)

		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("event_exporter.error.type", "invalid_max_events_value"),
			attribute.String("event_exporter.error.message", err.Error()),
		)

		return nil, err
	}

	span.SetAttributes(
		attribute.String("event_exporter.backfill.query.start", start.Format(time.RFC3339)),
		attribute.String("event_exporter.backfill.query.end", end.Format(time.RFC3339)),
		attribute.Int("event_exporter.backfill.query.max_events", e.cfg.Exporter.Backfill.MaxEvents),
	)

	filter := map[string]any{
		"createdAt": map[string]any{
			"$gte": start,
			"$lt":  end,
		},
	}

	limit := int64(e.cfg.Exporter.Backfill.MaxEvents)
	opts := &client.FindOptions{
		Limit: &limit,
	}

	cursor, err := e.dbClient.Find(ctx, filter, opts)
	if err != nil {
		tracing.RecordError(span, err)

		span.SetAttributes(
			attribute.String("event_exporter.error.type", "find_events_error"),
			attribute.String("event_exporter.error.message", err.Error()),
		)

		slog.ErrorContext(ctx, "Failed to query events", "error", err)

		return nil, fmt.Errorf("query events: %w", err)
	}

	return cursor, nil
}

func (e *HealthEventsExporter) processBackfillCursor(ctx context.Context, cursor client.Cursor) (int, error) {
	ctx, span := tracing.StartSpan(ctx, "event_exporter.backfill.process")
	defer span.End()

	var rateLimiter *time.Ticker

	useRateLimiting := e.cfg.Exporter.Backfill.RateLimit > 0

	if useRateLimiting {
		rateLimiter = time.NewTicker(time.Second / time.Duration(e.cfg.Exporter.Backfill.RateLimit))
		defer rateLimiter.Stop()
	} else {
		slog.InfoContext(ctx, "Rate limiting disabled for backfill (RateLimit=0)")
	}

	count := 0

	for cursor.Next(ctx) {
		if ctx.Err() != nil {
			slog.ErrorContext(ctx, "Context done", "error", ctx.Err())
			tracing.RecordError(span, ctx.Err())
			span.SetAttributes(
				attribute.String("event_exporter.error.type", "context_done_error"),
				attribute.String("event_exporter.error.message", ctx.Err().Error()),
			)

			return count, ctx.Err()
		}

		healthEvent, err := e.decodeBackfillEvent(ctx, cursor)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to decode backfill event", "error", err)

			tracing.RecordError(span, err)
			span.AddEvent(
				"decode_backfill_event_error",
				trace.WithAttributes(
					attribute.String("event_exporter.error.type", "decode_backfill_event_error"),
					attribute.String("event_exporter.error.message", err.Error()),
				),
			)

			continue
		}

		if healthEvent == nil {
			slog.DebugContext(ctx, "Skipping nil health event")
			continue
		}

		if err := e.publishWithRetry(ctx, healthEvent); err != nil {
			slog.ErrorContext(ctx, "Failed to publish backfill event", "error", err)
			tracing.RecordError(span, err)
			span.AddEvent(
				"publish_event_error",
				trace.WithAttributes(
					attribute.String("event_exporter.error.type", "publish_event_error"),
					attribute.String("event_exporter.error.message", err.Error()),
				),
			)

			return count, err
		}

		count++

		metrics.BackfillEventsProcessed.Inc()

		if err := e.waitForRateLimit(ctx, useRateLimiting, rateLimiter); err != nil {
			slog.ErrorContext(ctx, "Failed to wait for rate limit", "error", err)
			tracing.RecordError(span, err)
			span.AddEvent(
				"wait_for_rate_limit_error",
				trace.WithAttributes(
					attribute.String("event_exporter.error.type", "wait_for_rate_limit_error"),
					attribute.String("event_exporter.error.message", err.Error()),
				),
			)

			return count, fmt.Errorf("wait for rate limit: %w", err)
		}
	}

	if err := cursor.Err(); err != nil {
		slog.ErrorContext(ctx, "Failed to get cursor error", "error", err)
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("event_exporter.error.type", "cursor_error"),
			attribute.String("event_exporter.error.message", err.Error()),
		)

		return count, fmt.Errorf("cursor error: %w", err)
	}

	span.SetAttributes(attribute.Int("event_exporter.backfill.events_processed", count))

	return count, nil
}

func (e *HealthEventsExporter) decodeBackfillEvent(ctx context.Context, cursor client.Cursor) (*pb.HealthEvent, error) {
	var healthEventWithStatus model.HealthEventWithStatus
	if err := cursor.Decode(&healthEventWithStatus); err != nil {
		slog.WarnContext(ctx, "Failed to decode event", "error", err)
		return nil, err
	}

	if healthEventWithStatus.HealthEvent == nil {
		slog.DebugContext(ctx, "Skipping nil health event")
		return nil, nil
	}

	return healthEventWithStatus.HealthEvent, nil
}

func (e *HealthEventsExporter) waitForRateLimit(
	ctx context.Context,
	useRateLimiting bool,
	rateLimiter *time.Ticker,
) error {
	if useRateLimiting {
		select {
		case <-ctx.Done():
			slog.ErrorContext(ctx, "Context done", "error", ctx.Err())
			return ctx.Err()
		case <-rateLimiter.C:
		}
	} else if ctx.Err() != nil {
		slog.ErrorContext(ctx, "Context done", "error", ctx.Err())
		return ctx.Err()
	}

	return nil
}

func (e *HealthEventsExporter) streamEvents(ctx context.Context) error {
	numWorkers := e.workers
	if numWorkers <= 0 {
		numWorkers = 1
	}

	slog.InfoContext(ctx, "Starting event stream", "workers", numWorkers)

	return e.streamEventsConcurrent(ctx, numWorkers)
}

// streamEventsConcurrent dispatches events to a worker pool for parallel publishing.
// Resume tokens are advanced in order via a sequenceTracker.
func (e *HealthEventsExporter) streamEventsConcurrent(ctx context.Context, numWorkers int) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	pool := newWorkerPool(numWorkers, e.processEvent, e.source, cancel)

	// Run the worker pool in a separate goroutine.
	// It blocks until all workers finish and the token writer drains.
	poolErrCh := make(chan error, 1)

	go func() {
		poolErrCh <- pool.run(ctx)
	}()

	// Consumer loop: read events, unmarshal, and dispatch to workers.
	var seq uint64

	consumeErr := e.consumeAndDispatch(ctx, pool, &seq)

	pool.closeDispatch()

	// Wait for the pool to finish processing in-flight items
	poolErr := <-poolErrCh

	// When the pool triggers cancellation (e.g., publish failure), consumeErr
	// is just context.Canceled — prefer poolErr which has the root cause.
	if poolErr != nil {
		return fmt.Errorf("worker pool: %w", poolErr)
	}

	if consumeErr != nil {
		return fmt.Errorf("event consumer: %w", consumeErr)
	}

	return nil
}

func (e *HealthEventsExporter) consumeAndDispatch(
	ctx context.Context, pool *workerPool, seq *uint64,
) error {
	for {
		select {
		case <-ctx.Done():
			slog.InfoContext(ctx, "Context done", "error", ctx.Err())
			return ctx.Err()

		case healthEvent, ok := <-e.source.Events():
			if !ok {
				slog.InfoContext(ctx, "Event channel closed, finishing in-flight events")
				return nil
			}

			metrics.EventsReceived.Inc()

			*seq++

			if !pool.dispatch(ctx, workItem{
				seq:         *seq,
				event:       healthEvent,
				resumeToken: healthEvent.GetResumeToken(),
			}) {
				return ctx.Err()
			}
		}
	}
}

// processEvent handles a single change stream event: unmarshal, skip if
// invalid, or publish. Returns nil for skipped events (unmarshal errors,
// nil documents) so the token writer advances the resume token. Returns
// a non-nil error only for fatal publish failures.
func (e *HealthEventsExporter) processEvent(ctx context.Context, rawEvent client.Event) error {
	healthEventWithStatus, err := unmarshalHealthEventWithStatus(ctx, rawEvent)
	if err != nil {
		slog.WarnContext(ctx, "Failed to unmarshal event", "error", err)
		metrics.TransformErrors.Inc()

		return nil
	}

	if healthEventWithStatus.HealthEvent == nil {
		slog.DebugContext(ctx, "Skipping nil health event")
		return nil
	}

	traceID := tracing.TraceIDFromMetadata(healthEventWithStatus.HealthEvent.GetMetadata())
	parentSpanID := tracing.ParentSpanID(healthEventWithStatus.HealthEventStatus.SpanIds, tracing.ServicePlatformConnector)

	ctx, span := tracing.StartSpanWithLinkFromTraceContext(ctx, traceID, parentSpanID, "event_exporter.process_event")
	defer span.End()

	return e.publishWithRetry(ctx, healthEventWithStatus.HealthEvent)
}

func (e *HealthEventsExporter) publishWithRetry(ctx context.Context, event *pb.HealthEvent) error {
	ctx, span := tracing.StartSpan(ctx, "event_exporter.publish_with_retry")
	defer span.End()

	cloudEvent, transformErr := e.transformer.Transform(ctx, event)
	if transformErr != nil {
		tracing.RecordError(span, transformErr)
		span.SetAttributes(
			attribute.String("event_exporter.error.type", "transform_error"),
			attribute.String("event_exporter.error.message", transformErr.Error()),
		)
		metrics.TransformErrors.Inc()
		slog.ErrorContext(ctx, "Failed to transform event", "error", transformErr)

		return fmt.Errorf("transform event: %w", transformErr)
	}

	startTime := time.Now()
	attempt := 0

	backoffConfig := wait.Backoff{
		Steps:    e.cfg.Exporter.FailureHandling.MaxRetries + 1,
		Duration: e.cfg.Exporter.FailureHandling.GetInitialBackoff(),
		Factor:   e.cfg.Exporter.FailureHandling.BackoffMultiplier,
		Jitter:   0.1,
		Cap:      e.cfg.Exporter.FailureHandling.GetMaxBackoff(),
	}

	err := wait.ExponentialBackoffWithContext(ctx, backoffConfig, func(ctx context.Context) (bool, error) {
		if attempt > 0 {
			slog.InfoContext(ctx, "Publish failed, retrying", "attempt", attempt)
		}

		publishErr := e.sink.Publish(ctx, cloudEvent)
		if publishErr == nil {
			slog.DebugContext(ctx, "Publish succeeded", "attempt", attempt)
			metrics.PublishDuration.Observe(time.Since(startTime).Seconds())
			metrics.EventsPublished.WithLabelValues(metrics.StatusSuccess).Inc()

			return true, nil
		}

		attempt++
		slog.WarnContext(ctx, "Publish failed, retrying",
			"attempt", attempt,
			"maxRetries", e.cfg.Exporter.FailureHandling.MaxRetries,
			"error", publishErr)

		return false, nil
	})
	if err != nil {
		tracing.RecordError(span, err)
		span.SetAttributes(
			attribute.String("event_exporter.publish.failed_reason", "max_retries_exceeded"),
			attribute.String("event_exporter.error.type", "publish_failed"),
			attribute.String("event_exporter.error.message", err.Error()),
		)

		metrics.PublishErrors.WithLabelValues("max_retries_exceeded").Inc()
		metrics.EventsPublished.WithLabelValues(metrics.StatusFailure).Inc()
		slog.ErrorContext(ctx, "Publish failed after max retries", "error", err)

		return fmt.Errorf("publish failed after %d retries: %w", e.cfg.Exporter.FailureHandling.MaxRetries, err)
	}

	return nil
}

func unmarshalHealthEventWithStatus(ctx context.Context, event client.Event) (model.HealthEventWithStatus, error) {
	var healthEventWithStatus model.HealthEventWithStatus
	if err := event.UnmarshalDocument(&healthEventWithStatus); err != nil {
		slog.ErrorContext(ctx, "Failed to unmarshal document", "error", err)
		return healthEventWithStatus, fmt.Errorf("unmarshal document: %w", err)
	}

	return healthEventWithStatus, nil
}
