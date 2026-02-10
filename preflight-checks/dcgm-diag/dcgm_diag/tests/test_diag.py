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

from dcgm_diag.diag import DCGMDiagnostic

from .conftest import (
    MockDCGMEntityResult,
    MockDCGMStructs,
    MockDCGMTestRun,
    dcgm_structs_mock,
)


class TestDCGMDiagnosticConnect:
    """Tests for DCGM connection handling."""

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
