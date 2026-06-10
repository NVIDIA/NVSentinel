"""Tests for service_check module."""

import subprocess
import time
from collections import deque
from unittest.mock import patch, MagicMock

import sys
import os

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

    def _mock_systemctl_output(self, active="active", sub="running", pid=1234, restarts=0):
        """Build a mock systemctl show output string."""
        return (
            f"ActiveState={active}\n"
            f"SubState={sub}\n"
            f"MainPID={pid}\n"
            f"NRestarts={restarts}\n"
            f"ExecMainStartTimestamp=Thu 2026-01-15 20:00:00 UTC\n"
        )

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_active(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout=self._mock_systemctl_output(active="active", sub="running"),
            stderr="",
        )
        checker = ServiceChecker()
        status = checker.check_service("nvidia-fabricmanager")

        assert status.active is True
        assert status.sub_state == "running"
        assert status.main_pid == 1234
        assert status.error is None

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_failed(self, mock_run):
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
    def test_check_service_timeout(self, mock_run):
        mock_run.side_effect = subprocess.TimeoutExpired(cmd="", timeout=10)

        checker = ServiceChecker()
        status = checker.check_service("nvidia-fabricmanager")

        assert status.active is False
        assert status.error == "systemctl show timed out"

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_service_command_failure(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=4,
            stdout="",
            stderr="Unit not found",
        )
        checker = ServiceChecker()
        status = checker.check_service("nonexistent-service")

        assert status.active is False
        assert "Unit not found" in status.error

    def test_flap_detection_no_flap(self):
        checker = ServiceChecker(flap_window=600, flap_threshold=3)
        # Simulate 2 restarts (below threshold)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 0

        checker._update_flap_tracking("test", 2)
        assert not checker.is_flapping("test")

    def test_flap_detection_triggers(self):
        checker = ServiceChecker(flap_window=600, flap_threshold=3)
        checker._restart_history["test"] = deque()
        checker._last_restart_count["test"] = 0

        # Simulate 3 restarts (at threshold)
        checker._update_flap_tracking("test", 3)
        assert checker.is_flapping("test")

    def test_flap_detection_window_expiry(self):
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

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_fabric_manager_with_journal_errors(self, mock_run):
        def side_effect(cmd, timeout=10):
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

    @patch("checks.service_check.ServiceChecker._run_host_cmd")
    def test_check_all_gpu_services(self, mock_run):
        mock_run.return_value = subprocess.CompletedProcess(
            args=[], returncode=0,
            stdout=self._mock_systemctl_output(active="active", sub="running"),
            stderr="",
        )
        checker = ServiceChecker()
        results = checker.check_all_gpu_services([
            "nvidia-fabricmanager",
            "nvidia-persistenced",
            "nv-hostengine",
        ])

        assert len(results) == 3
        for name, status in results.items():
            assert status.active is True
