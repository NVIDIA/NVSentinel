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

from unittest.mock import MagicMock, patch

import grpc
import pytest

from dcgm_diag.health import BACKOFF_FACTOR, INITIAL_DELAY, MAX_RETRIES, HealthReporter
from dcgm_diag.protos import health_event_pb2 as pb


@pytest.fixture
def reporter() -> HealthReporter:
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


class TestSendEvent:
    @patch.object(HealthReporter, "_send_with_retries", return_value=False)
    def test_raises_on_failure(self, mock_send: MagicMock, reporter: HealthReporter) -> None:
        with pytest.raises(RuntimeError, match="Failed to send health event"):
            reporter.send_event(gpu_uuid="GPU-0", is_healthy=False, is_fatal=True, message="Error")
