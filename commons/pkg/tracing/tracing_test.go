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
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/trace/noop"
)

func TestSpanCreation(t *testing.T) {
	tracer = nil

	t.Run("GetTracer returns noop when not initialized", func(t *testing.T) {
		tr := GetTracer()
		require.NotNil(t, tr)

		_, span := tr.Start(context.Background(), "noop-span")
		assert.False(t, span.SpanContext().IsValid())
		span.End()
	})

	t.Run("StartChildSpan returns false with no parent", func(t *testing.T) {
		ctx := context.Background()

		newCtx, span, traced := StartChildSpanIfParentTraceActive(ctx, "child-span")
		assert.Equal(t, ctx, newCtx)
		assert.Nil(t, span)
		assert.False(t, traced)
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

	t.Run("SetSpanAttributes does not panic", func(t *testing.T) {
		assert.NotPanics(t, func() { SetSpanAttributes(span) })
	})

	t.Run("RecordError does not panic", func(t *testing.T) {
		assert.NotPanics(t, func() { RecordError(span, assert.AnError) })
	})
}
