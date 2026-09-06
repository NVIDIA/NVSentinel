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

"""Tests for service_check module."""

import os
import subprocess
import sys
import time
from collections import deque
from typing import List
from unittest.mock import MagicMock, patch

# Add parent directory to path for imports
sys.path.insert(0, os.path.join(os.path.dirname(__file__), ".."))

from checks.service_check import (
    ServiceChecker,
    ServiceStatus,
    FabricManagerStatus,
    ErrorCategory,
)


class TestServiceChecker:
    """Tests for ServiceChecker."""

    def _mock_systemctl_output(self, active: str = "active", sub: str = "running",
                               pid: int = 1234, restarts: int = 0,
                               load: str = "loaded") -> str:
        """Build a mock systemctl show output string.

        Note: tests that mock _run_host_cmd with this full block feed it to
        BOTH systemctl calls, including the NRestarts-only query — whose
        parser then fails int() on the multi-line payload and reports
        n_restarts=None (unobserved). Restart-count-specific tests must mock
        the NRestarts call separately (see test_restart_count_parsed).
        """
        return (
            f"LoadState={load}\n"
            f"ActiveState={active}\n"
            f"SubState={sub}\n"
            f"MainPID={pid}\n"
            f"NRestarts={restarts}\n"
            f"ExecMainStartTimestamp=Thu 2026-01-15 20:00:00 UTC\n"
        )

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_active(self, mock_run: MagicMock) -> None:
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout=self._mock_systemctl_output(active="active", sub="running"),
            stderr="",
        )
        checker = ServiceChecker()
        status = checker.check_service("nvidia-fabricmanager")

        assert status.active is True
        assert status.load_state == "loaded"
        assert status.sub_state == "running"
        assert status.main_pid == 1234
        assert status.error is None

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_failed(self, mock_run: MagicMock) -> None:
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout=self._mock_systemctl_output(active="failed", sub="failed"),
            stderr="",
        )
        checker = ServiceChecker()
        status = checker.check_service("nvidia-fabricmanager")

        assert status.active is False
        assert status.sub_state == "failed"

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_not_found_preserves_load_state(self, mock_run: MagicMock) -> None:
        """A unit absent on the host must stay distinguishable from a loaded
        but stopped unit (a missing unit is not a *_NOT_RUNNING failure)."""
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout=self._mock_systemctl_output(
                active="inactive", sub="dead", pid=0, load="not-found"),
            stderr="",
        )
        checker = ServiceChecker()
        status = checker.check_service("nvidia-fabricmanager")

        assert status.active is False
        assert status.load_state == "not-found"
        assert status.error is None

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_timeout(self, mock_run: MagicMock) -> None:
        mock_run.side_effect = subprocess.TimeoutExpired(cmd="", timeout=10)

        checker = ServiceChecker()
        status = checker.check_service("nvidia-fabricmanager")

        assert status.active is False
        assert status.error == "systemctl show timed out"

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_command_failure(self, mock_run: MagicMock) -> None:
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=4,
            stdout="",
            stderr="Unit not found",
        )
        checker = ServiceChecker()
        status = checker.check_service("nonexistent-service")

        assert status.active is False
        assert "Unit not found" in status.error

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_restart_count_unobserved_is_none(self, mock_run: MagicMock) -> None:
        """A failed NRestarts probe must be None, not a fake 0."""
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=1, stdout="", stderr="boom",
        )
        checker = ServiceChecker()
        assert checker._get_restart_count("svc") is None

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_restart_count_parsed(self, mock_run: MagicMock) -> None:
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0, stdout="NRestarts=4\n", stderr="",
        )
        checker = ServiceChecker()
        assert checker._get_restart_count("svc") == 4

    def test_flap_detection_no_flap(self) -> None:
        checker = ServiceChecker(flap_window=600, flap_threshold=3)
        # Simulate 2 restarts (below threshold)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 0

        checker._update_flap_tracking("test", 2)
        assert not checker.is_flapping("test")

    def test_flap_detection_triggers(self) -> None:
        checker = ServiceChecker(flap_window=600, flap_threshold=3)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 0

        # Simulate 3 restarts (at threshold)
        checker._update_flap_tracking("test", 3)
        assert checker.is_flapping("test")

    def test_flap_detection_window_expiry(self) -> None:
        checker = ServiceChecker(flap_window=1, flap_threshold=3)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 0

        # Add restarts
        checker._update_flap_tracking("test", 3)
        assert checker.is_flapping("test")

        # Wait for window to expire
        time.sleep(1.1)
        checker._update_flap_tracking("test", 3)  # same count, triggers prune
        assert not checker.is_flapping("test")

    def test_flap_tracking_ignores_unobserved(self) -> None:
        """None (probe failed) must not move the baseline or add samples."""
        checker = ServiceChecker(flap_window=600, flap_threshold=3)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 5

        checker._update_flap_tracking("test", None)
        assert checker._last_restart_count["test"] == 5
        assert len(checker._restart_history["test"]) == 0

    def test_flap_tracking_rebaselines_on_counter_reset(self) -> None:
        """NRestarts is not monotonic (reset-failed / reboot); a decrease
        re-baselines instead of going quiet until the old high-water mark."""
        checker = ServiceChecker(flap_window=600, flap_threshold=3)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 5

        checker._update_flap_tracking("test", 1)
        assert checker._last_restart_count["test"] == 1
        # The reset itself is recorded as one restart observation.
        assert len(checker._restart_history["test"]) == 1

        # Counting resumes from the new baseline immediately.
        checker._update_flap_tracking("test", 3)
        assert len(checker._restart_history["test"]) == 3
        assert checker.is_flapping("test")

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_fabric_manager_with_journal_errors(self, mock_run: MagicMock) -> None:
        def side_effect(cmd: List[str], timeout: int = 10) -> subprocess.CompletedProcess:
            if "systemctl" in cmd:
                return subprocess.CompletedProcess(
                    args=[], returncode=0,
                    stdout=self._mock_systemctl_output(active="failed", sub="failed"),
                    stderr="",
                )
            elif "journalctl" in cmd:
                return subprocess.CompletedProcess(
                    args=[], returncode=0,
                    stdout="Jan 15 20:00:00 node1 fabricmanager: NVSwitch fatal error detected\n",
                    stderr="",
                )
            return subprocess.CompletedProcess(args=[], returncode=1, stdout="", stderr="")

        mock_run.side_effect = side_effect
        checker = ServiceChecker()
        status = checker.check_fabric_manager()

        assert isinstance(status, FabricManagerStatus)
        assert status.active is False
        assert ErrorCategory.NVSWITCH_ERROR in status.journal_errors
        assert status.journal_probe_failed is False

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_journal_probe_failure_is_not_a_clean_journal(self, mock_run: MagicMock) -> None:
        """A failed journal probe must be reported as UNKNOWN, not as
        'no errors found'."""
        def side_effect(cmd: List[str], timeout: int = 10) -> subprocess.CompletedProcess:
            if "systemctl" in cmd:
                return subprocess.CompletedProcess(
                    args=[], returncode=0,
                    stdout=self._mock_systemctl_output(),
                    stderr="",
                )
            elif "journalctl" in cmd:
                return subprocess.CompletedProcess(
                    args=[], returncode=1, stdout="",
                    stderr="Failed to open journal",
                )
            return subprocess.CompletedProcess(args=[], returncode=1, stdout="", stderr="")

        mock_run.side_effect = side_effect
        checker = ServiceChecker()
        status = checker.check_fabric_manager()

        assert status.journal_probe_failed is True
        assert status.journal_errors == []

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_journal_probe_timeout_is_not_a_clean_journal(self, mock_run: MagicMock) -> None:
        def side_effect(cmd: List[str], timeout: int = 10) -> subprocess.CompletedProcess:
            if "systemctl" in cmd:
                return subprocess.CompletedProcess(
                    args=[], returncode=0,
                    stdout=self._mock_systemctl_output(),
                    stderr="",
                )
            raise subprocess.TimeoutExpired(cmd="journalctl", timeout=15)

        mock_run.side_effect = side_effect
        checker = ServiceChecker()
        status = checker.check_fabric_manager()

        assert status.journal_probe_failed is True

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_journal_probe_skipped_when_unit_absent(self, mock_run: MagicMock) -> None:
        """No journalctl fork for a unit that is absent on the host."""
        def side_effect(cmd: List[str], timeout: int = 10) -> subprocess.CompletedProcess:
            assert "journalctl" not in cmd, "journal probe must be skipped"
            return subprocess.CompletedProcess(
                args=[], returncode=0,
                stdout=self._mock_systemctl_output(
                    active="inactive", sub="dead", pid=0, load="not-found"),
                stderr="",
            )

        mock_run.side_effect = side_effect
        checker = ServiceChecker()
        status = checker.check_fabric_manager()

        assert status.load_state == "not-found"
        assert status.journal_probe_failed is False
        assert status.journal_errors == []

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_journal_clean_is_empty_not_failed(self, mock_run: MagicMock) -> None:
        def side_effect(cmd: List[str], timeout: int = 10) -> subprocess.CompletedProcess:
            if "systemctl" in cmd:
                return subprocess.CompletedProcess(
                    args=[], returncode=0,
                    stdout=self._mock_systemctl_output(),
                    stderr="",
                )
            return subprocess.CompletedProcess(args=[], returncode=0, stdout="", stderr="")

        mock_run.side_effect = side_effect
        checker = ServiceChecker()
        status = checker.check_fabric_manager()

        assert status.journal_probe_failed is False
        assert status.journal_errors == []

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_all_gpu_services(self, mock_run: MagicMock) -> None:
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout=self._mock_systemctl_output(active="active", sub="running"),
            stderr="",
        )
        checker = ServiceChecker()
        results = checker.check_all_gpu_services([
            "nvidia-persistenced",
            "nvidia-imex",
        ])

        assert len(results) == 2
        for name, status in results.items():
            assert status.active is True
