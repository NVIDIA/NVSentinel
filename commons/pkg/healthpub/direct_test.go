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
	"strings"
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
// blocks each call until released (for slot and deadline tests). A call is
// counted when it starts and recorded once it passes the gate. inFlight and
// maxInFlight observe how many calls overlap.
type directFakeClient struct {
	mu         sync.Mutex
	keys       []string
	checkNames []string
	calls      atomic.Int64

	inFlight    atomic.Int64
	maxInFlight atomic.Int64

	gate       chan struct{}
	responseFn func(call int) error
}

func (f *directFakeClient) HealthEventOccurredV1(
	ctx context.Context, events *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	n := int(f.calls.Add(1))

	f.trackInFlight(f.inFlight.Add(1))
	defer f.inFlight.Add(-1)

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

// trackInFlight raises maxInFlight to current when current is a new high.
func (f *directFakeClient) trackInFlight(current int64) {
	for {
		seen := f.maxInFlight.Load()
		if current <= seen || f.maxInFlight.CompareAndSwap(seen, current) {
			return
		}
	}
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

// retryingClient puts a fake behind the retry interceptor of dial.go, the way
// a dialed connection carries it, by standing in for the connection the
// generated client invokes. The direct-mode tests so run the retry policy the
// monitors run.
func retryingClient(client pb.PlatformConnectorClient, tune directTuning) pb.PlatformConnectorClient {
	return pb.NewPlatformConnectorClient(&interceptedConn{client: client, intercept: retryInterceptor(tune)})
}

type interceptedConn struct {
	client    pb.PlatformConnectorClient
	intercept grpc.UnaryClientInterceptor
}

func (c *interceptedConn) Invoke(ctx context.Context, method string, args, reply any, opts ...grpc.CallOption) error {
	invoke := func(ctx context.Context, _ string, args, reply any, _ *grpc.ClientConn, opts ...grpc.CallOption) error {
		events, ok := args.(*pb.HealthEvents)
		if !ok {
			return fmt.Errorf("unexpected request type %T", args)
		}

		resp, err := c.client.HealthEventOccurredV1(ctx, events, opts...)
		if err != nil {
			return err
		}

		out, ok := reply.(proto.Message)
		if !ok {
			return fmt.Errorf("unexpected reply type %T", reply)
		}

		proto.Merge(out, resp)

		return nil
	}

	return c.intercept(ctx, method, args, reply, nil, invoke, opts...)
}

func (c *interceptedConn) NewStream(
	context.Context, *grpc.StreamDesc, string, ...grpc.CallOption,
) (grpc.ClientStream, error) {
	return nil, errors.New("the health publisher makes no streaming calls")
}

// fastDirectTuning keeps direct-mode tests in the millisecond range: attempts
// start a millisecond apart, doubling to a second, without jitter, so counts
// and timings are exact.
func fastDirectTuning() directTuning {
	return directTuning{
		retryWindow:    5 * time.Second,
		rpcTimeout:     2 * time.Second,
		backoffInitial: time.Millisecond,
		backoffMax:     time.Second,
		backoffJitter:  0,
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

// pendingCount is the number of Publish calls in progress: waiting for the
// slot or sending.
func pendingCount(p *Publisher) int {
	p.direct.mu.Lock()
	defer p.direct.mu.Unlock()

	return p.direct.pending
}

// waitForPending blocks until n Publish calls are in progress.
func waitForPending(t *testing.T, p *Publisher, n int) {
	t.Helper()

	require.Eventually(t, func() bool { return pendingCount(p) == n },
		5*time.Second, time.Millisecond, "expected %d pending publishes", n)
}

// newDirectPublisher wires a Publisher in direct mode around fakes: the fake
// client behind the retry interceptor, as on a dialed connection. The
// returned closer is the fake owned connection.
func newDirectPublisher(
	monitor string, client pb.PlatformConnectorClient, tune directTuning,
) (*Publisher, *fakeCloser) {
	conn := &fakeCloser{}
	p := New(retryingClient(client, tune), "dns:///pcd.nvsentinel:50051", monitor,
		withDirect(conn, tune))

	return p, conn
}

// newSlowBackoffPublisher is newDirectPublisher with a 10s pause before every
// retry, far longer than any test window, for tests that must catch a call
// inside its backoff sleep.
func newSlowBackoffPublisher(
	monitor string, client pb.PlatformConnectorClient, tune directTuning,
) (*Publisher, *fakeCloser) {
	tune.backoffInitial = 10 * time.Second
	tune.backoffMax = 10 * time.Second

	return newDirectPublisher(monitor, client, tune)
}

func closePublisher(t *testing.T, p *Publisher) {
	t.Helper()

	require.NoError(t, p.Close())
}

// TestDirectPublish_DeliversWithKey: the happy path. Publish sends the batch
// itself with exactly one well-formed idempotency-key header, returns nil once
// the server answered, and leaves nothing pending.
func TestDirectPublish_DeliversWithKey(t *testing.T) {
	monitor := "test-direct-happy"
	fc := &directFakeClient{}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

	successBefore := testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))
	assert.Equal(t, int64(1), fc.calls.Load())

	keys := fc.recordedKeys()
	require.Len(t, keys, 1, "exactly one idempotency-key header value per send")
	assert.Regexp(t, idempotencyKeyFormat, keys[0],
		"generated key must satisfy the server's format contract")

	assert.Equal(t, successBefore+1,
		testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)))
	assert.Zero(t, pendingCount(p), "nothing is pending once Publish returned")

	closePublisher(t, p)
	assert.True(t, conn.closed.Load(), "Close must close the owned connection")
}

// TestDirectPublish_StableKeyAcrossRetries: every retry of a batch must carry
// the key of its first attempt, so the server can recognise a resend.
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

	retriesBefore := testutil.ToFloat64(sendRetries.WithLabelValues(monitor))

	require.NoError(t, p.Publish(context.Background(), sampleEvents()))
	assert.Equal(t, int64(3), fc.calls.Load())

	keys := fc.recordedKeys()
	require.Len(t, keys, 3)
	assert.Equal(t, keys[0], keys[1], "retry must reuse the original key verbatim")
	assert.Equal(t, keys[0], keys[2], "retry must reuse the original key verbatim")
	assert.Equal(t, retriesBefore+2, testutil.ToFloat64(sendRetries.WithLabelValues(monitor)))

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

	keys := fc.recordedKeys()
	require.Len(t, keys, 2)
	assert.NotEqual(t, keys[0], keys[1])

	closePublisher(t, p)
}

// TestDirectPublish_RetryWindowExpiryDrops: a batch failing past the elapsed
// retry window must be dropped with the retry_window_exhausted reason and its
// Publish call must report the drop (auth-style failures included: every
// error other than a permanent rejection retries inside the window, then
// drops).
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
	assert.Zero(t, pendingCount(p))

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
	assert.Equal(t, successBefore+1, testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)))
	assert.Equal(t, int64(2), fc.calls.Load())

	closePublisher(t, p)
}

// TestDirectPublish_ResourceExhaustedIsRetried: gRPC uses RESOURCE_EXHAUSTED
// for transient overload and quotas, and a proxy may answer it for throttling,
// so it is retried inside the window like any other transient failure.
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

	assert.Equal(t, successBefore+1, testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor)))
	assert.Equal(t, int64(2), fc.calls.Load())
	assert.Equal(t, rejectedBefore, testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected)))

	closePublisher(t, p)
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

// TestDirectPublish_OneSendAtATime: however many goroutines publish at once,
// the server sees one attempt at a time, and every caller learns the outcome
// of its own batch.
func TestDirectPublish_OneSendAtATime(t *testing.T) {
	monitor := "test-direct-one-at-a-time"
	fc := &directFakeClient{responseFn: func(_ int) error {
		time.Sleep(2 * time.Millisecond)

		return nil
	}}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	const publishers, perPublisher = 8, 5

	var wg sync.WaitGroup

	for range publishers {
		wg.Add(1)

		go func() {
			defer wg.Done()

			for range perPublisher {
				assert.NoError(t, p.Publish(context.Background(), sampleEvents()))
			}
		}()
	}

	wg.Wait()

	assert.Equal(t, int64(publishers*perPublisher), fc.calls.Load())
	assert.Equal(t, int64(1), fc.maxInFlight.Load(), "attempts never overlap")

	closePublisher(t, p)
}

// TestDirectPublish_ArrivalOrder: calls waiting for the slot get it in the
// order they were made, so batches reach the wire in publish order also when
// several callers wait at once. Callers are started one at a time with a
// short pause, since a goroutine has to reach its wait before the next one
// starts for the order to be defined.
func TestDirectPublish_ArrivalOrder(t *testing.T) {
	monitor := "test-direct-order"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	names := []string{"Check0", "Check1", "Check2", "Check3", "Check4"}

	results := make([]<-chan error, 0, len(names))
	for i, name := range names {
		results = append(results, publishAsync(context.Background(), p, namedEvents(name)))
		waitForPending(t, p, i+1)
		time.Sleep(50 * time.Millisecond)
	}

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"only the first caller holds the slot; the rest wait for it")

	close(gate)

	for _, result := range results {
		require.NoError(t, awaitPublish(t, result))
	}

	assert.Equal(t, names, fc.recordedCheckNames(),
		"delivery order must equal publish order")

	closePublisher(t, p)
}

// TestDirectPublish_RetryWindowCountsWaitingForTheSlot: the retry window runs
// from the Publish call. During an outage a batch that waited for the slot
// behind a slow head past its own window is dropped without an attempt, so an
// old batch is never delivered late; a batch published after the outage is
// delivered at once.
func TestDirectPublish_RetryWindowCountsWaitingForTheSlot(t *testing.T) {
	monitor := "test-direct-window-slot-time"
	window := 100 * time.Millisecond

	var down atomic.Bool

	down.Store(true)

	fc := &directFakeClient{
		responseFn: func(call int) error {
			if down.Load() {
				if call == 1 {
					// A server that answers late, past every waiting batch's window.
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

	waiting := publishAsync(context.Background(), p, namedEvents("stale-waiting"))

	require.ErrorIs(t, awaitPublish(t, head), ErrPublishDropped)
	require.ErrorIs(t, awaitPublish(t, waiting), ErrPublishDropped)
	assert.Equal(t, droppedBefore+2,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)),
		"both batches expire when their windows end")

	assert.NotContains(t, fc.recordedCheckNames(), "stale-waiting",
		"a batch whose window ended while it waited for the slot is dropped without an attempt")

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

	start := time.Now()

	require.ErrorIs(t, p.Publish(context.Background(), sampleEvents()), ErrPublishDropped,
		"the hung attempt ends with the window and the batch is dropped")
	assert.Less(t, time.Since(start), 5*time.Second, "the minute-long RPC timeout is clipped to the window")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)))

	closePublisher(t, p)
}

// TestDirectPublish_WindowEndsDuringBackoff: a backoff pause longer than what
// is left of the window ends with the window, not after the pause: the batch
// drops as retry_window_exhausted at the deadline, with no further attempt.
func TestDirectPublish_WindowEndsDuringBackoff(t *testing.T) {
	monitor := "test-direct-window-ends-in-backoff"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	tune := fastDirectTuning()
	tune.retryWindow = 200 * time.Millisecond

	// A 10s pause: uncut, the call would sleep far past the deadline.
	p, _ := newSlowBackoffPublisher(monitor, fc, tune)

	droppedBefore := testutil.ToFloat64(
		sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted))

	start := time.Now()
	err := p.Publish(context.Background(), sampleEvents())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrPublishDropped)
	assert.Contains(t, err.Error(), dropReasonRetryWindowExhausted)
	assert.Equal(t, int64(1), fc.calls.Load(), "the one attempt fails and the pause outlasts the window")
	assert.GreaterOrEqual(t, elapsed, 150*time.Millisecond, "the call lasts until the window ends")
	assert.Less(t, elapsed, 2*time.Second, "the 10s pause is cut by the deadline")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRetryWindowExhausted)))

	closePublisher(t, p)
}

// TestDirectPublish_RetriesUntilTheWindowEnds: a failing batch keeps
// retrying, each pause twice the previous, until its window ends, then drops.
func TestDirectPublish_RetriesUntilTheWindowEnds(t *testing.T) {
	monitor := "test-direct-retries-until-window"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	// The production pacing at a tenth of its scale and without jitter, in a
	// 2.5 s window: attempts at 0, 0.2, 0.6 and 1.4 s, then the window ends
	// during the fourth pause, which would have lasted until 3 s.
	tune := defaultDirectTuning()
	tune.retryWindow = 2500 * time.Millisecond
	tune.backoffInitial = 200 * time.Millisecond
	tune.backoffMax = 3 * time.Second
	tune.backoffJitter = 0

	p, _ := newDirectPublisher(monitor, fc, tune)

	retriesBefore := testutil.ToFloat64(sendRetries.WithLabelValues(monitor))

	start := time.Now()
	err := p.Publish(context.Background(), sampleEvents())
	elapsed := time.Since(start)

	require.ErrorIs(t, err, ErrPublishDropped)
	assert.Equal(t, int64(4), fc.calls.Load(), "attempts at 0, 0.2, 0.6 and 1.4 s")
	assert.GreaterOrEqual(t, elapsed, 2400*time.Millisecond, "retries run until the window ends")
	assert.Less(t, elapsed, 3500*time.Millisecond)
	assert.Equal(t, retriesBefore+3, testutil.ToFloat64(sendRetries.WithLabelValues(monitor)))

	closePublisher(t, p)
}

// TestClose_EndsACallInBackoff: Close ends a call in the middle of a long
// backoff pause at once, dropping the batch under the shutdown reason, and
// closes the connection.
func TestClose_EndsACallInBackoff(t *testing.T) {
	monitor := "test-direct-close-backoff"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	// A window longer than the pause, so the call enters the pause instead of
	// dropping the batch as expired.
	tune := fastDirectTuning()
	tune.retryWindow = time.Minute

	p, conn := newSlowBackoffPublisher(monitor, fc, tune)

	droppedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))

	result := publishAsync(context.Background(), p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() >= 1 }, 5*time.Second, time.Millisecond,
		"the first attempt must fail so the call enters its backoff pause")

	start := time.Now()
	require.NoError(t, p.Close())

	err := awaitPublish(t, result)
	require.ErrorIs(t, err, ErrPublishDropped, "the caller learns its batch was not delivered")
	assert.Contains(t, err.Error(), dropReasonShutdown)
	assert.Less(t, time.Since(start), 2*time.Second, "Close ends the 10s pause, it does not wait it out")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown)),
		"the batch in its pause is metered as a shutdown drop")
	assert.True(t, conn.closed.Load())
	assert.Zero(t, pendingCount(p))
}

// TestClose_EndsACallInFlight: Close ends an attempt on the wire at once too;
// the call reports a shutdown drop and the connection is closed without
// waiting for the server.
func TestClose_EndsACallInFlight(t *testing.T) {
	monitor := "test-direct-close-in-flight"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

	droppedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))

	result := publishAsync(context.Background(), p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the batch is inside its attempt")

	start := time.Now()
	require.NoError(t, p.Close())

	err := awaitPublish(t, result)
	require.ErrorIs(t, err, ErrPublishDropped)
	assert.Contains(t, err.Error(), dropReasonShutdown)
	assert.Less(t, time.Since(start), 2*time.Second, "Close does not wait for the server")
	assert.Equal(t, droppedBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown)))
	assert.True(t, conn.closed.Load())

	close(gate)
	assert.Zero(t, pendingCount(p))
}

// TestDirectPublish_ConcurrentPublishRacingClose: under -race, every Publish
// that returned nil was delivered, every one reporting a drop was metered as
// a shutdown drop, and the rest were refused as closed.
func TestDirectPublish_ConcurrentPublishRacingClose(t *testing.T) {
	monitor := "test-direct-publish-close-race"
	fc := &directFakeClient{responseFn: func(_ int) error {
		// Slow the sends slightly so Close cuts real work short.
		time.Sleep(time.Millisecond)

		return nil
	}}

	p, conn := newDirectPublisher(monitor, fc, fastDirectTuning())

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

	require.Eventually(t, func() bool { return delivered.Load() > 0 }, 5*time.Second, time.Millisecond,
		"batches deliver before Close")
	require.NoError(t, p.Close())

	wg.Wait()

	assert.True(t, conn.closed.Load())
	assert.Equal(t, int64(publishers*perPublisher), delivered.Load()+droppedOnShutdown.Load()+refused.Load())
	assert.Positive(t, refused.Load(), "calls made after Close were refused")
	assert.Equal(t, float64(delivered.Load()),
		testutil.ToFloat64(sendsSuccess.WithLabelValues(monitor))-successBefore,
		"every Publish that returned nil was delivered, and nothing else was")
	assert.Equal(t, float64(droppedOnShutdown.Load()),
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonShutdown))-shutdownBefore,
		"every drop reported was metered as a shutdown drop")
	assert.Zero(t, pendingCount(p))
}

// TestDirectPublish_OversizeBatchIsRejected: a batch larger than the server
// receives could never be delivered, so Publish refuses it at once as
// rejected, metered but never attempted, instead of spending its window.
func TestDirectPublish_OversizeBatchIsRejected(t *testing.T) {
	monitor := "test-direct-oversize"
	fc := &directFakeClient{}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	rejectedBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected))

	events := sampleEvents()
	events.Events[0].Message = strings.Repeat("x", maxSendBytes+1)

	require.ErrorIs(t, p.Publish(context.Background(), events), ErrPublishRejected)
	assert.Equal(t, rejectedBefore+1, testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonRejected)))
	assert.Equal(t, int64(0), fc.calls.Load(), "an oversize batch is never sent")
	assert.Zero(t, pendingCount(p))

	closePublisher(t, p)
}

// TestDirectPublish_PreCancelledContextIsWithdrawn: a caller that has already
// left gets its batch withdrawn before anything is attempted.
func TestDirectPublish_PreCancelledContextIsWithdrawn(t *testing.T) {
	monitor := "test-direct-precancelled"
	fc := &directFakeClient{}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	withdrawnBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := p.Publish(ctx, sampleEvents())
	require.ErrorIs(t, err, ErrPublishDropped)
	require.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, withdrawnBefore+1, testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn)))

	closePublisher(t, p)
	assert.Equal(t, int64(0), fc.calls.Load(), "a withdrawn batch never reaches the wire")
	assert.Zero(t, pendingCount(p))
}

// TestDirectPublish_CancelledCallerWaitingForTheSlotIsWithdrawn: a caller
// that leaves while its batch still waits for the slot takes the batch with
// it, so its re-emit cannot be stored next to it; the drop is metered as
// withdrawn and the batch never reaches the wire.
func TestDirectPublish_CancelledCallerWaitingForTheSlotIsWithdrawn(t *testing.T) {
	monitor := "test-direct-withdraw-waiting"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	withdrawnBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn))

	head := publishAsync(context.Background(), p, namedEvents("head"))

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the head is inside its attempt before the second batch waits behind it")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	waiting := publishAsync(ctx, p, namedEvents("withdrawn"))
	waitForPending(t, p, 2)

	cancel()

	err := awaitPublish(t, waiting)
	require.ErrorIs(t, err, ErrPublishDropped)
	require.ErrorIs(t, err, context.Canceled)
	assert.Contains(t, err.Error(), dropReasonWithdrawn)

	waitForPending(t, p, 1)
	assert.Equal(t, withdrawnBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn)))

	close(gate)
	require.NoError(t, awaitPublish(t, head))
	closePublisher(t, p)

	assert.Equal(t, []string{"head"}, fc.recordedCheckNames(), "the withdrawn batch is never sent")
}

// TestDirectPublish_CancelledCallerEndsTheAttemptInFlight: a caller that
// leaves while its attempt is on the wire gets its batch withdrawn at once;
// the attempt is cut with the caller's context, not waited for.
func TestDirectPublish_CancelledCallerEndsTheAttemptInFlight(t *testing.T) {
	monitor := "test-direct-withdraw-in-flight"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	withdrawnBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := publishAsync(ctx, p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the batch is inside its attempt")

	start := time.Now()

	cancel()

	err := awaitPublish(t, result)
	require.ErrorIs(t, err, ErrPublishDropped)
	require.ErrorIs(t, err, context.Canceled, "the caller's cancellation stays readable in the error")
	assert.Less(t, time.Since(start), 2*time.Second, "the attempt ends with the caller, not with the gate")
	assert.Equal(t, int64(1), fc.calls.Load(), "no retry for a caller that left")
	assert.Equal(t, withdrawnBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn)))

	close(gate)
	closePublisher(t, p)
}

// TestDirectPublish_CancelledCallerStopsRetries: once the caller has left, a
// failed attempt is not retried, even in the middle of a long backoff pause.
func TestDirectPublish_CancelledCallerStopsRetries(t *testing.T) {
	monitor := "test-direct-withdraw-retries"
	fc := &directFakeClient{responseFn: func(_ int) error {
		return status.Error(codes.Unavailable, "down")
	}}

	tune := fastDirectTuning()
	tune.retryWindow = time.Minute

	p, _ := newSlowBackoffPublisher(monitor, fc, tune)

	withdrawnBefore := testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := publishAsync(ctx, p, sampleEvents())

	require.Eventually(t, func() bool { return fc.calls.Load() == 1 }, 5*time.Second, time.Millisecond,
		"the first attempt fails and the call enters its backoff pause")

	start := time.Now()

	cancel()

	err := awaitPublish(t, result)
	require.ErrorIs(t, err, ErrPublishDropped)
	require.ErrorIs(t, err, context.Canceled, "the caller's cancellation stays readable in the error")
	assert.Less(t, time.Since(start), 2*time.Second, "the backoff pause ends with the caller")
	assert.Equal(t, int64(1), fc.calls.Load(), "no attempt is made for a caller that left")
	assert.Equal(t, withdrawnBefore+1,
		testutil.ToFloat64(sendsDropped.WithLabelValues(monitor, dropReasonWithdrawn)))

	closePublisher(t, p)
}

// TestPublisher_WaitingOnServer: while a call is pending a caller is waiting
// for the server, which a liveness check may treat as alive; an idle
// publisher and a socket-mode publisher never report waiting.
func TestPublisher_WaitingOnServer(t *testing.T) {
	monitor := "test-direct-waiting"
	gate := make(chan struct{})
	fc := &directFakeClient{gate: gate}

	p, _ := newDirectPublisher(monitor, fc, fastDirectTuning())

	assert.False(t, p.WaitingOnServer(), "nothing pending")

	result := publishAsync(context.Background(), p, sampleEvents())
	require.Eventually(t, p.WaitingOnServer, 5*time.Second, time.Millisecond, "a pending call is a wait")

	close(gate)
	require.NoError(t, awaitPublish(t, result))
	assert.False(t, p.WaitingOnServer(), "delivered: nothing pending")

	closePublisher(t, p)

	socket := New(&fakePCClient{}, "unix:///nonexistent/nvsentinel.sock", monitor)
	assert.False(t, socket.WaitingOnServer(), "socket mode never waits for a window")
}
