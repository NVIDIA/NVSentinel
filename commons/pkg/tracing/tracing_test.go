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
	"go.opentelemetry.io/otel/attribute"
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

func TestNewResource_EnvAndOptions_SetsExpectedAttributes(t *testing.T) {
	tests := []struct {
		name           string
		envAttrs       string
		envServiceName string
		version        string
		want           map[string]string
		wantAbsent     []string
	}{
		{
			name:       "no environment and no version",
			want:       map[string]string{"service.name": "node-drainer", "telemetry.sdk.language": "go"},
			wantAbsent: []string{"service.version", "k8s.pod.name"},
		},
		{
			name:    "version option sets service.version",
			version: "v1.31.0",
			want:    map[string]string{"service.name": "node-drainer", "service.version": "v1.31.0"},
		},
		{
			name:     "OTEL_RESOURCE_ATTRIBUTES adds the Kubernetes attributes",
			envAttrs: "k8s.pod.name=nd-0,k8s.namespace.name=nvsentinel,k8s.node.name=gpu-1,k8s.cluster.name=prod",
			want: map[string]string{
				"service.name":       "node-drainer",
				"k8s.pod.name":       "nd-0",
				"k8s.namespace.name": "nvsentinel",
				"k8s.node.name":      "gpu-1",
				"k8s.cluster.name":   "prod",
			},
		},
		{
			name:           "environment cannot rename the service or change the version",
			envAttrs:       "service.name=renamed,service.version=v0.0.1",
			envServiceName: "renamed-too",
			version:        "v1.31.0",
			want:           map[string]string{"service.name": "node-drainer", "service.version": "v1.31.0"},
		},
		{
			name:       "malformed attribute keeps the valid ones",
			envAttrs:   "k8s.pod.name=nd-0,not-a-pair",
			want:       map[string]string{"service.name": "node-drainer", "k8s.pod.name": "nd-0"},
			wantAbsent: []string{"not-a-pair"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("OTEL_RESOURCE_ATTRIBUTES", tt.envAttrs)
			t.Setenv("OTEL_SERVICE_NAME", tt.envServiceName)

			res, err := newResource(context.Background(), "node-drainer", tt.version)
			require.NoError(t, err)
			require.NotNil(t, res)

			for key, want := range tt.want {
				got, ok := res.Set().Value(attribute.Key(key))
				require.True(t, ok, "attribute %q missing", key)
				assert.Equal(t, want, got.AsString(), "attribute %q", key)
			}

			for _, key := range tt.wantAbsent {
				_, ok := res.Set().Value(attribute.Key(key))
				assert.False(t, ok, "attribute %q must not be set", key)
			}
		})
	}
}

func TestWithServiceVersion_EmptyVersion_AddsNoAttribute(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")

	cfg := initConfig{}
	WithServiceVersion("")(&cfg)

	res, err := newResource(context.Background(), "janitor", cfg.serviceVersion)
	require.NoError(t, err)

	_, ok := res.Set().Value(attribute.Key("service.version"))
	assert.False(t, ok)
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
