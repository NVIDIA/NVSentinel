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

package central

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/connectors/store"
	"github.com/nvidia/nvsentinel/platform-connectors/pkg/pipeline"
	nodemeta "github.com/nvidia/nvsentinel/platform-connectors/pkg/transformers/metadata"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func TestClientIdempotencyKey(t *testing.T) {
	cases := []struct {
		name     string
		md       metadata.MD
		wantKey  string
		wantCode codes.Code
	}{
		{name: "valid key", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-001.a:b_c"), wantKey: "batch-001.a:b_c"},
		{name: "absent key is rejected", md: metadata.MD{}, wantCode: codes.InvalidArgument},
		{name: "empty key is rejected", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, ""), wantCode: codes.InvalidArgument},
		{
			name:     "multiple headers rejected",
			md:       metadata.Pairs(healthpub.IdempotencyKeyHeader, "a", healthpub.IdempotencyKeyHeader, "b"),
			wantCode: codes.InvalidArgument,
		},
		{name: "illegal character rejected", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, "no spaces"), wantCode: codes.InvalidArgument},
		{name: "hash separator rejected", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, "a#b"), wantCode: codes.InvalidArgument},
		{
			name:     "overlong key rejected",
			md:       metadata.Pairs(healthpub.IdempotencyKeyHeader, strings.Repeat("k", 129)),
			wantCode: codes.InvalidArgument,
		},
		{name: "max length key accepted", md: metadata.Pairs(healthpub.IdempotencyKeyHeader, strings.Repeat("k", 128)), wantKey: strings.Repeat("k", 128)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := clientIdempotencyKey(tc.md)
			if tc.wantCode != codes.OK {
				require.Error(t, err)
				require.Equal(t, tc.wantCode, status.Code(err))

				return
			}

			require.NoError(t, err)
			require.Equal(t, tc.wantKey, key)
		})
	}
}

func TestStampIdempotencyKeys(t *testing.T) {
	const podUID = "8b9e6c1a-pod-uid"

	he := &pb.HealthEvents{Events: []*pb.HealthEvent{
		{NodeName: "n1"},
		{NodeName: "n1", Metadata: map[string]string{
			datastore.HealthEventIdempotencyKeyMetadataField: "forged-by-caller",
			"other": "kept",
		}},
	}}

	stampIdempotencyKeys(he, podUID, "batch-1")
	require.Equal(t, podUID+"#batch-1#0", he.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, podUID+"#batch-1#1", he.Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField],
		"an inbound idempotencyKey value must be overwritten, never trusted")
	require.Equal(t, "kept", he.Events[1].Metadata["other"])
}

// fakeStore records batches and answers with a scripted outcome.
type fakeStore struct {
	calls   atomic.Int32
	outcome string
	err     error
	// delay holds the write so the test can observe the two paths running in
	// parallel.
	delay time.Duration
	// failFirst fails the first write only, as a datastore hiccup would; the
	// client then resends the batch with the same key.
	failFirst bool

	// stored keeps a copy of every batch a successful write received, as it
	// was at write time.
	mu     sync.Mutex
	stored []*pb.HealthEvents
}

func (f *fakeStore) InsertBatch(ctx context.Context, he *pb.HealthEvents) (string, error) {
	if n := f.calls.Add(1); f.failFirst && n == 1 {
		return "", errors.New("primary stepped down")
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	if f.err == nil {
		f.mu.Lock()
		f.stored = append(f.stored, cloneBatch(he))
		f.mu.Unlock()
	}

	return f.outcome, f.err
}

func cloneBatch(he *pb.HealthEvents) *pb.HealthEvents {
	clone, _ := proto.Clone(he).(*pb.HealthEvents)

	return clone
}

// fakeProcessor stands in for the k8s connector: it records calls and can
// fail or block until the context is done.
type fakeProcessor struct {
	calls atomic.Int32
	err   error
	block bool
}

func (f *fakeProcessor) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	f.calls.Add(1)

	if f.block {
		<-ctx.Done()

		return ctx.Err()
	}

	return f.err
}

// requestFrom is a request context carrying the interceptor's decision for a
// node-pinned publisher plus the idempotency-key header when withKey is set.
func requestFrom(withKey bool) context.Context {
	ctx := context.Background()
	if withKey {
		ctx = metadata.NewIncomingContext(ctx, metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-1"))
	}

	return contextWithCaller(ctx, identityFor(testPublisher, "node-a"))
}

func newHandler(st *fakeStore, conditions *fakeProcessor, timeout time.Duration) *writeServer {
	// Production-shaped: the index verified, an empty pipeline and a bounded
	// pipeline run, as initComponents wires them.
	ready := &readiness{}
	ready.indexVerified.Store(true)

	w := &writeServer{
		store:            st,
		conditionTimeout: timeout,
		pipelineTimeout:  time.Second,
		pipeline:         pipeline.New(),
		ready:            ready,
	}
	if conditions != nil {
		w.conditions = conditions
	}

	return w
}

// TestHandler_AcknowledgesOnceStored: the happy path stores the batch,
// updates conditions and stamps the derived idempotency keys.
func TestHandler_AcknowledgesOnceStored(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	conditions := &fakeProcessor{}
	w := newHandler(st, conditions, time.Second)

	he := batchNaming("node-a", "node-a")

	resp, err := w.HealthEventOccurredV1(requestFrom(true), he)
	require.NoError(t, err)
	require.NotNil(t, resp)
	require.EqualValues(t, 1, st.calls.Load())
	require.EqualValues(t, 1, conditions.calls.Load())
	require.Equal(t, "pod-uid-1#batch-1#0", he.Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, "pod-uid-1#batch-1#1", he.Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, he.Events[0].ProcessingStrategy,
		"defaults are applied as in the node-local handler")
}

// TestHandler_StoreFailureIsNotAcknowledged: the write decides the reply. A
// failed write is Unavailable so the client retries with the same key.
func TestHandler_StoreFailureIsNotAcknowledged(t *testing.T) {
	st := &fakeStore{err: errors.New("primary stepped down")}
	conditions := &fakeProcessor{}
	w := newHandler(st, conditions, time.Second)

	_, err := w.HealthEventOccurredV1(requestFrom(true), batchNaming("node-a"))
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.EqualValues(t, 1, conditions.calls.Load(), "the condition update runs in parallel with the write")
}

// TestHandler_ConditionFailureStillAcknowledges: the condition update is best
// effort. Its failure is counted and the batch is acknowledged anyway.
func TestHandler_ConditionFailureStillAcknowledges(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	w := newHandler(st, &fakeProcessor{err: errors.New("node not found")}, time.Second)

	failedBefore := testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionFailed))

	_, err := w.HealthEventOccurredV1(requestFrom(true), batchNaming("node-a"))
	require.NoError(t, err)
	require.Equal(t, failedBefore+1, testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionFailed)))
}

// TestHandler_ConditionTimeoutIsBounded: a hung API server cannot hold the
// reply past the bounded wait; the timeout is counted separately.
func TestHandler_ConditionTimeoutIsBounded(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	w := newHandler(st, &fakeProcessor{block: true}, 50*time.Millisecond)

	timedOutBefore := testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionTimedOut))

	start := time.Now()
	_, err := w.HealthEventOccurredV1(requestFrom(true), batchNaming("node-a"))
	require.NoError(t, err)
	require.Less(t, time.Since(start), 2*time.Second)
	require.Equal(t, timedOutBefore+1, testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionTimedOut)))
}

// TestHandler_DuplicateIsAcknowledged: a resent batch whose events already
// exist is a success for the client.
func TestHandler_DuplicateIsAcknowledged(t *testing.T) {
	w := newHandler(&fakeStore{outcome: store.OutcomeDuplicate}, nil, time.Second)

	outcome, err := w.handle(requestFrom(true), batchNaming("node-a"))
	require.NoError(t, err)
	require.Equal(t, store.OutcomeDuplicate, outcome, "a resent batch is acknowledged and counted as a duplicate")
}

// TestHandler_RejectsBeforeAnySideEffect: a missing key, an invalid batch or
// a missing caller identity is rejected before the store or the k8s connector
// see anything.
func TestHandler_RejectsBeforeAnySideEffect(t *testing.T) {
	t.Run("missing idempotency key", func(t *testing.T) {
		st := &fakeStore{outcome: store.OutcomeStored}
		conditions := &fakeProcessor{}
		w := newHandler(st, conditions, time.Second)

		_, err := w.HealthEventOccurredV1(requestFrom(false), batchNaming("node-a"))
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Zero(t, st.calls.Load())
		require.Zero(t, conditions.calls.Load())
	})

	t.Run("invalid batch", func(t *testing.T) {
		st := &fakeStore{outcome: store.OutcomeStored}
		w := newHandler(st, nil, time.Second)

		he := batchNaming("node-a")
		he.Events[0].RecommendedAction = pb.RecommendedAction_CUSTOM

		_, err := w.HealthEventOccurredV1(requestFrom(true), he)
		require.Equal(t, codes.InvalidArgument, status.Code(err))
		require.Zero(t, st.calls.Load())
	})

	t.Run("missing caller identity", func(t *testing.T) {
		st := &fakeStore{outcome: store.OutcomeStored}
		w := newHandler(st, nil, time.Second)

		ctx := metadata.NewIncomingContext(context.Background(),
			metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-1"))

		_, err := w.HealthEventOccurredV1(ctx, batchNaming("node-a"))
		require.Equal(t, codes.Internal, status.Code(err))
		require.Zero(t, st.calls.Load())
	})
}

// TestHandler_CallerCancelIsReportedAsSuch: when the client gives up while
// the write is in flight, the reply carries the context error, not
// Unavailable, and the client resends with the same key.
func TestHandler_CallerCancelIsReportedAsSuch(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored, delay: time.Second}
	w := newHandler(st, nil, time.Second)

	ctx, cancel := context.WithTimeout(requestFrom(true), 20*time.Millisecond)
	defer cancel()

	_, err := w.HealthEventOccurredV1(ctx, batchNaming("node-a"))
	require.Equal(t, codes.DeadlineExceeded, status.Code(err))
}

func TestMDCarrierRoundTripsTraceContext(t *testing.T) {
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

	got := trace.SpanContextFromContext(
		propagation.TraceContext{}.Extract(context.Background(), healthpub.MetadataCarrier(md)))
	require.Equal(t, traceID, got.TraceID())
	require.Equal(t, spanID, got.SpanID())
	require.True(t, got.IsRemote())
}

func TestReadiness(t *testing.T) {
	r := &readiness{}
	require.Error(t, r.Ready(context.Background()), "not ready before the index is verified")

	r.indexVerified.Store(true)
	require.NoError(t, r.Ready(context.Background()))

	r.shuttingDown.Store(true)
	require.Error(t, r.Ready(context.Background()), "shutting down flips unready even with the index verified")
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

// TestHandler_RetryAfterFailedWriteKeepsStrategy: the pipeline runs before
// the write, and dedup remembers what it saw. When the write fails, the
// client resends the same batch with the same key; the resend must be stored
// with the strategy the first attempt decided, not downgraded as a repeat.
// A later batch with the same content and another key is a repeat.
func TestHandler_RetryAfterFailedWriteKeepsStrategy(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored, failFirst: true}
	w := newHandler(st, nil, time.Second)
	w.pipeline = dedupPipeline(t, "check")

	original := batchNaming("node-a")

	_, err := w.handle(requestFrom(true), cloneBatch(original))
	require.Error(t, err, "the first write fails")
	require.Equal(t, codes.Unavailable, status.Code(err))

	outcome, err := w.handle(requestFrom(true), cloneBatch(original))
	require.NoError(t, err)
	require.Equal(t, store.OutcomeStored, outcome)
	require.Len(t, st.stored, 1)
	require.Equal(t, pb.ProcessingStrategy_EXECUTE_REMEDIATION, st.stored[0].Events[0].ProcessingStrategy,
		"the resend keeps the decision of the first attempt")

	another := metadata.NewIncomingContext(context.Background(),
		metadata.Pairs(healthpub.IdempotencyKeyHeader, "batch-2"))
	another = contextWithCaller(another, identityFor(testPublisher, "node-a"))

	_, err = w.handle(another, cloneBatch(original))
	require.NoError(t, err)
	require.Len(t, st.stored, 2)
	require.Equal(t, pb.ProcessingStrategy_STORE_AND_ANALYSE, st.stored[1].Events[0].ProcessingStrategy,
		"the same content in another batch is a repeat")
}

// metadataOverwriter stands in for a transformer that copies node labels into
// the event metadata, including one named like the server's key.
type metadataOverwriter struct{}

func (metadataOverwriter) Name() string { return "overwriter" }

func (metadataOverwriter) Transform(_ context.Context, ev *pb.HealthEvent) error {
	if ev.Metadata == nil {
		ev.Metadata = map[string]string{}
	}

	ev.Metadata[datastore.HealthEventIdempotencyKeyMetadataField] = "node-label-value"

	return nil
}

// TestHandler_OwnsTheIdempotencyKeyAfterThePipeline: whatever a transformer
// writes into the metadata, the stored events carry the server-derived keys.
func TestHandler_OwnsTheIdempotencyKeyAfterThePipeline(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	w := newHandler(st, nil, time.Second)
	w.pipeline = pipeline.New(metadataOverwriter{})
	t.Cleanup(w.pipeline.Close)

	_, err := w.handle(requestFrom(true), batchNaming("node-a", "node-a"))
	require.NoError(t, err)
	require.Len(t, st.stored, 1)
	require.Equal(t, "pod-uid-1#batch-1#0", st.stored[0].Events[0].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
	require.Equal(t, "pod-uid-1#batch-1#1", st.stored[0].Events[1].Metadata[datastore.HealthEventIdempotencyKeyMetadataField])
}

// stallingTransformer blocks each event until its context ends, like a node
// metadata read against a stalled API server.
type stallingTransformer struct{ calls atomic.Int32 }

func (s *stallingTransformer) Name() string { return "stalling" }

func (s *stallingTransformer) Transform(ctx context.Context, _ *pb.HealthEvent) error {
	s.calls.Add(1)
	<-ctx.Done()

	return nil
}

// TestHandler_PipelineBudgetCoversTheWholeBatch: one budget bounds the
// pipeline for the batch, not one per event, so a batch naming many stalled
// nodes still reaches the write, which keeps the request's own context.
func TestHandler_PipelineBudgetCoversTheWholeBatch(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	w := newHandler(st, nil, time.Second)
	w.pipelineTimeout = 200 * time.Millisecond

	stalling := &stallingTransformer{}
	w.pipeline = pipeline.New(stalling)
	t.Cleanup(w.pipeline.Close)

	start := time.Now()
	outcome, err := w.handle(requestFrom(true), batchNaming("node-a", "node-b", "node-c", "node-d"))
	elapsed := time.Since(start)

	require.NoError(t, err)
	require.Equal(t, store.OutcomeStored, outcome)
	require.Less(t, elapsed, 600*time.Millisecond, "the batch spends one budget, not one per event")
	require.EqualValues(t, 4, stalling.calls.Load(), "every event still passes through the pipeline")
	require.Len(t, st.stored, 1, "the write happened on the request context, not the spent budget")
}

// TestHandler_RefusesWritesUntilTheIndexIsVerified: a replica whose
// idempotency index is not verified refuses batches with a retryable status,
// so an established connection cannot make it store a resend twice.
func TestHandler_RefusesWritesUntilTheIndexIsVerified(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	w := newHandler(st, nil, time.Second)
	w.ready = &readiness{}

	outcome, err := w.handle(requestFrom(true), batchNaming("node-a"))
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Equal(t, outcomeFailed, outcome)
	require.EqualValues(t, 0, st.calls.Load(), "nothing is written while the index is unverified")

	w.ready.indexVerified.Store(true)

	outcome, err = w.handle(requestFrom(true), batchNaming("node-a"))
	require.NoError(t, err)
	require.Equal(t, store.OutcomeStored, outcome)
}

// TestHandler_CancelledRequestIsNotAConditionFailure: when the caller gives
// up, the condition update ends with the request and is neither counted as a
// failure nor as a timeout.
func TestHandler_CancelledRequestIsNotAConditionFailure(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored, delay: time.Second}
	w := newHandler(st, &fakeProcessor{block: true}, time.Minute)

	failedBefore := testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionFailed))
	timedOutBefore := testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionTimedOut))

	ctx, cancel := context.WithCancel(requestFrom(true))
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := w.handle(ctx, batchNaming("node-a"))
	require.Error(t, err)
	require.Equal(t, codes.Canceled, status.Code(err))
	require.Equal(t, failedBefore, testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionFailed)))
	require.Equal(t, timedOutBefore, testutil.ToFloat64(conditionUpdateFailures.WithLabelValues(conditionTimedOut)))
}

// TestHandler_CountsAcceptedBatchesInProm: with the Prometheus connector
// enabled, a batch that passed authentication and validation is counted once,
// after the pipeline, like the node-local role counts what reaches its ring
// buffer; a batch refused for a missing idempotency key is not counted.
func TestHandler_CountsAcceptedBatchesInProm(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	promCounter := &fakeProcessor{}
	w := newHandler(st, nil, time.Second)
	w.prom = promCounter

	_, err := w.HealthEventOccurredV1(requestFrom(true), batchNaming("node-a", "node-a"))
	require.NoError(t, err)
	require.Equal(t, int32(1), promCounter.calls.Load(), "one accepted batch, one count")

	_, err = w.HealthEventOccurredV1(requestFrom(false), batchNaming("node-a", "node-a"))
	require.Error(t, err, "a batch without an idempotency key is refused")
	require.Equal(t, int32(1), promCounter.calls.Load(), "a refused batch is not counted")
}

// budgetPrewarmer stands in for the metadata augmentor whose batch budget
// ended before every node was read.
type budgetPrewarmer struct{ err error }

func (b budgetPrewarmer) Transform(context.Context, *pb.HealthEvent) error { return nil }
func (b budgetPrewarmer) Name() string                                     { return "budget-prewarmer" }
func (b budgetPrewarmer) Prewarm(context.Context, []*pb.HealthEvent) error { return b.err }

// TestHandler_DefersWhenTheMetadataBudgetEnds: a batch whose node metadata
// was not all read inside the pipeline budget is answered Unavailable before
// any write or condition update, so the client resends it with the same key
// instead of the unread nodes passing the managed-label gate.
func TestHandler_DefersWhenTheMetadataBudgetEnds(t *testing.T) {
	st := &fakeStore{outcome: store.OutcomeStored}
	conditions := &fakeProcessor{}
	w := newHandler(st, conditions, time.Second)
	w.pipeline = pipeline.New(budgetPrewarmer{err: fmt.Errorf("%w: 2 of 2 node(s)", nodemeta.ErrBudgetExhausted)})

	outcome, err := w.handle(requestFrom(true), batchNaming("node-a", "node-a"))
	require.Error(t, err)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.Contains(t, status.Convert(err).Message(), "resend")
	require.Equal(t, outcomeDeferred, outcome)
	require.Zero(t, st.calls.Load(), "nothing is written")
	require.Zero(t, conditions.calls.Load(), "no condition update either")
}
