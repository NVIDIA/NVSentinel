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

from pathlib import Path
from unittest.mock import MagicMock, patch

import grpc
import pytest

from nccl_allreduce.errors import NCCLError
from nccl_allreduce.health import MAX_RETRIES, HealthReporter
from nccl_allreduce.protos import health_event_pb2 as pb


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
