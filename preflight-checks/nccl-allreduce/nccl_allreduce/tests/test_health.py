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

"""Unit tests for nccl_allreduce/health.py"""

import re
from collections.abc import Callable, Iterator
from concurrent import futures
from contextlib import contextmanager
from pathlib import Path
from typing import Any
from unittest.mock import MagicMock, patch

import grpc
import pytest
from google.protobuf.empty_pb2 import Empty

from nccl_allreduce.config import DirectPublisherConfig
from nccl_allreduce.errors import NCCLError
from nccl_allreduce.health import (
    INITIAL_BACKOFF_SECONDS,
    MAX_BACKOFF_SECONDS,
    MAX_RETRIES,
    HealthReporter,
    RPC_TIMEOUT,
    SPENT_BUDGET_ATTEMPT_SECONDS,
)
from nccl_allreduce.protos import health_event_pb2 as pb
from nccl_allreduce.protos import health_event_pb2_grpc as pb_grpc

# The deployment platform connector's rule for the idempotency-key header.
IDEMPOTENCY_KEY_FORMAT = re.compile(r"^[A-Za-z0-9._:-]{1,128}$")


class RpcErrorWithCode(grpc.RpcError):
    """An RpcError carrying a status code, the way a live channel's failures do."""

    def __init__(self, code: grpc.StatusCode) -> None:
        super().__init__()
        self._code = code

    def code(self) -> grpc.StatusCode:
        return self._code


@pytest.fixture()
def reporter(monkeypatch: pytest.MonkeyPatch) -> HealthReporter:
    # Keep the ambient environment from leaking a token path into the reporter.
    monkeypatch.delenv("PLATFORM_CONNECTOR_TOKEN_PATH", raising=False)
    return HealthReporter(
        socket_path="unix:///tmp/test.sock",
        node_name="test-node",
        processing_strategy=pb.ProcessingStrategy.EXECUTE_REMEDIATION,
    )


class TestBuildEvent:
    """Tests for HealthReporter event building."""

    def test_build_success_event(self, reporter: HealthReporter) -> None:
        event = reporter._build_event(
            is_healthy=True,
            is_fatal=False,
            message="Test passed",
            recommended_action=pb.RecommendedAction.NONE,
            error_code=None,
        )

        assert event.isHealthy is True
        assert event.isFatal is False
        assert event.message == "Test passed"
        assert event.agent == "preflight-nccl-allreduce"
        assert event.componentClass == "Node"
        assert event.checkName == "NCCLAllReduceTest"
        assert event.nodeName == "test-node"
        assert len(event.errorCode) == 0

    def test_build_failure_event(self, reporter: HealthReporter) -> None:
        event = reporter._build_event(
            is_healthy=False,
            is_fatal=True,
            message="BW degraded",
            recommended_action=pb.RecommendedAction.CONTACT_SUPPORT,
            error_code="NCCL_ALLREDUCE_BW_DEGRADED",
        )

        assert event.isHealthy is False
        assert event.isFatal is True
        assert event.errorCode == ["NCCL_ALLREDUCE_BW_DEGRADED"]
        assert event.recommendedAction == pb.RecommendedAction.CONTACT_SUPPORT

    def test_build_event_has_timestamp(self, reporter: HealthReporter) -> None:
        event = reporter._build_event(
            is_healthy=True,
            is_fatal=False,
            message="test",
            recommended_action=pb.RecommendedAction.NONE,
            error_code=None,
        )
        assert event.generatedTimestamp.seconds > 0

    def test_socket_path_strips_unix_prefix(self) -> None:
        r = HealthReporter(
            socket_path="unix:///var/run/nvsentinel.sock",
            node_name="node",
            processing_strategy=pb.ProcessingStrategy.EXECUTE_REMEDIATION,
        )
        assert r._socket_path == "/var/run/nvsentinel.sock"


class TestSendFailure:
    """Tests for send_failure validation."""

    def test_raises_for_error_without_error_code(self, reporter: HealthReporter) -> None:
        """Errors with no error_code (like HEALTH_REPORT_FAILED) cannot send events."""
        with pytest.raises(ValueError, match="does not generate health events"):
            reporter.send_failure(NCCLError.HEALTH_REPORT_FAILED, "test")

    def test_raises_for_success_error_code(self, reporter: HealthReporter) -> None:
        """SUCCESS has no error_code, so send_failure should reject it."""
        with pytest.raises(ValueError, match="does not generate health events"):
            reporter.send_failure(NCCLError.SUCCESS, "test")


class TestSendWithRetries:
    """Which gRPC failures are worth another attempt."""

    @staticmethod
    def _send_with_failing_stub(reporter: HealthReporter, error: grpc.RpcError) -> tuple[bool, MagicMock]:
        """Runs one send whose every attempt raises `error`; returns (result, stub)."""
        stub = MagicMock()
        stub.HealthEventOccurredV1.side_effect = error
        with patch("nccl_allreduce.health.sleep"), patch("nccl_allreduce.health.grpc.insecure_channel"), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            result = reporter._send_with_retries(pb.HealthEvents(version=1))
        return result, stub

    def test_success_first_attempt(self, reporter: HealthReporter) -> None:
        stub = MagicMock()
        with patch("nccl_allreduce.health.grpc.insecure_channel"), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            result = reporter._send_with_retries(pb.HealthEvents(version=1))

        assert result is True
        stub.HealthEventOccurredV1.assert_called_once()

    @pytest.mark.parametrize(
        "code",
        [
            grpc.StatusCode.PERMISSION_DENIED,
            grpc.StatusCode.UNAUTHENTICATED,
            grpc.StatusCode.INVALID_ARGUMENT,
        ],
    )
    def test_does_not_retry_deterministic_rejection(self, code: grpc.StatusCode, reporter: HealthReporter) -> None:
        """A deterministic rejection answers the same way every time, so retrying only delays the workload."""
        result, stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(code))

        assert result is False
        stub.HealthEventOccurredV1.assert_called_once()

    @pytest.mark.parametrize("code", [grpc.StatusCode.UNAVAILABLE, grpc.StatusCode.DEADLINE_EXCEEDED])
    def test_retries_transient_status(self, code: grpc.StatusCode, reporter: HealthReporter) -> None:
        result, stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(code))

        assert result is False
        assert stub.HealthEventOccurredV1.call_count == MAX_RETRIES


class TestTokenAuth:
    """Bearer-token call metadata attached to HealthEventOccurredV1 sends."""

    @pytest.fixture(autouse=True)
    def _clear_token_env(self, monkeypatch: pytest.MonkeyPatch) -> None:
        """Keep the ambient environment from leaking a token path into tests."""
        monkeypatch.delenv("PLATFORM_CONNECTOR_TOKEN_PATH", raising=False)

    @staticmethod
    def _write_token(tmp_path: Path, contents: str) -> str:
        token_path = tmp_path / "token"
        token_path.write_text(contents)
        return str(token_path)

    @staticmethod
    def _make_reporter(token_path: str | None = None) -> HealthReporter:
        return HealthReporter(
            socket_path="unix:///tmp/test.sock",
            node_name="test-node",
            processing_strategy=pb.ProcessingStrategy.EXECUTE_REMEDIATION,
            token_path=token_path,
        )

    @staticmethod
    def _send_with_mock_stub(reporter: HealthReporter) -> tuple[bool, MagicMock]:
        """Runs one send with the gRPC stub mocked out; returns (result, stub)."""
        stub = MagicMock()
        with patch("nccl_allreduce.health.grpc.insecure_channel"), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            result = reporter._send_with_retries(pb.HealthEvents(version=1))
        return result, stub

    def test_metadata_carries_bearer_token_from_file(self, tmp_path: Path) -> None:
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "projected-token"))

        result, stub = self._send_with_mock_stub(reporter)

        assert result is True
        stub.HealthEventOccurredV1.assert_called_once()
        assert stub.HealthEventOccurredV1.call_args.kwargs["metadata"] == [("authorization", "Bearer projected-token")]

    def test_token_file_is_reread_on_every_call(self, tmp_path: Path) -> None:
        """The kubelet rewrites the projected token file, so every send must read it fresh."""
        token_path = self._write_token(tmp_path, "token-one")
        reporter = self._make_reporter(token_path=token_path)

        _, first_stub = self._send_with_mock_stub(reporter)
        self._write_token(tmp_path, "token-two")
        _, second_stub = self._send_with_mock_stub(reporter)

        assert first_stub.HealthEventOccurredV1.call_args.kwargs["metadata"] == [("authorization", "Bearer token-one")]
        assert second_stub.HealthEventOccurredV1.call_args.kwargs["metadata"] == [("authorization", "Bearer token-two")]

    def test_token_file_is_sent_verbatim(self, tmp_path: Path) -> None:
        """Kubelet writes the token with no surrounding whitespace, so none is removed.

        Verified on-cluster: a projected token file's byte count is identical
        before and after stripping whitespace. Trimming here would only mask a
        mount that is not a projected token volume, and gRPC rejects a header
        value containing a newline anyway.
        """
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "plain-token"))

        _, stub = self._send_with_mock_stub(reporter)

        assert stub.HealthEventOccurredV1.call_args.kwargs["metadata"] == [("authorization", "Bearer plain-token")]

    def test_no_metadata_when_token_path_unconfigured(self) -> None:
        reporter = self._make_reporter(token_path=None)

        result, stub = self._send_with_mock_stub(reporter)

        assert result is True
        stub.HealthEventOccurredV1.assert_called_once()
        assert stub.HealthEventOccurredV1.call_args.kwargs["metadata"] is None

    def test_ambient_env_var_does_not_override_an_explicit_empty_token_path(
        self, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
    ) -> None:
        """The config layer owns PLATFORM_CONNECTOR_TOKEN_PATH; the reporter uses what it is given.

        Resolving the environment a second time here would let ambient process
        state re-enable token auth for a caller that explicitly disabled it.
        """
        monkeypatch.setenv("PLATFORM_CONNECTOR_TOKEN_PATH", self._write_token(tmp_path, "env-token"))
        reporter = self._make_reporter(token_path=None)

        _, stub = self._send_with_mock_stub(reporter)

        assert stub.HealthEventOccurredV1.call_args.kwargs["metadata"] is None

    def test_missing_token_file_raises_instead_of_sending_without_token(self, tmp_path: Path) -> None:
        """A reporter configured with a token path must not send when the read fails."""
        reporter = self._make_reporter(token_path=str(tmp_path / "does-not-exist"))

        stub = MagicMock()
        with patch("nccl_allreduce.health.grpc.insecure_channel"), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            # RuntimeError, not OSError: send_success/send_failure document
            # RuntimeError and their callers catch only that.
            with pytest.raises(RuntimeError):
                reporter._send_with_retries(pb.HealthEvents(version=1))

        stub.HealthEventOccurredV1.assert_not_called()

    @pytest.mark.parametrize("contents", [""])
    def test_blank_token_file_raises_instead_of_sending_a_blank_credential(self, tmp_path: Path, contents: str) -> None:
        """An empty file is a broken mount, not a credential."""
        token_path = self._write_token(tmp_path, contents)
        reporter = self._make_reporter(token_path=token_path)

        stub = MagicMock()
        with patch("nccl_allreduce.health.grpc.insecure_channel"), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            with pytest.raises(RuntimeError) as raised:
                reporter._send_with_retries(pb.HealthEvents(version=1))

        # The message must name the file, since "Bearer " would come back as a
        # generic authentication error that says nothing about the mount.
        assert token_path in str(raised.value)
        stub.HealthEventOccurredV1.assert_not_called()


class RecordingServicer(pb_grpc.PlatformConnectorServicer):
    """A platform connector that records every request and its call metadata."""

    def __init__(self) -> None:
        self.received: list[tuple[pb.HealthEvents, dict[str, str]]] = []

    def HealthEventOccurredV1(self, request: pb.HealthEvents, context: Any) -> Empty:
        self.received.append((request, dict(context.invocation_metadata())))
        return Empty()


class FakeClock:
    """A monotonic clock that only moves when the reporter sleeps."""

    def __init__(self) -> None:
        self.now = 1000.0

    def monotonic(self) -> float:
        return self.now

    def sleep(self, seconds: float) -> None:
        assert seconds >= 0
        self.now += seconds


class TestDirectMode:
    """Publishing straight to the deployment platform connector (HEALTH_PUBLISH_TARGET set)."""

    @staticmethod
    def _write_token(tmp_path: Path, contents: str) -> str:
        token_path = tmp_path / "token"
        token_path.write_text(contents)
        return str(token_path)

    @staticmethod
    def _make_config(**overrides: Any) -> DirectPublisherConfig:
        settings: dict[str, Any] = {
            "target": "127.0.0.1:1",
            "insecure": True,
            "ca_file": None,
            "server_name_override": None,
            "token_path": "/nonexistent/token",
            "retry_window_seconds": 10.0,
        }
        settings.update(overrides)
        return DirectPublisherConfig(**settings)

    @classmethod
    def _make_reporter(cls, **overrides: Any) -> HealthReporter:
        return HealthReporter(
            socket_path="unix:///tmp/test.sock",
            node_name="test-node",
            processing_strategy=pb.ProcessingStrategy.EXECUTE_REMEDIATION,
            publish=cls._make_config(**overrides),
        )

    @staticmethod
    @contextmanager
    def _direct_patches(
        clock: FakeClock, stub: MagicMock | None = None, sleep: Callable[[float], None] | None = None
    ) -> Iterator[None]:
        """Runs the reporter on `clock` without jitter, over a mocked plaintext channel that serves `stub`."""
        with patch("nccl_allreduce.health.sleep", sleep or clock.sleep), patch(
            "nccl_allreduce.health.monotonic", clock.monotonic
        ), patch("nccl_allreduce.health.random.uniform", return_value=0.0), patch(
            "nccl_allreduce.health.grpc.insecure_channel"
        ), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub or MagicMock()
        ):
            yield

    @classmethod
    def _send_with_failing_stub(
        cls,
        reporter: HealthReporter,
        error: Exception,
        clock: FakeClock | None = None,
        reason: str = "Failed to send health event",
    ) -> MagicMock:
        """Runs one send whose every attempt raises `error` on a fake clock until it is given up for `reason`."""
        stub = MagicMock()
        stub.HealthEventOccurredV1.side_effect = error
        with cls._direct_patches(clock or FakeClock(), stub), pytest.raises(RuntimeError, match=reason):
            reporter._send_with_retries(pb.HealthEvents(version=1))
        return stub

    def test_event_arrives_with_bearer_token_and_idempotency_key(self, tmp_path: Path) -> None:
        """End to end over a real in-process gRPC server: both headers reach the wire."""
        servicer = RecordingServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=2))
        pb_grpc.add_PlatformConnectorServicer_to_server(servicer, server)
        port = server.add_insecure_port("127.0.0.1:0")
        server.start()
        try:
            reporter = self._make_reporter(
                target=f"127.0.0.1:{port}", token_path=self._write_token(tmp_path, "wire-token")
            )

            assert reporter._send_with_retries(pb.HealthEvents(version=1, events=[pb.HealthEvent(version=1)])) is True
        finally:
            server.stop(0)

        assert len(servicer.received) == 1
        request, received_metadata = servicer.received[0]
        assert len(request.events) == 1
        assert received_metadata["authorization"] == "Bearer wire-token"
        assert IDEMPOTENCY_KEY_FORMAT.match(received_metadata["idempotency-key"])

    @pytest.mark.parametrize(
        "code",
        [grpc.StatusCode.INVALID_ARGUMENT, grpc.StatusCode.PERMISSION_DENIED, grpc.StatusCode.UNIMPLEMENTED],
    )
    def test_permanent_rejection_is_not_retried(self, code: grpc.StatusCode, tmp_path: Path) -> None:
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"))

        stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(code))

        stub.HealthEventOccurredV1.assert_called_once()

    @pytest.mark.parametrize(
        "error",
        [
            RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE),
            RpcErrorWithCode(grpc.StatusCode.DEADLINE_EXCEEDED),
            RpcErrorWithCode(grpc.StatusCode.UNAUTHENTICATED),
            grpc.RpcError(),
        ],
    )
    def test_transient_failure_is_retried_until_the_window_ends_under_one_key(
        self, error: grpc.RpcError, tmp_path: Path
    ) -> None:
        """UNAUTHENTICATED is retried too: the projected token rotates. The key never changes across retries."""
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"), retry_window_seconds=10.0)
        clock = FakeClock()

        stub = self._send_with_failing_stub(reporter, error, clock)

        # Attempts at t=0, 2, 6; the last pause is cut to the 4 s left, then the window is over.
        assert stub.HealthEventOccurredV1.call_count == 3
        assert clock.now == pytest.approx(1010.0)
        keys = {dict(call.kwargs["metadata"])["idempotency-key"] for call in stub.HealthEventOccurredV1.call_args_list}
        assert len(keys) == 1
        assert IDEMPOTENCY_KEY_FORMAT.match(keys.pop())

    def test_the_retry_window_is_one_budget_for_every_event_of_a_check(self, tmp_path: Path) -> None:
        """A check that reports many results waits through one outage at most; later events get one short attempt."""
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"), retry_window_seconds=10.0)
        clock = FakeClock()

        stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE), clock)
        assert stub.HealthEventOccurredV1.call_count == 3
        assert clock.now == pytest.approx(1010.0)

        stub = self._send_with_failing_stub(
            reporter, RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE), clock, reason="the retry budget is spent"
        )
        # One short attempt, no pause, no further retry.
        assert stub.HealthEventOccurredV1.call_count == 1
        assert stub.HealthEventOccurredV1.call_args.kwargs["timeout"] == SPENT_BUDGET_ATTEMPT_SECONDS
        assert clock.now == pytest.approx(1010.0)

    def test_a_nearly_spent_budget_still_gives_the_first_attempt_the_short_timeout(self, tmp_path: Path) -> None:
        """An event that leaves 0.5 s of the budget behind must not hand the next event a sub-second attempt."""
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"), retry_window_seconds=2.5)
        clock = FakeClock()
        stub = MagicMock()
        stub.HealthEventOccurredV1.side_effect = [RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE), None]
        with self._direct_patches(clock, stub):
            assert reporter._send_with_retries(pb.HealthEvents(version=1)) is True
        # One 2 s pause came off the 2.5 s budget.
        assert clock.now == pytest.approx(1002.0)

        stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE), clock)
        assert stub.HealthEventOccurredV1.call_count == 1
        assert stub.HealthEventOccurredV1.call_args.kwargs["timeout"] == SPENT_BUDGET_ATTEMPT_SECONDS

    def test_a_delivered_event_leaves_the_retry_budget_intact(self, tmp_path: Path) -> None:
        """An event delivered on its first attempt spends nothing; the next event still gets the full window."""
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"), retry_window_seconds=10.0)
        clock = FakeClock()

        with self._direct_patches(clock):
            assert reporter._send_with_retries(pb.HealthEvents(version=1)) is True

        stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE), clock)
        assert stub.HealthEventOccurredV1.call_count == 3
        assert clock.now == pytest.approx(1010.0)

    def test_backoff_doubles_to_the_cap(self, tmp_path: Path) -> None:
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"), retry_window_seconds=200.0)
        clock = FakeClock()
        pauses: list[float] = []

        def sleep(seconds: float) -> None:
            pauses.append(seconds)
            clock.sleep(seconds)

        stub = MagicMock()
        stub.HealthEventOccurredV1.side_effect = RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE)
        with self._direct_patches(clock, stub, sleep), pytest.raises(RuntimeError, match="the retry window ended"):
            reporter._send_with_retries(pb.HealthEvents(version=1))

        expected = [INITIAL_BACKOFF_SECONDS]
        while expected[-1] < MAX_BACKOFF_SECONDS:
            expected.append(min(expected[-1] * 2, MAX_BACKOFF_SECONDS))
        assert pauses[: len(expected)] == expected
        assert max(pauses) == MAX_BACKOFF_SECONDS
        assert sum(pauses) == pytest.approx(200.0)

    def test_each_attempt_gets_a_fresh_token_and_timeout_within_the_window(self, tmp_path: Path) -> None:
        token_path = self._write_token(tmp_path, "token-one")
        reporter = self._make_reporter(token_path=token_path, retry_window_seconds=40.0)
        clock = FakeClock()
        stub = MagicMock()

        def fail_then_rotate(*_args: Any, **_kwargs: Any) -> None:
            self._write_token(tmp_path, "token-two")
            raise RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE)

        stub.HealthEventOccurredV1.side_effect = fail_then_rotate
        with self._direct_patches(clock, stub), pytest.raises(RuntimeError, match="the retry window ended"):
            reporter._send_with_retries(pb.HealthEvents(version=1))

        calls = stub.HealthEventOccurredV1.call_args_list
        assert dict(calls[0].kwargs["metadata"])["authorization"] == "Bearer token-one"
        assert dict(calls[1].kwargs["metadata"])["authorization"] == "Bearer token-two"
        # Attempts at t=0, 2, 6, 14, 30: the per attempt timeout is 30 s until less is left in the window.
        assert [call.kwargs["timeout"] for call in calls] == [RPC_TIMEOUT, RPC_TIMEOUT, RPC_TIMEOUT, 26.0, 10.0]

    def test_the_first_attempt_is_bounded_by_the_window(self, tmp_path: Path) -> None:
        """A 10 s window gives the first attempt a 10 s timeout, not the 30 s per attempt cap."""
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"), retry_window_seconds=10.0)

        stub = self._send_with_failing_stub(reporter, RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE))

        assert stub.HealthEventOccurredV1.call_args_list[0].kwargs["timeout"] == 10.0

    def test_unreadable_token_raises_instead_of_retrying(self, tmp_path: Path) -> None:
        reporter = self._make_reporter(token_path=str(tmp_path / "does-not-exist"))
        stub = MagicMock()
        with patch("nccl_allreduce.health.sleep"), patch("nccl_allreduce.health.grpc.insecure_channel"), patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            with pytest.raises(RuntimeError) as raised:
                reporter._send_with_retries(pb.HealthEvents(version=1))

        assert "does-not-exist" in str(raised.value)
        stub.HealthEventOccurredV1.assert_not_called()

    def test_unreadable_ca_file_raises_instead_of_retrying(self, tmp_path: Path) -> None:
        reporter = self._make_reporter(
            insecure=False, ca_file=str(tmp_path / "missing-ca.crt"), token_path=self._write_token(tmp_path, "token")
        )
        with patch("nccl_allreduce.health.sleep"), patch("nccl_allreduce.health.grpc.secure_channel") as secure_channel:
            with pytest.raises(RuntimeError) as raised:
                reporter._send_with_retries(pb.HealthEvents(version=1))

        assert "missing-ca.crt" in str(raised.value)
        secure_channel.assert_not_called()

    def test_tls_channel_uses_the_ca_bundle_and_the_name_override(self, tmp_path: Path) -> None:
        ca_path = tmp_path / "ca.crt"
        ca_path.write_bytes(b"not really a certificate")
        reporter = self._make_reporter(
            target="host:50051",
            insecure=False,
            ca_file=str(ca_path),
            server_name_override="platform-connector-deployment.nvsentinel.svc",
            token_path=self._write_token(tmp_path, "token"),
        )
        stub = MagicMock()
        with patch(
            "nccl_allreduce.health.grpc.ssl_channel_credentials", return_value="creds"
        ) as ssl_credentials, patch("nccl_allreduce.health.grpc.secure_channel") as secure_channel, patch(
            "nccl_allreduce.health.grpc.insecure_channel"
        ) as insecure_channel, patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            assert reporter._send_with_retries(pb.HealthEvents(version=1)) is True

        insecure_channel.assert_not_called()
        ssl_credentials.assert_called_once_with(root_certificates=b"not really a certificate")
        args, kwargs = secure_channel.call_args
        assert args == ("host:50051", "creds")
        assert ("grpc.ssl_target_name_override", "platform-connector-deployment.nvsentinel.svc") in kwargs["options"]

    def test_ca_file_wins_over_insecure_with_no_name_override_by_default(self, tmp_path: Path) -> None:
        ca_path = tmp_path / "ca.crt"
        ca_path.write_bytes(b"ca")
        reporter = self._make_reporter(
            insecure=True, ca_file=str(ca_path), token_path=self._write_token(tmp_path, "token")
        )
        stub = MagicMock()
        with patch("nccl_allreduce.health.grpc.ssl_channel_credentials"), patch(
            "nccl_allreduce.health.grpc.secure_channel"
        ) as secure_channel, patch("nccl_allreduce.health.grpc.insecure_channel") as insecure_channel, patch(
            "nccl_allreduce.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            assert reporter._send_with_retries(pb.HealthEvents(version=1)) is True

        # A CA file wins over the insecure flag, as in the Go client.
        insecure_channel.assert_not_called()
        assert secure_channel.call_args.kwargs["options"] is None

    @pytest.mark.parametrize(
        ("error", "reason"),
        [
            (
                RpcErrorWithCode(grpc.StatusCode.PERMISSION_DENIED),
                r"the platform-connector rejected it \(PERMISSION_DENIED\)",
            ),
            (RpcErrorWithCode(grpc.StatusCode.UNAVAILABLE), "the retry window ended"),
        ],
    )
    def test_send_names_why_the_event_was_given_up(self, error: grpc.RpcError, reason: str, tmp_path: Path) -> None:
        reporter = self._make_reporter(token_path=self._write_token(tmp_path, "token"))
        stub = MagicMock()
        stub.HealthEventOccurredV1.side_effect = error
        with self._direct_patches(FakeClock(), stub), pytest.raises(
            RuntimeError, match=f"Failed to send health event: {reason}"
        ):
            reporter.send_success("all good")
