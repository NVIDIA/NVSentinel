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

"""Systemd service health checks for Fabric Manager and GPU services.

Uses nsenter to inspect host systemd services from within a container.
Includes flap detection (rapid restart cycling) and journal error parsing.
NRestarts is queried in a separate systemctl call for compatibility with
older systemd versions that do not support it in a combined --property list.
"""

import logging as log
import subprocess
import time
from collections import deque
from dataclasses import dataclass, field
from enum import Enum
from typing import Dict, List, Optional

from .types import CheckResult


class ErrorCategory(Enum):
    NVSWITCH_ERROR = "nvswitch_error"
    INITIALIZATION_FAILED = "initialization_failed"
    TIMEOUT = "timeout"
    GENERAL_ERROR = "general_error"


# Journal patterns that indicate specific failure modes
_ERROR_PATTERNS = {
    ErrorCategory.NVSWITCH_ERROR: [
        "nvswitch",
        "NVSwitch",
        "fabric error",
    ],
    ErrorCategory.INITIALIZATION_FAILED: [
        "initialization failed",
        "failed to initialize",
        "Init Failed",
        "unable to start",
    ],
    ErrorCategory.TIMEOUT: [
        "timed out",
        "timeout",
        "deadline exceeded",
    ],
    ErrorCategory.GENERAL_ERROR: [
        "error",
        "fatal",
        "failed",
    ],
}


@dataclass
class ServiceStatus:
    """Result of a single systemd service check."""

    name: str
    active: bool  # True if ActiveState == "active"
    sub_state: str = ""  # e.g. "running", "dead", "failed"
    main_pid: int = 0
    n_restarts: int = 0
    start_timestamp: str = ""
    error: Optional[str] = None  # non-None if the check itself failed


@dataclass
class FabricManagerStatus(ServiceStatus):
    """Extended status for Fabric Manager with journal analysis."""

    journal_errors: List[ErrorCategory] = field(default_factory=list)
    flapping: bool = False


class ServiceChecker:
    """Checks host systemd services via nsenter."""

    # Additional GPU-related services to check alongside Fabric Manager.
    # nv-hostengine (DCGM) is intentionally excluded -- GpuDcgmConnectivityFailure
    # in gpu-health-monitor already covers DCGM service health.
    DEFAULT_GPU_SERVICES = [
        "nvidia-persistenced",
    ]

    def __init__(self, flap_window: int = 600, flap_threshold: int = 3):
        self._flap_window = flap_window
        self._flap_threshold = flap_threshold
        # Track restart timestamps per service for flap detection
        self._restart_history: Dict[str, deque] = {}
        # Track last-seen restart count to detect new restarts
        self._last_restart_count: Dict[str, int] = {}

    def _run_host_cmd(self, cmd: List[str], timeout: int = 10) -> subprocess.CompletedProcess:
        """Run a command on the host via nsenter into PID 1's mount namespace."""
        full_cmd = ["nsenter", "-t", "1", "-m", "--"] + cmd
        return subprocess.run(
            full_cmd,
            capture_output=True,
            text=True,
            timeout=timeout,
        )

    def check_service(self, service_name: str) -> ServiceStatus:
        """Check a single systemd service via nsenter.

        Parses systemctl show output for ActiveState, SubState, MainPID,
        and ExecMainStartTimestamp. NRestarts is queried separately since
        older systemd versions don't support it in a combined property list.
        """
        try:
            result = self._run_host_cmd([
                "systemctl",
                "show",
                service_name,
                "--property=ActiveState,SubState,MainPID,ExecMainStartTimestamp",
            ])

            if result.returncode != 0 and not result.stdout.strip():
                return ServiceStatus(
                    name=service_name,
                    active=False,
                    error=f"systemctl show failed: {result.stderr.strip()}",
                )

            props = {}
            for line in result.stdout.strip().splitlines():
                if "=" in line:
                    key, _, value = line.partition("=")
                    props[key.strip()] = value.strip()

            active_state = props.get("ActiveState", "unknown")

            # NRestarts isn't available on older systemd; query separately
            n_restarts = self._get_restart_count(service_name)

            # Flap detection
            self._update_flap_tracking(service_name, n_restarts)

            return ServiceStatus(
                name=service_name,
                active=(active_state == "active"),
                sub_state=props.get("SubState", ""),
                main_pid=int(props.get("MainPID", "0")),
                n_restarts=n_restarts,
                start_timestamp=props.get("ExecMainStartTimestamp", ""),
            )

        except subprocess.TimeoutExpired:
            return ServiceStatus(
                name=service_name,
                active=False,
                error="systemctl show timed out",
            )
        except Exception as e:
            return ServiceStatus(
                name=service_name,
                active=False,
                error=str(e),
            )

    def _get_restart_count(self, service_name: str) -> int:
        """Get NRestarts from systemd, returning 0 if unsupported."""
        try:
            result = self._run_host_cmd([
                "systemctl",
                "show",
                service_name,
                "--property=NRestarts",
            ])
            if result.returncode == 0 and result.stdout.strip():
                _, _, val = result.stdout.strip().partition("=")
                return int(val)
        except Exception:
            pass
        return 0

    def _update_flap_tracking(self, service_name: str, current_restarts: int) -> None:
        """Track restart events for flap detection."""
        if service_name not in self._restart_history:
            self._restart_history[service_name] = deque()
            self._last_restart_count[service_name] = current_restarts
            return

        last_count = self._last_restart_count[service_name]
        if current_restarts > last_count:
            # New restarts detected -- record timestamp for each
            now = time.monotonic()
            for _ in range(current_restarts - last_count):
                self._restart_history[service_name].append(now)
            self._last_restart_count[service_name] = current_restarts

        # Prune entries outside the flap window
        cutoff = time.monotonic() - self._flap_window
        history = self._restart_history[service_name]
        while history and history[0] < cutoff:
            history.popleft()

    def is_flapping(self, service_name: str) -> bool:
        """Return True if the service has restarted too many times within the window."""
        history = self._restart_history.get(service_name, deque())
        return len(history) >= self._flap_threshold

    def check_fabric_manager(self) -> FabricManagerStatus:
        """Check Fabric Manager with journal error analysis."""
        base = self.check_service("nvidia-fabricmanager")

        journal_errors = self._parse_journal_errors("nvidia-fabricmanager")
        flapping = self.is_flapping("nvidia-fabricmanager")

        return FabricManagerStatus(
            name=base.name,
            active=base.active,
            sub_state=base.sub_state,
            main_pid=base.main_pid,
            n_restarts=base.n_restarts,
            start_timestamp=base.start_timestamp,
            error=base.error,
            journal_errors=journal_errors,
            flapping=flapping,
        )

    def _parse_journal_errors(self, service_name: str) -> List[ErrorCategory]:
        """Scan recent journal entries for known error patterns."""
        try:
            result = self._run_host_cmd(
                [
                    "journalctl",
                    "-u",
                    service_name,
                    "--since",
                    "5 minutes ago",
                    "--no-pager",
                    "-q",
                ],
                timeout=15,
            )

            if result.returncode != 0 or not result.stdout.strip():
                return []

            found: List[ErrorCategory] = []
            text = result.stdout.lower()
            for category, patterns in _ERROR_PATTERNS.items():
                if any(p.lower() in text for p in patterns):
                    found.append(category)

            return found

        except (subprocess.TimeoutExpired, Exception) as e:
            log.warning(f"Journal parsing failed for {service_name}: {e}")
            return []

    def check_all_gpu_services(self, service_names: Optional[List[str]] = None) -> Dict[str, ServiceStatus]:
        """Check all configured GPU services."""
        if service_names is None:
            service_names = self.DEFAULT_GPU_SERVICES
        results = {}
        for name in service_names:
            results[name] = self.check_service(name)
        return results
