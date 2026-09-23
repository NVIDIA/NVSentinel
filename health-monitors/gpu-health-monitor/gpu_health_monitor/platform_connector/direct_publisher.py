# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

"""Direct-mode publisher for the deployment platform connector.

When HEALTH_PUBLISH_TARGET is set, the GPU health monitor publishes health
events over the network to the central deployment platform connector instead
of the node-local Unix socket. The client-side contract, shared with the Go
client in commons/pkg/healthpub, is:

- publish() sends the batch itself, on the calling thread, and returns only
  once the server has stored it (True) or the batch was given up on (False);
- one batch is sent at a time: a call takes the publisher's single send slot,
  retries its batch in place until it is stored or dropped, and only then
  hands the slot to the next call in line (first come, first served), so a
  batch is in the datastore before the next one leaves the monitor, which
  keeps this monitor's events in order on the server, also when several
  threads publish at once (so behind an outage the batch holding the slot
  delays every batch behind it, and a caller with a timeout is withdrawn if
  the slot does not free up in time);
- a per-batch idempotency key generated once and reused verbatim on every
  retry, carried in the "idempotency-key" gRPC header;
- a retry window counted from the publish() call, waiting for the slot
  included, with jittered exponential backoff between attempts; when the
  window ends the batch is dropped (metered, with reason), so a batch stuck
  behind an outage is dropped rather than delivered late;
- close() ends the calls in progress at once and closes the channel.

The server acknowledges a batch only once it is stored in the datastore. A
rejection the server would repeat on every retry (PERMANENT_STATUS_CODES)
drops the batch at once; UNAVAILABLE, DEADLINE_EXCEEDED, transport errors,
and authentication failures all retry within the window: the projected token
is rewritten by the kubelet, so a failure now can succeed on a later attempt
with a fresh read. An UNAVAILABLE failure also discards the channel, so the
next attempt dials again and re-reads the CA bundle; that is how a
cert-manager CA rotation takes effect without a restart.

When HEALTH_PUBLISH_TARGET is unset this module is inert and the publisher
keeps today's Unix-socket behavior unchanged.
"""

import dataclasses
import logging as log
import os
import random
import re
import uuid
from collections import deque
from collections.abc import Mapping
from threading import Condition
from time import monotonic

import grpc

from gpu_health_monitor.protos import (
    health_event_pb2 as platformconnector_pb2,
    health_event_pb2_grpc as platformconnector_pb2_grpc,
)
from . import metrics

# Environment contract (identical names in the shared Go client).
TARGET_ENV = "HEALTH_PUBLISH_TARGET"
INSECURE_ENV = "HEALTH_PUBLISH_INSECURE"
TLS_CA_FILE_ENV = "HEALTH_PUBLISH_TLS_CA_FILE"
TLS_SERVER_NAME_ENV = "HEALTH_PUBLISH_TLS_SERVER_NAME"
TOKEN_PATH_ENV = "HEALTH_PUBLISH_TOKEN_PATH"
RETRY_WINDOW_ENV = "HEALTH_PUBLISH_RETRY_WINDOW"

IDEMPOTENCY_KEY_HEADER = "idempotency-key"

DEFAULT_RETRY_WINDOW_SECONDS = 300.0
# Room for the server's datastore write and node condition update inside the request, plus a MongoDB primary election.
GRPC_CALL_TIMEOUT_SECONDS = 30.0
# The pause after a failed attempt starts at INITIAL_BACKOFF_SECONDS and
# doubles up to MAX_BACKOFF_SECONDS, with +/-JITTER_FRACTION of spread so a
# fleet of retrying monitors does not hit the server in lockstep. Same pacing
# as the Go client.
INITIAL_BACKOFF_SECONDS = 2.0
MAX_BACKOFF_SECONDS = 30.0
BACKOFF_MULTIPLIER = 2.0
JITTER_FRACTION = 0.1
# Cap on the pause between the channel's own attempts to reconnect to the
# server. gRPC's default grows to two minutes over an outage, and a publish
# attempted while the channel waits fails at once without touching the
# network, so a batch could sit out most of a minute after the server was
# back. Capped, the channel is connected within seconds of the server
# returning and the retry cadence above alone decides when a batch is
# resent. Mirrors the Go client.
MAX_RECONNECT_BACKOFF_MS = 10_000

# The gRPC server's default receive limit (4 MiB). A batch over it would be
# refused by the server on every attempt, so publish() refuses it before the
# first one, as rejected.
MAX_MESSAGE_BYTES = 4194304

# Drop-metric reasons. The metric names differ per client, the reason label
# values do not: they match the Go healthpub client's drop-reason vocabulary
# (commons/pkg/healthpub/metrics.go), so a dashboard can key on the reason
# across both clients.
DROP_REASON_REJECTED = "rejected"
DROP_REASON_WINDOW_EXPIRED = "retry_window_exhausted"
DROP_REASON_SHUTDOWN = "shutdown"
DROP_REASON_WITHDRAWN = "withdrawn"

# Status codes the server would answer the same way on every retry of the same
# batch, so retrying only spends the window: the batch failed validation
# (INVALID_ARGUMENT), the caller may not publish what it sent
# (PERMISSION_DENIED), or the server does not serve this RPC (UNIMPLEMENTED).
# UNAUTHENTICATED is deliberately not here: the token rotates, so it is
# retried. Neither is RESOURCE_EXHAUSTED: gRPC uses it for transient overload
# and quotas, and a proxy may answer it for throttling, so dropping on it could
# lose batches for good; the one permanent cause, a batch too large for the
# server, is refused before the first attempt. The Go client uses the same set.
PERMANENT_STATUS_CODES = frozenset(
    {
        grpc.StatusCode.INVALID_ARGUMENT,
        grpc.StatusCode.PERMISSION_DENIED,
        grpc.StatusCode.UNIMPLEMENTED,
    }
)


def rpc_status_code(error: grpc.RpcError) -> grpc.StatusCode | None:
    """The status code carried by a gRPC failure, or None when it carries none.

    Failures raised by a live channel are ``grpc.Call`` instances and always
    carry a code. A bare ``grpc.RpcError`` does not; it expresses no verdict
    either way, so callers keep treating it as retryable.
    """
    code_getter = getattr(error, "code", None)
    if not callable(code_getter):
        return None
    code = code_getter()
    return code if isinstance(code, grpc.StatusCode) else None


# Go-style durations ("5m", "1m30s", "500ms"); bare numbers are rejected the
# same way Go's time.ParseDuration rejects them, so both clients read the
# same value the same way.
_DURATION_PATTERN = re.compile(r"^(?:\d+(?:\.\d+)?(?:ms|s|m|h))+$")
_DURATION_COMPONENT = re.compile(r"(\d+(?:\.\d+)?)(ms|s|m|h)")
_UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0}


def parse_duration(text: str) -> float:
    """Parses a Go-style duration string into seconds."""
    candidate = text.strip()
    if not _DURATION_PATTERN.match(candidate):
        raise ValueError(f"invalid duration {text!r}: expected a Go-style duration such as '5m' or '90s'")
    return sum(float(value) * _UNIT_SECONDS[unit] for value, unit in _DURATION_COMPONENT.findall(candidate))


def read_bearer_token(token_path: str, description: str) -> str:
    """The bearer token read fresh from token_path.

    The kubelet rewrites projected token files, so callers re-read on every
    attempt rather than caching. An unreadable file raises OSError; an empty
    file is a broken mount, not a credential, and raises a ValueError naming
    the file. `description` prefixes that message so each caller keeps its own
    wording.
    """
    with open(token_path) as token_file:
        token = token_file.read()
    if not token:
        raise ValueError(f"{description} {token_path} is empty")
    return token


def _bool_env(environ: Mapping[str, str], name: str) -> bool:
    """A boolean setting in the vocabulary Go's strconv.ParseBool accepts; anything else is refused."""
    raw = environ.get(name, "").strip()
    if not raw:
        return False
    lowered = raw.lower()
    if lowered in ("1", "t", "true"):
        return True
    if lowered in ("0", "f", "false"):
        return False
    raise ValueError(f"invalid {name} {raw!r}: must be true or false")


@dataclasses.dataclass(frozen=True)
class DirectPublisherConfig:
    """Resolved HEALTH_PUBLISH_* configuration for direct mode."""

    target: str
    insecure: bool
    ca_file: str | None
    server_name_override: str | None
    token_path: str
    retry_window_seconds: float

    @classmethod
    def from_env(cls, environ: Mapping[str, str] | None = None) -> "DirectPublisherConfig | None":
        """The direct-mode configuration, or None when HEALTH_PUBLISH_TARGET is unset.

        A set target with missing or invalid companion settings raises: a
        misconfigured direct-mode publisher must fail loudly at startup, not
        silently fall back to the socket.
        """
        if environ is None:
            environ = os.environ
        target = environ.get(TARGET_ENV, "").strip()
        if not target:
            return None

        insecure = _bool_env(environ, INSECURE_ENV)
        ca_file = environ.get(TLS_CA_FILE_ENV, "").strip() or None
        if not insecure and not ca_file:
            raise ValueError(f"{TLS_CA_FILE_ENV} is required when {TARGET_ENV} is set unless {INSECURE_ENV}=true")

        token_path = environ.get(TOKEN_PATH_ENV, "").strip()
        if not token_path:
            # The server authenticates every publish; there is no token-less mode.
            raise ValueError(f"{TOKEN_PATH_ENV} is required when {TARGET_ENV} is set")

        retry_window_raw = environ.get(RETRY_WINDOW_ENV, "").strip()
        retry_window_seconds = parse_duration(retry_window_raw) if retry_window_raw else DEFAULT_RETRY_WINDOW_SECONDS
        if retry_window_seconds <= 0:
            raise ValueError(f"invalid {RETRY_WINDOW_ENV} {retry_window_raw!r}: must be a positive duration")

        return cls(
            target=target,
            insecure=insecure,
            ca_file=ca_file,
            server_name_override=environ.get(TLS_SERVER_NAME_ENV, "").strip() or None,
            token_path=token_path,
            retry_window_seconds=retry_window_seconds,
        )


@dataclasses.dataclass(eq=False)
class _Batch:
    """One publish() call in progress. Compared by identity: the line of waiters is a line of calls, not of payloads."""

    request: platformconnector_pb2.HealthEvents
    idempotency_key: str
    # When the retry window ends (monotonic seconds): the publish() call plus
    # the window. Waiting for the send slot, attempts and backoff pauses all
    # count against it, so during a long outage an old batch expires instead
    # of being delivered late.
    deadline: float
    # When the caller stops waiting (monotonic seconds), or None to wait for
    # the outcome. Past it the batch is withdrawn, an attempt on the wire
    # included.
    wait_deadline: float | None = None
    # Retries that ran, for the drop log; the first attempt is not one.
    attempts: int = 0

    def caller_left(self) -> bool:
        return self.wait_deadline is not None and monotonic() >= self.wait_deadline

    def wait_limit(self, seconds: float) -> float:
        """How long a wait may last before the caller's deadline needs a look."""
        if self.wait_deadline is not None:
            seconds = min(seconds, self.wait_deadline - monotonic())
        return max(seconds, 0.0)

    def attempt_timeout(self, call_timeout: float) -> float:
        """One attempt never outlives the batch's window or the caller's wait."""
        return self.wait_limit(min(call_timeout, self.deadline - monotonic()))


class DirectPublisher:
    """Publishes to the deployment platform connector, one batch at a time, retrying within a window."""

    def __init__(self, config: DirectPublisherConfig) -> None:
        self._config = config
        if config.ca_file:
            # A CA bundle that cannot be read is a deployment error to fail on
            # now, not on the first attempt; the file is re-read per dial. As in
            # _dial, a CA file is used whenever one is set.
            with open(config.ca_file, "rb"):
                pass
        # The backoff knobs live on the instance so tests can shrink them.
        self._initial_backoff = INITIAL_BACKOFF_SECONDS
        self._max_backoff = MAX_BACKOFF_SECONDS
        self._retry_window = config.retry_window_seconds

        # _state guards the fields below and wakes calls waiting for the send
        # slot or pausing between attempts.
        self._state = Condition()
        # The send slot: True while a publish() call is sending. One batch is
        # sent at a time, and the current one is stored or dropped before the
        # next one is sent, which keeps this monitor's events in order.
        self._sending = False
        # Calls waiting for the slot, in arrival order; the head goes next.
        self._waiters: deque[_Batch] = deque()
        self._closed = False

        # Channel and stub are used by the call holding the send slot; close()
        # closes the channel from outside to cancel a call on the wire.
        self._channel: grpc.Channel | None = None
        self._stub: platformconnector_pb2_grpc.PlatformConnectorStub | None = None

    def publish(self, health_events: list[platformconnector_pb2.HealthEvent], timeout: float | None = None) -> bool:
        """Sends a batch and waits for its outcome.

        Returns True once the server has stored the batch and False when it
        was not delivered: the publisher is closed, the batch is larger than
        the server receives, the server rejected it, its retry window ended,
        or the caller's timeout passed first. So, as
        on the socket path, callers record an event as reported only when it
        really was. With a timeout the wait is bounded: a batch still pending
        when it expires is withdrawn at once, whether it was waiting for the
        send slot or on the wire. So False means no acknowledgement was
        received and no further attempt will be made, and the caller should
        re-emit; a duplicate is possible if an attempt was cut or timed out
        after the server had stored the batch, which the server tolerates.

        An empty batch is a no-op success, matching the Go client: there is
        nothing to deliver, and sending it anyway would spend an RPC on
        nothing.
        """
        if not health_events:
            return True
        now = monotonic()
        batch = _Batch(
            request=platformconnector_pb2.HealthEvents(events=health_events, version=1),
            # Generated once per batch and reused verbatim on every retry so
            # the server can recognise a resend.
            idempotency_key=uuid.uuid4().hex,
            deadline=now + self._retry_window,
            wait_deadline=None if timeout is None else now + timeout,
        )
        # Closed first, like the Go client: a refused batch is not metered.
        with self._state:
            if self._closed:
                log.warning("Direct publisher is closed; refusing batch of %d event(s).", len(batch.request.events))
                return False
        size = batch.request.ByteSize()
        if size > MAX_MESSAGE_BYTES:
            return self._drop(
                batch, DROP_REASON_REJECTED, f"{size} bytes exceed the {MAX_MESSAGE_BYTES} byte message limit"
            )
        if not self._acquire_slot(batch):
            return False
        try:
            return self._deliver(batch)
        finally:
            with self._state:
                self._sending = False
                self._state.notify_all()

    def close(self) -> None:
        """Stops intake, ends the publish() calls in progress and closes the channel.

        Each call in progress drops its batch with the shutdown reason: a call
        waiting for the slot or pausing between attempts wakes up to find the
        publisher closed, and a call on the wire is cancelled with the channel.
        Waits for nothing. Idempotent.
        """
        with self._state:
            self._closed = True
            self._state.notify_all()
        self._close_channel()

    # ------------------------------------------------------------------
    # The send slot and the retry loop, both on the calling thread
    # ------------------------------------------------------------------

    def _acquire_slot(self, batch: _Batch) -> bool:
        """Waits for the send slot; waiting calls get it in the order they arrived.

        The wait ends early when the caller stops waiting (the batch is
        withdrawn, never sent), when the publisher is closed, or when the
        batch's own window ends first, typically behind an outage: then it is
        dropped without an attempt, so an old batch is never delivered late.
        """
        with self._state:
            self._waiters.append(batch)
            try:
                while self._sending or self._waiters[0] is not batch:
                    if self._closed:
                        return self._drop(batch, DROP_REASON_SHUTDOWN, "the publisher is closed")
                    if batch.caller_left():
                        return self._drop(batch, DROP_REASON_WITHDRAWN, "the caller stopped waiting")
                    remaining = batch.deadline - monotonic()
                    if remaining <= 0:
                        return self._drop_expired(batch)
                    self._state.wait(batch.wait_limit(remaining))
                self._sending = True
                return True
            finally:
                # Taken or given up, this call leaves the line; the next one
                # has to look again.
                self._waiters.remove(batch)
                self._state.notify_all()

    def _deliver(self, batch: _Batch) -> bool:
        """Sends one batch, retrying until its retry window ends, while holding the send slot.

        A rejection the server would repeat on every retry drops at once. The
        caller's wait ending, or close(), ends the call at once, an attempt on
        the wire included: the caller is told the batch was not delivered, and
        an attempt the server had already stored is a duplicate it tolerates.
        """
        backoff = self._initial_backoff
        failed = False
        while True:
            if self._closed:
                return self._drop(batch, DROP_REASON_SHUTDOWN, "the publisher is closed")
            if batch.caller_left():
                return self._drop(batch, DROP_REASON_WITHDRAWN, "the caller stopped waiting")
            if monotonic() >= batch.deadline:
                return self._drop_expired(batch)

            if failed:
                # Counted when the retry really runs: a pause cut short by the
                # caller or by close() ends in a drop, not in a retry.
                batch.attempts += 1
                metrics.health_events_direct_publish_retries.inc()
            try:
                self._ensure_stub().HealthEventOccurredV1(
                    batch.request,
                    timeout=batch.attempt_timeout(GRPC_CALL_TIMEOUT_SECONDS),
                    metadata=self._call_metadata(batch.idempotency_key),
                )
                metrics.health_events_direct_publish_succeed.inc()
                return True
            except grpc.RpcError as e:
                # A live channel's errors are grpc.Call objects and carry a
                # code; a bare RpcError carries none and is retried.
                code = rpc_status_code(e)
                if code in PERMANENT_STATUS_CODES:
                    return self._drop(batch, DROP_REASON_REJECTED, f"the server rejected it: {e}")
                if code == grpc.StatusCode.UNAVAILABLE:
                    # A transport failure may mean the channel still trusts a
                    # CA bundle cert-manager has since rotated. Discarding it
                    # makes the next attempt dial again and re-read the CA
                    # file, the way the Go client reloads it per handshake.
                    self._close_channel()
                if not self._closed:
                    log.warning("Failed to publish health events to %s; will retry: %s", self._config.target, e)
            except Exception as e:  # noqa: BLE001 - a publish call must survive any failure and keep retrying
                # Token-file reads and channel construction can raise outside
                # gRPC; all of it is retryable within the window (the kubelet
                # rewrites the projected token, a config fix redeploys us).
                if not self._closed:
                    log.warning("Failed to publish health events to %s; will retry: %s", self._config.target, e)

            failed = True
            # Pause, but not past the deadline: the loop then drops the batch.
            self._sleep(min(self._jitter(backoff), batch.deadline - monotonic()), batch)
            backoff = min(backoff * BACKOFF_MULTIPLIER, self._max_backoff)

    def _sleep(self, seconds: float, batch: _Batch) -> None:
        """Pauses between attempts; the caller's wait ending or close() cuts the pause short."""
        deadline = monotonic() + seconds
        with self._state:
            while not self._closed and not batch.caller_left():
                remaining = deadline - monotonic()
                if remaining <= 0:
                    return
                self._state.wait(batch.wait_limit(remaining))

    def _drop(self, batch: _Batch, reason: str, cause: str) -> bool:
        """Meters a permanent drop by reason and reports it to the publish() call (False)."""
        metrics.health_events_direct_publish_dropped.labels(reason=reason).inc()
        # A rejection points at a bug or a misconfiguration; the other drops
        # are the expected face of an outage or a shutdown.
        (log.error if reason == DROP_REASON_REJECTED else log.warning)(
            "Dropping health event batch of %d event(s) permanently (%s, %d retries): %s. "
            "Events will be re-emitted on the next health check cycle.",
            len(batch.request.events),
            reason,
            batch.attempts,
            cause,
        )
        return False

    def _drop_expired(self, batch: _Batch) -> bool:
        return self._drop(batch, DROP_REASON_WINDOW_EXPIRED, f"retry window ({self._retry_window:.0f}s) ended")

    # ------------------------------------------------------------------
    # Channel and call plumbing
    # ------------------------------------------------------------------

    def _ensure_stub(self) -> platformconnector_pb2_grpc.PlatformConnectorStub:
        """The stub on the current channel, dialing one if needed; refuses once closed so no channel outlives close()."""
        with self._state:
            if self._closed:
                raise RuntimeError("the publisher is closed")
            if self._stub is None:
                self._channel = self._dial()
                self._stub = platformconnector_pb2_grpc.PlatformConnectorStub(self._channel)
            return self._stub

    def _dial(self) -> grpc.Channel:
        options = [("grpc.max_reconnect_backoff_ms", MAX_RECONNECT_BACKOFF_MS)]
        # As in the Go client, a CA file wins over the insecure flag.
        if self._config.insecure and not self._config.ca_file:
            return grpc.insecure_channel(self._config.target, options=options)
        with open(self._config.ca_file, "rb") as ca_file:
            credentials = grpc.ssl_channel_credentials(root_certificates=ca_file.read())
        # gRPC already validates against the host part of the target; the
        # override is only needed when the certificate names something else.
        if self._config.server_name_override:
            options.append(("grpc.ssl_target_name_override", self._config.server_name_override))
        return grpc.secure_channel(self._config.target, credentials, options=options)

    def _close_channel(self) -> None:
        with self._state:
            channel, self._channel, self._stub = self._channel, None, None
        if channel is not None:
            channel.close()

    def _call_metadata(self, idempotency_key: str) -> list[tuple[str, str]]:
        """Per-attempt call metadata: the stable idempotency key plus a fresh token.

        The projected token file is rewritten by the kubelet, so it is re-read
        on every attempt. An unreadable or empty token raises so the attempt
        fails instead of publishing without a credential.
        """
        token = read_bearer_token(self._config.token_path, "health publish token file")
        return [(IDEMPOTENCY_KEY_HEADER, idempotency_key), ("authorization", "Bearer " + token)]

    def _jitter(self, seconds: float) -> float:
        return seconds * (1.0 + random.uniform(-JITTER_FRACTION, JITTER_FRACTION))


def maybe_create_from_env(environ: Mapping[str, str] | None = None) -> DirectPublisher | None:
    """A DirectPublisher when HEALTH_PUBLISH_TARGET is set, else None."""
    config = DirectPublisherConfig.from_env(environ)
    if config is None:
        return None
    log.info(
        "Direct health publishing enabled: target=%s tls=%s token=%s retry_window=%.0fs",
        config.target,
        "insecure" if config.insecure else f"ca={config.ca_file}",
        config.token_path,
        config.retry_window_seconds,
    )
    return DirectPublisher(config)
