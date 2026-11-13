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

package tracing

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace/noop"
)

// shutdownErrProcessor is a span processor whose Shutdown returns err.
type shutdownErrProcessor struct {
	err error
}

func (p shutdownErrProcessor) OnStart(context.Context, sdktrace.ReadWriteSpan) {}
func (p shutdownErrProcessor) OnEnd(sdktrace.ReadOnlySpan)                     {}
func (p shutdownErrProcessor) Shutdown(context.Context) error                  { return p.err }
func (p shutdownErrProcessor) ForceFlush(context.Context) error                { return nil }

func providerWithShutdownErr(err error) *sdktrace.TracerProvider {
	return sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(shutdownErrProcessor{err: err}))
}

func TestShutdownTracing_ProviderErrors_ReturnsEveryError(t *testing.T) {
	errChild := errors.New("child-only flush failed")
	errPrimary := errors.New("primary flush failed")

	tests := []struct {
		name       string
		child      *sdktrace.TracerProvider
		primary    *sdktrace.TracerProvider
		wantErrors []error
	}{
		{
			name: "not initialized",
		},
		{
			name:    "both providers shut down cleanly",
			child:   providerWithShutdownErr(nil),
			primary: providerWithShutdownErr(nil),
		},
		{
			name:       "child-only provider fails",
			child:      providerWithShutdownErr(errChild),
			primary:    providerWithShutdownErr(nil),
			wantErrors: []error{errChild},
		},
		{
			name:       "primary provider fails",
			child:      providerWithShutdownErr(nil),
			primary:    providerWithShutdownErr(errPrimary),
			wantErrors: []error{errPrimary},
		},
		{
			name:       "both providers fail",
			child:      providerWithShutdownErr(errChild),
			primary:    providerWithShutdownErr(errPrimary),
			wantErrors: []error{errChild, errPrimary},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prevChild, prevPrimary := childOnlyTracerProvider, tracerProvider

			t.Cleanup(func() {
				childOnlyTracerProvider, tracerProvider = prevChild, prevPrimary
			})

			childOnlyTracerProvider, tracerProvider = tt.child, tt.primary

			err := ShutdownTracing(context.Background())

			if len(tt.wantErrors) == 0 {
				require.NoError(t, err)

				return
			}

			require.Error(t, err)

			for _, want := range tt.wantErrors {
				assert.ErrorIs(t, err, want)
			}
		})
	}
}

func TestSpanCreation(t *testing.T) {
	tracer = nil

	t.Run("GetTracer returns noop when not initialized", func(t *testing.T) {
		tr := GetTracer()
		require.NotNil(t, tr)

		_, span := tr.Start(context.Background(), "noop-span")
		assert.False(t, span.SpanContext().IsValid())
		span.End()
	})

	t.Run("StartSpanFromTraceContext with empty trace ID", func(t *testing.T) {
		ctx, span := StartSpanFromTraceContext(context.Background(), "", "", "test-span")
		assert.NotNil(t, ctx)
		assert.NotNil(t, span)
		span.End()
	})

	t.Run("StartSpanFromTraceContext with invalid trace ID", func(t *testing.T) {
		ctx, span := StartSpanFromTraceContext(context.Background(), "not-a-valid-hex", "", "test-span")
		assert.NotNil(t, ctx)
		assert.NotNil(t, span)
		span.End()
	})

	t.Run("StartSpanFromTraceContext with valid trace and span ID", func(t *testing.T) {
		ctx, span := StartSpanFromTraceContext(
			context.Background(),
			"0af7651916cd43dd8448eb211c80319c",
			"00f067aa0ba902b7",
			"test-span",
		)
		assert.NotNil(t, ctx)
		assert.NotNil(t, span)
		span.End()
	})

	t.Run("StartSpanFromTraceContext with valid trace and invalid span ID", func(t *testing.T) {
		ctx, span := StartSpanFromTraceContext(
			context.Background(),
			"0af7651916cd43dd8448eb211c80319c",
			"bad-span",
			"test-span",
		)
		assert.NotNil(t, ctx)
		assert.NotNil(t, span)
		span.End()
	})
}

func TestMetadataHelpers(t *testing.T) {
	t.Run("TraceIDFromMetadata missing key", func(t *testing.T) {
		assert.Equal(t, "", TraceIDFromMetadata(map[string]string{"other": "value"}))
	})

	t.Run("TraceIDFromMetadata present", func(t *testing.T) {
		m := map[string]string{MetadataKeyTraceID: "abc123"}
		assert.Equal(t, "abc123", TraceIDFromMetadata(m))
	})

	t.Run("ParentSpanID present", func(t *testing.T) {
		m := map[string]string{"platform_connector": "span123"}
		assert.Equal(t, "span123", ParentSpanID(m, "platform_connector"))
	})
}

func TestSpanHelpers(t *testing.T) {
	_, span := noop.NewTracerProvider().Tracer("test").Start(context.Background(), "test")
	defer span.End()

	t.Run("RecordError does not panic", func(t *testing.T) {
		assert.NotPanics(t, func() { RecordError(span, assert.AnError) })
	})
}
