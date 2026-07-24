# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

"""Tests for FabricStateChecker (per-GPU fabric state via nvidia-smi)."""

import subprocess
from unittest.mock import patch

import pytest

from system_services_monitor.checkers.fabric_state_check import (
    FabricFailureState,
    FabricStateChecker,
    GpuFabricState,
)


def _completed(stdout: str = "", stderr: str = "", returncode: int = 0) -> subprocess.CompletedProcess:
    """Build a fake CompletedProcess as returned by subprocess.run."""
    return subprocess.CompletedProcess(args=[], returncode=returncode, stdout=stdout, stderr=stderr)


@pytest.fixture
def checker() -> FabricStateChecker:
    return FabricStateChecker()


class TestCheck:
    def test_healthy_gpus_are_parsed(self, checker: FabricStateChecker) -> None:
        """A successful nvidia-smi query yields one GpuFabricState per line (happy path)."""
        csv = "0, Completed, Success\n1, Completed, Success\n"
        with patch("subprocess.run", return_value=_completed(stdout=csv)):
            states = checker.check()

        assert len(states) == 2
        assert states[0].gpu_index == 0
        assert states[0].fabric_state == "Completed"
        assert states[0].fabric_status == "Success"

    def test_nonzero_returncode_returns_empty(self, checker: FabricStateChecker) -> None:
        """A non-zero nvidia-smi exit is logged and yields no states (failure path)."""
        with patch("subprocess.run", return_value=_completed(returncode=6, stderr="NVML error")):
            states = checker.check()

        assert states == []

    def test_timeout_returns_empty(self, checker: FabricStateChecker) -> None:
        """subprocess.TimeoutExpired is caught and yields no states."""
        with patch("subprocess.run", side_effect=subprocess.TimeoutExpired(cmd=["nvidia-smi"], timeout=15)):
            states = checker.check()

        assert states == []

    def test_missing_nvidia_smi_returns_empty(self, checker: FabricStateChecker) -> None:
        """FileNotFoundError (nvidia-smi absent) is caught and yields no states."""
        with patch("subprocess.run", side_effect=FileNotFoundError()):
            states = checker.check()

        assert states == []


class TestParseOutput:
    def test_malformed_lines_are_skipped(self, checker: FabricStateChecker) -> None:
        """Lines with too few fields or a non-int index are skipped, not fatal."""
        csv = "0, Completed, Success\ngarbage-line\nX, Completed, Success\n2, In Progress, In Progress\n"
        states = checker._parse_output(csv)

        # Only GPU 0 and GPU 2 parse cleanly.
        assert [s.gpu_index for s in states] == [0, 2]


class TestClassifyFailure:
    def test_healthy_returns_none(self, checker: FabricStateChecker) -> None:
        gpu = GpuFabricState(gpu_index=0, fabric_state="Completed", fabric_status="Success")
        assert checker.classify_failure(gpu) is None

    def test_na_returns_none(self, checker: FabricStateChecker) -> None:
        gpu = GpuFabricState(gpu_index=0, fabric_state="N/A", fabric_status="N/A")
        assert checker.classify_failure(gpu) is None

    def test_not_started_classified(self, checker: FabricStateChecker) -> None:
        gpu = GpuFabricState(gpu_index=0, fabric_state="Not Started", fabric_status="N/A")
        assert checker.classify_failure(gpu) is FabricFailureState.FM_NOT_STARTED

    def test_in_progress_classified_as_stuck(self, checker: FabricStateChecker) -> None:
        gpu = GpuFabricState(gpu_index=0, fabric_state="In Progress", fabric_status="In Progress")
        assert checker.classify_failure(gpu) is FabricFailureState.FM_REGISTRATION_STUCK

    def test_completed_with_bad_status_is_fabric_error(self, checker: FabricStateChecker) -> None:
        gpu = GpuFabricState(gpu_index=0, fabric_state="Completed", fabric_status="Failure")
        assert checker.classify_failure(gpu) is FabricFailureState.FM_FABRIC_ERROR


class TestToCheckResults:
    def test_na_gpus_are_dropped(self, checker: FabricStateChecker) -> None:
        """N/A (non-NVSwitch) GPUs produce no CheckResult."""
        statuses = [GpuFabricState(gpu_index=0, fabric_state="N/A", fabric_status="N/A")]
        results = checker.to_check_results(statuses, node_name="node-a")

        assert results == []

    def test_healthy_gpu_yields_healthy_result(self, checker: FabricStateChecker) -> None:
        statuses = [GpuFabricState(gpu_index=1, fabric_state="Completed", fabric_status="Success")]
        results = checker.to_check_results(statuses, node_name="node-a")

        assert len(results) == 1
        assert results[0].check_name == "FabricStateUnhealthy"
        assert results[0].is_healthy is True
        assert results[0].is_fatal is False

    def test_unhealthy_gpu_yields_fatal_result_with_error_code(self, checker: FabricStateChecker) -> None:
        statuses = [GpuFabricState(gpu_index=3, fabric_state="In Progress", fabric_status="In Progress")]
        results = checker.to_check_results(statuses, node_name="node-a")

        assert len(results) == 1
        assert results[0].is_healthy is False
        assert results[0].is_fatal is True
        assert results[0].error_codes == [FabricFailureState.FM_REGISTRATION_STUCK.value]
        assert results[0].entities_impacted == [{"entityType": "GPU", "entityValue": "3"}]
