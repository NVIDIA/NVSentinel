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

"""Tests for the direct-mode publisher (HEALTH_PUBLISH_TARGET set)."""

import json
import os
import re
import shutil
import tempfile
import threading
import time
import unittest
import unittest.mock
from concurrent import futures
from typing import Any

import grpc
from google.protobuf.timestamp_pb2 import Timestamp

from gpu_health_monitor.platform_connector import direct_publisher
from gpu_health_monitor.platform_connector import metrics as pc_metrics
from gpu_health_monitor.platform_connector import platform_connector
from gpu_health_monitor.protos import (
    health_event_pb2 as platformconnector_pb2,
    health_event_pb2_grpc as platformconnector_pb2_grpc,
)

IDEMPOTENCY_KEY_FORMAT = re.compile(r"^[A-Za-z0-9._:-]{1,128}$")

# Direct mode always requires a token and every attempt reads it, so
# publishers whose tests do not care about the token share one readable file
# for the module's lifetime.
_shared_token_dir = ""
SHARED_TOKEN_PATH = ""


def setUpModule() -> None:
    global _shared_token_dir, SHARED_TOKEN_PATH
    _shared_token_dir = tempfile.mkdtemp(prefix="ghm_direct_token_")
    SHARED_TOKEN_PATH = os.path.join(_shared_token_dir, "token")
    with open(SHARED_TOKEN_PATH, "w") as token_file:
        token_file.write("test-token")


def tearDownModule() -> None:
    shutil.rmtree(_shared_token_dir, ignore_errors=True)


def sample_events(count: int = 1) -> list[platformconnector_pb2.HealthEvent]:
    timestamp = Timestamp()
    timestamp.GetCurrentTime()
    return [
        platformconnector_pb2.HealthEvent(
            version=1,
            agent="gpu-health-monitor",
            componentClass="GPU",
            checkName=f"GpuMemWatch{i}",
            generatedTimestamp=timestamp,
            isFatal=False,
            isHealthy=True,
            nodeName="node1",
            processingStrategy=platformconnector_pb2.STORE_ONLY,
        )
        for i in range(count)
    ]


def named_events(name: str) -> list[platformconnector_pb2.HealthEvent]:
    """sample_events with a caller-chosen check name, so delivery order can be observed on the receiving side."""
    events = sample_events()
    events[0].checkName = name
    return events


def wait_until(predicate, timeout: float = 5.0, interval: float = 0.005) -> bool:
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        if predicate():
            return True
        time.sleep(interval)
    return predicate()


def dropped(reason: str) -> float:
    return pc_metrics.health_events_direct_publish_dropped.labels(reason=reason)._value.get()


def succeeded() -> float:
    return pc_metrics.health_events_direct_publish_succeed._value.get()


def retried() -> float:
    return pc_metrics.health_events_direct_publish_retries._value.get()


def publish_in_background(
    publisher: direct_publisher.DirectPublisher,
    events: list[platformconnector_pb2.HealthEvent],
    timeout: float | None = None,
) -> "futures.Future[bool]":
    """publish() waits for the batch's outcome, so a test that must act while the batch is pending runs it on a worker."""
    worker = futures.ThreadPoolExecutor(max_workers=1)
    future = worker.submit(publisher.publish, events, timeout)
    worker.shutdown(wait=False)
    return future


class FakeRpcError(grpc.RpcError):
    """An RpcError carrying code and details, the way a live channel's failures do."""

    def __init__(self, code: grpc.StatusCode, details: str = "") -> None:
        super().__init__()
        self._code = code
        self._details = details

    def code(self) -> grpc.StatusCode:
        return self._code

    def details(self) -> str:
        return self._details


class ScriptedStub:
    """A stub whose send outcomes are scripted; records every call's metadata and how many calls overlap."""

    def __init__(self, script: list[Any] | None = None, gate: threading.Event | None = None) -> None:
        self.script = list(script or [])
        self.calls: list[dict[str, str]] = []
        self.check_names: list[str] = []
        self.gate = gate
        self.started = threading.Event()
        self.in_flight = 0
        self.max_in_flight = 0
        self._lock = threading.Lock()

    def HealthEventOccurredV1(self, request: Any, timeout: float | None = None, metadata: Any = None) -> Any:
        with self._lock:
            self.in_flight += 1
            self.max_in_flight = max(self.max_in_flight, self.in_flight)
        try:
            self.started.set()
            if self.gate is not None and not self.gate.wait(5.0 if timeout is None else min(timeout, 5.0)):
                # A live channel gives up on the attempt at its timeout.
                raise FakeRpcError(grpc.StatusCode.DEADLINE_EXCEEDED, "attempt timed out")
            with self._lock:
                self.calls.append(dict(metadata or []))
                self.check_names.extend(event.checkName for event in request.events)
                action = self.script.pop(0) if self.script else None
            if isinstance(action, Exception):
                raise action
            return platformconnector_pb2.HealthEvents()
        finally:
            with self._lock:
                self.in_flight -= 1


class AlwaysUnavailable(ScriptedStub):
    """A server that is down: every call fails with UNAVAILABLE."""

    def HealthEventOccurredV1(self, request, timeout=None, metadata=None):
        self.started.set()
        with self._lock:
            self.calls.append(dict(metadata or []))
        raise FakeRpcError(grpc.StatusCode.UNAVAILABLE, "connection refused")


def make_config(
    retry_window: float = 5.0,
    token_path: str | None = None,
    insecure: bool = True,
    ca_file: str | None = None,
    server_name_override: str | None = None,
    target: str = "127.0.0.1:1",
) -> direct_publisher.DirectPublisherConfig:
    """An insecure direct-mode config; token_path defaults to the shared readable token."""
    return direct_publisher.DirectPublisherConfig(
        target=target,
        insecure=insecure,
        ca_file=ca_file,
        server_name_override=server_name_override,
        token_path=token_path or SHARED_TOKEN_PATH,
        retry_window_seconds=retry_window,
    )


def make_publisher(
    stub: Any,
    retry_window: float = 5.0,
    token_path: str | None = None,
) -> direct_publisher.DirectPublisher:
    publisher = direct_publisher.DirectPublisher(make_config(retry_window, token_path))
    # Shrink the timing knobs so tests run in milliseconds; they are read only
    # once a batch is published, so patching here is race-free.
    publisher._initial_backoff = 0.01
    publisher._max_backoff = 0.02
    publisher._ensure_stub = lambda: stub
    return publisher


class TestDurationParsing(unittest.TestCase):
    def test_go_style_durations(self) -> None:
        self.assertEqual(direct_publisher.parse_duration("5m"), 300.0)
        self.assertEqual(direct_publisher.parse_duration("90s"), 90.0)
        self.assertEqual(direct_publisher.parse_duration("1m30s"), 90.0)
        self.assertEqual(direct_publisher.parse_duration("500ms"), 0.5)
        self.assertEqual(direct_publisher.parse_duration("1h"), 3600.0)

    def test_invalid_durations_raise(self) -> None:
        for bad in ("", "300", "5 m", "m5", "5x", "-5m"):
            with self.assertRaises(ValueError, msg=bad):
                direct_publisher.parse_duration(bad)


class TestConfigFromEnv(unittest.TestCase):
    def test_unset_target_selects_socket_mode(self) -> None:
        self.assertIsNone(direct_publisher.DirectPublisherConfig.from_env({}))
        self.assertIsNone(direct_publisher.DirectPublisherConfig.from_env({"HEALTH_PUBLISH_TARGET": "  "}))
        self.assertIsNone(direct_publisher.maybe_create_from_env({}))

    def test_full_direct_mode_environment(self) -> None:
        config = direct_publisher.DirectPublisherConfig.from_env(
            {
                "HEALTH_PUBLISH_TARGET": "platform-connector-deployment.nvsentinel.svc:50051",
                "HEALTH_PUBLISH_TLS_CA_FILE": "/etc/tls/ca.crt",
                "HEALTH_PUBLISH_TLS_SERVER_NAME": "platform-connector-deployment",
                "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/token",
                "HEALTH_PUBLISH_RETRY_WINDOW": "2m",
            }
        )
        self.assertEqual(config.target, "platform-connector-deployment.nvsentinel.svc:50051")
        self.assertFalse(config.insecure)
        self.assertEqual(config.ca_file, "/etc/tls/ca.crt")
        self.assertEqual(config.server_name_override, "platform-connector-deployment")
        self.assertEqual(config.token_path, "/var/run/token")
        self.assertEqual(config.retry_window_seconds, 120.0)

    def test_defaults_when_only_required_settings_are_set(self) -> None:
        config = direct_publisher.DirectPublisherConfig.from_env(
            {
                "HEALTH_PUBLISH_TARGET": "host:50051",
                "HEALTH_PUBLISH_INSECURE": "true",
                "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/token",
            }
        )
        self.assertTrue(config.insecure)
        self.assertIsNone(config.ca_file)
        self.assertIsNone(config.server_name_override)
        self.assertEqual(config.token_path, "/var/run/token")
        self.assertEqual(config.retry_window_seconds, 300.0)

    def test_ca_file_required_unless_insecure(self) -> None:
        with self.assertRaisesRegex(ValueError, "HEALTH_PUBLISH_TLS_CA_FILE"):
            direct_publisher.DirectPublisherConfig.from_env(
                {"HEALTH_PUBLISH_TARGET": "host:50051", "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/token"}
            )

    def test_token_path_required_in_direct_mode(self) -> None:
        """The server authenticates every publish, so a target without a token path fails at startup."""
        target = {"HEALTH_PUBLISH_TARGET": "host:50051"}
        for env in (
            {**target, "HEALTH_PUBLISH_INSECURE": "true"},
            {**target, "HEALTH_PUBLISH_TLS_CA_FILE": "/etc/tls/ca.crt"},
            {**target, "HEALTH_PUBLISH_INSECURE": "true", "HEALTH_PUBLISH_TOKEN_PATH": "  "},
        ):
            with self.assertRaisesRegex(ValueError, "HEALTH_PUBLISH_TOKEN_PATH", msg=str(env)):
                direct_publisher.DirectPublisherConfig.from_env(env)

    def test_insecure_reads_the_same_booleans_as_the_go_client(self) -> None:
        """HEALTH_PUBLISH_INSECURE takes the vocabulary Go's strconv.ParseBool takes, and nothing else.

        The two clients read the same variable, so a value must mean the same
        thing to both: anything outside the shared vocabulary fails at startup
        instead of silently keeping or dropping TLS.
        """
        base = {"HEALTH_PUBLISH_TARGET": "host:50051", "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/token"}
        for spelled_true in ("true", "TRUE", "True", "1", "t", "T"):
            config = direct_publisher.DirectPublisherConfig.from_env({**base, "HEALTH_PUBLISH_INSECURE": spelled_true})
            self.assertTrue(config.insecure, msg=spelled_true)
        for spelled_false in ("false", "FALSE", "0", "f", "F"):
            config = direct_publisher.DirectPublisherConfig.from_env(
                {**base, "HEALTH_PUBLISH_TLS_CA_FILE": "/etc/tls/ca.crt", "HEALTH_PUBLISH_INSECURE": spelled_false}
            )
            self.assertFalse(config.insecure, msg=spelled_false)
        for not_a_boolean in ("yes", "on", "enabled"):
            with self.assertRaisesRegex(ValueError, "HEALTH_PUBLISH_INSECURE", msg=not_a_boolean):
                direct_publisher.DirectPublisherConfig.from_env({**base, "HEALTH_PUBLISH_INSECURE": not_a_boolean})

    def test_invalid_tuning_values_raise(self) -> None:
        base = {
            "HEALTH_PUBLISH_TARGET": "host:50051",
            "HEALTH_PUBLISH_INSECURE": "true",
            "HEALTH_PUBLISH_TOKEN_PATH": "/var/run/token",
        }
        for extra in (
            {"HEALTH_PUBLISH_RETRY_WINDOW": "soon"},
            {"HEALTH_PUBLISH_RETRY_WINDOW": "0s"},
        ):
            with self.assertRaises(ValueError, msg=extra):
                direct_publisher.DirectPublisherConfig.from_env({**base, **extra})


class TestDropReasonVocabulary(unittest.TestCase):
    def test_drop_reasons_match_the_go_healthpub_client(self) -> None:
        """The reason label values are shared with the Go client (commons/pkg/healthpub/metrics.go).

        Dashboards key on the reason label across both clients, so the same
        condition must be metered under the same string, and neither client
        may meter a reason the other does not know.
        """
        self.assertEqual(direct_publisher.DROP_REASON_REJECTED, "rejected")
        self.assertEqual(direct_publisher.DROP_REASON_WINDOW_EXPIRED, "retry_window_exhausted")
        self.assertEqual(direct_publisher.DROP_REASON_SHUTDOWN, "shutdown")
        self.assertEqual(direct_publisher.DROP_REASON_WITHDRAWN, "withdrawn")
        python_reasons = {value for name, value in vars(direct_publisher).items() if name.startswith("DROP_REASON_")}
        self.assertEqual(python_reasons, {"rejected", "retry_window_exhausted", "shutdown", "withdrawn"})


class TestDirectPublisherDelivery(unittest.TestCase):
    def test_publish_sends_and_returns_once_the_server_answered(self) -> None:
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before = succeeded()

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(len(stub.calls), 1)
        self.assertEqual(succeeded(), before + 1)

        key = stub.calls[0][direct_publisher.IDEMPOTENCY_KEY_HEADER]
        self.assertRegex(key, IDEMPOTENCY_KEY_FORMAT)

    def test_publish_empty_batch_is_a_noop(self) -> None:
        """An empty batch is a success with no RPC.

        Matches the Go client: sending HealthEvents(events=[]) would spend an
        RPC on nothing.
        """
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before_success = succeeded()

        self.assertTrue(publisher.publish([]))
        self.assertEqual(len(stub.calls), 0)
        self.assertEqual(succeeded(), before_success)

    def test_token_read_failure_retries_within_the_window(self) -> None:
        """A missing token file is retryable: the kubelet may still mount or rewrite it."""
        tmpdir = tempfile.mkdtemp(prefix="ghm_token_retry_")
        self.addCleanup(shutil.rmtree, tmpdir, True)
        token_path = os.path.join(tmpdir, "token")

        stub = ScriptedStub()
        publisher = make_publisher(stub, token_path=token_path)
        self.addCleanup(publisher.close)
        before_retries = retried()
        before_success = succeeded()

        # Every attempt fails reading the token, before any RPC is made.
        pending = publish_in_background(publisher, sample_events())
        self.assertTrue(wait_until(lambda: retried() >= before_retries + 2))
        self.assertEqual(len(stub.calls), 0, "no attempt may publish without the configured token")

        with open(token_path, "w") as token_file:
            token_file.write("late-token")
        self.assertTrue(pending.result(timeout=5.0), "the batch is delivered once the token appears")
        self.assertEqual(succeeded(), before_success + 1)
        self.assertEqual(stub.calls[-1]["authorization"], "Bearer late-token")

    def test_unreadable_ca_file_fails_at_startup(self) -> None:
        """A CA bundle that cannot be read is a deployment error, reported when the publisher is built."""
        with self.assertRaises(OSError):
            direct_publisher.DirectPublisher(make_config(insecure=False, ca_file="/nonexistent/ca.crt"))

    def test_tls_dial_failure_retries_within_the_window(self) -> None:
        """A CA bundle that is not a certificate fails inside the real channel plumbing and must retry like any outage."""
        tmpdir = tempfile.mkdtemp(prefix="ghm_bad_ca_")
        self.addCleanup(shutil.rmtree, tmpdir, True)
        bad_ca_path = os.path.join(tmpdir, "ca.crt")
        with open(bad_ca_path, "w") as ca_file:
            ca_file.write("not a certificate")

        publisher = direct_publisher.DirectPublisher(make_config(retry_window=0.3, insecure=False, ca_file=bad_ca_path))
        # Same shrink as make_publisher, but without touching _ensure_stub:
        # the dial itself is under test.
        publisher._initial_backoff = 0.01
        publisher._max_backoff = 0.02
        self.addCleanup(publisher.close)
        before_dropped = dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED)
        before_retries = retried()

        self.assertFalse(publisher.publish(sample_events()), "the batch is dropped when its window ends")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED), before_dropped + 1)
        self.assertGreater(retried(), before_retries)
        # The publisher is still usable afterwards: with a stub standing in for
        # the channel, the next batch is delivered.
        publisher._ensure_stub = lambda: ScriptedStub()
        self.assertTrue(publisher.publish(sample_events()), "a dial failure leaves the publisher usable")

    def test_idempotency_key_is_stable_across_retries_and_unique_per_batch(self) -> None:
        stub = ScriptedStub(
            script=[
                FakeRpcError(grpc.StatusCode.UNAVAILABLE, "connection refused"),
                FakeRpcError(grpc.StatusCode.UNAVAILABLE, "connection refused"),
                None,  # first batch succeeds on the third attempt
                None,  # second batch succeeds immediately
            ]
        )
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(len(stub.calls), 3)
        first_batch_keys = {call[direct_publisher.IDEMPOTENCY_KEY_HEADER] for call in stub.calls}
        self.assertEqual(len(first_batch_keys), 1, "the key must be reused verbatim on every retry")

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(len(stub.calls), 4)
        second_key = stub.calls[3][direct_publisher.IDEMPOTENCY_KEY_HEADER]
        self.assertNotIn(second_key, first_batch_keys, "each batch needs its own key")

    def test_error_without_status_code_is_retried(self) -> None:
        # A bare RpcError carries no verdict, so it is retried like a transport failure.
        stub = ScriptedStub(script=[grpc.RpcError(), None])
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before_rejected = dropped(direct_publisher.DROP_REASON_REJECTED)

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(len(stub.calls), 2)
        self.assertEqual(dropped(direct_publisher.DROP_REASON_REJECTED), before_rejected)

    def test_retry_window_expiry_drops_with_reason(self) -> None:
        # Every attempt fails; the elapsed-time window must end the retries.
        stub = AlwaysUnavailable()
        publisher = make_publisher(stub, retry_window=0.05)
        self.addCleanup(publisher.close)
        before_dropped = dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED)
        before_retries = retried()

        self.assertFalse(publisher.publish(sample_events()), "the caller learns the batch was not delivered")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED), before_dropped + 1)
        self.assertGreater(retried(), before_retries)

    def test_auth_failures_retry_within_window(self) -> None:
        """UNAUTHENTICATED is not a permanent rejection: the kubelet rewrites the token, so a retry can succeed."""
        stub = ScriptedStub(script=[FakeRpcError(grpc.StatusCode.UNAUTHENTICATED, "token expired"), None])
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before_success = succeeded()
        before_retries = retried()
        before_rejected = dropped(direct_publisher.DROP_REASON_REJECTED)

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(succeeded(), before_success + 1)
        self.assertEqual(len(stub.calls), 2)
        self.assertEqual(retried(), before_retries + 1)
        self.assertEqual(dropped(direct_publisher.DROP_REASON_REJECTED), before_rejected)

    def test_unavailable_failure_redials_so_a_rotated_ca_is_reread(self) -> None:
        """UNAVAILABLE discards the channel: the next attempt dials again and re-reads the CA file.

        Every other retryable failure keeps the channel, so the UNAUTHENTICATED
        in the middle of the script must not add a dial.
        """
        stub = ScriptedStub(
            script=[
                FakeRpcError(grpc.StatusCode.UNAVAILABLE, "connection refused"),
                FakeRpcError(grpc.StatusCode.UNAUTHENTICATED, "token expired"),
                None,
            ]
        )

        class FakeChannel:
            """Just enough of grpc.Channel for PlatformConnectorStub(channel) and close()."""

            def __init__(self) -> None:
                self.closed = False

            def unary_unary(self, method: str, **kwargs: Any) -> Any:
                return stub.HealthEventOccurredV1

            def close(self) -> None:
                self.closed = True

        dialed: list[FakeChannel] = []

        def fake_dial() -> FakeChannel:
            channel = FakeChannel()
            dialed.append(channel)
            return channel

        publisher = direct_publisher.DirectPublisher(make_config())
        # Same shrink as make_publisher, but _ensure_stub stays real: the dial
        # and channel bookkeeping are what is under test.
        publisher._initial_backoff = 0.01
        publisher._max_backoff = 0.02
        publisher._dial = fake_dial
        self.addCleanup(publisher.close)
        before_success = succeeded()

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(succeeded(), before_success + 1)
        self.assertEqual(len(stub.calls), 3)
        self.assertEqual(len(dialed), 2, "exactly one re-dial: after UNAVAILABLE, not after UNAUTHENTICATED")
        self.assertTrue(dialed[0].closed, "the channel that failed with UNAVAILABLE must be closed")
        self.assertFalse(dialed[1].closed)

        publisher.close()
        self.assertTrue(dialed[1].closed, "close() releases the live channel")


class TestPermanentRejections(unittest.TestCase):
    def test_permanent_status_codes_match_the_go_healthpub_client(self) -> None:
        """The codes the server answers the same way on every retry of the same batch.

        UNAUTHENTICATED is deliberately absent: the projected token rotates,
        so a later attempt can succeed.
        """
        self.assertEqual(
            direct_publisher.PERMANENT_STATUS_CODES,
            {
                grpc.StatusCode.INVALID_ARGUMENT,
                grpc.StatusCode.PERMISSION_DENIED,
                grpc.StatusCode.UNIMPLEMENTED,
            },
        )
        self.assertNotIn(grpc.StatusCode.UNAUTHENTICATED, direct_publisher.PERMANENT_STATUS_CODES)
        # A proxy may answer RESOURCE_EXHAUSTED for throttling; dropping on it could lose batches.
        self.assertNotIn(grpc.StatusCode.RESOURCE_EXHAUSTED, direct_publisher.PERMANENT_STATUS_CODES)

    def test_oversize_batch_is_rejected(self) -> None:
        """A batch larger than the server receives could never be delivered: refused at once, never attempted."""
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before_rejected = dropped(direct_publisher.DROP_REASON_REJECTED)

        events = sample_events()
        events[0].message = "x" * (direct_publisher.MAX_MESSAGE_BYTES + 1)

        self.assertFalse(publisher.publish(events), "the caller learns the batch was refused")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_REJECTED), before_rejected + 1)
        self.assertEqual(stub.calls, [], "an oversize batch is never sent")
        self.assertFalse(publisher._sending)

    def test_transient_resource_exhausted_is_retried(self) -> None:
        stub = ScriptedStub(script=[FakeRpcError(grpc.StatusCode.RESOURCE_EXHAUSTED, "rate limited"), None])
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before_rejected = dropped(direct_publisher.DROP_REASON_REJECTED)
        before_success = succeeded()

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(succeeded(), before_success + 1, "the batch delivers on the retry")
        self.assertEqual(len(stub.calls), 2)
        self.assertEqual(dropped(direct_publisher.DROP_REASON_REJECTED), before_rejected)

    def test_permanent_rejection_drops_immediately_with_reason_rejected(self) -> None:
        for code in sorted(direct_publisher.PERMANENT_STATUS_CODES, key=lambda status: status.name):
            with self.subTest(code=code.name):
                # A success is scripted behind the rejection: if the call
                # retried, it would land there and show up in the counters.
                stub = ScriptedStub(script=[FakeRpcError(code, "rejected"), None])
                publisher = make_publisher(stub)
                self.addCleanup(publisher.close)
                before_dropped = dropped(direct_publisher.DROP_REASON_REJECTED)
                before_retries = retried()
                before_success = succeeded()

                self.assertFalse(publisher.publish(sample_events()), "the caller learns the batch was refused")
                self.assertEqual(dropped(direct_publisher.DROP_REASON_REJECTED), before_dropped + 1)
                self.assertEqual(len(stub.calls), 1, "a permanent rejection must make exactly one attempt")
                self.assertEqual(retried(), before_retries)
                self.assertEqual(succeeded(), before_success)


class TestSendSlotAndClose(unittest.TestCase):
    def test_one_send_at_a_time(self) -> None:
        """However many threads publish at once, the server sees one attempt at a time and every caller gets its outcome."""

        class SlowStub(ScriptedStub):
            def HealthEventOccurredV1(self, request, timeout=None, metadata=None):
                time.sleep(0.002)
                return super().HealthEventOccurredV1(request, timeout, metadata)

        stub = SlowStub()
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)

        pending = [publish_in_background(publisher, sample_events()) for _ in range(12)]
        for future in pending:
            self.assertTrue(future.result(timeout=5.0))
        self.assertEqual(len(stub.calls), 12)
        self.assertEqual(stub.max_in_flight, 1, "attempts never overlap")

    def test_waiting_callers_are_served_in_arrival_order(self) -> None:
        """Calls waiting for the slot get it first come, first served, one at a time, as in the Go client."""
        gate = threading.Event()
        stub = ScriptedStub(gate=gate)
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        self.addCleanup(gate.set)

        pending = [publish_in_background(publisher, named_events("Check0"))]
        self.assertTrue(stub.started.wait(5.0), "the first call holds the slot inside its attempt")
        for i in range(1, 5):
            pending.append(publish_in_background(publisher, named_events(f"Check{i}")))
            # Each caller joins the line before the next one starts, so the
            # arrival order is defined.
            self.assertTrue(wait_until(lambda: len(publisher._waiters) == i))

        gate.set()
        for future in pending:
            self.assertTrue(future.result(timeout=5.0))
        self.assertEqual(stub.check_names, [f"Check{i}" for i in range(5)], "delivery order equals arrival order")
        self.assertEqual(stub.max_in_flight, 1, "the waiting calls were sent one at a time")
        self.assertEqual(len(publisher._waiters), 0)

    def test_publish_after_close_is_rejected(self) -> None:
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        publisher.close()
        before = dropped(direct_publisher.DROP_REASON_SHUTDOWN)

        self.assertFalse(publisher.publish(sample_events()))
        self.assertEqual(dropped(direct_publisher.DROP_REASON_SHUTDOWN), before, "a refused batch was never accepted")

    def test_close_is_idempotent(self) -> None:
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        publisher.close()
        publisher.close()
        self.assertFalse(publisher.publish(sample_events()))

    def test_oversize_batch_after_close_is_refused_not_rejected(self) -> None:
        """Closed is checked first, like the Go client: a refused batch is never metered as a rejection."""
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        publisher.close()
        before_rejected = dropped(direct_publisher.DROP_REASON_REJECTED)

        events = sample_events()
        events[0].message = "x" * (direct_publisher.MAX_MESSAGE_BYTES + 1)

        self.assertFalse(publisher.publish(events))
        self.assertEqual(dropped(direct_publisher.DROP_REASON_REJECTED), before_rejected, "refused, not rejected")
        self.assertEqual(stub.calls, [])

    def test_close_ends_the_calls_in_progress(self) -> None:
        """close() ends the call on the wire (the channel is closed under it) and the calls waiting for the slot at once."""
        gate = threading.Event()
        # A cancelled call fails with CANCELLED once the channel is closed.
        stub = ScriptedStub(script=[FakeRpcError(grpc.StatusCode.CANCELLED, "channel closed")], gate=gate)
        publisher = make_publisher(stub)

        class CancellingChannel:
            """Closing a live grpc channel cancels its in-flight calls; releasing the gate stands in for that."""

            def __init__(self) -> None:
                self.closed = False

            def close(self) -> None:
                self.closed = True
                gate.set()

        channel = CancellingChannel()
        publisher._channel = channel
        self.addCleanup(gate.set)
        before_shutdown = dropped(direct_publisher.DROP_REASON_SHUTDOWN)

        # The first call is parked inside the blocked send; the second waits for the slot.
        pending = [publish_in_background(publisher, sample_events())]
        self.assertTrue(stub.started.wait(5.0))
        pending.append(publish_in_background(publisher, sample_events()))
        self.assertTrue(wait_until(lambda: len(publisher._waiters) == 1))

        started = time.monotonic()
        publisher.close()
        self.assertLess(time.monotonic() - started, 2.0, "close() waits for nothing")
        self.assertTrue(channel.closed, "the channel is closed under the blocked call to cancel it")
        for future in pending:
            self.assertFalse(future.result(timeout=5.0), "a batch ended at shutdown is reported undelivered")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_SHUTDOWN), before_shutdown + 2)
        self.assertEqual(len(publisher._waiters), 0)

    def test_attempt_timeout_never_outlives_the_window_or_the_caller(self) -> None:
        """One attempt is clipped to what is left of the batch's window and of the caller's wait."""
        batch = direct_publisher._Batch(
            request=platformconnector_pb2.HealthEvents(),
            idempotency_key="k",
            deadline=time.monotonic() + 300.0,
        )
        self.assertAlmostEqual(batch.attempt_timeout(30.0), 30.0, delta=0.5)

        batch.deadline = time.monotonic() + 2.0
        self.assertAlmostEqual(batch.attempt_timeout(30.0), 2.0, delta=0.5)

        batch.deadline = time.monotonic() + 300.0
        batch.wait_deadline = time.monotonic() + 0.3
        self.assertAlmostEqual(batch.attempt_timeout(30.0), 0.3, delta=0.2)

    def test_retry_window_counts_time_waiting_for_the_slot(self) -> None:
        """The window runs from the publish() call: during an outage a batch that waited for the slot past its own
        window is dropped without an attempt, and a batch published after the outage is delivered."""

        class Outage(ScriptedStub):
            def __init__(self) -> None:
                super().__init__()
                self.down = True
                self.release = threading.Event()

            def HealthEventOccurredV1(self, request, timeout=None, metadata=None):
                self.started.set()
                with self._lock:
                    first_call = not self.calls
                    self.calls.append(dict(metadata or []))
                if self.down:
                    if first_call:
                        # The head's attempt lasts until the test releases it,
                        # once the waiting batch's window has ended; the timing
                        # is then decided by the test, not by the scheduler.
                        self.release.wait(5.0)
                    raise FakeRpcError(grpc.StatusCode.UNAVAILABLE, "outage")
                return platformconnector_pb2.HealthEvents()

        stub = Outage()
        publisher = make_publisher(stub, retry_window=0.1)
        self.addCleanup(publisher.close)
        before_expired = dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED)
        before_success = succeeded()

        head = publish_in_background(publisher, sample_events())
        self.assertTrue(stub.started.wait(5.0), "the head batch is inside its attempt")
        waiting = publish_in_background(publisher, sample_events())
        self.assertTrue(wait_until(lambda: publisher._waiters), "the second batch is waiting for the slot")
        waiting_deadline = publisher._waiters[0].deadline
        time.sleep(max(0.0, waiting_deadline - time.monotonic()) + 0.05)
        stub.release.set()
        self.assertFalse(head.result(timeout=5.0))
        self.assertFalse(waiting.result(timeout=5.0))
        self.assertEqual(
            dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED),
            before_expired + 2,
            "both batches expire when their windows end",
        )
        keys_attempted = {call[direct_publisher.IDEMPOTENCY_KEY_HEADER] for call in stub.calls}
        self.assertEqual(len(keys_attempted), 1, "the batch whose window ended while waiting was never attempted")

        stub.down = False
        self.assertTrue(publisher.publish(sample_events()), "a fresh batch is delivered at once")
        self.assertEqual(succeeded(), before_success + 1)

    def test_publish_timeout_withdraws_a_batch_waiting_for_the_slot(self) -> None:
        """A caller that stops waiting takes its batch with it before it is ever sent, so a re-emit cannot duplicate it."""
        gate = threading.Event()
        stub = ScriptedStub(gate=gate)
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        self.addCleanup(gate.set)
        before_withdrawn = dropped(direct_publisher.DROP_REASON_WITHDRAWN)
        before_success = succeeded()

        head = publish_in_background(publisher, sample_events())
        self.assertTrue(stub.started.wait(5.0), "the head batch is inside its attempt")

        self.assertFalse(publisher.publish(sample_events(), timeout=0.05), "not delivered in time: withdrawn")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WITHDRAWN), before_withdrawn + 1)

        gate.set()
        self.assertTrue(head.result(timeout=5.0))
        self.assertEqual(succeeded(), before_success + 1)
        self.assertEqual(len(stub.calls), 1, "the withdrawn batch is never sent")

    def test_publish_timeout_cuts_the_attempt_in_flight(self) -> None:
        """A caller that stops waiting takes its batch with it even from the wire: the attempt is bounded by the wait, and no retry follows."""
        gate = threading.Event()
        stub = ScriptedStub(gate=gate)
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        self.addCleanup(gate.set)
        before_withdrawn = dropped(direct_publisher.DROP_REASON_WITHDRAWN)
        before_success = succeeded()

        started = time.monotonic()
        self.assertFalse(publisher.publish(sample_events(), timeout=0.1), "not delivered in time: withdrawn")
        self.assertLess(time.monotonic() - started, 2.0, "the attempt ends with the caller's wait")
        self.assertEqual(len(stub.calls), 0, "the attempt never completed, and no retry follows")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WITHDRAWN), before_withdrawn + 1)
        self.assertEqual(succeeded(), before_success)

    def test_publish_timeout_stops_retries(self) -> None:
        """Once the caller has left, a failed attempt is not retried, even in the middle of a long backoff."""
        stub = AlwaysUnavailable()
        publisher = make_publisher(stub, retry_window=60.0)
        self.addCleanup(publisher.close)
        publisher._initial_backoff = 10.0
        publisher._max_backoff = 10.0
        before_withdrawn = dropped(direct_publisher.DROP_REASON_WITHDRAWN)

        started = time.monotonic()
        self.assertFalse(publisher.publish(sample_events(), timeout=0.1))
        self.assertLess(time.monotonic() - started, 2.0, "the backoff sleep ends with the caller")
        self.assertEqual(len(stub.calls), 1, "no attempt is made for a caller that left")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WITHDRAWN), before_withdrawn + 1)

    def test_close_interrupts_a_long_backoff_sleep(self) -> None:
        """close() ends a backoff pause instead of waiting it out, dropping the batch as shutdown."""
        stub = AlwaysUnavailable()
        publisher = make_publisher(stub, retry_window=60.0)
        publisher._initial_backoff = 10.0
        publisher._max_backoff = 10.0
        before_shutdown = dropped(direct_publisher.DROP_REASON_SHUTDOWN)

        pending = publish_in_background(publisher, sample_events())
        self.assertTrue(wait_until(lambda: len(stub.calls) >= 1), "the first attempt fails and the call sleeps")

        started = time.monotonic()
        publisher.close()
        self.assertLess(time.monotonic() - started, 2.0, "close() must interrupt the 10s backoff sleep")
        self.assertFalse(pending.result(timeout=5.0), "the caller learns its batch was not delivered")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_SHUTDOWN), before_shutdown + 1)

    def test_window_ends_during_backoff(self) -> None:
        """A backoff pause longer than what is left of the window ends with the window: the batch drops at the deadline, with no further attempt."""
        stub = AlwaysUnavailable()
        publisher = make_publisher(stub, retry_window=0.2)
        self.addCleanup(publisher.close)
        publisher._initial_backoff = 10.0
        publisher._max_backoff = 10.0
        before_expired = dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED)

        started = time.monotonic()
        self.assertFalse(publisher.publish(sample_events()))
        elapsed = time.monotonic() - started
        self.assertGreaterEqual(elapsed, 0.15, "the call lasts until the window ends")
        self.assertLess(elapsed, 2.0, "the 10s pause is cut by the window")
        self.assertEqual(len(stub.calls), 1, "the one attempt fails and the pause outlasts the window")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WINDOW_EXPIRED), before_expired + 1)

    def test_publish_with_a_spent_timeout_is_withdrawn_before_any_attempt(self) -> None:
        """A caller whose wait is already over gets its batch withdrawn without an attempt."""
        stub = ScriptedStub()
        publisher = make_publisher(stub)
        self.addCleanup(publisher.close)
        before_withdrawn = dropped(direct_publisher.DROP_REASON_WITHDRAWN)

        self.assertFalse(publisher.publish(sample_events(), timeout=0))
        self.assertEqual(len(stub.calls), 0, "nothing is attempted for a caller that already left")
        self.assertEqual(dropped(direct_publisher.DROP_REASON_WITHDRAWN), before_withdrawn + 1)

    def test_no_channel_is_dialed_after_close(self) -> None:
        """A publish racing close() must not dial a channel that nothing would ever close."""
        publisher = direct_publisher.DirectPublisher(make_config())
        dialed = []
        publisher._dial = lambda: dialed.append(object()) or unittest.mock.MagicMock()
        publisher.close()

        with self.assertRaises(RuntimeError):
            publisher._ensure_stub()
        self.assertEqual(dialed, [], "the closed publisher refuses to dial")


class TestChannelOptions(unittest.TestCase):
    """The channel reconnects to a returned server within seconds, as the Go client does."""

    def test_reconnect_backoff_is_capped_below_the_retry_cadence(self) -> None:
        self.assertLess(direct_publisher.MAX_RECONNECT_BACKOFF_MS / 1000.0, direct_publisher.MAX_BACKOFF_SECONDS)

    def test_plaintext_channel_caps_the_reconnect_backoff(self) -> None:
        publisher = direct_publisher.DirectPublisher(make_config())
        with unittest.mock.patch.object(
            grpc, "insecure_channel", return_value=unittest.mock.sentinel.channel
        ) as insecure_channel:
            self.assertIs(publisher._dial(), unittest.mock.sentinel.channel)
        _, kwargs = insecure_channel.call_args
        self.assertIn(("grpc.max_reconnect_backoff_ms", direct_publisher.MAX_RECONNECT_BACKOFF_MS), kwargs["options"])

    def test_tls_channel_caps_the_reconnect_backoff_and_keeps_the_name_override(self) -> None:
        with tempfile.NamedTemporaryFile(suffix=".crt") as ca_file:
            ca_file.write(b"not really a certificate")
            ca_file.flush()
            publisher = direct_publisher.DirectPublisher(
                make_config(
                    target="host:50051",
                    insecure=False,
                    ca_file=ca_file.name,
                    server_name_override="platform-connector-deployment.nvsentinel.svc",
                )
            )
            with unittest.mock.patch.object(
                grpc, "ssl_channel_credentials", return_value=unittest.mock.sentinel.creds
            ), unittest.mock.patch.object(
                grpc, "secure_channel", return_value=unittest.mock.sentinel.channel
            ) as secure_channel:
                self.assertIs(publisher._dial(), unittest.mock.sentinel.channel)
        args, kwargs = secure_channel.call_args
        self.assertEqual(args, ("host:50051", unittest.mock.sentinel.creds))
        self.assertIn(("grpc.max_reconnect_backoff_ms", direct_publisher.MAX_RECONNECT_BACKOFF_MS), kwargs["options"])
        self.assertIn(
            ("grpc.ssl_target_name_override", "platform-connector-deployment.nvsentinel.svc"), kwargs["options"]
        )


class TestProcessorModeSelection(unittest.TestCase):
    """The processor picks direct mode from HEALTH_PUBLISH_TARGET alone."""

    def setUp(self) -> None:
        self._env_patcher = unittest.mock.patch.dict(
            os.environ, {k: v for k, v in os.environ.items() if not k.startswith("HEALTH_PUBLISH_")}, clear=True
        )
        self._env_patcher.start()

        self._tmpdir = tempfile.mkdtemp(prefix="ghm_direct_mode_")
        self._socket_path = os.path.join(self._tmpdir, "nvsentinel.sock")
        self._state_file_path = os.path.join(self._tmpdir, "statefile")
        with open(self._state_file_path, "w") as state_file:
            state_file.write("test_boot_id")
        self._metadata_path = os.path.join(self._tmpdir, "gpu_metadata.json")
        with open(self._metadata_path, "w") as metadata_file:
            json.dump({"version": "1.0", "gpus": [], "nvswitches": []}, metadata_file)
        self._token_path = os.path.join(self._tmpdir, "token")
        with open(self._token_path, "w") as token_file:
            token_file.write("processor-token")

    def tearDown(self) -> None:
        self._env_patcher.stop()
        shutil.rmtree(self._tmpdir, ignore_errors=True)

    def _make_processor(self) -> platform_connector.PlatformConnectorEventProcessor:
        return platform_connector.PlatformConnectorEventProcessor(
            config=platform_connector.PlatformConnectorConfig(
                socket_path=self._socket_path,
                node_name="node1",
                dcgm_errors_info_dict={},
                state_file_path=self._state_file_path,
                metadata_path=self._metadata_path,
                processing_strategy=platformconnector_pb2.STORE_ONLY,
            ),
            exit=threading.Event(),
        )

    def test_env_unset_keeps_socket_mode(self) -> None:
        processor = self._make_processor()
        self.assertIsNone(processor._direct_publisher)
        # close() must be a harmless no-op in socket mode.
        processor.close()

    def test_env_set_selects_direct_mode(self) -> None:
        os.environ["HEALTH_PUBLISH_TARGET"] = "127.0.0.1:1"
        os.environ["HEALTH_PUBLISH_INSECURE"] = "true"
        os.environ["HEALTH_PUBLISH_TOKEN_PATH"] = self._token_path
        processor = self._make_processor()
        self.addCleanup(processor.close)
        self.assertIsNotNone(processor._direct_publisher)

    def test_direct_mode_requires_token_path(self) -> None:
        os.environ["HEALTH_PUBLISH_TARGET"] = "127.0.0.1:1"
        os.environ["HEALTH_PUBLISH_INSECURE"] = "true"
        with self.assertRaisesRegex(ValueError, "HEALTH_PUBLISH_TOKEN_PATH"):
            self._make_processor()

    def test_direct_mode_publishes_and_skips_the_socket_gate(self) -> None:
        os.environ["HEALTH_PUBLISH_TARGET"] = "127.0.0.1:1"
        os.environ["HEALTH_PUBLISH_INSECURE"] = "true"
        os.environ["HEALTH_PUBLISH_TOKEN_PATH"] = self._token_path
        processor = self._make_processor()
        real_publisher = processor._direct_publisher
        self.addCleanup(real_publisher.close)

        fake = unittest.mock.MagicMock()
        fake.publish.return_value = True
        processor._direct_publisher = fake

        before_skipped = pc_metrics.health_events_insertion_skipped_pc_unavailable._value.get()
        events = sample_events()
        # The socket does not exist, and direct mode must not care.
        self.assertTrue(processor.send_health_event_with_retries(events))
        fake.publish.assert_called_once_with(events, timeout=None)
        self.assertEqual(
            pc_metrics.health_events_insertion_skipped_pc_unavailable._value.get(),
            before_skipped,
            "direct mode must not consult the socket-presence gate",
        )

        # A rejected or dropped batch surfaces as False so callers keep their caches.
        fake.publish.return_value = False
        self.assertFalse(processor.send_health_event_with_retries(events))

        # The critical paths bound their wait; the bound reaches the publisher.
        fake.publish.reset_mock()
        fake.publish.return_value = True
        self.assertTrue(processor.send_health_event_with_retries(events, delivery_timeout_seconds=7.5))
        fake.publish.assert_called_once_with(events, timeout=7.5)

        # close() shuts the injected publisher down.
        processor.close()
        fake.close.assert_called_once_with()


class RecordingServicer(platformconnector_pb2_grpc.PlatformConnectorServicer):
    def __init__(self) -> None:
        self.received: list[tuple[platformconnector_pb2.HealthEvents, dict[str, str]]] = []

    def HealthEventOccurredV1(self, request: platformconnector_pb2.HealthEvents, context: Any):
        self.received.append((request, dict(context.invocation_metadata())))
        return platformconnector_pb2.HealthEvents()


class TestDirectPublisherOnTheWire(unittest.TestCase):
    """End-to-end over a real in-process gRPC server: header and token on the wire."""

    def test_batch_arrives_with_idempotency_key_and_bearer_token(self) -> None:
        servicer = RecordingServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(servicer, server)
        port = server.add_insecure_port("127.0.0.1:0")
        server.start()
        self.addCleanup(server.stop, 0)

        with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix="_token") as token_file:
            token_file.write("wire-token")
            token_path = token_file.name
        self.addCleanup(os.unlink, token_path)

        publisher = direct_publisher.DirectPublisher(make_config(target=f"127.0.0.1:{port}", token_path=token_path))
        self.addCleanup(publisher.close)

        self.assertTrue(publisher.publish(sample_events()))
        self.assertEqual(len(servicer.received), 1)

        request, received_metadata = servicer.received[0]
        self.assertEqual(len(request.events), 1)
        self.assertRegex(received_metadata["idempotency-key"], IDEMPOTENCY_KEY_FORMAT)
        self.assertEqual(received_metadata["authorization"], "Bearer wire-token")


if __name__ == "__main__":
    unittest.main()
