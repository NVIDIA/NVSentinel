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

// Package tracing provides OpenTelemetry distributed tracing utilities for all services.
// It initializes an OTLP gRPC exporter from standard OTel environment variables, offers helpers
// for starting spans with trace-context propagation across async boundaries (ring buffers,
// change streams), and includes a conditional HTTP round tripper that instruments outbound
// API calls only when an active trace is present.
package tracing

import (
	"context"
	"fmt"
	"log/slog"
	"os"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/exporters/otlp/otlptrace/otlptracegrpc"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/sdk/resource"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
	"go.opentelemetry.io/otel/trace/noop"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	tracerProvider          *sdktrace.TracerProvider
	childOnlyTracerProvider *sdktrace.TracerProvider
	tracer                  trace.Tracer
)

// Service name constants used across multiple modules for span-ID lookups.
// For example, platform-connector's span ID is read by fault-quarantine,
// event-exporter, and health-events-analyzer to establish parent-child
// trace relationships.
const (
	ServicePlatformConnector    = "platform-connector"
	ServiceFaultQuarantine      = "fault-quarantine"
	ServiceEventExporter        = "event-exporter"
	ServiceHealthEventsAnalyzer = "health-events-analyzer"
	ServiceNodeDrainer          = "node-drainer"
)

// MetadataKeyTraceID is the key used to store the trace ID in the health event's
// Metadata map. Platform-connector writes it at ingestion time so that all
// downstream event consumers are linked to same trace.
const MetadataKeyTraceID = "trace_id"

// defaultTraceID is the hex representation of the zero-value OpenTelemetry trace ID,
// which is present when the context doesn't carry trace info.
var defaultTraceID = trace.TraceID{}.String()

// InitTracing initializes OpenTelemetry tracing with OTLP gRPC exporter.
func InitTracing(serviceName string) error {
	res, err := resource.New(
		context.Background(),
		resource.WithAttributes(
			attribute.String("service.name", serviceName),
		),
	)
	if err != nil {
		return fmt.Errorf("failed to create resource: %w", err)
	}

	endpoint := os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT")
	if endpoint == "" {
		return fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT is not configured")
	}

	opts := []otlptracegrpc.Option{
		otlptracegrpc.WithEndpoint(endpoint),
	}

	if os.Getenv("OTEL_EXPORTER_OTLP_INSECURE") == "true" {
		opts = append(opts, otlptracegrpc.WithTLSCredentials(insecure.NewCredentials()))
	}

	primaryExporter, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		return fmt.Errorf("failed to create primary OTLP exporter: %w", err)
	}

	childOnlyExporter, err := otlptracegrpc.New(context.Background(), opts...)
	if err != nil {
		return fmt.Errorf("failed to create child-only OTLP exporter: %w", err)
	}

	otel.SetErrorHandler(otel.ErrorHandlerFunc(func(err error) {
		slog.Warn("OpenTelemetry export error", "error", err)
	}))

	tracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(primaryExporter),
		sdktrace.WithResource(res),
	)

	// Separate provider for driver-level DB instrumentation (otelmongo, otelsql).
	// ParentBased(NeverSample()) only records spans that have a sampled parent,
	// preventing standalone root traces from background operations like change
	// stream getMore polling and listCollections.
	childOnlyTracerProvider = sdktrace.NewTracerProvider(
		sdktrace.WithBatcher(childOnlyExporter),
		sdktrace.WithResource(res),
		sdktrace.WithSampler(sdktrace.ParentBased(sdktrace.NeverSample())),
	)

	otel.SetTracerProvider(tracerProvider)

	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))

	tracer = otel.Tracer(serviceName)

	slog.Info("OpenTelemetry tracing initialized", "endpoint", endpoint)

	return nil
}

// ShutdownTracing flushes pending spans and shuts down all tracer providers.
// Call this during service shutdown to
// ensure all buffered spans are exported before the process exits.
func ShutdownTracing(ctx context.Context) error {
	if childOnlyTracerProvider != nil {
		_ = childOnlyTracerProvider.Shutdown(ctx)
	}

	if tracerProvider != nil {
		return tracerProvider.Shutdown(ctx)
	}

	return nil
}

// GetChildOnlyTracerProvider returns a TracerProvider that only records spans
// when a sampled parent span exists in the context. Use this for driver-level
// database instrumentation (otelmongo, otelsql) to avoid generating standalone
// root traces from background operations (e.g. change stream polling, health checks).
// Falls back to the global TracerProvider if tracing has not been initialized.
func GetChildOnlyTracerProvider() trace.TracerProvider {
	if childOnlyTracerProvider != nil {
		return childOnlyTracerProvider
	}

	return otel.GetTracerProvider()
}

// GetTracer returns the package-level tracer initialized by InitTracing.
// If tracing has not been initialized, returns a no-op tracer that silently
// discards all spans.
func GetTracer() trace.Tracer {
	if tracer == nil {
		// Return a no-op tracer if not initialized
		return noop.NewTracerProvider().Tracer("noop")
	}

	return tracer
}

// StartSpan creates a new span with the given name. If ctx carries a parent span,
// the new span becomes its child. Returns the updated context and the span.
func StartSpan(ctx context.Context, name string, opts ...trace.SpanStartOption) (context.Context, trace.Span) {
	return GetTracer().Start(ctx, name, opts...)
}

// StartChildSpanIfParentTraceActive starts a child span only when ctx already carries a valid
// span. If there is no active trace, returns ctx unchanged with traced=false;
func StartChildSpanIfParentTraceActive(
	ctx context.Context, name string, opts ...trace.SpanStartOption,
) (context.Context, trace.Span, bool) {
	if !trace.SpanFromContext(ctx).SpanContext().IsValid() {
		return ctx, nil, false
	}

	ctx, span := StartSpan(ctx, name, opts...)

	return ctx, span, true
}

// StartSpanFromTraceID starts a new span that belongs to an existing trace.
// When only traceID is provided, the span is placed under the same trace but
// without a parent-child link (sibling root span). For proper parent-child
// relationships, use StartSpanFromTraceContext with both traceID and spanID.
func StartSpanFromTraceID(
	ctx context.Context, traceID string, name string, opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	return StartSpanFromTraceContext(ctx, traceID, "", name, opts...)
}

// StartSpanFromTraceContext starts a new span from an existing trace ID and
// optional parent span ID using a parent-child relationship.
//
// When both traceID and parentSpanID are provided, the new span becomes a
// child of the remote parent, forming a proper parent-child relationship in
// the trace tree.
//
// When only traceID is provided (parentSpanID is empty), the span is placed
// under the same trace with a Span Link back to the trace origin, but without
// a direct parent-child relationship.
func StartSpanFromTraceContext(
	ctx context.Context, traceID, parentSpanID, name string, opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if traceID == "" || traceID == defaultTraceID {
		return StartSpan(ctx, name, opts...)
	}

	tid, err := trace.TraceIDFromHex(traceID)
	if err != nil {
		slog.Warn("Failed to parse trace ID, starting new trace", "traceID", traceID, "error", err)
		return StartSpan(ctx, name, opts...)
	}

	if parentSpanID != "" {
		sid, err := trace.SpanIDFromHex(parentSpanID)
		if err != nil {
			slog.Warn("Failed to parse parent span ID, falling back to trace-only",
				"traceID", traceID, "spanID", parentSpanID, "error", err)
		} else {
			// Full parent context: creates a proper parent-child relationship
			parentCtx := trace.NewSpanContext(trace.SpanContextConfig{
				TraceID:    tid,
				SpanID:     sid,
				TraceFlags: trace.FlagsSampled,
				Remote:     true,
			})
			ctx = trace.ContextWithSpanContext(ctx, parentCtx)

			return StartSpan(ctx, name, opts...)
		}
	}

	// Trace-only context: same trace but no parent-child link.
	remoteCtx := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID:    tid,
		TraceFlags: trace.FlagsSampled,
		Remote:     true,
	})
	ctx = trace.ContextWithSpanContext(ctx, remoteCtx)

	return StartSpan(ctx, name, opts...)
}

// StartSpanWithLinkFromTraceContext starts a span in the provided trace and
// adds a span link to the upstream span context (when available), without
// creating a parent-child relationship. This is preferred for async handoffs
// across modules or queues.
func StartSpanWithLinkFromTraceContext(
	ctx context.Context,
	traceID, parentSpanID, name string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if traceID == "" || traceID == defaultTraceID {
		return StartSpan(ctx, name, opts...)
	}

	if parentSpanID != "" {
		tid, traceErr := trace.TraceIDFromHex(traceID)
		sid, spanErr := trace.SpanIDFromHex(parentSpanID)

		if traceErr == nil && spanErr == nil {
			opts = append(opts, trace.WithLinks(trace.Link{
				SpanContext: trace.NewSpanContext(trace.SpanContextConfig{
					TraceID:    tid,
					SpanID:     sid,
					TraceFlags: trace.FlagsSampled,
					Remote:     true,
				}),
			}))
		}
	}

	return StartSpanFromTraceID(ctx, traceID, name, opts...)
}

// StartSpanWithLinkFromSpanContext starts a span in the same trace as the
// upstream span context and adds a span link to it, without parent-child
// nesting. This is useful for async fan-out within a service.
func StartSpanWithLinkFromSpanContext(
	ctx context.Context,
	upstream trace.SpanContext,
	name string,
	opts ...trace.SpanStartOption,
) (context.Context, trace.Span) {
	if !upstream.IsValid() {
		return StartSpan(ctx, name, opts...)
	}

	opts = append(opts, trace.WithLinks(trace.Link{SpanContext: upstream}))

	return StartSpanFromTraceID(ctx, upstream.TraceID().String(), name, opts...)
}

// SpanIDFromSpan returns the hex-encoded span ID of the given span,
// suitable for storing in a database or CR annotation for downstream
// services to use as a parent span ID.
func SpanIDFromSpan(span trace.Span) string {
	if span == nil {
		return ""
	}

	sc := span.SpanContext()
	if !sc.HasSpanID() {
		return ""
	}

	return sc.SpanID().String()
}

// TraceIDFromMetadata extracts the trace ID from a health event's Metadata map.
func TraceIDFromMetadata(metadata map[string]string) string {
	if metadata == nil {
		return ""
	}

	return metadata[MetadataKeyTraceID]
}

// ParentSpanID looks up the span ID written by parentService in the span_ids
// map stored in the healthEventStatus. Used to establish parent-child trace
// relationships across services.
func ParentSpanID(spanIDs map[string]string, parentService string) string {
	if spanIDs == nil {
		return ""
	}

	return spanIDs[parentService]
}

// SetSpanAttributes attaches one or more key-value attributes to the given span.
func SetSpanAttributes(span trace.Span, attrs ...attribute.KeyValue) {
	span.SetAttributes(attrs...)
}

// RecordError records the error as a span event and sets the span status to Error.
func RecordError(span trace.Span, err error, opts ...trace.EventOption) {
	span.RecordError(err, opts...)
	span.SetStatus(codes.Error, err.Error())
}

// SpanFromContext returns the active span stored in ctx, or a no-op span if none exists.
func SpanFromContext(ctx context.Context) trace.Span {
	return trace.SpanFromContext(ctx)
}
