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

"""Tests for ServiceChecker (systemd service health via nsenter)."""

import subprocess
from unittest.mock import patch

import pytest

from system_services_monitor.checkers.service_check import (
    ErrorCategory,
    ServiceChecker,
)


def _completed(stdout: str = "", stderr: str = "", returncode: int = 0) -> subprocess.CompletedProcess:
    """Build a fake CompletedProcess as returned by subprocess.run."""
    return subprocess.CompletedProcess(args=[], returncode=returncode, stdout=stdout, stderr=stderr)


@pytest.fixture
def checker() -> ServiceChecker:
    return ServiceChecker(flap_window=600, flap_threshold=3)


class TestCheckService:
    def test_active_service_parses_properties(self, checker: ServiceChecker) -> None:
        """systemctl show output is parsed into a populated ServiceStatus."""
        show_output = (
            "ActiveState=active\n"
            "SubState=running\n"
            "MainPID=1234\n"
            "ExecMainStartTimestamp=Wed 2026-01-01 00:00:00 UTC\n"
        )
        nrestarts_output = "NRestarts=0\n"

        # check_service calls _run_host_cmd twice: once for show, once for NRestarts
        with patch.object(
            ServiceChecker,
            "_run_host_cmd",
            side_effect=[_completed(stdout=show_output), _completed(stdout=nrestarts_output)],
        ):
            status = checker.check_service("nvidia-fabricmanager")

        assert status.name == "nvidia-fabricmanager"
        assert status.active is True
        assert status.sub_state == "running"
        assert status.main_pid == 1234
        assert status.n_restarts == 0
        assert status.error is None

    def test_inactive_service_with_failed_returncode(self, checker: ServiceChecker) -> None:
        """A non-zero returncode with no stdout surfaces as error."""
        with patch.object(
            ServiceChecker,
            "_run_host_cmd",
            return_value=_completed(returncode=1, stderr="Unit nvidia-fabricmanager.service could not be found."),
        ):
            status = checker.check_service("nvidia-fabricmanager")

        assert status.active is False
        assert status.error is not None
        assert "could not be found" in status.error

    def test_timeout_returns_error_status(self, checker: ServiceChecker) -> None:
        """subprocess.TimeoutExpired is caught and returns an error ServiceStatus."""
        with patch.object(
            ServiceChecker,
            "_run_host_cmd",
            side_effect=subprocess.TimeoutExpired(cmd=["systemctl"], timeout=10),
        ):
            status = checker.check_service("nvidia-fabricmanager")

        assert status.active is False
        assert status.error == "systemctl show timed out"


class TestFlapDetection:
    def test_flap_detection_triggers_at_threshold(self) -> None:
        """is_flapping flips True once restart count crosses the threshold."""
        c = ServiceChecker(flap_window=600, flap_threshold=3)
        # Seed history (first call only records baseline)
        c._update_flap_tracking("svc", current_restarts=0)
        assert c.is_flapping("svc") is False

        # Each new restart count increment records one timestamp
        c._update_flap_tracking("svc", current_restarts=1)
        c._update_flap_tracking("svc", current_restarts=2)
        assert c.is_flapping("svc") is False  # 2 < threshold(3)

        c._update_flap_tracking("svc", current_restarts=3)
        assert c.is_flapping("svc") is True  # 3 >= threshold(3)


class TestParseJournalErrors:
    def test_recognizes_known_error_patterns(self, checker: ServiceChecker) -> None:
        """Journal text matching pattern lists is classified into categories."""
        journal_text = (
            "fabricmanager: NVSwitch initialization failed\n"
            "fabricmanager: timed out waiting for response\n"
        )
        with patch.object(ServiceChecker, "_run_host_cmd", return_value=_completed(stdout=journal_text)):
            cats = checker._parse_journal_errors("nvidia-fabricmanager")

        # NVSWITCH_ERROR ("nvswitch"), INITIALIZATION_FAILED ("initialization failed"),
        # TIMEOUT ("timed out"), GENERAL_ERROR ("error"/"failed") all match.
        assert ErrorCategory.NVSWITCH_ERROR in cats
        assert ErrorCategory.INITIALIZATION_FAILED in cats
        assert ErrorCategory.TIMEOUT in cats
        assert ErrorCategory.GENERAL_ERROR in cats

    def test_empty_journal_returns_no_categories(self, checker: ServiceChecker) -> None:
        """A clean journal yields an empty category list."""
        with patch.object(ServiceChecker, "_run_host_cmd", return_value=_completed(stdout="")):
            cats = checker._parse_journal_errors("nvidia-fabricmanager")

        assert cats == []
