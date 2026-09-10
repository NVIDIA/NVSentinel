// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package healthpub

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// idempotencyKeyFormat is the server-side validation pattern the generated
// keys must satisfy (idempotency).
var idempotencyKeyFormat = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// directFakeClient records every attempt's idempotency-key header and the
// CheckNames it carried, and serves scripted responses; gate, when non-nil,
// blocks each call until released (for queue-bound and drain-deadline tests).
// A call is counted when it starts and recorded once it passes the gate.
type directFakeClient struct {
	mu         sync.Mutex
	keys       []string
	checkNames []string
	calls      atomic.Int64

	gate       chan struct{}
	responseFn func(call int) error
}

func (f *directFakeClient) HealthEventOccurredV1(
	ctx context.Context, events *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	n := int(f.calls.Add(1))

	if f.gate != nil {
		select {
		case <-f.gate:
		case <-ctx.Done():
			return nil, status.FromContextError(ctx.Err()).Err()
		}
	}

	f.mu.Lock()

	if md, ok := metadata.FromOutgoingContext(ctx); ok {
		if keys := md.Get(IdempotencyKeyHeader); len(keys) == 1 {
			f.keys = append(f.keys, keys[0])
		}
	}

	for _, event := range events.GetEvents() {
		f.checkNames = append(f.checkNames, event.GetCheckName())
	}

	f.mu.Unlock()

	if f.responseFn != nil {
		if err := f.responseFn(n); err != nil {
			return nil, err
		}
	}

	return &emptypb.Empty{}, nil
}

func (f *directFakeClient) recordedKeys() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.keys...)
}

func (f *directFakeClient) recordedCheckNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.checkNames...)
}

// namedEvents is sampleEvents with a caller-chosen CheckName, so delivery
// order can be observed on the receiving side.
func namedEvents(name string) *pb.HealthEvents {
	events := sampleEvents()
	events.Events[0].CheckName = name

	return events
}

// fakeCloser stands in for the owned *grpc.ClientConn.
type fakeCloser struct{ closed atomic.Bool }

func (f *fakeCloser) Close() error {
	f.closed.Store(true)

	return nil
}

// fastDirectTuning keeps direct-mode tests in the millisecond range.
func fastDirectTuning() directTuning {
	return directTuning{
		retryWindow:        5 * time.Second,
		maxBatches:         defaultQueueMaxBatches,
		maxBytes:           defaultQueueMaxBytes,
		rpcTimeout:         2 * time.Second,
		maxMessageBytes:    defaultMaxSendBytes,
		finalAttemptWindow: 5 * time.Millisecond,
	}
}

// publishAsync runs Publish on its own goroutine, since in direct mode Publish
// waits for the batch's outcome; the returned channel carries its result.
func publishAsync(ctx context.Context, p *Publisher, events *pb.HealthEvents) <-chan error {
	result := make(chan error, 1)

	go func() { result <- p.Publish(ctx, events) }()

	return result
}

// awaitPublish returns the result of a publishAsync call, failing the test if
// it does not arrive in time.
func awaitPublish(t *testing.T, result <-chan error) error {
	t.Helper()

	select {
	case err := <-result:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("Publish did not return")

		return nil
	}
}

// waitForQueued blocks until the monitor's queue gauge reports n batches; the
// batch in flight stays counted until it completes.
func waitForQueued(t *testing.T, monitor string, n int) {
	t.Helper()

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(queueBatches.WithLabelValues(monitor)) == float64(n)
	}, 5*time.Second, time.Millisecond, "expected %d queued batches", n)
}

// newDirectPublisher wires a Publisher in direct mode around fakes. The
// returned closer is the fake owned connection.
func newDirectPublisher(
	monitor string, client pb.PlatformConnectorClient, tune directTuning,
) (*Publisher, *fakeCloser) {
	conn := &fakeCloser{}
	p := New(client, "dns:///pcd.nvsentinel:50051", monitor,
		WithRetryPolicy(1, time.Millisecond, 1.0, 0),
		withDirect(conn, tune),
	)

	return p, conn
}

func closePublisher(t *testing.T, p *Publisher) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, p.Close(ctx))
}

// TestDirectPublish_EnqueueDeliversWithKeyAndTrace: the happy path. The
// background sender delivers with exactly one well-formed idempotency-key
// header, Publish returns nil once it has, and the queue gauges return to zero.
func TestDirectPublish_EnqueueDeliversWithKeyAndTrace(t *testing.T) {
	monitor := "test-direct-happy"
	fc := &directFakeClient{}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"background sender must deliver the enqueued batch")

	keys := fc.recordedKeys()
	require.Len(t, keys, 1, "exactly one idempotency-key header value per send")
	assert.Regexp(t, idempotencyKeyFormat, keys[0],
		"generated key must satisfy the server's format contract")

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(queueBatches.WithLabelValues(monitor)) == 0 &&
			testutil.ToFloat64(queueBytes.WithLabelValues(monitor)) == 0
	}, 5*time.Second, time.Millisecond, "queue gauges must return to zero after delivery")

	assert.Equal(t, successBefore+1,
		testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)))

	closePublisher(t, p)
	assert.True(t, conn.closed.Load(), "Close must close the owned connection")
}

// TestDirectPublish_QueueBatchBoundRejects: with the sender wedged, the
// batch cap must reject further Publish calls with ErrPublishQueueFull and
// meter the rejection, while the caller keeps its copy (no delivery).
func TestDirectPublish_QueueBatchBoundRejects(t *testing.T) {
	monitor := "test-direct-queue-batches"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	tune := fastDirectTuning()
	tune.maxBatches = 2

	p, _ := newDirectPublisher(monitor, fc, tune)

	droppedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonQueueFull))

	// The in-flight batch stays at the queue head until it completes, so two
	// waiting Publish calls fill the bound deterministically regardless of
	// sender timing.
	first := publishAsync(context.Background(), p, sampleEvents())
	second := publishAsync(context.Background(), p, sampleEvents())
	waitForQueued(t, monitor, 2)

	err := p.Publish(context.Background(), sampleEvents())
	require.ErrorIs(t, err, ErrPublishQueueFull)

	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonQueueFull)))

	close(gate)
	require.NoError(t, awaitPublish(t, first))
	require.NoError(t, awaitPublish(t, second))
	closePublisher(t, p)
	assert.Equal(t, int64(2), fc.calls.Load(),
		"the rejected batch must never reach the wire")
}

// TestDirectPublish_QueueByteBoundRejects: the byte cap must reject a batch
// that would push the queued total over HEALTH_PUBLISH_QUEUE_MAX_BYTES.
func TestDirectPublish_QueueByteBoundRejects(t *testing.T) {
	monitor := "test-direct-queue-bytes"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	batchSize := int64(proto.Size(sampleEvents()))

	tune := fastDirectTuning()
	tune.maxBytes = batchSize + batchSize/2 // room for one batch, not two

	p, _ := newDirectPublisher(monitor, fc, tune)

	first := publishAsync(context.Background(), p, sampleEvents())
	waitForQueued(t, monitor, 1)
	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublishQueueFull)

	close(gate)
	require.NoError(t, awaitPublish(t, first))
	closePublisher(t, p)
}

// TestDirectPublish_StableKeyAcrossRetries: every retry of a batch must carry
// the identical idempotency-key header value (contract item 1).
func TestDirectPublish_StableKeyAcrossRetries(t *testing.T) {
	monitor := "test-direct-stable-key"
	fc := &directFakeClient{
		responseFn: func(call int) error {
			if call <= 2 {
				return status.Error(codes.Unavailable, "transient")
			}

			return nil
		},
	}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))

	require.Eventually(t, func() bool { return fc.calls.Load() == 3 }, 5*time.Second, time.Millisecond)

	keys := fc.recordedKeys()
	require.Len(t, keys, 3)
	assert.Equal(t, keys[0], keys[1], "retry must reuse the original key verbatim")
	assert.Equal(t, keys[0], keys[2], "retry must reuse the original key verbatim")

	closePublisher(t, p)
}

// TestDirectPublish_DistinctKeysPerBatch: two different batches must not
// share a key, or the server would suppress the second as a replay.
func TestDirectPublish_DistinctKeysPerBatch(t *testing.T) {
	monitor := "test-direct-distinct-keys"
	fc := &directFakeClient{}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))
	require.NoError(t, p.Publish(context.Background(), sampleEvents()))

	require.Eventually(t, func() bool { return fc.calls.Load() == 2 }, 5*time.Second, time.Millisecond)

	keys := fc.recordedKeys()
	require.Len(t, keys, 2)
	assert.NotEqual(t, keys[0], keys[1])

	closePublisher(t, p)
}

// TestDirectPublish_RetryWindowExpiryDrops: a batch failing past the elapsed
// retry window must be dropped with the retry_window_exhausted reason, its
// Publish call must report the drop, and the sender must move on (auth-style
// failures included: every error other than a permanent rejection retries
// inside the window, then drops).
func TestDirectPublish_RetryWindowExpiryDrops(t *testing.T) {
	monitor := "test-direct-window-expiry"
	fc := &directFakeClient{
		responseFn: func(_ int) error {
			return status.Error(codes.Unauthenticated, "validator flap")
		},
	}

	tune := fastDirectTuning()
	tune.retryWindow = 40 * time.Millisecond

	p, _ := newDirectPublisher(monitor, fc, tune)

	droppedBefore := testutil.ToFloat64(
		sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted))
	retriesBefore := testutil.ToFloat64(sendRetries.WithLabelValues(monitor))

	err := p.Publish(context.Background(), sampleEvents())
	require.ErrorIs(t, err, ErrPublishDropped,
		"Publish reports the drop, so the caller does not record the events as reported")
	assert.Contains(t, err.Error(), dropReasonRetryWindowExhausted)

	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)),
		"window expiry must meter a retry_window_exhausted drop")

	assert.GreaterOrEqual(t,
		testutil.ToFloat64(sendRetries.WithLabelValues(monitor)), retriesBefore+1,
		"at least one retry must be metered before the window expires")

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(queueBatches.WithLabelValues(monitor)) == 0
	}, 5*time.Second, time.Millisecond, "the dropped batch must leave the queue")

	closePublisher(t, p)
}

// TestDirectPublish_PermanentRejectionDropsAtOnce: a rejection the server
// would repeat on every retry (an invalid batch, a scope violation, an
// unknown RPC) must drop on the first attempt under the rejected reason
// instead of spending the whole window, and Publish must report it as
// rejected, with the server's status still readable.
func TestDirectPublish_PermanentRejectionDropsAtOnce(t *testing.T) {
	cases := []struct {
		name string
		err  error
	}{
		{"invalid_argument", status.Error(codes.InvalidArgument, "idempotency-key header is required")},
		{"unimplemented", status.Error(codes.Unimplemented, "unknown service")},
		{"permission_denied", status.Error(codes.PermissionDenied, "event names node outside authorized scope")},
	}

	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			monitor := fmt.Sprintf("test-direct-rejected-%d", i)
			fc := &directFakeClient{responseFn: func(_ int) error { return tc.err }}

			p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

			droppedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected))

			err := p.Publish(context.Background(), sampleEvents())
			require.ErrorIs(t, err, ErrPublishRejected)
			assert.Equal(t, status.Code(tc.err), status.Code(err))

			assert.Equal(t, droppedBefore+1,
				testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected)))
			assert.Equal(t, int64(1), fc.calls.Load(), "a permanent rejection must not consume retries")

			closePublisher(t, p)
		})
	}
}

// TestDirectPublish_UnauthenticatedRetries: the projected token rotates, so
// an authentication failure is retried inside the window and the batch
// delivers once a later attempt is accepted.
func TestDirectPublish_UnauthenticatedRetries(t *testing.T) {
	monitor := "test-direct-unauthenticated"
	fc := &directFakeClient{responseFn: func(call int) error {
		if call == 1 {
			return status.Error(codes.Unauthenticated, "token expired")
		}

		return nil
	}}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)) == successBefore+1
	}, 5*time.Second, time.Millisecond)
	assert.Equal(t, int64(2), fc.calls.Load())

	closePublisher(t, p)
}

// TestClose_DrainsQueueBeforeDeadline: Close with a generous deadline must
// deliver everything already queued, report success to every waiting caller,
// then close the connection.
func TestClose_DrainsQueueBeforeDeadline(t *testing.T) {
	monitor := "test-direct-close-drain"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

	const batches = 5

	results := make([]<-chan error, 0, batches)
	for range batches {
		results = append(results, publishAsync(context.Background(), p, sampleEvents()))
	}

	waitForQueued(t, monitor, batches)
	close(gate)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, p.Close(ctx))
	assert.Equal(t, int64(batches), fc.calls.Load(), "Close must drain every queued batch")

	for _, result := range results {
		require.NoError(t, awaitPublish(t, result), "a drained batch is reported as delivered")
	}

	assert.True(t, conn.closed.Load())

	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublisherClosed)
	require.NoError(t, p.Close(ctx), "Close must be idempotent")
}

// TestClose_DeadlineDropsRemainder: with the sender wedged, Close must give
// up at its deadline, drop the remainder under the shutdown reason, tell every
// waiting caller, and still close the connection.
func TestClose_DeadlineDropsRemainder(t *testing.T) {
	monitor := "test-direct-close-deadline"
	gate := make(chan struct{})
	defer close(gate)

	fc := &directFakeClient{gate: gate}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

	droppedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))

	const batches = 3

	results := make([]<-chan error, 0, batches)
	for range batches {
		results = append(results, publishAsync(context.Background(), p, sampleEvents()))
	}

	waitForQueued(t, monitor, batches)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := p.Close(ctx)
	require.Error(t, err, "an incomplete drain must be reported")
	assert.Contains(t, err.Error(), "dropped")

	for _, result := range results {
		require.ErrorIs(t, awaitPublish(t, result), ErrPublishDropped,
			"every undelivered batch is reported to its caller")
	}

	assert.Equal(t, droppedBefore+batches,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown)),
		"every undelivered batch, in-flight included, must be metered as a shutdown drop")
	assert.True(t, conn.closed.Load(), "the connection must close even when the drain is cut short")
	assert.Zero(t, testutil.ToFloat64(queueBatches.WithLabelValues(monitor)))
	assert.Zero(t, testutil.ToFloat64(queueBytes.WithLabelValues(monitor)))
}

// TestDirectPublish_NilAndEmptyStayNoOps: the empty-batch short-circuit must
// hold in direct mode too.
func TestDirectPublish_NilAndEmptyStayNoOps(t *testing.T) {
	monitor := "test-direct-noop"
	fc := &directFakeClient{}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	require.NoError(t, p.Publish(context.Background(), nil))
	require.NoError(t, p.Publish(context.Background(), &pb.HealthEvents{}))

	closePublisher(t, p)
	assert.Equal(t, int64(0), fc.calls.Load())
}

// TestNewIdempotencyKeyFormat: generated keys must satisfy the server's
// validation pattern and be unique.
func TestNewIdempotencyKeyFormat(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		key := newIdempotencyKey()
		assert.Regexp(t, idempotencyKeyFormat, key)
		assert.False(t, seen[key], "keys must not repeat")
		seen[key] = true
	}
}

// TestDirectPublish_RetryWindowCountsQueueTime: the retry window runs from
// enqueue. During an outage every queued batch expires when its own window
// ends, and a batch that waited behind a slow head past its window is dropped
// without an attempt, so the bounded queue holds fresh state rather than
// hours-old state; a batch published after the outage is delivered at once.
func TestDirectPublish_RetryWindowCountsQueueTime(t *testing.T) {
	monitor := "test-direct-window-queue-time"
	window := 100 * time.Millisecond

	var down atomic.Bool

	down.Store(true)

	fc := &directFakeClient{
		responseFn: func(call int) error {
			if down.Load() {
				if call == 1 {
					// A server that answers late, past every queued batch's window.
					time.Sleep(2 * window)
				}

				return status.Error(codes.Unavailable, "outage")
			}

			return nil
		},
	}

	tune := fastDirectTuning()
	tune.retryWindow = window

	p, _ := newDirectPublisher(monitor, fc, tune)

	droppedBefore := testutil.ToFloat64(
		sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted))
	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	head := publishAsync(context.Background(), p, namedEvents("stale-head"))
	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the head batch is inside its slow attempt")

	queued := publishAsync(context.Background(), p, namedEvents("stale-queued"))

	require.ErrorIs(t, awaitPublish(t, head), ErrPublishDropped)
	require.ErrorIs(t, awaitPublish(t, queued), ErrPublishDropped)
	assert.Equal(t, droppedBefore+2,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)),
		"both batches expire when their windows end")

	assert.NotContains(t, fc.recordedCheckNames(), "stale-queued",
		"a batch whose window ended in the queue is dropped without an attempt")

	down.Store(false)

	require.NoError(t, p.Publish(context.Background(), namedEvents("fresh")),
		"a batch published after the outage is delivered")
	assert.Equal(t, successBefore+1, testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)))
	assert.Equal(t, "fresh", fc.recordedCheckNames()[len(fc.recordedCheckNames())-1])

	closePublisher(t, p)
}

// TestDirectPublish_AttemptNeverOutlivesTheWindow: the RPC timeout is clipped
// to what is left of the batch's window, so a hung server cannot hold a
// batch past its window.
func TestDirectPublish_AttemptNeverOutlivesTheWindow(t *testing.T) {
	monitor := "test-direct-window-clips-rpc"
	fc := &directFakeClient{gate: make(chan struct{})}

	tune := fastDirectTuning()
	tune.retryWindow = 100 * time.Millisecond
	tune.rpcTimeout = time.Minute

	p, _ := newDirectPublisher(monitor, fc, tune)

	droppedBefore := testutil.ToFloat64(
		sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted))

	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublishDropped,
		"the hung attempt ends with the window and the batch is dropped")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)))

	closePublisher(t, p)
}

// TestDirectPublish_LastAttemptRunsAtTheDeadline: the last backoff sleep is
// cut short so a final attempt still starts inside the window, instead of the
// window ending unused in the middle of a sleep.
func TestDirectPublish_LastAttemptRunsAtTheDeadline(t *testing.T) {
	monitor := "test-direct-final-attempt"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	tune := fastDirectTuning()
	tune.retryWindow = 200 * time.Millisecond
	tune.finalAttemptWindow = 50 * time.Millisecond

	// A backoff far longer than the window: uncut, the sender would sleep
	// through the deadline after one attempt.
	conn := &fakeCloser{}
	p := New(fc, "dns:///pcd.nvsentinel:50051", monitor,
		WithRetryPolicy(1, 10*time.Second, 1.0, 0),
		withDirect(conn, tune),
	)

	start := time.Now()
	err := p.Publish(context.Background(), sampleEvents())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrPublishDropped)
	assert.Equal(t, int64(2), fc.calls.Load(),
		"one attempt at once and one final attempt just before the deadline")
	assert.Less(t, elapsed, 2*time.Second, "the 10s backoff sleep is cut to the window")

	closePublisher(t, p)
}

// TestClose_PromptDuringBackoffSleep: Close's deadline must interrupt a long
// backoff sleep instead of waiting it out, dropping the batch under the
// shutdown reason.
func TestClose_PromptDuringBackoffSleep(t *testing.T) {
	monitor := "test-direct-close-backoff"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	// A window longer than the backoff, so the sender enters the sleep
	// instead of dropping the batch as expired.
	tune := fastDirectTuning()
	tune.retryWindow = time.Minute

	conn := &fakeCloser{}
	p := New(fc, "dns:///pcd.nvsentinel:50051", monitor,
		WithRetryPolicy(1, 10*time.Second, 1.0, 0),
		withDirect(conn, tune),
	)

	droppedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))

	result := publishAsync(context.Background(), p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() >= 1 }, 5*time.Second, time.Millisecond,
		"the first attempt must fail so the sender enters its backoff sleep")

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_ = p.Close(ctx)
	elapsed := time.Since(start)

	assert.Less(t, elapsed, 2*time.Second,
		"Close must interrupt the 10s backoff sleep, not wait it out")
	require.ErrorIs(t, awaitPublish(t, result), ErrPublishDropped,
		"the caller learns its batch was not delivered")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown)),
		"the batch stuck in backoff must be metered as a shutdown drop")
	assert.True(t, conn.closed.Load())
}

// TestDirectPublish_FIFOOrder: batches must reach the wire in publish order,
// also when several callers are waiting at once.
func TestDirectPublish_FIFOOrder(t *testing.T) {
	monitor := "test-direct-fifo"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	names := []string{"Check0", "Check1", "Check2", "Check3", "Check4"}

	results := make([]<-chan error, 0, len(names))
	for i, name := range names {
		results = append(results, publishAsync(context.Background(), p, namedEvents(name)))
		waitForQueued(t, monitor, i+1)
	}

	close(gate)

	for _, result := range results {
		require.NoError(t, awaitPublish(t, result))
	}

	assert.Equal(t, names, fc.recordedCheckNames(),
		"delivery order must equal publish order")

	closePublisher(t, p)
}

// TestDirectPublish_ConcurrentPublishRacingClose: under -race, every Publish
// that returned nil was delivered, every one reporting a drop was metered as
// a shutdown drop, the rest were refused as closed, and nothing may be
// delivered after Close returns.
func TestDirectPublish_ConcurrentPublishRacingClose(t *testing.T) {
	monitor := "test-direct-publish-close-race"
	fc := &directFakeClient{responseFn: func(_ int) error {
		// Slow the sender slightly so Close's deadline cuts a real drain short.
		time.Sleep(time.Millisecond)

		return nil
	}}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))
	shutdownBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))

	var delivered, droppedOnShutdown, refused atomic.Int64

	var wg sync.WaitGroup

	const publishers, perPublisher = 4, 30

	for range publishers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range perPublisher {
				err := p.Publish(context.Background(), sampleEvents())

				switch {
				case err == nil:
					delivered.Add(1)
				case errors.Is(err, ErrPublishDropped):
					droppedOnShutdown.Add(1)
				case errors.Is(err, ErrPublisherClosed):
					refused.Add(1)
				default:
					t.Errorf("unexpected Publish error: %v", err)
				}
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	_ = p.Close(ctx)

	callsAtCloseReturn := fc.calls.Load()

	wg.Wait()
	time.Sleep(50 * time.Millisecond)

	assert.Equal(t, callsAtCloseReturn, fc.calls.Load(),
		"nothing may be delivered after Close returns")

	assert.Equal(t, float64(delivered.Load()),
		testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))-successBefore,
		"every Publish that returned nil was delivered, and nothing else was")
	assert.Equal(t, float64(droppedOnShutdown.Load()),
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))-shutdownBefore,
		"every Publish reporting a drop was metered as a shutdown drop")
	assert.Equal(t, int64(publishers*perPublisher), delivered.Load()+droppedOnShutdown.Load()+refused.Load())
}

// TestCloseWithTimeout_DrainsDirectQueue: the convenience wrapper must behave
// like Close with a deadline: drain, then close the owned connection.
func TestCloseWithTimeout_DrainsDirectQueue(t *testing.T) {
	monitor := "test-direct-close-with-timeout"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

	result := publishAsync(context.Background(), p, sampleEvents())
	waitForQueued(t, monitor, 1)
	close(gate)

	p.CloseWithTimeout(5 * time.Second)

	require.NoError(t, awaitPublish(t, result), "the queued batch drains before the timeout")
	assert.Equal(t, int64(1), fc.calls.Load())
	assert.True(t, conn.closed.Load(), "CloseWithTimeout must close the owned connection")

	p.CloseWithTimeout(5 * time.Second)
	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublisherClosed)
}

// TestDirectPublish_QueueHoldsItsOwnCopy: the queue holds the publisher's
// own copy, so a caller that changes its message while the batch is still
// pending does not change what is delivered.
func TestDirectPublish_QueueHoldsItsOwnCopy(t *testing.T) {
	monitor := "test-direct-own-copy"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	events := namedEvents("original")
	result := publishAsync(context.Background(), p, events)

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the batch is inside its attempt")

	events.Events[0].CheckName = "changed-while-pending"
	close(gate)

	require.NoError(t, awaitPublish(t, result))
	assert.Equal(t, []string{"original"}, fc.recordedCheckNames(),
		"the delivered batch is the one handed to Publish")

	closePublisher(t, p)
}

// TestDirectPublish_PreCancelledContextIsRefused: nothing is accepted for a
// caller that has already left; socket mode sends nothing either.
func TestDirectPublish_PreCancelledContextIsRefused(t *testing.T) {
	monitor := "test-direct-precancelled"
	fc := &directFakeClient{}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, p.Publish(ctx, sampleEvents()), context.Canceled)

	closePublisher(t, p)
	assert.Equal(t, int64(0), fc.calls.Load(), "a refused batch never reaches the wire")
	assert.Zero(t, testutil.ToFloat64(queueBatches.WithLabelValues(monitor)))
}

// TestDirectPublish_CancelledCallerWithdrawsAQueuedBatch: a caller that
// leaves while its batch still waits in the queue takes the batch with it,
// so its re-emit cannot be stored next to it; the drop is metered as
// withdrawn and the batch never reaches the wire.
func TestDirectPublish_CancelledCallerWithdrawsAQueuedBatch(t *testing.T) {
	monitor := "test-direct-withdraw-queued"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	withdrawnBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn))

	head := publishAsync(context.Background(), p, namedEvents("head"))
	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the head is inside its attempt before the second batch is queued behind it")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	queued := publishAsync(ctx, p, namedEvents("withdrawn"))
	waitForQueued(t, monitor, 2)

	cancel()

	err := awaitPublish(t, queued)
	require.ErrorIs(t, err, ErrPublishDropped)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), dropReasonWithdrawn)

	waitForQueued(t, monitor, 1)
	assert.Equal(t, withdrawnBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn)))

	close(gate)
	require.NoError(t, awaitPublish(t, head))
	closePublisher(t, p)

	assert.Equal(t, []string{"head"}, fc.recordedCheckNames(), "the withdrawn batch is never sent")
}

// TestDirectPublish_CancelledCallerKeepsTheAttemptInFlight: an attempt already
// on the wire may have stored the batch, so a caller leaving during it waits
// for that attempt's answer and learns the real outcome.
func TestDirectPublish_CancelledCallerKeepsTheAttemptInFlight(t *testing.T) {
	monitor := "test-direct-withdraw-in-flight"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := publishAsync(ctx, p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the batch is inside its attempt")
	cancel()

	select {
	case err := <-result:
		t.Fatalf("Publish returned %v while its attempt was still in flight", err)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	require.NoError(t, awaitPublish(t, result), "the attempt stored the batch, and the caller is told so")

	closePublisher(t, p)
}

// TestDirectPublish_CancelledCallerStopsRetries: once the caller has left, a
// failed attempt is not retried, even in the middle of a long backoff.
func TestDirectPublish_CancelledCallerStopsRetries(t *testing.T) {
	monitor := "test-direct-withdraw-retries"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	tune := fastDirectTuning()
	tune.retryWindow = time.Minute

	conn := &fakeCloser{}
	p := New(fc, "dns:///pcd.nvsentinel:50051", monitor,
		WithRetryPolicy(1, 10*time.Second, 1.0, 0),
		withDirect(conn, tune),
	)

	withdrawnBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := publishAsync(ctx, p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the first attempt fails and the sender enters its backoff sleep")

	start := time.Now()

	cancel()

	err := awaitPublish(t, result)
	require.ErrorIs(t, err, ErrPublishDropped)
	assert.Less(t, time.Since(start), 2*time.Second, "the backoff sleep ends with the caller")
	assert.Equal(t, int64(1), fc.calls.Load(), "no attempt is made for a caller that left")
	assert.Equal(t, withdrawnBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn)))

	closePublisher(t, p)
}

// TestPublisher_WaitingOnServer: while a batch is pending inside its window a
// caller is waiting for the server, which a liveness check may treat as
// alive; an idle publisher and a socket-mode publisher never report waiting.
func TestPublisher_WaitingOnServer(t *testing.T) {
	monitor := "test-direct-waiting"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	assert.False(t, p.WaitingOnServer(), "nothing pending")

	result := publishAsync(context.Background(), p, sampleEvents())
	require.Eventually(t, p.WaitingOnServer, 5*time.Second, time.Millisecond, "a pending batch is a wait")

	close(gate)
	require.NoError(t, awaitPublish(t, result))
	require.Eventually(t, func() bool { return !p.WaitingOnServer() }, 5*time.Second, time.Millisecond,
		"delivered: nothing pending")

	closePublisher(t, p)

	socket := New(&fakePCClient{}, "unix:///nonexistent/nvsentinel.sock", monitor)
	assert.False(t, socket.WaitingOnServer(), "socket mode never waits on a queue")
}

// TestDirectPublish_DefaultFinalAttemptWindowKeepsRetrying: with the
// production final-attempt allowance a failing batch retries until about a
// second before its window ends, then drops; no override is needed for a
// window that leaves room to retry.
func TestDirectPublish_DefaultFinalAttemptWindowKeepsRetrying(t *testing.T) {
	monitor := "test-direct-default-final-window"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	tune := defaultDirectTuning()
	tune.retryWindow = 3 * time.Second

	p, _ := newDirectPublisher(monitor, fc, tune)

	retriesBefore := testutil.ToFloat64(sendRetries.WithLabelValues(monitor))

	start := time.Now()
	err := p.Publish(context.Background(), sampleEvents())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrPublishDropped)
	assert.GreaterOrEqual(t, elapsed, 1900*time.Millisecond, "retries run until the final-attempt window")
	assert.Less(t, elapsed, 3500*time.Millisecond)
	assert.GreaterOrEqual(t, testutil.ToFloat64(sendRetries.WithLabelValues(monitor)), retriesBefore+1)

	closePublisher(t, p)
}

// TestDirectPublish_ResourceExhaustedIsRetried: gRPC uses RESOURCE_EXHAUSTED
// for transient overload and quotas too, so it is retried inside the window
// like any other transient failure.
func TestDirectPublish_ResourceExhaustedIsRetried(t *testing.T) {
	monitor := "test-direct-resource-exhausted"
	fc := &directFakeClient{responseFn: func(call int) error {
		if call == 1 {
			return status.Error(codes.ResourceExhausted, "rate limited by the mesh")
		}

		return nil
	}}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	rejectedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected))
	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))

	require.Eventually(t, func() bool {
		return testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)) == successBefore+1
	}, 5*time.Second, time.Millisecond, "the batch delivers on the retry")

	assert.Equal(t, int64(2), fc.calls.Load())
	assert.Equal(t, rejectedBefore, testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected)))

	closePublisher(t, p)
}

// TestDirectPublish_OversizeBatchIsRejected: a batch larger than the server
// receives could never be delivered, so Publish refuses it at once as
// rejected, metered but never queued or attempted.
func TestDirectPublish_OversizeBatchIsRejected(t *testing.T) {
	monitor := "test-direct-oversize"
	fc := &directFakeClient{}

	tune := fastDirectTuning()
	tune.maxMessageBytes = 8

	p, _ := newDirectPublisher(monitor, fc, tune)

	rejectedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected))

	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublishRejected)
	assert.Equal(t, rejectedBefore+1, testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected)))

	closePublisher(t, p)
	assert.Equal(t, int64(0), fc.calls.Load(), "an oversize batch is never sent")
	assert.Equal(t, float64(0), testutil.ToFloat64(queueBatches.WithLabelValues(monitor)), "and never queued")
}

// TestDirectPublish_OversizeAfterCloseIsClosed: after Close, an oversize batch
// is refused as closed like any other, not metered as rejected.
func TestDirectPublish_OversizeAfterCloseIsClosed(t *testing.T) {
	monitor := "test-direct-oversize-closed"
	fc := &directFakeClient{}

	tune := fastDirectTuning()
	tune.maxMessageBytes = 8

	p, _ := newDirectPublisher(monitor, fc, tune)
	closePublisher(t, p)

	rejectedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected))

	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublisherClosed)
	assert.Equal(t, rejectedBefore, testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected)),
		"a closed publisher does not count the batch as an oversize drop")
}
