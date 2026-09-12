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

"""Checks 1 & 2: Systemd service health for Fabric Manager and GPU services.

Uses nsenter to inspect host systemd services from within a container.
Includes flap detection (rapid restart cycling) and journal error parsing.
NRestarts is queried in a separate systemctl call for compatibility with
older systemd versions that do not support it in a combined --property list.
"""

import logging
import subprocess
import time
from collections import deque
from dataclasses import dataclass, field
from enum import Enum
from typing import Dict, List, Optional

logger = logging.getLogger(__name__)


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
    active: bool               # True if ActiveState == "active"
    load_state: str = ""       # e.g. "loaded", "not-found" (unit absent on this host)
    sub_state: str = ""        # e.g. "running", "dead", "failed"
    main_pid: int = 0
    # None means NRestarts could not be observed (unsupported systemd or a
    # failed probe) — deliberately distinct from a real 0 so consumers don't
    # mistake "unobserved" for "never restarted" when computing deltas.
    n_restarts: Optional[int] = None
    start_timestamp: str = ""
    error: Optional[str] = None  # non-None if the check itself failed


@dataclass
class FabricManagerStatus(ServiceStatus):
    """Extended status for Fabric Manager with journal analysis."""
    journal_errors: List[ErrorCategory] = field(default_factory=list)
    # True when the journal probe itself failed (journalctl error or timeout):
    # journal state is UNKNOWN, which is not the same as "no errors found".
    journal_probe_failed: bool = False
    flapping: bool = False


class ServiceChecker:
    """Checks host systemd services via nsenter."""

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

        Parses systemctl show output for LoadState, ActiveState, SubState,
        MainPID, and ExecMainStartTimestamp. NRestarts is queried separately
        since older systemd versions don't support it in a combined list.
        LoadState is preserved so callers can tell a unit that is absent on
        this host (LoadState=not-found) from one that is loaded but stopped.
        """
        try:
            result = self._run_host_cmd([
                "systemctl", "show", service_name,
                "--property=LoadState,ActiveState,SubState,MainPID,ExecMainStartTimestamp",
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
                load_state=props.get("LoadState", ""),
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

    def _get_restart_count(self, service_name: str) -> Optional[int]:
        """Get NRestarts from systemd.

        Returns the counter value, or None when it could not be observed
        (NRestarts unsupported on older systemd, or the probe failed). None is
        deliberately distinct from 0: a real 0 participates in flap tracking's
        reset detection, while an unobserved value must not.
        """
        try:
            result = self._run_host_cmd([
                "systemctl", "show", service_name, "--property=NRestarts",
            ])
            if result.returncode == 0 and result.stdout.strip():
                _, _, val = result.stdout.strip().partition("=")
                return int(val)
        except Exception as e:
            # Intentional silent fallback: NRestarts is unsupported on older
            # systemd. Keep the reason visible for troubleshooting.
            logger.debug("NRestarts query failed for %s (likely unsupported): %s", service_name, e)
        return None

    def _update_flap_tracking(self, service_name: str, current_restarts: Optional[int]) -> None:
        """Track restart events for flap detection.

        systemd's NRestarts is not monotonic: it resets to 0 on
        `systemctl reset-failed`, on unit re-creation, and after a reboot. A
        decrease is therefore re-baselined (not ignored) so counting resumes
        from the new value instead of going quiet until the counter climbs
        past the old high-water mark. An unobserved probe (None) leaves the
        baseline untouched entirely.
        """
        if current_restarts is None:
            # Probe failed / NRestarts unsupported: no observation, no
            # baseline movement. Still prune below so stale entries age out.
            if service_name not in self._restart_history:
                return
        elif service_name not in self._restart_history:
            self._restart_history[service_name] = deque()
            self._last_restart_count[service_name] = current_restarts
            return
        else:
            last_count = self._last_restart_count[service_name]
            if current_restarts > last_count:
                # New restarts detected — record timestamp for each
                now = time.monotonic()
                for _ in range(current_restarts - last_count):
                    self._restart_history[service_name].append(now)
                self._last_restart_count[service_name] = current_restarts
            elif current_restarts < last_count:
                # Counter reset (reset-failed, unit re-creation, reboot):
                # re-baseline. The restart that caused a reboot-reset is
                # itself observable as a restart event; record one sample so
                # flap detection doesn't go quiet right after the reboot a
                # flapping service would cause.
                logger.info(
                    "NRestarts for %s reset (%d -> %d); re-baselining flap tracking",
                    service_name, last_count, current_restarts,
                )
                self._restart_history[service_name].append(time.monotonic())
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

        if base.error is not None or base.load_state == "not-found":
            # The unit is absent or the service probe itself failed: callers
            # act on those conditions before looking at the journal, so the
            # journalctl fork would be pure waste.
            journal_errors = []
        else:
            journal_errors = self._parse_journal_errors("nvidia-fabricmanager")
        flapping = self.is_flapping("nvidia-fabricmanager")

        return FabricManagerStatus(
            name=base.name,
            active=base.active,
            load_state=base.load_state,
            sub_state=base.sub_state,
            main_pid=base.main_pid,
            n_restarts=base.n_restarts,
            start_timestamp=base.start_timestamp,
            error=base.error,
            journal_errors=journal_errors if journal_errors is not None else [],
            journal_probe_failed=journal_errors is None,
            flapping=flapping,
        )

    def _parse_journal_errors(self, service_name: str) -> Optional[List[ErrorCategory]]:
        """Scan recent journal entries for known error patterns.

        Returns the categories found ([] when the journal is clean), or None
        when the probe itself failed (non-zero journalctl exit, timeout, or
        exception) — a failed probe must stay distinguishable from a clean one.
        """
        try:
            result = self._run_host_cmd([
                "journalctl", "-u", service_name,
                "--since", "5 minutes ago",
                "--no-pager", "-q",
            ], timeout=15)

            if result.returncode != 0:
                logger.warning("Journal probe failed for %s (rc=%d): %s",
                               service_name, result.returncode, result.stderr.strip())
                return None

            if not result.stdout.strip():
                return []

            found: List[ErrorCategory] = []
            text = result.stdout.lower()
            for category, patterns in _ERROR_PATTERNS.items():
                if any(p.lower() in text for p in patterns):
                    found.append(category)

            return found

        except Exception as e:
            # Covers subprocess.TimeoutExpired and any nsenter/journalctl
            # launch failure alike: the probe did not complete.
            logger.warning("Journal probe failed for %s: %s", service_name, e)
            return None

    def check_all_gpu_services(self, service_names: List[str]) -> Dict[str, ServiceStatus]:
        """Check all configured GPU services."""
        results = {}
        for name in service_names:
            results[name] = self.check_service(name)
        return results
