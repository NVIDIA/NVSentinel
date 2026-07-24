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

"""Tests for FabricManagerWatcher (poll loop + metric wiring)."""

from threading import Event
from unittest.mock import MagicMock, patch

import pytest

from system_services_monitor import metrics
from system_services_monitor.checkers.service_check import FabricManagerStatus, ServiceStatus
from system_services_monitor.checkers.types import CheckResult
from system_services_monitor.checkers.watcher import FabricManagerWatcher

NODE = "test-node"


def _make_watcher(boot_grace_period: int = 0):
    """Build a watcher with ServiceChecker and FabricStateChecker mocked out.

    boot_grace_period defaults to 0 so _in_grace_period() is False and
    unhealthy results are emitted (the grace window is exercised separately).
    """
    with patch("system_services_monitor.checkers.watcher.ServiceChecker") as mock_svc_cls, patch(
        "system_services_monitor.checkers.watcher.FabricStateChecker"
    ) as mock_fabric_cls:
        watcher = FabricManagerWatcher(
            poll_interval=1,
            callbacks=[],
            node_name=NODE,
            boot_grace_period=boot_grace_period,
            enable_fabric_check=True,
        )
    # Expose the mocked checker instances for per-test configuration.
    watcher._service_checker = watcher._service_checker
    watcher._fabric_state_checker = watcher._fabric_state_checker
    return watcher


def _fm_status(active: bool = True, n_restarts: int = 0, flapping: bool = False) -> FabricManagerStatus:
    return FabricManagerStatus(
        name="nvidia-fabricmanager",
        active=active,
        sub_state="running" if active else "dead",
        main_pid=1234 if active else 0,
        n_restarts=n_restarts,
        journal_errors=[],
        flapping=flapping,
    )


class TestRunServiceChecks:
    def test_active_fabric_manager_yields_healthy_result(self) -> None:
        """Happy path: an active FM produces a healthy, non-fatal CheckResult."""
        w = _make_watcher()
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True)
        w._service_checker.check_all_gpu_services.return_value = {}

        results = w._run_service_checks()

        fm_results = [r for r in results if r.check_name == "FabricManagerServiceDown"]
        assert len(fm_results) == 1
        assert fm_results[0].is_healthy is True
        assert fm_results[0].is_fatal is False

    def test_down_fabric_manager_yields_fatal_result(self) -> None:
        """Failure path: an inactive FM outside the grace period is fatal + unhealthy."""
        w = _make_watcher(boot_grace_period=0)
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=False, flapping=True)
        w._service_checker.check_all_gpu_services.return_value = {}

        results = w._run_service_checks()

        fm_results = [r for r in results if r.check_name == "FabricManagerServiceDown"]
        assert len(fm_results) == 1
        assert fm_results[0].is_healthy is False
        assert fm_results[0].is_fatal is True
        assert "FABRIC_MANAGER_NOT_RUNNING" in fm_results[0].error_codes
        assert "FABRIC_MANAGER_FLAPPING" in fm_results[0].error_codes

    def test_restart_counter_increments_by_delta(self) -> None:
        """fabric_manager_restarts_total advances by the NRestarts delta across cycles."""
        w = _make_watcher()
        w._service_checker.check_all_gpu_services.return_value = {}

        counter = metrics.fabric_manager_restarts_total.labels(NODE)
        before = counter._value.get()

        # First cycle observes NRestarts=2 -> +2 from the initial baseline of 0.
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True, n_restarts=2)
        w._run_service_checks()
        # Second cycle observes NRestarts=5 -> +3 more.
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True, n_restarts=5)
        w._run_service_checks()

        assert counter._value.get() - before == pytest.approx(5.0)

    def test_restart_counter_ignores_negative_delta(self) -> None:
        """A NRestarts reset (systemd reload) must not decrement the counter."""
        w = _make_watcher()
        w._service_checker.check_all_gpu_services.return_value = {}

        counter = metrics.fabric_manager_restarts_total.labels(NODE)
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True, n_restarts=4)
        w._run_service_checks()
        before = counter._value.get()

        # NRestarts drops (counter reset upstream) -> no change here.
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True, n_restarts=1)
        w._run_service_checks()

        assert counter._value.get() == before

    def test_gpu_service_down_is_non_fatal(self) -> None:
        """A downed non-FM GPU service yields a non-fatal CheckResult."""
        w = _make_watcher(boot_grace_period=0)
        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True)
        w._service_checker.check_all_gpu_services.return_value = {
            "nvidia-persistenced": ServiceStatus(name="nvidia-persistenced", active=False, sub_state="dead"),
        }

        results = w._run_service_checks()

        svc_results = [r for r in results if r.check_name == "GpuServiceDown"]
        assert len(svc_results) == 1
        assert svc_results[0].is_healthy is False
        assert svc_results[0].is_fatal is False
        assert "GPU_SERVICE_NOT_RUNNING" in svc_results[0].error_codes


class TestFireCallbacks:
    def test_dispatch_delivers_results_and_records_success(self) -> None:
        """_fire_callback_funcs runs each callback with the results (happy path)."""
        w = _make_watcher()
        callback = MagicMock()
        w._callbacks = [callback]

        results = [
            CheckResult(
                check_name="FabricManagerServiceDown",
                is_healthy=True,
                is_fatal=False,
                error_codes=[],
                message="ok",
                entities_impacted=[{"entityType": "NODE", "entityValue": NODE}],
            )
        ]
        w._fire_callback_funcs(results)
        # Deterministically wait for the pooled callback future to finish.
        w._callback_thread_pool.shutdown(wait=True)

        callback.health_check_completed.assert_called_once_with(results)

    def test_dispatch_swallows_callback_exception(self) -> None:
        """A raising callback is caught by the done-callback (failure path)."""
        w = _make_watcher()
        callback = MagicMock()
        callback.health_check_completed.side_effect = RuntimeError("boom")
        w._callbacks = [callback]

        # Must not propagate out of the watcher.
        w._fire_callback_funcs([])
        w._callback_thread_pool.shutdown(wait=True)

        callback.health_check_completed.assert_called_once()


class TestStartLoop:
    def test_single_cycle_runs_checks_then_exits(self) -> None:
        """start() runs one full cycle then terminates once exit is signaled."""
        w = _make_watcher(boot_grace_period=0)
        exit_event = Event()

        w._service_checker.check_fabric_manager.return_value = _fm_status(active=True)
        w._service_checker.check_all_gpu_services.return_value = {}
        w._fabric_state_checker.check.return_value = []
        # Signal exit from inside the cycle so the loop body executes exactly
        # once, then falls through exit.wait() and terminates.
        w._fabric_state_checker.to_check_results.side_effect = lambda *a, **k: (exit_event.set() or [])

        w.start(exit_event)

        # One cycle's worth of checks ran and the loop returned cleanly.
        assert w._service_checker.check_fabric_manager.call_count == 1
        assert w._fabric_state_checker.check.call_count == 1
        assert exit_event.is_set()

    def test_disabled_fabric_check_registers_no_checkers(self) -> None:
        """With fabric checks disabled the watcher has no checkers to run."""
        w = FabricManagerWatcher(
            poll_interval=1,
            callbacks=[],
            node_name=NODE,
            enable_fabric_check=False,
        )
        assert w._checkers == []
