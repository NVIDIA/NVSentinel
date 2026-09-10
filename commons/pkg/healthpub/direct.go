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
	"io"
	"log/slog"
	"slices"
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/wait"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// IdempotencyKeyHeader is the gRPC metadata header carrying the per-batch
// idempotency key; the deployment platform connector rejects direct sends
// without it. Part of the wire contract shared by this client and the
// server.
const IdempotencyKeyHeader = "idempotency-key"

// ErrPublishQueueFull is returned by Publish in direct mode when the bounded
// client queue is at its batch or byte cap; the batch is rejected, not
// queued. Callers must NOT advance any local "have I sent this?" cache on
// this error so the next poll re-emits with a fresh GeneratedTimestamp.
var ErrPublishQueueFull = errors.New("health event publish queue full; batch rejected")

// ErrPublisherClosed is returned by Publish in direct mode after Close has
// begun; no new batches are accepted during or after the drain.
var ErrPublisherClosed = errors.New("health event publisher is closed")

// ErrPublishRejected is returned by Publish when the server would refuse the
// batch on every attempt: it is invalid, larger than the server receives, from
// a caller not allowed to publish it, or for an RPC the server does not serve.
// Offering the same batch again gets the same answer, so a caller should not
// hold on to it.
var ErrPublishRejected = errors.New("health event batch rejected")

// ErrPublishDropped is returned by Publish in direct mode when the batch was
// accepted but not delivered: its retry window ended, the publisher was
// closed while it waited, or the caller's context ended and the batch was
// withdrawn. The caller must not record it as reported.
var ErrPublishDropped = errors.New("health event batch dropped")

// errWithdrawn is the drop cause of a batch whose caller stopped waiting.
var errWithdrawn = errors.New("caller stopped waiting for the batch")

// withDirect switches the Publisher to direct publishing against the
// deployment platform connector. DialFromEnvOr builds it in direct mode with
// the already-validated tuning and the dialed conn, which the Publisher then
// owns (Close closes it).
//
// In direct mode Publish enqueues onto a bounded FIFO queue and waits for the
// batch's outcome: one background sender delivers the batches in order, one
// at a time, each with a stable idempotency key and one retry window counted
// from enqueue, and Publish returns nil once the server has stored the batch
// or an error once it was dropped. A caller whose context ends withdraws its
// batch, so an error never leaves a batch behind that could still be stored
// next to the caller's re-emit. So, as on the socket path, a monitor records
// an event as reported only when it really was. Sending one batch at a time
// and waiting for the acknowledgement is what keeps a monitor's events in
// order on the server. The socket-presence gate is skipped: gRPC reconnection
// replaces it.
func withDirect(conn io.Closer, tune directTuning) Option {
	return func(p *Publisher) {
		p.direct = &directState{
			conn: conn,
			tune: tune,
		}
	}
}

// queuedBatch is one Publish call waiting in the direct-mode queue. The
// idempotency key is generated once at enqueue and reused verbatim on every
// retry, so the server can detect replays across connections and replicas.
type queuedBatch struct {
	events *pb.HealthEvents
	key    string
	bytes  int64

	// spanCtx is the caller's span context captured at Publish time and
	// injected into every send, so the server joins the trace the monitor
	// started; the RPC runs on the sender goroutine under the publisher's
	// lifecycle context, not the caller's.
	spanCtx trace.SpanContext

	// enqueuedAt is when the batch was accepted; see WaitingOnServer.
	enqueuedAt time.Time

	// deadline ends the retry window: enqueue time plus the window. Queue
	// residence, attempts and backoff sleeps all count against it, so during
	// a long outage old batches expire and make room for fresh state instead
	// of being delivered hours later. backoff paces the retries inside the
	// window, the same way the socket mode paces its attempts.
	deadline time.Time
	backoff  wait.Backoff
	attempts int

	// done carries the batch's outcome to the waiting Publish call: nil once
	// stored, an error once dropped.
	done chan error

	// withdrawn is closed when the caller stopped waiting: the batch gets no
	// further attempt, and an attempt already in flight decides its outcome.
	withdrawn    chan struct{}
	withdrawOnce sync.Once
}

// finish reports the batch's outcome to its Publish call, once.
func (b *queuedBatch) finish(err error) {
	select {
	case b.done <- err:
	default:
	}
}

// withdraw marks the batch as no longer waited for, once.
func (b *queuedBatch) withdraw() {
	b.withdrawOnce.Do(func() { close(b.withdrawn) })
}

// isWithdrawn reports whether the caller stopped waiting for the batch.
func (b *queuedBatch) isWithdrawn() bool {
	select {
	case <-b.withdrawn:
		return true
	default:
		return false
	}
}

// directState is the direct-mode half of a Publisher: the bounded FIFO queue,
// the single background sender, and the owned connection.
type directState struct {
	monitor string
	tune    directTuning

	initialBackoff time.Duration
	backoffFactor  float64
	backoffJitter  float64

	// ctx is the sender's lifecycle; cancel aborts in-flight sends and sleeps
	// when the Close drain deadline expires.
	ctx    context.Context
	cancel context.CancelFunc

	client pb.PlatformConnectorClient
	conn   io.Closer

	// mu guards the queue; cond wakes the sender on enqueue and close.
	mu          sync.Mutex
	cond        *sync.Cond
	queue       []*queuedBatch
	queuedBytes int64
	closed      bool
	// inFlight is the queue head while the sender works on it; a withdrawal
	// must not take that batch away from under its attempt.
	inFlight *queuedBatch

	senderDone chan struct{}
	closeOnce  sync.Once
	closeErr   error
}

// start finalizes the direct state from the fully-optioned Publisher and
// launches the background sender. Called once from New.
func (d *directState) start(p *Publisher) {
	d.monitor = p.monitor
	d.client = p.client
	d.initialBackoff = p.initialBackoff
	d.backoffFactor = p.backoffFactor
	d.backoffJitter = p.backoffJitter
	d.cond = sync.NewCond(&d.mu)
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.senderDone = make(chan struct{})

	go d.run()
}

// enqueue appends a batch to the bounded FIFO queue and waits for its
// outcome. It refuses at once when the caller's context has already ended,
// when the publisher is closed, when the batch is larger than the server
// receives (metered as rejected; refusing it here rather than on the wire is
// also what lets a RESOURCE_EXHAUSTED answer be retried as the transient
// condition gRPC uses that status for), or when the batch or byte cap is
// reached (metered as queue_full).
func (d *directState) enqueue(ctx context.Context, events *pb.HealthEvents) error {
	// Nothing is accepted for a caller that has already left.
	if err := ctx.Err(); err != nil {
		return err
	}

	// The queue holds the publisher's own copy, so the caller may reuse or
	// change its message as soon as Publish returns.
	copied, ok := proto.Clone(events).(*pb.HealthEvents)
	if !ok {
		return fmt.Errorf("unexpected health events message type %T", events)
	}

	now := time.Now()
	size := int64(proto.Size(copied))
	batch := &queuedBatch{
		events:     copied,
		key:        newIdempotencyKey(),
		bytes:      size,
		spanCtx:    trace.SpanContextFromContext(ctx),
		enqueuedAt: now,
		deadline:   now.Add(d.tune.retryWindow),
		backoff:    d.newBackoff(),
		done:       make(chan error, 1),
		withdrawn:  make(chan struct{}),
	}

	d.mu.Lock()

	if d.closed {
		d.mu.Unlock()

		return ErrPublisherClosed
	}

	if d.tune.maxMessageBytes > 0 && size > d.tune.maxMessageBytes {
		d.mu.Unlock()

		sendsDropped.WithLabelValues(d.monitor, dropReasonRejected).Inc()
		slog.Error("Health event batch exceeds the maximum message size; rejecting it.",
			"monitor", d.monitor,
			"bytes", size,
			"maxBytes", d.tune.maxMessageBytes,
			"eventCount", len(copied.GetEvents()))

		return fmt.Errorf("%w: %d bytes exceed the %d byte message limit",
			ErrPublishRejected, size, d.tune.maxMessageBytes)
	}

	if len(d.queue) >= d.tune.maxBatches || d.queuedBytes+size > d.tune.maxBytes {
		depth, queuedBytes := len(d.queue), d.queuedBytes
		d.mu.Unlock()

		sendsDropped.WithLabelValues(d.monitor, dropReasonQueueFull).Inc()
		slog.Warn("Direct publish queue full; rejecting batch.",
			"monitor", d.monitor,
			"queuedBatches", depth,
			"queuedBytes", queuedBytes,
			"maxBatches", d.tune.maxBatches,
			"maxBytes", d.tune.maxBytes)

		return fmt.Errorf("%w: %d batches / %d bytes queued", ErrPublishQueueFull, depth, queuedBytes)
	}

	d.queue = append(d.queue, batch)
	d.queuedBytes += size
	d.updateQueueGauges()
	d.cond.Signal()
	d.mu.Unlock()

	select {
	case err := <-batch.done:
		return err
	case <-ctx.Done():
		return d.withdraw(batch, ctx.Err())
	}
}

// withdraw takes a batch back for a caller whose context ended. A batch still
// waiting in the queue is removed and dropped as withdrawn, so the caller's
// re-emit cannot end up stored next to it. A batch whose attempt is in flight
// keeps that attempt, since the server may already have stored it, and is
// dropped after it unless it succeeded. Either way the caller gets the batch's
// real outcome: nil only if it was stored.
func (d *directState) withdraw(batch *queuedBatch, cause error) error {
	d.mu.Lock()

	removed := d.inFlight != batch && d.removeQueued(batch)
	if !removed {
		batch.withdraw()
	}

	d.mu.Unlock()

	if removed {
		d.drop(batch, dropReasonWithdrawn, cause)
	}

	return <-batch.done
}

// removeQueued takes a batch that is not in flight out of the queue and
// reports whether it was still there. Callers hold d.mu.
func (d *directState) removeQueued(batch *queuedBatch) bool {
	idx := slices.Index(d.queue, batch)
	if idx < 0 {
		return false
	}

	d.queue = slices.Delete(d.queue, idx, idx+1)
	d.queuedBytes -= batch.bytes
	d.updateQueueGauges()

	return true
}

// waitingOnServer reports whether a batch is pending within its retry window,
// that is, whether a caller is legitimately waiting for the server. Every
// pending batch is resolved within its window plus one attempt, so an older
// head means the publisher itself is stuck, which must not count as waiting.
func (d *directState) waitingOnServer() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.queue) == 0 {
		return false
	}

	return time.Since(d.queue[0].enqueuedAt) <= d.tune.retryWindow+d.tune.rpcTimeout
}

// updateQueueGauges publishes the queue-pressure gauges. Callers hold d.mu.
func (d *directState) updateQueueGauges() {
	queueBatches.WithLabelValues(d.monitor).Set(float64(len(d.queue)))
	queueBytes.WithLabelValues(d.monitor).Set(float64(d.queuedBytes))
}

// run is the single background sender: it processes the queue head to a
// terminal outcome (delivered or dropped) before moving on, preserving FIFO
// order, and exits when Close has drained the queue or cancelled the
// lifecycle.
func (d *directState) run() {
	defer close(d.senderDone)

	for {
		batch, ok := d.next()
		if !ok {
			return
		}

		d.process(batch)
		d.remove(batch)
	}
}

// next blocks until a batch is available, returning false when the publisher
// is closed with an empty queue or the lifecycle context is cancelled.
func (d *directState) next() (*queuedBatch, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()

	for len(d.queue) == 0 && !d.closed && d.ctx.Err() == nil {
		d.cond.Wait()
	}

	if d.ctx.Err() != nil || len(d.queue) == 0 {
		return nil, false
	}

	d.inFlight = d.queue[0]

	return d.inFlight, true
}

// remove pops the processed batch off the queue head.
func (d *directState) remove(batch *queuedBatch) {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.inFlight = nil

	if len(d.queue) > 0 && d.queue[0] == batch {
		d.queue[0] = nil
		d.queue = d.queue[1:]
		d.queuedBytes -= batch.bytes
		d.updateQueueGauges()
	}
}

// process retries one batch to a terminal outcome: a rejection the server
// would repeat (an invalid batch, a scope violation) drops at once,
// and every other failure, transport, UNAVAILABLE and auth alike, retries
// with jittered exponential backoff until the batch's retry window ends,
// then drops. A batch whose window ended while it waited in the queue, or
// whose caller stopped waiting, is dropped without another attempt. Auth
// failures retry because the projected token is rewritten by the kubelet, so
// a later attempt can succeed with a fresh read.
func (d *directState) process(batch *queuedBatch) {
	for {
		if d.ctx.Err() != nil {
			d.drop(batch, dropReasonShutdown, context.Cause(d.ctx))

			return
		}

		if batch.isWithdrawn() {
			d.drop(batch, dropReasonWithdrawn, errWithdrawn)

			return
		}

		if time.Until(batch.deadline) <= 0 {
			d.drop(batch, dropReasonRetryWindowExhausted, errRetryWindowEnded)

			return
		}

		err := d.send(batch)
		if err == nil {
			sendsSuccess.WithLabelValues(d.monitor).Inc()
			slog.Info("Successfully sent health events",
				"monitor", d.monitor, "count", len(batch.events.GetEvents()))
			batch.finish(nil)

			return
		}

		if d.ctx.Err() != nil {
			d.drop(batch, dropReasonShutdown, err)

			return
		}

		if done := d.handleSendFailure(batch, err); done {
			return
		}
	}
}

// handleSendFailure classifies one failed attempt; true means the batch
// reached a terminal drop, false means process should retry it.
func (d *directState) handleSendFailure(batch *queuedBatch, err error) bool {
	if isPermanentRejection(err) {
		d.drop(batch, dropReasonRejected, err)

		return true
	}

	if batch.isWithdrawn() {
		// The caller left during the attempt; nothing waits for a retry.
		d.drop(batch, dropReasonWithdrawn, err)

		return true
	}

	remaining := time.Until(batch.deadline)
	if remaining <= d.tune.finalAttemptWindow {
		// Too little of the window is left for another attempt to succeed.
		d.drop(batch, dropReasonRetryWindowExhausted, err)

		return true
	}

	batch.attempts++

	sendRetries.WithLabelValues(d.monitor).Inc()
	slog.Warn("Error sending health events to deployment platform connector; will retry.",
		"monitor", d.monitor,
		"error", err,
		"retries", batch.attempts,
		"remainingWindow", remaining,
		"idempotencyKey", batch.key)

	// Back off, but never past the point where a final attempt can still start
	// finalAttemptWindow before the deadline: the window is used whole instead
	// of ending unused in the middle of a sleep.
	d.sleep(batch, min(batch.backoff.Step(), remaining-d.tune.finalAttemptWindow))

	return false
}

// permanentRejectionCodes are the status codes the server would answer the
// same way on every retry of the same batch, so retrying only spends the
// window: the batch failed validation (InvalidArgument), the caller may not
// publish what it sent (PermissionDenied), or the server does not serve this
// RPC (Unimplemented). Unauthenticated is deliberately not here: the token
// rotates, so it is retried. Neither is ResourceExhausted: gRPC uses it for
// transient overload and quotas as well as for oversize messages, and an
// oversize batch is refused at enqueue, before any attempt. The Python client
// uses the same set.
var permanentRejectionCodes = map[codes.Code]bool{
	codes.InvalidArgument:  true,
	codes.PermissionDenied: true,
	codes.Unimplemented:    true,
}

// isPermanentRejection reports whether err is a gRPC status the server would
// repeat on every retry.
func isPermanentRejection(err error) bool {
	s, ok := status.FromError(err)

	return ok && permanentRejectionCodes[s.Code()]
}

// send performs one RPC attempt with the batch's stable idempotency key and
// its trace context injected into the outgoing metadata (trace
// propagation, W3C traceparent via propagation.TraceContext). The attempt
// never outlives the batch's retry window.
func (d *directState) send(batch *queuedBatch) error {
	timeout := d.tune.rpcTimeout
	if remaining := time.Until(batch.deadline); remaining < timeout {
		timeout = remaining
	}

	rpcCtx, cancel := context.WithTimeout(d.ctx, timeout)
	defer cancel()

	rpcCtx = metadata.AppendToOutgoingContext(rpcCtx, IdempotencyKeyHeader, batch.key)

	carrier := MetadataCarrier{}
	propagation.TraceContext{}.Inject(
		trace.ContextWithSpanContext(rpcCtx, batch.spanCtx), carrier)

	for key, values := range carrier {
		for _, value := range values {
			rpcCtx = metadata.AppendToOutgoingContext(rpcCtx, key, value)
		}
	}

	_, err := d.client.HealthEventOccurredV1(rpcCtx, batch.events)

	return err
}

// drop meters a permanent drop by reason, logs the batch identity and reports
// the outcome to the waiting Publish call.
func (d *directState) drop(batch *queuedBatch, reason string, err error) {
	sendsDropped.WithLabelValues(d.monitor, reason).Inc()
	slog.Error("Dropping health event batch permanently.",
		"monitor", d.monitor,
		"reason", reason,
		"error", err,
		"retries", batch.attempts,
		"eventCount", len(batch.events.GetEvents()),
		"idempotencyKey", batch.key)

	if reason == dropReasonRejected {
		batch.finish(fmt.Errorf("%w: %w", ErrPublishRejected, err))

		return
	}

	batch.finish(fmt.Errorf("%w (%s): %w", ErrPublishDropped, reason, err))
}

// errRetryWindowEnded is the drop cause of a batch whose window ran out
// before an attempt could be made, typically while it waited in the queue
// behind an outage.
var errRetryWindowEnded = errors.New("retry window ended before the next attempt")

// maxRetrySleep caps the backoff between attempts; wait.Backoff stops growing
// the delay once it reaches the cap.
const maxRetrySleep = 30 * time.Second

// retryBackoffSteps is how many attempts the delay may keep growing for. The
// cap is reached long before, so this only has to be large enough never to
// freeze the delay at its initial value.
const retryBackoffSteps = 64

// newBackoff is the retry pacing of one batch: the publisher's policy, capped
// at maxRetrySleep.
func (d *directState) newBackoff() wait.Backoff {
	return wait.Backoff{
		Duration: d.initialBackoff,
		Factor:   d.backoffFactor,
		Jitter:   d.backoffJitter,
		Steps:    retryBackoffSteps,
		Cap:      maxRetrySleep,
	}
}

// sleep waits for delay, cut short by the lifecycle context or by the batch's
// caller leaving.
func (d *directState) sleep(batch *queuedBatch, delay time.Duration) {
	if delay <= 0 {
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-d.ctx.Done():
	case <-batch.withdrawn:
	case <-timer.C:
	}
}

// close stops accepting new batches, lets the sender drain the queue until
// ctx is done, then cancels the remainder (metered under the shutdown drop
// reason) and closes the connection. Idempotent; later calls return the
// first result.
func (d *directState) close(ctx context.Context) error {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.cond.Broadcast()
		d.mu.Unlock()

		select {
		case <-d.senderDone:
		case <-ctx.Done():
			d.cancel()

			d.mu.Lock()
			d.cond.Broadcast()
			d.mu.Unlock()

			<-d.senderDone
		}

		d.cancel()
		d.closeErr = errors.Join(d.dropRemainder(), d.closeConn())
	})

	return d.closeErr
}

// dropRemainder empties whatever the drain deadline left behind, metering
// each batch under the shutdown drop reason.
func (d *directState) dropRemainder() error {
	d.mu.Lock()
	remainder := d.queue
	d.queue = nil
	d.queuedBytes = 0
	d.updateQueueGauges()
	d.mu.Unlock()

	for _, batch := range remainder {
		d.drop(batch, dropReasonShutdown, context.Canceled)
	}

	if len(remainder) > 0 {
		return fmt.Errorf("shutdown deadline reached before the queue drained: dropped %d queued batches",
			len(remainder))
	}

	return nil
}

// closeConn closes the owned connection.
func (d *directState) closeConn() error {
	conn := d.conn
	d.conn = nil

	if conn == nil {
		return nil
	}

	if err := conn.Close(); err != nil {
		return fmt.Errorf("closing deployment platform connector connection: %w", err)
	}

	return nil
}

// MetadataCarrier adapts gRPC metadata to OpenTelemetry's TextMapCarrier so
// the batch's span context crosses the hop without new instrumentation
// dependencies. Exported because the server side extracts with the same
// adapter, and the two ends must not drift.
type MetadataCarrier metadata.MD

func (c MetadataCarrier) Get(key string) string {
	values := metadata.MD(c).Get(key)
	if len(values) == 0 {
		return ""
	}

	return values[0]
}

func (c MetadataCarrier) Set(key, value string) {
	metadata.MD(c).Set(key, value)
}

func (c MetadataCarrier) Keys() []string {
	keys := make([]string, 0, len(c))
	for k := range c {
		keys = append(keys, k)
	}

	return keys
}

// newIdempotencyKey mints the per-batch idempotency key: a random UUID (36
// chars, within the server's ^[A-Za-z0-9._:-]{1,128}$ format), as the Python
// client does. Generated once per batch and reused verbatim on every retry.
func newIdempotencyKey() string {
	return uuid.NewString()
}
