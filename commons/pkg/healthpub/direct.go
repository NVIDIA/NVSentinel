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
	"github.com/grpc-ecosystem/go-grpc-middleware/v2/interceptors/retry"
	"go.opentelemetry.io/otel/propagation"
	"golang.org/x/sync/semaphore"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// IdempotencyKeyHeader is the gRPC metadata header carrying the per-batch
// idempotency key; the deployment platform connector rejects direct sends
// without it. Part of the wire contract shared by this client and the
// server.
const IdempotencyKeyHeader = "idempotency-key"

// ErrPublisherClosed is returned by Publish in direct mode after Close has
// begun; no new batches are accepted, and the calls in progress are cancelled.
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

// maxSendBytes is the gRPC server's default receive limit (4 MiB). A batch
// over it would be refused by the server on every attempt, so publish refuses
// it before the first one, as rejected.
const maxSendBytes = 4 * 1024 * 1024

// withDirect switches the Publisher to direct publishing against the
// deployment platform connector, with the validated tuning and the dialed
// conn, which the Publisher then owns. Publish then sends on the caller's
// goroutine, one batch at a time behind a single send slot, so a batch is in
// the datastore before the next one leaves the monitor, which is what keeps a
// monitor's events in order on the server, also when several goroutines
// publish at once. The socket-presence gate is skipped: gRPC reconnection
// replaces it.
func withDirect(conn io.Closer, tune directTuning) Option {
	return func(p *Publisher) {
		p.conn = conn
		p.direct = &directState{tune: tune}
	}
}

// pendingBatch is one Publish call in progress. The idempotency key is
// generated once and reused verbatim on every retry, so the server can detect
// replays across connections and replicas.
type pendingBatch struct {
	events *pb.HealthEvents
	key    string

	// deadline is when the retry window ends: the Publish call plus the
	// window. Waiting for the slot, attempts and backoff all count against it.
	deadline time.Time

	// attempts is how many retries the interceptor reported, for the drop log.
	attempts int
}

// directState is the direct-mode half of a Publisher: the single send slot
// and the Publish calls in progress.
type directState struct {
	monitor string
	tune    directTuning

	// ctx is the publisher's lifecycle; cancel ends attempts, backoff pauses
	// and slot waits when Close runs.
	ctx    context.Context
	cancel context.CancelFunc

	client pb.PlatformConnectorClient

	// slot admits one send at a time. A Publish call holds it from its first
	// attempt to its outcome. The semaphore serves waiters in the order they
	// arrived, so calls waiting for the slot get it in the order they were
	// made.
	slot *semaphore.Weighted

	// mu guards closed and pending.
	mu      sync.Mutex
	closed  bool
	pending int
}

// start finalizes the direct state from the fully-optioned Publisher. Called
// once from New.
func (d *directState) start(p *Publisher) {
	d.monitor = p.monitor
	d.client = p.client
	d.ctx, d.cancel = context.WithCancel(context.Background())
	d.slot = semaphore.NewWeighted(1)
}

// publish sends one batch to its outcome on the caller's goroutine: it waits
// for the send slot, then makes the RPC, which the retry interceptor on the
// connection (retryInterceptor) repeats after a backoff pause until the
// call's deadline, the batch's retry window, ends. A rejection the server
// would repeat (an invalid or oversize batch, a scope violation) is not
// retried; every other failure, transport, UNAVAILABLE and auth alike, is,
// since the projected token is re-read per attempt and a rotated one can
// succeed. The caller's context ending, or Close, ends the wait or the call
// at once, an attempt on the wire included: the caller is told the batch was
// not delivered, and an attempt the server had already stored is a duplicate
// it tolerates. The batch is the caller's message: it is read until publish
// returns and never kept afterwards.
func (d *directState) publish(ctx context.Context, events *pb.HealthEvents) error {
	if err := d.admit(); err != nil {
		return err
	}

	defer d.release()

	batch := &pendingBatch{
		events:   events,
		key:      newIdempotencyKey(),
		deadline: time.Now().Add(d.tune.retryWindow),
	}

	if size := proto.Size(events); size > maxSendBytes {
		return d.drop(batch, dropReasonRejected,
			fmt.Errorf("%d bytes exceed the %d byte message limit", size, maxSendBytes))
	}

	// One context for the slot wait and the call: it ends with the caller's
	// context, the batch's window or Close, whichever comes first.
	callCtx, cancel := context.WithDeadline(ctx, batch.deadline)
	defer cancel()

	stop := context.AfterFunc(d.ctx, cancel)
	defer stop()

	// The semaphore serves waiters in arrival order, so calls get the slot in
	// the order they were made. A batch whose wait ends before its attempt,
	// typically behind an outage, is dropped without one, never delivered late.
	if err := d.slot.Acquire(callCtx, 1); err != nil {
		return d.failed(ctx, batch, err)
	}

	defer d.slot.Release(1)

	_, err := d.client.HealthEventOccurredV1(outgoingContext(callCtx, batch.key), batch.events,
		retry.WithOnRetryCallback(d.onRetry(batch)))
	if err != nil {
		return d.failed(ctx, batch, err)
	}

	sendsSuccess.WithLabelValues(d.monitor).Inc()
	slog.Info("Successfully sent health events",
		"monitor", d.monitor, "count", len(batch.events.GetEvents()))

	return nil
}

// admit counts a Publish call in, or refuses it once Close has begun. The
// closed check and the count happen under one lock, so a call is either
// refused or counted before Close cancels the lifecycle; none slips past it.
func (d *directState) admit() error {
	d.mu.Lock()
	defer d.mu.Unlock()

	if d.closed {
		return ErrPublisherClosed
	}

	d.pending++

	return nil
}

// release counts a Publish call out.
func (d *directState) release() {
	d.mu.Lock()
	defer d.mu.Unlock()

	d.pending--
}

// outgoingContext attaches the batch's idempotency key and the caller's trace
// context to the call; every attempt of the call carries them.
func outgoingContext(ctx context.Context, key string) context.Context {
	ctx = metadata.AppendToOutgoingContext(ctx, IdempotencyKeyHeader, key)

	carrier := MetadataCarrier{}
	propagation.TraceContext{}.Inject(ctx, carrier)

	for k, values := range carrier {
		for _, value := range values {
			ctx = metadata.AppendToOutgoingContext(ctx, k, value)
		}
	}

	return ctx
}

// onRetry is the retry interceptor's hook for one batch, run right before
// each retry: it meters and logs the retry.
func (d *directState) onRetry(batch *pendingBatch) retry.OnRetryCallback {
	return func(_ context.Context, attempt uint, err error) {
		batch.attempts = int(attempt)

		sendRetries.WithLabelValues(d.monitor).Inc()
		slog.Warn("Error sending health events to deployment platform connector; retrying.",
			"monitor", d.monitor,
			"error", err,
			"retries", attempt,
			"remainingWindow", time.Until(batch.deadline),
			"idempotencyKey", batch.key)
	}
}

// failed drops a batch whose slot wait or call ended in err, under the reason
// that ended it: a rejection the server would repeat, Close, the caller
// leaving, or the retry window running out.
func (d *directState) failed(ctx context.Context, batch *pendingBatch, err error) error {
	switch {
	case isPermanentRejection(err):
		return d.drop(batch, dropReasonRejected, err)
	case d.ctx.Err() != nil:
		return d.drop(batch, dropReasonShutdown, errors.Join(context.Cause(d.ctx), err))
	case ctx.Err() != nil:
		return d.drop(batch, dropReasonWithdrawn, errors.Join(context.Cause(ctx), err))
	default:
		return d.drop(batch, dropReasonRetryWindowExhausted, err)
	}
}

// permanentRejectionCodes are the status codes the server would answer the
// same way on every retry of the same batch, so retrying only spends the
// window: the batch failed validation (InvalidArgument), the caller may not
// publish what it sent (PermissionDenied), or the server does not serve this
// RPC (Unimplemented). Unauthenticated is deliberately not here: the token
// rotates, so it is retried. Neither is ResourceExhausted: gRPC uses it for
// transient overload and quotas, and a proxy may answer it for throttling, so
// dropping on it could lose batches for good; the one permanent cause, a
// batch too large for the server, is refused before the first attempt. The
// Python client uses the same set.
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

// retriable is the retry predicate of the connection's interceptor: every
// failure but a permanent rejection is retried.
func retriable(err error) bool {
	return !isPermanentRejection(err)
}

// drop meters a permanent drop by reason, logs the batch identity and returns
// the error the Publish call reports. A rejection points at a bug or a
// misconfiguration and is logged as an error; the other drops are the expected
// face of an outage or a shutdown.
func (d *directState) drop(batch *pendingBatch, reason string, err error) error {
	sendsDropped.WithLabelValues(d.monitor, reason).Inc()

	level := slog.LevelWarn
	if reason == dropReasonRejected {
		level = slog.LevelError
	}

	slog.Log(context.Background(), level, "Dropping health event batch permanently.",
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

// waitingOnServer reports whether a Publish call is pending, that is, whether
// a caller is waiting for the server. Every call ends within its retry
// window, so the wait is bounded.
func (d *directState) waitingOnServer() bool {
	d.mu.Lock()
	defer d.mu.Unlock()

	return d.pending > 0
}

// stop refuses new batches and ends the Publish calls in progress: attempts,
// backoff pauses and slot waits alike, each metered under the shutdown drop
// reason and reporting ErrPublishDropped.
func (d *directState) stop() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()

	d.cancel()
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
