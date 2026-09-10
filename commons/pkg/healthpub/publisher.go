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

// Package healthpub is the shared health-event publisher used by every
// nvsentinel health monitor. It has two modes, chosen by DialFromEnvOr.
//
// In socket mode (HEALTH_PUBLISH_TARGET unset) it centralises the gRPC retry
// policy against the node-local platform-connector and gates every send on the
// platform-connector Unix socket being present, so events are never queued in
// the retry loop with a stale GeneratedTimestamp. platform-connector removes
// the socket on shutdown and on startup before binding (see
// platform-connectors/main.go), so file-presence is a faithful proxy for "PC
// is up" on this node.
//
// In direct mode (HEALTH_PUBLISH_TARGET set) it publishes to the deployment
// platform connector over TLS with a projected token: see direct.go.
package healthpub

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/apimachinery/pkg/util/wait"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"

	"github.com/nvidia/nvsentinel/commons/pkg/grpcauth"
)

// ErrPlatformConnectorUnavailable is returned by Publisher.Publish when
// the platform-connector Unix socket is absent at send time. Callers
// must NOT advance any local "have I sent this?" cache on this error so
// the next poll re-emits with a fresh GeneratedTimestamp.
var ErrPlatformConnectorUnavailable = errors.New("platform-connector unix socket missing; send skipped")

// unixScheme covers both gRPC Unix-target forms: "unix:///path" (with
// empty authority) and "unix:/path" (no authority).
const unixScheme = "unix:"

const (
	defaultMaxRetries     = 5
	defaultInitialBackoff = 2 * time.Second
	defaultBackoffFactor  = 1.5
	defaultBackoffJitter  = 0.1
)

// Publisher publishes health events to platform-connector. Safe for
// concurrent use.
type Publisher struct {
	client pb.PlatformConnectorClient

	// monitor is the agent name used as the "monitor" Prometheus label.
	monitor string

	// socketPath is the filesystem path derived from a unix:// target.
	// Empty for non-unix targets; in that case the existence gate is
	// skipped and only the retry path is exercised.
	socketPath string

	maxRetries     int
	initialBackoff time.Duration
	backoffFactor  float64
	backoffJitter  float64

	// direct, when non-nil, switches Publish to the direct mode: one send at
	// a time to the deployment platform connector, retried within a window,
	// with the socket gate skipped. Set via the direct option DialFromEnvOr
	// returns.
	direct *directState
}

// Option configures a Publisher.
type Option func(*Publisher)

// WithRetryPolicy overrides the default backoff (5 / 2s / 1.5 / 0.1).
// Primarily for tests; production should accept defaults. maxRetries applies
// to the socket mode only: the direct mode retries inside each batch's retry
// window instead and ignores the attempt count, while initialBackoff, factor
// and jitter pace both modes. A factor below 1 would shrink the delay towards
// zero and hammer the server for the rest of the window, so it keeps the
// default.
func WithRetryPolicy(maxRetries int, initialBackoff time.Duration, factor, jitter float64) Option {
	return func(p *Publisher) {
		if maxRetries > 0 {
			p.maxRetries = maxRetries
		}

		if initialBackoff > 0 {
			p.initialBackoff = initialBackoff
		}

		if factor >= 1 {
			p.backoffFactor = factor
		}

		if jitter >= 0 {
			p.backoffJitter = jitter
		}
	}
}

// New constructs a Publisher. client must already be dialed. target is
// the gRPC dial target; its unix-socket path drives the existence gate
// (non-unix schemes bypass the gate). monitor is the Prometheus label.
// Nil options are skipped, so the Option DialFromEnvOr returns (nil in
// socket mode) can be passed through unconditionally.
func New(client pb.PlatformConnectorClient, target, monitor string, opts ...Option) *Publisher {
	p := &Publisher{
		client:         client,
		monitor:        monitor,
		maxRetries:     defaultMaxRetries,
		initialBackoff: defaultInitialBackoff,
		backoffFactor:  defaultBackoffFactor,
		backoffJitter:  defaultBackoffJitter,
	}

	p.socketPath = unixSocketPathFromTarget(target)

	for _, opt := range opts {
		if opt == nil {
			continue
		}

		opt(p)
	}

	if p.direct != nil {
		p.direct.start(p)
	}

	return p
}

// SocketPath returns the resolved Unix-socket path, or "" for non-unix
// targets. Exposed for tests.
func (p *Publisher) SocketPath() string { return p.socketPath }

// Close shuts the publisher down. In direct mode it stops accepting new
// batches, lets the Publish calls in progress finish until ctx is done, then
// cancels them (metered under the shutdown drop reason) and closes the owned
// connection. In socket mode it is a no-op: the caller owns that connection,
// as today. Idempotent.
func (p *Publisher) Close(ctx context.Context) error {
	if p.direct == nil {
		return nil
	}

	return p.direct.close(ctx)
}

// CloseWithTimeout is Close bounded by a fresh timeout context, with the
// error logged instead of returned. It exists for shutdown epilogues that
// have nothing left to do with the error.
func (p *Publisher) CloseWithTimeout(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	if err := p.Close(ctx); err != nil {
		slog.Warn("Error closing health event publisher.",
			"monitor", p.monitor, "error", err)
	}
}

// TimedCloser is what CloseWhenDone shuts down: a Publisher, or a monitor's
// wrapper around one.
type TimedCloser interface {
	CloseWithTimeout(timeout time.Duration)
}

// CloseWhenDone runs closer.CloseWithTimeout(timeout) as soon as ctx ends, so
// that a monitor's shutdown starts the publisher's bounded drain the moment
// its root context is cancelled, not after its loops return. That matters
// because an attempt already on the wire runs on the publisher's lifecycle,
// not on the loop's context: a loop that first waits for a stalled server to
// answer could spend the whole termination grace period there. Close is
// idempotent, so the explicit close a main runs after its loops stays correct
// for every other exit. The returned stop function releases the hook and
// reports false once the hook has run.
func CloseWhenDone(ctx context.Context, closer TimedCloser, timeout time.Duration) (stop func() bool) {
	return context.AfterFunc(ctx, func() { closer.CloseWithTimeout(timeout) })
}

// WaitingOnServer reports whether a batch is pending in direct mode within
// its retry window: a caller blocked in Publish is then waiting for the
// deployment platform connector, not hung. A monitor's liveness check can
// stay green while this is true. Every pending batch is resolved within its
// window plus one attempt, so a wedged publisher cannot hide behind it.
// Always false in socket mode, where Publish never waits for a window.
func (p *Publisher) WaitingOnServer() bool {
	return p.direct != nil && p.direct.waitingOnServer()
}

// Publish sends a batch of health events. In socket mode it returns nil on
// success, ErrPlatformConnectorUnavailable when the unix socket is absent (no
// retries, no cache mutation expected from caller), ErrPublishRejected when
// the server refuses the batch for good, or any other error after retries are
// exhausted. In direct mode it sends the batch itself, one batch at a time
// across all callers, and waits for the outcome: nil once the server has
// stored it, ErrPublishRejected when the server would refuse it on every
// attempt (or it exceeds the message limit), ErrPublishDropped when its retry
// window ended, the publisher closed while it waited, or the caller's context
// ended and the batch was withdrawn, or ErrPublisherClosed after Close. A
// context that ends withdraws the batch: never sent if it was still waiting
// for the send slot, or dropped after its attempt in flight unless that
// attempt stored it, in which case Publish still returns nil. In every case
// but nil no acknowledgement was received and no further attempt will be
// made, so the caller must not record the events as reported and should
// re-emit them (a duplicate is possible only if an attempt timed out after the
// server had stored the batch; the server tolerates that).
func (p *Publisher) Publish(ctx context.Context, events *pb.HealthEvents) error {
	if events == nil || len(events.GetEvents()) == 0 {
		return nil
	}

	if p.direct != nil {
		return p.direct.publish(ctx, events)
	}

	if !p.socketPresent() {
		sendsSkippedPCUnavailable.WithLabelValues(p.monitor).Inc()
		slog.Warn("Platform-connector socket missing; skipping send.",
			"monitor", p.monitor, "socket", p.socketPath)

		return ErrPlatformConnectorUnavailable
	}

	backoff := wait.Backoff{
		Steps:    p.maxRetries,
		Duration: p.initialBackoff,
		Factor:   p.backoffFactor,
		Jitter:   p.backoffJitter,
	}

	var lastErr error

	err := wait.ExponentialBackoffWithContext(ctx, backoff, func(ctx context.Context) (bool, error) {
		return p.attemptSend(ctx, events, &lastErr)
	})

	return p.finalize(err, lastErr)
}

// attemptSend performs one gRPC attempt, re-probing the socket first so
// a mid-retry connector exit short-circuits instead of burning the
// budget. The returned (done, err) tuple is what wait.ExponentialBackoff
// expects: done=true on success, done=false to retry, non-nil err to
// abort the loop.
func (p *Publisher) attemptSend(
	ctx context.Context, events *pb.HealthEvents, lastErr *error,
) (bool, error) {
	if !p.socketPresent() {
		sendsSkippedPCUnavailable.WithLabelValues(p.monitor).Inc()
		slog.Warn("Platform-connector socket disappeared mid-retry; aborting send.",
			"monitor", p.monitor, "socket", p.socketPath)

		*lastErr = ErrPlatformConnectorUnavailable

		return false, ErrPlatformConnectorUnavailable
	}

	if _, sendErr := p.client.HealthEventOccurredV1(ctx, events); sendErr != nil {
		*lastErr = sendErr

		if isRetryable(sendErr) {
			slog.Warn("Retryable error sending health events; will retry.",
				"monitor", p.monitor, "error", sendErr)

			return false, nil
		}

		slog.Error("Non-retryable error sending health events.",
			"monitor", p.monitor, "error", sendErr)

		if isPermanentRejection(sendErr) {
			// The server would answer the same on every attempt; callers can
			// tell this apart from a connector that is unavailable.
			return false, fmt.Errorf("%w: %w", ErrPublishRejected, sendErr)
		}

		return false, fmt.Errorf("non-retryable error sending health events: %w", sendErr)
	}

	sendsSuccess.WithLabelValues(p.monitor).Inc()
	slog.Info("Successfully sent health events",
		"monitor", p.monitor, "count", len(events.GetEvents()))

	return true, nil
}

// socketPresent reports whether the Unix socket exists. True for
// non-unix targets (TCP fall-through) and for non-ENOENT stat errors —
// those are surfaced through the normal gRPC path rather than masked
// as a connector outage. Callers are responsible for logging and the
// skip-counter on a false return.
func (p *Publisher) socketPresent() bool {
	if p.socketPath == "" {
		return true
	}

	_, err := os.Stat(p.socketPath)
	if err == nil {
		return true
	}

	if !errors.Is(err, fs.ErrNotExist) {
		slog.Warn("Platform-connector socket stat failed; proceeding to gRPC.",
			"monitor", p.monitor,
			"socket", p.socketPath,
			"stat_error", err,
		)

		return true
	}

	return false
}

// finalize converts the (loopErr, lastErr) pair from
// wait.ExponentialBackoff into the public Publish error contract.
func (p *Publisher) finalize(loopErr, lastErr error) error {
	if errors.Is(loopErr, ErrPlatformConnectorUnavailable) || errors.Is(lastErr, ErrPlatformConnectorUnavailable) {
		return ErrPlatformConnectorUnavailable
	}

	if loopErr == nil {
		return nil
	}

	sendsError.WithLabelValues(p.monitor, errorCodeLabel(lastErr)).Inc()

	if lastErr != nil && !errors.Is(loopErr, lastErr) {
		return fmt.Errorf("%w: last error: %w", loopErr, lastErr)
	}

	return loopErr
}

// isRetryable reports whether a gRPC error is transient.
func isRetryable(err error) bool {
	if err == nil {
		return false
	}

	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unavailable, codes.DeadlineExceeded:
			return true
		case codes.OK,
			codes.Canceled,
			codes.Unknown,
			codes.InvalidArgument,
			codes.NotFound,
			codes.AlreadyExists,
			codes.PermissionDenied,
			codes.ResourceExhausted,
			codes.FailedPrecondition,
			codes.Aborted,
			codes.OutOfRange,
			codes.Unimplemented,
			codes.Internal,
			codes.DataLoss,
			codes.Unauthenticated:
			// Non-retryable gRPC codes; fall through to the
			// connection-level checks below.
		}
	}

	if errors.Is(err, io.EOF) {
		return true
	}

	msg := err.Error()
	if strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "broken pipe") {
		return true
	}

	return false
}

// unixSocketPathFromTarget extracts the absolute filesystem path from a
// gRPC unix-scheme target, returning "" for non-unix targets or
// relative-path forms (which would require resolving a working dir the
// publisher does not own).
func unixSocketPathFromTarget(target string) string {
	rest, ok := strings.CutPrefix(target, unixScheme)
	if !ok {
		return ""
	}
	// Collapse the empty-authority form ("unix:///path") to the
	// no-authority form ("unix:/path") so both reduce to a leading "/".
	rest = strings.TrimPrefix(rest, "//")
	if strings.HasPrefix(rest, "/") {
		return rest
	}

	return ""
}

// authBackendUnavailableLabel distinguishes an authentication-backend outage
// from the platform-connector itself being unreachable. Both arrive as
// Unavailable.
const authBackendUnavailableLabel = "AuthBackendUnavailable"

// errorCodeLabel produces a bounded-cardinality label value for the
// sendsError counter using the gRPC status code string.
func errorCodeLabel(err error) string {
	if err == nil {
		return ""
	}

	// Checked before the gRPC code. An authentication-backend outage is reported
	// as Unavailable, which is the same code the platform-connector being down
	// produces, so without this the two are indistinguishable on the metric. They
	// call for different responses: one is a control-plane problem affecting every
	// node, the other is local to this node.
	if grpcauth.IsAuthBackendUnavailable(err) {
		return authBackendUnavailableLabel
	}

	if s, ok := status.FromError(err); ok {
		return s.Code().String()
	}

	return "Unknown"
}
