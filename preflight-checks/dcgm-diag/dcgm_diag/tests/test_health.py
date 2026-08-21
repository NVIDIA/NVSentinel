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

from pathlib import Path
from unittest.mock import MagicMock, patch

import grpc
import pytest

from dcgm_diag.health import BACKOFF_FACTOR, INITIAL_DELAY, MAX_RETRIES, HealthReporter
from dcgm_diag.protos import health_event_pb2 as pb


class RpcErrorWithCode(grpc.RpcError):
    """An RpcError carrying a status code, the way a live channel's failures do."""

    def __init__(self, code: grpc.StatusCode) -> None:
        super().__init__()
        self._code = code

    def code(self) -> grpc.StatusCode:
        return self._code


@pytest.fixture
def reporter(monkeypatch: pytest.MonkeyPatch) -> HealthReporter:
    # Keep the ambient environment from leaking a token path into the reporter.
    monkeypatch.delenv("PLATFORM_CONNECTOR_TOKEN_PATH", raising=False)
    return HealthReporter(
        socket_path="unix:///var/run/nvsentinel.sock",
        node_name="test-node",
        processing_strategy=pb.ProcessingStrategy.Value("EXECUTE_REMEDIATION"),
    )


class TestSendWithRetries:
    @patch("dcgm_diag.health.grpc.insecure_channel")
    def test_success_first_attempt(self, mock_channel: MagicMock, reporter: HealthReporter) -> None:
        mock_stub = MagicMock()
        mock_channel.return_value.__enter__.return_value.unary_unary = mock_stub

        result = reporter._send_with_retries(pb.HealthEvents(version=1))
        assert result is True

    @patch("dcgm_diag.health.sleep")
    @patch("dcgm_diag.health.grpc.insecure_channel")
    def test_retries_on_failure(self, mock_channel: MagicMock, mock_sleep: MagicMock, reporter: HealthReporter) -> None:
        mock_ctx = MagicMock()
        mock_channel.return_value.__enter__.return_value = mock_ctx

        stub_mock = MagicMock()
        stub_mock.HealthEventOccurredV1.side_effect = [grpc.RpcError(), grpc.RpcError(), None]

        with patch("dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub_mock):
            result = reporter._send_with_retries(pb.HealthEvents(version=1))

        assert result is True
        assert mock_sleep.call_count == 2

    @patch("dcgm_diag.health.sleep")
    @patch("dcgm_diag.health.grpc.insecure_channel")
    def test_fails_after_max_retries(
        self, mock_channel: MagicMock, mock_sleep: MagicMock, reporter: HealthReporter
    ) -> None:
        mock_ctx = MagicMock()
        mock_channel.return_value.__enter__.return_value = mock_ctx

        stub_mock = MagicMock()
        stub_mock.HealthEventOccurredV1.side_effect = grpc.RpcError()

        with patch("dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub_mock):
            result = reporter._send_with_retries(pb.HealthEvents(version=1))

        assert result is False
        assert stub_mock.HealthEventOccurredV1.call_count == MAX_RETRIES

    @patch("dcgm_diag.health.sleep")
    @patch("dcgm_diag.health.grpc.insecure_channel")
    def test_exponential_backoff(
        self, mock_channel: MagicMock, mock_sleep: MagicMock, reporter: HealthReporter
    ) -> None:
        mock_ctx = MagicMock()
        mock_channel.return_value.__enter__.return_value = mock_ctx

        stub_mock = MagicMock()
        stub_mock.HealthEventOccurredV1.side_effect = grpc.RpcError()

        with patch("dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub_mock):
            reporter._send_with_retries(pb.HealthEvents(version=1))

        delays = [call.args[0] for call in mock_sleep.call_args_list]
        expected = INITIAL_DELAY
        for delay in delays:
            assert delay == pytest.approx(expected)
            expected *= BACKOFF_FACTOR

    @staticmethod
    def _send_with_failing_stub(reporter: HealthReporter, error: grpc.RpcError) -> tuple[bool, MagicMock]:
        """Runs one send whose every attempt raises `error`; returns (result, stub)."""
        stub = MagicMock()
        stub.HealthEventOccurredV1.side_effect = error
        with patch("dcgm_diag.health.sleep"), patch("dcgm_diag.health.grpc.insecure_channel"), patch(
            "dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            result = reporter._send_with_retries(pb.HealthEvents(version=1))
        return result, stub

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


class TestSendEvent:
    @patch.object(HealthReporter, "_send_with_retries", return_value=False)
    def test_raises_on_failure(self, mock_send: MagicMock, reporter: HealthReporter) -> None:
        with pytest.raises(RuntimeError, match="Failed to send health event"):
            reporter.send_event(gpu_uuid="GPU-0", is_healthy=False, is_fatal=True, message="Error")

    @patch.object(HealthReporter, "_send_with_retries", return_value=True)
    @patch("dcgm_diag.health.resolve_recommended_action", return_value=pb.RecommendedAction.NONE)
    @patch("dcgm_diag.health.get_error_name", return_value="DCGM_FR_XID_ERROR")
    def test_emits_event_faithfully(
        self,
        mock_name: MagicMock,
        mock_action: MagicMock,
        mock_send: MagicMock,
        reporter: HealthReporter,
    ) -> None:
        """send_event emits the given fatality and the resolved recommended action."""
        reporter.send_event(
            gpu_uuid="GPU-0",
            is_healthy=False,
            is_fatal=False,
            message="XID 13 detected",
            error_code=1234,
        )

        events = mock_send.call_args.args[0]
        event = events.events[0]
        assert event.isFatal is False
        assert event.recommendedAction == pb.RecommendedAction.NONE
        assert list(event.errorCode) == ["DCGM_FR_XID_ERROR"]

    @patch.object(HealthReporter, "_send_with_retries", return_value=True)
    def test_emits_dcgm_status_error_code_name(self, mock_send: MagicMock, reporter: HealthReporter) -> None:
        reporter.send_event(
            gpu_uuid="",
            is_healthy=False,
            is_fatal=False,
            message="DCGM_ST_IN_USE",
            error_code_name="DCGM_ST_IN_USE",
            recommended_action=pb.RecommendedAction.NONE,
        )

        events = mock_send.call_args.args[0]
        event = events.events[0]
        assert event.isFatal is False
        assert event.recommendedAction == pb.RecommendedAction.NONE
        assert list(event.errorCode) == ["DCGM_ST_IN_USE"]


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
            socket_path="unix:///var/run/nvsentinel.sock",
            node_name="test-node",
            processing_strategy=pb.ProcessingStrategy.Value("EXECUTE_REMEDIATION"),
            token_path=token_path,
        )

    @staticmethod
    def _send_with_mock_stub(reporter: HealthReporter) -> tuple[bool, MagicMock]:
        """Runs one send with the gRPC stub mocked out; returns (result, stub)."""
        stub = MagicMock()
        with patch("dcgm_diag.health.grpc.insecure_channel"), patch(
            "dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub
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
        with patch("dcgm_diag.health.grpc.insecure_channel"), patch(
            "dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            # RuntimeError, not OSError: that is the failure mode send_event
            # documents and the only one the entrypoint catches.
            with pytest.raises(RuntimeError) as raised:
                reporter._send_with_retries(pb.HealthEvents(version=1))

        assert "does-not-exist" in str(raised.value)

        stub.HealthEventOccurredV1.assert_not_called()

    @pytest.mark.parametrize("contents", [""])
    def test_blank_token_file_raises_instead_of_sending_a_blank_credential(self, tmp_path: Path, contents: str) -> None:
        """An empty file is a broken mount, not a credential."""
        token_path = self._write_token(tmp_path, contents)
        reporter = self._make_reporter(token_path=token_path)

        stub = MagicMock()
        with patch("dcgm_diag.health.grpc.insecure_channel"), patch(
            "dcgm_diag.health.pb_grpc.PlatformConnectorStub", return_value=stub
        ):
            with pytest.raises(RuntimeError) as raised:
                reporter._send_with_retries(pb.HealthEvents(version=1))

        # The message must name the file, since "Bearer " would come back as a
        # generic authentication error that says nothing about the mount.
        assert token_path in str(raised.value)
        stub.HealthEventOccurredV1.assert_not_called()
