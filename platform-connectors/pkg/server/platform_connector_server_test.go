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

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/ringbuffer"
	_ "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/dedup"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

type storeOnlyTransformer struct {
	checkName string
}

func (t *storeOnlyTransformer) Transform(ctx context.Context, event *pb.HealthEvent) error {
	if event.CheckName == t.checkName {
		event.ProcessingStrategy = pb.ProcessingStrategy_STORE_ONLY
	}

	return nil
}

func (t *storeOnlyTransformer) Name() string {
	return "store-only"
}

func TestHealthEventOccurredV1_ProcessingStrategyNormalization(t *testing.T) {
	tests := []struct {
		name             string
		inputStrategy    pb.ProcessingStrategy
		expectedStrategy pb.ProcessingStrategy
	}{
		{
			name:             "UNSPECIFIED is normalized to EXECUTE_REMEDIATION",
			inputStrategy:    pb.ProcessingStrategy_UNSPECIFIED,
			expectedStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		},
		{
			name:             "EXECUTE_REMEDIATION remains unchanged",
			inputStrategy:    pb.ProcessingStrategy_EXECUTE_REMEDIATION,
			expectedStrategy: pb.ProcessingStrategy_EXECUTE_REMEDIATION,
		},
		{
			name:             "STORE_ONLY remains unchanged",
			inputStrategy:    pb.ProcessingStrategy_STORE_ONLY,
			expectedStrategy: pb.ProcessingStrategy_STORE_ONLY,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := &PlatformConnectorServer{}

			healthEvents := &pb.HealthEvents{
				Events: []*pb.HealthEvent{
					{
						NodeName:           "test-node",
						CheckName:          "test-check",
						ProcessingStrategy: tt.inputStrategy,
					},
				},
			}

			_, err := server.HealthEventOccurredV1(context.Background(), healthEvents)

			assert.NoError(t, err)
			assert.Equal(t, tt.expectedStrategy, healthEvents.Events[0].ProcessingStrategy)
		})
	}
}

func TestHealthEventOccurredV1_PipelineMutationsKeepFullBatch(t *testing.T) {
	server := &PlatformConnectorServer{
		Pipeline: pipeline.New(&storeOnlyTransformer{checkName: "duplicate"}),
	}
	healthEvents := &pb.HealthEvents{
		Events: []*pb.HealthEvent{
			{NodeName: "test-node", CheckName: "keep-me"},
			{NodeName: "test-node", CheckName: "duplicate"},
		},
	}

	_, err := server.HealthEventOccurredV1(context.Background(), healthEvents)

	assert.NoError(t, err)
	assert.Len(t, healthEvents.Events, 2)
	assert.Equal(t, "keep-me", healthEvents.Events[0].CheckName)
	assert.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, healthEvents.Events[0].ProcessingStrategy)
	assert.Equal(t, "duplicate", healthEvents.Events[1].CheckName)
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, healthEvents.Events[1].ProcessingStrategy)
}

func TestApplyEventDefaultsAndValidate_StampsMissingGeneratedTimestamp(t *testing.T) {
	missing := &pb.HealthEvent{NodeName: "node-a", CheckName: "check"}
	kept := &pb.HealthEvent{
		NodeName:           "node-a",
		CheckName:          "check",
		GeneratedTimestamp: timestamppb.New(time.Unix(1700000000, 0)),
	}

	before := time.Now().Add(-time.Second)

	assert.NoError(t, ApplyEventDefaultsAndValidate([]*pb.HealthEvent{missing, kept}))

	// The event that arrived without a timestamp carries the arrival time;
	// the one that had a timestamp keeps it.
	assert.NotNil(t, missing.GeneratedTimestamp)
	assert.False(t, missing.GeneratedTimestamp.AsTime().Before(before))
	assert.Equal(t, int64(1700000000), kept.GeneratedTimestamp.GetSeconds())
}

// fakeConnector stands in for the store: it records the batches it accepted
// and answers with a scripted result.
type fakeConnector struct {
	calls atomic.Int32
	err   error
	// delay holds the batch so the test can observe the caller giving up.
	delay time.Duration
	// failFirst fails the first batch only, as a datastore hiccup would; the
	// client then resends the batch with the same key.
	failFirst bool

	mu       sync.Mutex
	accepted []*pb.HealthEvents
}

func (f *fakeConnector) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	if n := f.calls.Add(1); f.failFirst && n == 1 {
		return errors.New("primary stepped down")
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if f.err != nil {
		return f.err
	}

	f.mu.Lock()
	f.accepted = append(f.accepted, cloneBatch(he))
	f.mu.Unlock()

	return nil
}

func cloneBatch(he *pb.HealthEvents) *pb.HealthEvents {
	clone, _ := proto.Clone(he).(*pb.HealthEvents)

	return clone
}

// keyedBatch is a batch as the deployment's idempotency interceptor leaves it:
// every event stamped with the server-derived key.
func keyedBatch(key string, nodes ...string) *pb.HealthEvents {
	events := make([]*pb.HealthEvent, 0, len(nodes))
	for i, node := range nodes {
		events = append(events, &pb.HealthEvent{
			NodeName:  node,
			CheckName: "check",
			Metadata:  map[string]string{datastore.HealthEventIdempotencyKeyMetadataField: fmt.Sprintf("pod-1#%s#%d", key, i)},
		})
	}

	return &pb.HealthEvents{Events: events}
}

func newServer(c connectors.Connector, p *pipeline.Pipeline) *PlatformConnectorServer {
	if p == nil {
		p = pipeline.New()
	}

	return &PlatformConnectorServer{Pipeline: p, Connector: c}
}

// TestDispatch_AcknowledgesWhenTheConnectorAccepts: the happy path applies the
// defaults, hands the batch to the connector and acknowledges.
func TestDispatch_AcknowledgesWhenTheConnectorAccepts(t *testing.T) {
	store := &fakeConnector{}
	he := keyedBatch("batch-1", "node-a", "node-a")

	resp, err := newServer(store, nil).HealthEventOccurredV1(context.Background(), he)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.EqualValues(t, 1, store.calls.Load())
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, he.Events[0].ProcessingStrategy,
		"defaults are applied before the connector sees the batch")
}

// TestDispatch_ConnectorFailureIsNotAcknowledged: the connector decides the
// reply. Its failure is Unavailable so the client retries with the same key.
func TestDispatch_ConnectorFailureIsNotAcknowledged(t *testing.T) {
	store := &fakeConnector{err: errors.New("primary stepped down")}

	_, err := newServer(store, nil).HealthEventOccurredV1(context.Background(), keyedBatch("batch-1", "node-a"))
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "primary stepped down")
}

// TestDispatch_RejectsBeforeAnySideEffect: an invalid batch is rejected
// before the pipeline or the connector see anything.
func TestDispatch_RejectsBeforeAnySideEffect(t *testing.T) {
	store := &fakeConnector{}
	he := keyedBatch("batch-1", "node-a")
	he.Events[0].RecommendedAction = pb.RecommendedAction_CUSTOM

	_, err := newServer(store, nil).HealthEventOccurredV1(context.Background(), he)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Zero(t, store.calls.Load())
}

// TestDispatch_CallerCancelIsReportedAsSuch: when the client gives up while
// the connector is busy, the reply carries the context error, not
// Unavailable, and the client resends with the same key.
func TestDispatch_CallerCancelIsReportedAsSuch(t *testing.T) {
	store := &fakeConnector{delay: time.Second}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := newServer(store, nil).HealthEventOccurredV1(ctx, keyedBatch("batch-1", "node-a"))
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
}

// TestDispatch_JoinsTheCallerTrace: a span context sent in the request
// metadata is the parent of everything the server does with the batch, so
// the trace the monitor started continues here.
func TestDispatch_JoinsTheCallerTrace(t *testing.T) {
	traceID, err := trace.TraceIDFromHex("0102030405060708090a0b0c0d0e0f10")
	require.NoError(t, err)
	spanID, err := trace.SpanIDFromHex("0102030405060708")
	require.NoError(t, err)

	sc := trace.NewSpanContext(trace.SpanContextConfig{
		TraceID: traceID, SpanID: spanID, TraceFlags: trace.FlagsSampled, Remote: true,
	})

	md := metadata.MD{}
	propagation.TraceContext{}.Inject(trace.ContextWithSpanContext(context.Background(), sc), healthpub.MetadataCarrier(md))
	require.NotEmpty(t, md.Get("traceparent"))

	var seen trace.SpanContext

	observer := connectorFunc(func(ctx context.Context, _ *pb.HealthEvents) error {
		seen = trace.SpanContextFromContext(ctx)

		return nil
	})

	_, err = newServer(observer, nil).HealthEventOccurredV1(
		metadata.NewIncomingContext(context.Background(), md), keyedBatch("batch-1", "node-a"))
	require.NoError(t, err)
	require.Equal(t, traceID, seen.TraceID(), "the connector runs inside the caller's trace")
}

// connectorFunc adapts a function to the Connector interface.
type connectorFunc func(ctx context.Context, he *pb.HealthEvents) error

func (f connectorFunc) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	return f(ctx, he)
}

// dedupPipeline builds a pipeline with only the dedup stage, configured from
// a config file the way the server does, so the registered factory is used.
func dedupPipeline(t *testing.T, check string) *pipeline.Pipeline {
	t.Helper()

	path := filepath.Join(t.TempDir(), "dedup.toml")
	require.NoError(t, os.WriteFile(path, fmt.Appendf(nil,
		"suppressionWindow = \"3m\"\ncleanupInterval = \"60s\"\nincludeChecks = [%q]\n", check), 0o600))

	p, err := pipeline.NewFromConfigs(context.Background(),
		[]pipeline.Config{{Name: "Deduplicator", Enabled: true, ConfigPath: path}}, pipeline.Options{})
	require.NoError(t, err)
	t.Cleanup(func() { p.Close() })

	return p
}

// TestDispatch_RetryAfterFailedWriteKeepsStrategy: the pipeline runs before
// the connector, and dedup remembers what it saw. When the write fails, the
// client resends the same batch with the same key; the resend must be stored
// with the strategy the first attempt decided, not downgraded as a repeat. A
// later batch with the same content and another key is a repeat.
func TestDispatch_RetryAfterFailedWriteKeepsStrategy(t *testing.T) {
	store := &fakeConnector{failFirst: true}
	srv := newServer(store, dedupPipeline(t, "check"))

	_, err := srv.HealthEventOccurredV1(context.Background(), keyedBatch("batch-1", "node-a"))
	require.Error(t, err, "the first write fails")
	require.Equal(t, codes.Unavailable, status.Code(err))

	_, err = srv.HealthEventOccurredV1(context.Background(), keyedBatch("batch-1", "node-a"))
	require.NoError(t, err)
	require.Len(t, store.accepted, 1)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, store.accepted[0].Events[0].ProcessingStrategy,
		"the resend keeps the decision of the first attempt")

	_, err = srv.HealthEventOccurredV1(context.Background(), keyedBatch("batch-2", "node-a"))
	require.NoError(t, err)
	require.Len(t, store.accepted, 2)
	require.Equal(t, pb.ProcessingStrategy_STORE_AND_ANALYSE, store.accepted[1].Events[0].ProcessingStrategy,
		"the same content in another batch is a repeat")
}

// TestDispatch_QueuesAcknowledgeAtOnceAndHoldTheBatch: the node-local shape.
// With ring buffers as the connectors, the reply is immediate and every queue
// holds the batch for its connector's loop.
func TestDispatch_QueuesAcknowledgeAtOnceAndHoldTheBatch(t *testing.T) {
	first := ringbuffer.NewRingBuffer("first", context.Background())
	second := ringbuffer.NewRingBuffer("second", context.Background())
	he := keyedBatch("batch-1", "node-a")

	_, err := newServer(connectors.Set{first, second}, nil).HealthEventOccurredV1(context.Background(), he)
	require.NoError(t, err)

	for _, rb := range []*ringbuffer.RingBuffer{first, second} {
		queued, quit := rb.Dequeue()
		require.False(t, quit)
		require.Same(t, he, queued.Events, "each queue holds the batch itself")
		rb.HealthMetricEleProcessingCompleted(queued)
	}
}

// TestDispatch_ConnectorStatusIsPassedThrough: a connector that chose the
// answer itself, a batch the datastore refuses as sent, is not turned into a
// retryable failure.
func TestDispatch_ConnectorStatusIsPassedThrough(t *testing.T) {
	refusing := connectorFunc(func(context.Context, *pb.HealthEvents) error {
		return status.Error(codes.InvalidArgument, "event 1 was rejected by the datastore")
	})

	_, err := newServer(refusing, nil).HealthEventOccurredV1(context.Background(), keyedBatch("batch-1", "node-a", "node-a"))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "event 1")
}
