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
	"sync"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
	"golang.org/x/sync/semaphore"
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

// ErrPublisherClosed is returned by Publish in direct mode after Close has
// begun; no new batches are accepted while the calls in progress finish.
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

// errRetryWindowEnded is the drop cause of a batch whose window ran out
// before the next attempt could start, typically while it waited for the send
// slot behind an outage.
var errRetryWindowEnded = errors.New("retry window ended before the next attempt")

// withDirect switches the Publisher to direct publishing against the
// deployment platform connector. DialFromEnvOr builds it in direct mode with
// the already-validated tuning and the dialed conn, which the Publisher then
// owns (Close closes it).
//
// In direct mode Publish does the sending itself, on the caller's goroutine,
// and returns nil once the server has stored the batch or an error once the
// batch was given up on. One batch is sent at a time: a call takes the
// publisher's single send slot, retries its batch in place until it is stored
// or dropped, and only then hands the slot to the next call. So a batch is in
// the datastore before the next one leaves the monitor, which is what keeps a
// monitor's events in order on the server, also when several goroutines
// publish at once. Every retry carries the same idempotency key, and the
// retry window is counted from the Publish call, waiting for the slot
// included, so a batch stuck behind an outage is dropped rather than
// delivered late. As on the socket path, a monitor records an event as
// reported only when it really was. The socket-presence gate is skipped:
// gRPC reconnection replaces it.
func withDirect(conn io.Closer, tune directTuning) Option {
	return func(p *Publisher) {
		p.direct = &directState{
			conn: conn,
			tune: tune,
		}
	}
}

// pendingBatch is one Publish call in progress. The idempotency key is
// generated once and reused verbatim on every retry, so the server can detect
// replays across connections and replicas.
type pendingBatch struct {
	events *pb.HealthEvents
	key    string

	// spanCtx is the caller's span at Publish time, propagated on every
	// attempt so the server's spans join the monitor's trace.
	spanCtx trace.SpanContext

	// deadline is when the retry window ends: the Publish call plus the
	// window. Waiting for the slot, attempts and backoff all count against it.
	deadline time.Time

	backoff  wait.Backoff
	attempts int
}

// directState is the direct-mode half of a Publisher: the single send slot,
// the Publish calls in progress, and the owned connection.
type directState struct {
	monitor string
	tune    directTuning
	// backoff is the retry pacing every batch starts from: the publisher's
	// policy, capped at maxRetrySleep.
	backoff wait.Backoff

	// ctx is the publisher's lifecycle; cancel ends attempts, backoff sleeps
	// and slot waits once Close's deadline has passed.
	ctx    context.Context
	cancel context.CancelFunc

	client pb.PlatformConnectorClient
	conn   io.Closer

	// slot admits one send at a time. A Publish call holds it from its first
	// attempt to its outcome. The semaphore serves waiters in the order they
	// arrived, so calls waiting for the slot get it in the order they were
	// made.
	slot *semaphore.Weighted

	// mu guards closed and pending.
	mu      sync.Mutex
	closed  bool
	pending map[*pendingBatch]struct{}
	// inProgress counts Publish calls between admission and return; Close
	// waits for it.
	inProgress sync.WaitGroup

	closeOnce sync.Once
	closeErr  error
}

// start finalizes the direct state from the fully-optioned Publisher. Called
// once from New.
func (d *directState) start(p *Publisher) {
	d.monitor = p.monitor
	d.client = p.client
	d.backoff = wait.Backoff{
		Duration: p.initialBackoff,
		Factor:   p.backoffFactor,
		Jitter:   p.backoffJitter,
		Steps:    retryBackoffSteps,
		Cap:      maxRetrySleep,
	}
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.slot = semaphore.NewWeighted(1)
	d.pending = make(map[*pendingBatch]struct{})
}

// publish sends one batch to its outcome on the caller's goroutine. It
// refuses at once when the caller's context has already ended, when the
// publisher is closed, or when the batch is larger than the server receives
// (metered as rejected; refusing it here rather than on the wire is also what
// lets a RESOURCE_EXHAUSTED answer be retried as the transient condition gRPC
// uses that status for). The batch is the caller's message: it is read until
// publish returns and never kept afterwards.
func (d *directState) publish(ctx context.Context, events *pb.HealthEvents) error {
	// Nothing is accepted for a caller that has already left.
	if err := ctx.Err(); err != nil {
		return err
	}

	batch := &pendingBatch{
		events:   events,
		key:      newIdempotencyKey(),
		spanCtx:  trace.SpanContextFromContext(ctx),
		deadline: time.Now().Add(d.tune.retryWindow),
		backoff:  d.backoff,
	}

	if err := d.admit(batch); err != nil {
		return err
	}

	defer d.release(batch)

	size := int64(proto.Size(events))
	if d.tune.maxMessageBytes > 0 && size > d.tune.maxMessageBytes {
		sendsDropped.WithLabelValues(d.monitor, dropReasonRejected).Inc()
		slog.Error("Health event batch exceeds the maximum message size; rejecting it.",
			"monitor", d.monitor,
			"bytes", size,
			"maxBytes", d.tune.maxMessageBytes,
			"eventCount", len(events.GetEvents()))

		return fmt.Errorf("%w: %d bytes exceed the %d byte message limit",
			ErrPublishRejected, size, d.tune.maxMessageBytes)
	}

	if err := d.acquireSlot(ctx, batch); err != nil {
		return err
	}

	defer d.slot.Release(1)

	return d.deliver(ctx, batch)
}

// admit registers a Publish call, or refuses it once Close has begun. The
// closed check and the WaitGroup increment happen under one lock so Close,
// which sets closed before waiting, never misses a call.
func (d *directState) admit(batch *pendingBatch) error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return ErrPublisherClosed
	}

	d.pending[batch] = struct{}{}
	d.inProgress.Add(1)

	return nil
}

// release forgets a Publish call that returned.
func (d *directState) release(batch *pendingBatch) {
	d.mu.Lock()
	delete(d.pending, batch)
	d.mu.Unlock()

	d.inProgress.Done()
}

// acquireSlot waits for the send slot. The wait ends early when the caller
// leaves (the batch is withdrawn, never sent), when the publisher shuts down,
// or when the batch's own window ends first, typically behind an outage: then
// it is dropped without an attempt, so an old batch is never delivered late.
func (d *directState) acquireSlot(ctx context.Context, batch *pendingBatch) error {
	// One context for the three ways the wait can end: the caller leaving,
	// the batch's deadline passing, and the publisher's lifecycle ending.
	waitCtx, cancel := context.WithDeadline(ctx, batch.deadline)
	defer cancel()

	stop := context.AfterFunc(d.ctx, cancel)
	defer stop()

	if err := d.slot.Acquire(waitCtx, 1); err != nil {
		switch {
		case d.ctx.Err() != nil:
			return d.drop(batch, dropReasonShutdown, context.Cause(d.ctx))
		case ctx.Err() != nil:
			return d.drop(batch, dropReasonWithdrawn, context.Cause(ctx))
		default:
			return d.drop(batch, dropReasonRetryWindowExhausted, errRetryWindowEnded)
		}
	}

	return nil
}

// deliver retries one batch to a terminal outcome while the caller holds the
// slot: a rejection the server would repeat (an invalid batch, a scope
// violation) drops at once, and every other failure, transport, UNAVAILABLE
// and auth alike, retries with jittered exponential backoff until the batch's
// retry window ends, then drops. Auth failures retry because the projected
// token is rewritten by the kubelet, so a later attempt can succeed with a
// fresh read. An attempt runs on the publisher's lifecycle, not the caller's
// context: an attempt already on the wire may have stored the batch, so it is
// allowed to finish and its answer decides. A caller that left is noticed
// between attempts and gets no retry.
func (d *directState) deliver(ctx context.Context, batch *pendingBatch) error {
	for {
		if d.ctx.Err() != nil {
			return d.drop(batch, dropReasonShutdown, context.Cause(d.ctx))
		}

		if ctx.Err() != nil {
			return d.drop(batch, dropReasonWithdrawn, context.Cause(ctx))
		}

		if time.Until(batch.deadline) <= 0 {
			return d.drop(batch, dropReasonRetryWindowExhausted, errRetryWindowEnded)
		}

		err := d.send(batch)
		if err == nil {
			sendsSuccess.WithLabelValues(d.monitor).Inc()
			slog.Info("Successfully sent health events",
				"monitor", d.monitor, "count", len(batch.events.GetEvents()))

			return nil
		}

		if d.ctx.Err() != nil {
			return d.drop(batch, dropReasonShutdown, err)
		}

		if isPermanentRejection(err) {
			return d.drop(batch, dropReasonRejected, err)
		}

		if ctx.Err() != nil {
			// The caller left during the attempt, and the attempt did not store
			// the batch; nothing waits for a retry.
			return d.drop(batch, dropReasonWithdrawn, errors.Join(context.Cause(ctx), err))
		}

		remaining := time.Until(batch.deadline)
		if remaining <= d.tune.finalAttemptWindow {
			// Too little of the window is left for another attempt to succeed.
			return d.drop(batch, dropReasonRetryWindowExhausted, err)
		}

		batch.attempts++

		slog.Warn("Error sending health events to deployment platform connector; will retry.",
			"monitor", d.monitor,
			"error", err,
			"retries", batch.attempts,
			"remainingWindow", remaining,
			"idempotencyKey", batch.key)

		// Back off, but never past the point where a final attempt can still
		// start finalAttemptWindow before the deadline: the window is used
		// whole instead of ending unused in the middle of a sleep.
		d.sleep(ctx, min(batch.backoff.Step(), remaining-d.tune.finalAttemptWindow))
	}
}

// permanentRejectionCodes are the status codes the server would answer the
// same way on every retry of the same batch, so retrying only spends the
// window: the batch failed validation (InvalidArgument), the caller may not
// publish what it sent (PermissionDenied), or the server does not serve this
// RPC (Unimplemented). Unauthenticated is deliberately not here: the token
// rotates, so it is retried. Neither is ResourceExhausted: gRPC uses it for
// transient overload and quotas as well as for oversize messages, and an
// oversize batch is refused before any attempt. The Python client uses the
// same set.
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
// its trace context injected into the outgoing metadata (trace propagation,
// W3C traceparent via propagation.TraceContext). The attempt never outlives
// the batch's retry window. Every attempt after the first is metered as a
// retry here, when it really runs: a backoff sleep cut short by the caller or
// by Close ends in a drop, not in a retry.
func (d *directState) send(batch *pendingBatch) error {
	if batch.attempts > 0 {
		sendRetries.WithLabelValues(d.monitor).Inc()
	}

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

// drop meters a permanent drop by reason, logs the batch identity and returns
// the error the Publish call reports.
func (d *directState) drop(batch *pendingBatch, reason string, err error) error {
	sendsDropped.WithLabelValues(d.monitor, reason).Inc()
	slog.Error("Dropping health event batch permanently.",
		"monitor", d.monitor,
		"reason", reason,
		"error", err,
		"retries", batch.attempts,
		"eventCount", len(batch.events.GetEvents()),
		"idempotencyKey", batch.key)

	if reason == dropReasonRejected {
		return fmt.Errorf("%w: %w", ErrPublishRejected, err)
	}

	return fmt.Errorf("%w (%s): %w", ErrPublishDropped, reason, err)
}

// maxRetrySleep caps the backoff between attempts; wait.Backoff stops growing
// the delay once it reaches the cap.
const maxRetrySleep = 30 * time.Second

// retryBackoffSteps is how many attempts the delay may keep growing for. The
// cap is reached long before, so this only has to be large enough never to
// freeze the delay at its initial value.
const retryBackoffSteps = 64

// sleep waits for delay, cut short by the publisher's lifecycle or by the
// caller leaving.
func (d *directState) sleep(ctx context.Context, delay time.Duration) {
	if delay <= 0 {
		return
	}

	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-d.ctx.Done():
	case <-ctx.Done():
	case <-timer.C:
	}
}

// waitingOnServer reports whether a Publish call is pending within its retry
// window, that is, whether a caller is legitimately waiting for the server.
// Every pending call is resolved within its window plus one attempt, so a
// call older than that means the publisher itself is stuck, which must not
// count as waiting.
func (d *directState) waitingOnServer() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	if len(d.pending) == 0 {
		return false
	}

	for batch := range d.pending {
		// Past its deadline by more than one attempt: nothing legitimate can
		// still be waiting for that call.
		if time.Until(batch.deadline) < -d.tune.rpcTimeout {
			return false
		}
	}

	return true
}

// close stops admitting new batches, lets the Publish calls in progress
// finish until ctx is done, then cancels them and closes the connection. A
// cancelled call whose attempt was already on the wire keeps that attempt's
// result; every other one is metered under the shutdown drop reason and
// reports ErrPublishDropped. Idempotent; later calls return the first result.
func (d *directState) close(ctx context.Context) error {
	d.closeOnce.Do(func() {
		d.mu.Lock()
		d.closed = true
		d.mu.Unlock()

		finished := make(chan struct{})

		go func() {
			d.inProgress.Wait()
			close(finished)
		}()

		var cutShort int

		select {
		case <-finished:
		case <-ctx.Done():
			d.mu.Lock()
			cutShort = len(d.pending)
			d.mu.Unlock()
		}

		// Ends the attempts, sleeps and slot waits of the calls cut short; a
		// no-op when every call already finished.
		d.cancel()
		<-finished

		d.closeErr = d.closeConn()
		if cutShort > 0 {
			d.closeErr = errors.Join(
				fmt.Errorf("shutdown deadline reached with %d publish call(s) still pending; cancelled them", cutShort),
				d.closeErr)
		}
	})

	return d.closeErr
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
// chars, within the server's ^[A-Za-z0-9._:-]{1,128}$ format; the Python
// client uses the same UUID without dashes). Generated once per batch and
// reused verbatim on every retry.
func newIdempotencyKey() string {
	return uuid.NewString()
}
