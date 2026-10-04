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

"""Unit tests for dcgm_diag/diag.py"""

from unittest.mock import MagicMock, patch

import pytest

from dcgm_diag.diag import DCGMDiagnostic, DiagnosticStatusError, get_dcgm_status_name

from .conftest import (
    MockDCGMEntityResult,
    MockDCGMStructs,
    MockDCGMTestRun,
    dcgm_structs_mock,
)


class MockDCGMError(Exception):
    def __init__(self, value: int, message: str = "") -> None:
        super().__init__(message)
        self.value = value


class TestDCGMDiagnosticConnect:
    """Tests for DCGM connection handling."""

    @pytest.mark.parametrize("max_attempts", [0, -1, 1.5, "3"])
    @patch("dcgm_diag.diag.GPUDiscovery")
    def test_rejects_invalid_status_retry_max_attempts(
        self, mock_gpu_discovery_class: MagicMock, max_attempts: object
    ) -> None:
        with pytest.raises(ValueError, match="status_retry_max_attempts must be an integer >= 1"):
            DCGMDiagnostic(hostengine_addr="localhost:5555", status_retry_max_attempts=max_attempts)

    @pytest.mark.parametrize("interval_seconds", [0, -1, "10"])
    @patch("dcgm_diag.diag.GPUDiscovery")
    def test_rejects_invalid_status_retry_interval(
        self, mock_gpu_discovery_class: MagicMock, interval_seconds: object
    ) -> None:
        with pytest.raises(ValueError, match="status_retry_interval_seconds must be a positive number"):
            DCGMDiagnostic(hostengine_addr="localhost:5555", status_retry_interval_seconds=interval_seconds)

    @patch("dcgm_diag.diag.GPUDiscovery")
    def test_disconnect_handles_shutdown_exception(self, mock_gpu_discovery_class: MagicMock) -> None:
        """Disconnect should handle shutdown exceptions gracefully."""
        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")
        diag._handle = MagicMock()
        diag._handle.Shutdown.side_effect = Exception("Connection lost")

        # Should not raise
        diag._disconnect()
        assert diag._handle is None


class TestDCGMDiagnosticParseResponse:
    """Tests for diagnostic response parsing - the core logic."""

    @patch("dcgm_diag.diag.GPUDiscovery")
    def test_extracts_error_code_and_message_from_errors_array(self, mock_gpu_discovery_class: MagicMock) -> None:
        """Verify error code and message are correctly extracted from response.errors."""
        mock_discovery = MagicMock()
        mock_discovery.get_uuid.side_effect = lambda idx: f"GPU-uuid-{idx}"
        mock_gpu_discovery_class.return_value = mock_discovery

        response = MockDCGMStructs.c_dcgmDiagResponse_v12()
        response.numTests = 1
        response.numResults = 1
        response.numErrors = 1
        response.tests = [MockDCGMTestRun(name="memory", num_results=1, result_indices=[0])]
        response.results = [
            MockDCGMEntityResult(entity_id=0, result=dcgm_structs_mock.DCGM_DIAG_RESULT_FAIL, test_id=0)
        ]
        response.errors = [MagicMock(testId=0, entity=MagicMock(entityId=0), code=123, msg=b"ECC error detected")]

        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")
        results = diag._parse_response(response, [0])

        assert len(results) == 1
        assert results[0].error_code == 123
        assert results[0].error_message == "ECC error detected"
        assert results[0].status == "fail"

    @patch("dcgm_diag.diag.GPUDiscovery")
    def test_filters_results_to_requested_gpu_indices_only(self, mock_gpu_discovery_class: MagicMock) -> None:
        """Only GPUs in the requested list should appear in results."""
        mock_discovery = MagicMock()
        mock_discovery.get_uuid.side_effect = lambda idx: f"GPU-uuid-{idx}"
        mock_gpu_discovery_class.return_value = mock_discovery

        response = MockDCGMStructs.c_dcgmDiagResponse_v12()
        response.numTests = 1
        response.numResults = 4
        response.numErrors = 0
        response.tests = [MockDCGMTestRun(name="test", num_results=4, result_indices=[0, 1, 2, 3])]
        response.results = [
            MockDCGMEntityResult(entity_id=i, result=dcgm_structs_mock.DCGM_DIAG_RESULT_PASS, test_id=0)
            for i in range(4)
        ]
        response.errors = []

        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")

        # Only request GPUs 0 and 2 (not 1 and 3)
        results = diag._parse_response(response, [0, 2])

        assert len(results) == 2
        gpu_indices = {r.gpu_index for r in results}
        assert gpu_indices == {0, 2}

    @patch("dcgm_diag.diag.GPUDiscovery")
    def test_handles_multiple_tests_with_different_statuses(self, mock_gpu_discovery_class: MagicMock) -> None:
        """Verify correct handling of multiple tests with pass/warn/fail."""
        mock_discovery = MagicMock()
        mock_discovery.get_uuid.side_effect = lambda idx: f"GPU-uuid-{idx}"
        mock_gpu_discovery_class.return_value = mock_discovery

        response = MockDCGMStructs.c_dcgmDiagResponse_v12()
        response.numTests = 2
        response.numResults = 2
        response.numErrors = 1
        response.tests = [
            MockDCGMTestRun(name="memory", num_results=1, result_indices=[0]),
            MockDCGMTestRun(name="pcie", num_results=1, result_indices=[1]),
        ]
        response.results = [
            MockDCGMEntityResult(entity_id=0, result=dcgm_structs_mock.DCGM_DIAG_RESULT_PASS, test_id=0),
            MockDCGMEntityResult(entity_id=0, result=dcgm_structs_mock.DCGM_DIAG_RESULT_WARN, test_id=1),
        ]
        response.errors = [MagicMock(testId=1, entity=MagicMock(entityId=0), code=50, msg=b"PCIe warning")]

        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")
        results = diag._parse_response(response, [0])

        assert len(results) == 2
        memory_result = next(r for r in results if r.test_name == "memory")
        pcie_result = next(r for r in results if r.test_name == "pcie")

        assert memory_result.status == "pass"
        assert memory_result.error_code == 0

        assert pcie_result.status == "warn"
        assert pcie_result.error_code == 50


class TestDCGMDiagnosticDecodeString:
    """Tests for string decoding - handles various input types."""

    def test_strips_null_bytes_from_c_strings(self) -> None:
        """C strings often have trailing null bytes that should be stripped."""
        result = DCGMDiagnostic._decode_string(b"test\x00\x00\x00")
        assert result == "test"

    def test_handles_none_gracefully(self) -> None:
        """None input should return empty string, not crash."""
        result = DCGMDiagnostic._decode_string(None)
        assert result == ""


class TestDCGMDiagnosticStatusToString:
    """Tests for status code conversion."""

    def test_unknown_status_returns_unknown_string(self) -> None:
        """Unknown status codes should return 'unknown', not crash."""
        result = DCGMDiagnostic._status_to_string(999)
        assert result == "unknown"


class TestDCGMDiagnosticRun:
    """Tests for the full diagnostic run flow."""

    @patch("dcgm_diag.diag.GPUDiscovery")
    @patch("dcgm_diag.diag.pydcgm.DcgmHandle")
    @patch("dcgm_diag.diag.pydcgm.DcgmGroup")
    def test_raises_when_no_gpus_allocated(
        self,
        mock_group_class: MagicMock,
        mock_handle_class: MagicMock,
        mock_gpu_discovery_class: MagicMock,
    ) -> None:
        """Should raise RuntimeError if no GPUs are allocated to container."""
        mock_discovery = MagicMock()
        mock_discovery.get_allocated_gpus.return_value = []
        mock_gpu_discovery_class.return_value = mock_discovery

        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")

        with pytest.raises(RuntimeError, match="No GPUs allocated"):
            diag.run(level=1)

    @patch("dcgm_diag.diag.GPUDiscovery")
    @patch("dcgm_diag.diag.pydcgm.DcgmHandle")
    @patch("dcgm_diag.diag.pydcgm.DcgmGroup")
    def test_cleans_up_group_even_on_diagnostic_failure(
        self,
        mock_group_class: MagicMock,
        mock_handle_class: MagicMock,
        mock_gpu_discovery_class: MagicMock,
    ) -> None:
        """Group.Delete() should be called even if RunDiagnostic raises."""
        mock_discovery = MagicMock()
        mock_discovery.get_allocated_gpus.return_value = [0]
        mock_gpu_discovery_class.return_value = mock_discovery

        mock_group = MagicMock()
        mock_group.action.RunDiagnostic.side_effect = Exception("DCGM error")
        mock_group_class.return_value = mock_group

        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")

        with pytest.raises(Exception, match="DCGM error"):
            diag.run(level=1)

        # Verify cleanup happened
        mock_group.Delete.assert_called_once()

    @patch("dcgm_diag.diag.GPUDiscovery")
    @patch("dcgm_diag.diag.pydcgm.DcgmGroup")
    def test_creates_group_with_all_allocated_gpus(
        self, mock_group_class: MagicMock, mock_gpu_discovery_class: MagicMock
    ) -> None:
        """GPU group should include all allocated GPUs."""
        mock_discovery = MagicMock()
        mock_discovery.get_allocated_gpus.return_value = [0, 1, 2]
        mock_gpu_discovery_class.return_value = mock_discovery

        mock_group = MagicMock()
        mock_group_class.return_value = mock_group

        diag = DCGMDiagnostic(hostengine_addr="localhost:5555")
        diag._handle = MagicMock()
        diag._create_gpu_group([0, 1, 2])

        # Verify all GPUs were added
        assert mock_group.AddGpu.call_count == 3
        mock_group.AddGpu.assert_any_call(0)
        mock_group.AddGpu.assert_any_call(1)
        mock_group.AddGpu.assert_any_call(2)

    @patch("dcgm_diag.diag.sleep")
    @patch("dcgm_diag.diag.dcgm_agent.dcgmStopDiagnostic")
    @patch("dcgm_diag.diag.GPUDiscovery")
    @patch("dcgm_diag.diag.pydcgm.DcgmGroup")
    def test_retries_when_diagnostic_resource_is_in_use(
        self,
        mock_group_class: MagicMock,
        mock_gpu_discovery_class: MagicMock,
        mock_stop_diagnostic: MagicMock,
        mock_sleep: MagicMock,
    ) -> None:
        """Resource-in-use errors should wait and retry until the diagnostic can run."""
        mock_discovery = MagicMock()
        mock_discovery.get_allocated_gpus.return_value = [0]
        mock_gpu_discovery_class.return_value = mock_discovery

        response = MockDCGMStructs.c_dcgmDiagResponse_v12()
        mock_group = MagicMock()
        mock_group.action.RunDiagnostic.side_effect = [
            MockDCGMError(
                dcgm_structs_mock.DCGM_ST_IN_USE,
                "The requested operation could not be completed because the affected resource is in use",
            ),
            response,
        ]
        mock_group_class.return_value = mock_group

        diag = DCGMDiagnostic(
            hostengine_addr="localhost:5555",
            status_retry_max_attempts=2,
            status_retry_interval_seconds=2,
        )
        diag._handle = MagicMock()

        results = diag._run_diagnostic(level=1)

        assert results == []
        assert mock_group.action.RunDiagnostic.call_count == 2
        mock_stop_diagnostic.assert_not_called()
        mock_sleep.assert_called_once_with(2)

    @patch("dcgm_diag.diag.sleep")
    @patch("dcgm_diag.diag.dcgm_agent.dcgmStopDiagnostic")
    @patch("dcgm_diag.diag.GPUDiscovery")
    @patch("dcgm_diag.diag.pydcgm.DcgmGroup")
    def test_raises_status_error_after_retry_timeout(
        self,
        mock_group_class: MagicMock,
        mock_gpu_discovery_class: MagicMock,
        mock_stop_diagnostic: MagicMock,
        mock_sleep: MagicMock,
    ) -> None:
        """Persistent DCGM_ST_* errors should be classified distinctly."""
        mock_discovery = MagicMock()
        mock_discovery.get_allocated_gpus.return_value = [0]
        mock_gpu_discovery_class.return_value = mock_discovery

        mock_group = MagicMock()
        mock_group.action.RunDiagnostic.side_effect = MockDCGMError(dcgm_structs_mock.DCGM_ST_TIMEOUT, "Timeout")
        mock_group_class.return_value = mock_group

        diag = DCGMDiagnostic(
            hostengine_addr="localhost:5555",
            status_retry_max_attempts=4,
            status_retry_interval_seconds=2,
        )
        diag._handle = MagicMock()

        with pytest.raises(DiagnosticStatusError, match="DCGM_ST_TIMEOUT") as exc_info:
            diag._run_diagnostic(level=1)

        assert exc_info.value.status_name == "DCGM_ST_TIMEOUT"
        assert (
            str(exc_info.value)
            == "DCGM diagnostic returned DCGM_ST_TIMEOUT after 4 attempts; diagnostic did not complete"
        )
        assert "\n" not in str(exc_info.value)
        assert mock_group.action.RunDiagnostic.call_count == 4
        mock_stop_diagnostic.assert_not_called()
        assert mock_sleep.call_count == 3
        assert [call.args[0] for call in mock_sleep.call_args_list] == [2, 2, 2]

    @patch("dcgm_diag.diag.dcgm_agent.dcgmStopDiagnostic")
    @patch("dcgm_diag.diag.GPUDiscovery")
    @patch("dcgm_diag.diag.pydcgm.DcgmGroup")
    def test_stops_stale_diagnostic_then_runs_after_retry_timeout(
        self,
        mock_group_class: MagicMock,
        mock_gpu_discovery_class: MagicMock,
        mock_stop_diagnostic: MagicMock,
    ) -> None:
        """After the wait budget is exhausted, stop the stale diagnostic and try to run."""
        mock_discovery = MagicMock()
        mock_discovery.get_allocated_gpus.return_value = [0]
        mock_gpu_discovery_class.return_value = mock_discovery

        response = MockDCGMStructs.c_dcgmDiagResponse_v12()
        mock_group = MagicMock()
        mock_group.action.RunDiagnostic.side_effect = [
            MockDCGMError(dcgm_structs_mock.DCGM_ST_DIAG_ALREADY_RUNNING, "A diag instance is already running"),
            response,
        ]
        mock_group_class.return_value = mock_group

        diag = DCGMDiagnostic(
            hostengine_addr="localhost:5555",
            status_retry_max_attempts=1,
            status_retry_interval_seconds=2,
        )
        diag._handle = MagicMock()

        results = diag._run_diagnostic(level=1)

        assert results == []
        assert mock_group.action.RunDiagnostic.call_count == 2
        mock_stop_diagnostic.assert_called_once()


class TestDCGMStatusDetection:
    def test_detects_dcgm_status_from_exception_value(self) -> None:
        err = MockDCGMError(dcgm_structs_mock.DCGM_ST_IN_USE)

        assert get_dcgm_status_name(err) == "DCGM_ST_IN_USE"

    @pytest.mark.parametrize(
        "err",
        [
            Exception("DCGM_ST_DIAG_ALREADY_RUNNING"),
            Exception("The requested operation could not be completed because the affected resource is in use"),
            Exception("DCGM hostengine connection failed"),
        ],
    )
    def test_ignores_untyped_errors(self, err: Exception) -> None:
        assert get_dcgm_status_name(err) == ""


class TestDCGMDiagnosticConnect:
    """Tests for hostengine address fallback."""

    @patch("dcgm_diag.diag.pydcgm.DcgmHandle")
    def test_connect_falls_back_to_next_listed_address(self, mock_handle_class: MagicMock) -> None:
        handle = MagicMock()
        mock_handle_class.side_effect = [Exception("no such host"), handle]

        diag = DCGMDiagnostic(hostengine_addr="nvidia-dcgm-dra.gpu-operator.svc:5555,nvidia-dcgm.gpu-operator.svc:5555")
        diag._connect()

        assert diag._handle is handle
        assert [c.kwargs["ipAddress"] for c in mock_handle_class.call_args_list] == [
            "nvidia-dcgm-dra.gpu-operator.svc:5555",
            "nvidia-dcgm.gpu-operator.svc:5555",
        ]

    @patch("dcgm_diag.diag.pydcgm.DcgmHandle")
    def test_connect_raises_when_no_address_works(self, mock_handle_class: MagicMock) -> None:
        mock_handle_class.side_effect = Exception("no such host")

        diag = DCGMDiagnostic(hostengine_addr="a:5555,b:5555")
        with pytest.raises(RuntimeError, match="Unable to connect to any DCGM hostengine: a:5555.*b:5555"):
            diag._connect()
