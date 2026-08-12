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

"""Main polling loop for system-services-monitor.

Runs non-DCGM health checks on a configurable interval and fires
callbacks (e.g. PlatformConnectorEventProcessor) with the aggregated
results. Mirrors the DCGMWatcher pattern from gpu-health-monitor.

Scope (per ADR-049): only checks that DCGM cannot see --
  FM service health, FM flap detection, fabric state,
  GPU service lifecycle. PCIe, NVLink, and clock throttling are
  owned by gpu-health-monitor via pydcgm.
"""

import logging as log
import time
from concurrent.futures import ThreadPoolExecutor
from functools import partial
from threading import Event
from typing import Callable, List

from system_services_monitor import metrics
from .fabric_state_check import FabricStateChecker
from .service_check import ServiceChecker
from .types import CallbackInterface, CheckResult


class FabricManagerWatcher:
    """Orchestrates non-DCGM health checks and fires callbacks with results.

    Follows the same callback pattern as DCGMWatcher: a list of CallbackInterface
    implementations are invoked after each check cycle.

    PCIe link health, NVLink fabric, and clock throttling are intentionally
    excluded -- those signals are DCGM-visible and belong in gpu-health-monitor
    (see ADR-049).
    """

    def __init__(
        self,
        poll_interval: int,
        callbacks: List[CallbackInterface],
        node_name: str,
        boot_grace_period: int = 300,
        flap_window: int = 600,
        flap_threshold: int = 3,
        enable_fabric_check: bool = True,
    ) -> None:
        self._poll_interval = poll_interval
        self._callbacks = callbacks
        self._node_name = node_name
        self._boot_grace_period = boot_grace_period
        self._start_time = time.monotonic()
        self._callback_thread_pool = ThreadPoolExecutor()
        # Last-observed cumulative FM restart count (systemd NRestarts) used to
        # increment the fabric_manager_restarts_total counter by the per-cycle
        # delta. Negative deltas (systemd reset / reload) are ignored.
        self._last_fm_restarts = 0

        # Initialize checkers and build the check list based on enabled flags
        self._checkers: List[tuple[str, Callable[[], List[CheckResult]]]] = []

        if enable_fabric_check:
            self._service_checker = ServiceChecker(
                flap_window=flap_window,
                flap_threshold=flap_threshold,
            )
            self._checkers.append(("services", self._run_service_checks))

            # Per-GPU fabric state query (depends on FM being monitored)
            self._fabric_state_checker = FabricStateChecker()
            self._checkers.append(("fabric_state", self._run_fabric_state_checks))

    def _in_grace_period(self) -> bool:
        return (time.monotonic() - self._start_time) < self._boot_grace_period

    def _fire_callback_funcs(self, results: List[CheckResult]) -> None:
        """Invoke health_check_completed on all registered callbacks."""

        def done_callback(class_name: str, future):
            e = future.exception()
            if e is not None:
                log.exception(f"Callback failed: {e}")
                metrics.callback_failures.labels(class_name, "health_check_completed").inc()
            else:
                metrics.callback_success.labels(class_name, "health_check_completed").inc()

        for callback in self._callbacks:
            log.debug(f"Invoking health_check_completed on {callback.__class__.__name__}")
            self._callback_thread_pool.submit(callback.health_check_completed, results).add_done_callback(
                partial(done_callback, callback.__class__.__name__)
            )

    def start(self, exit: Event) -> None:
        """Run the polling loop until exit is signaled."""
        log.info(
            f"Starting FabricManagerWatcher on {self._node_name} with "
            f"{len(self._checkers)} checkers, poll_interval={self._poll_interval}s"
        )

        while not exit.is_set():
            with metrics.overall_reconcile_loop_time.time():
                results: List[CheckResult] = []

                for name, check_func in self._checkers:
                    with metrics.check_duration.labels(name).time():
                        try:
                            check_results = check_func()
                            results.extend(check_results)
                        except Exception as e:
                            log.error(f"Check '{name}' failed with exception: {e}")
                            metrics.check_errors.labels(name).inc()

                # Update overall node health metric
                if self._in_grace_period():
                    metrics.gpu_node_health_up.labels(self._node_name).set(1)
                    log.debug("In boot grace period, reporting healthy")
                else:
                    overall_healthy = all(r.is_healthy for r in results) if results else True
                    metrics.gpu_node_health_up.labels(self._node_name).set(1 if overall_healthy else 0)

                # Fire callbacks with all results
                if results:
                    self._fire_callback_funcs(results)

            log.debug("Waiting till next cycle")
            exit.wait(self._poll_interval)

        # Cleanup on exit
        self._callback_thread_pool.shutdown(cancel_futures=True)

    def _run_service_checks(self) -> List[CheckResult]:
        """Check Fabric Manager and GPU services."""
        results: List[CheckResult] = []

        fm = self._service_checker.check_fabric_manager()

        if fm.load_state == "not-found":
            # Unit not present on this host (e.g. a platform without
            # nvidia-fabricmanager installed). Not a failure -- emit nothing
            # rather than a false FABRIC_MANAGER_NOT_RUNNING.
            log.debug(f"nvidia-fabricmanager not present on {self._node_name} (LoadState=not-found); skipping")
        elif fm.error is not None:
            # The probe itself failed (nsenter/systemctl error or timeout):
            # FM state is UNKNOWN, which is not the same as FM down. Skip
            # emission instead of reporting a fatal NOT_RUNNING.
            log.warning(f"Fabric Manager probe failed on {self._node_name}, state unknown: {fm.error}")
            metrics.check_errors.labels("services").inc()
        else:
            # Update Prometheus metrics
            metrics.fabric_manager_up.labels(self._node_name).set(1 if fm.active else 0)
            if fm.active:
                metrics.fabric_manager_last_healthy_seconds.labels(self._node_name).set(time.time())

            # Increment the restart counter by the delta in systemd NRestarts since
            # the last cycle. Backs the FabricManagerFlapping alert.
            restart_delta = fm.n_restarts - self._last_fm_restarts
            if restart_delta > 0:
                metrics.fabric_manager_restarts_total.labels(self._node_name).inc(restart_delta)
            self._last_fm_restarts = fm.n_restarts

            if fm.flapping:
                log.warning(f"Fabric Manager is flapping on {self._node_name}")

            if fm.journal_errors:
                log.warning(
                    f"Fabric Manager journal errors on {self._node_name}: {[e.value for e in fm.journal_errors]}"
                )

            if not fm.active and not self._in_grace_period():
                error_codes = ["FABRIC_MANAGER_NOT_RUNNING"]
                if fm.flapping:
                    error_codes.append("FABRIC_MANAGER_FLAPPING")
                if fm.journal_errors:
                    error_codes.extend([f"JOURNAL_{e.value.upper()}" for e in fm.journal_errors])

                results.append(
                    CheckResult(
                        check_name="FabricManagerServiceDown",
                        is_healthy=False,
                        is_fatal=True,
                        error_codes=error_codes,
                        message=f"Fabric Manager is {fm.sub_state} on {self._node_name}",
                        entities_impacted=[{"entityType": "NODE", "entityValue": self._node_name}],
                        metadata={
                            "sub_state": fm.sub_state,
                            "n_restarts": str(fm.n_restarts),
                            "flapping": str(fm.flapping),
                        },
                    )
                )
            elif fm.active:
                results.append(
                    CheckResult(
                        check_name="FabricManagerServiceDown",
                        is_healthy=True,
                        is_fatal=False,
                        error_codes=[],
                        message=f"Fabric Manager is running on {self._node_name}",
                        entities_impacted=[{"entityType": "NODE", "entityValue": self._node_name}],
                    )
                )

        # Check additional GPU services
        svc_results = self._service_checker.check_all_gpu_services()
        for svc_name, status in svc_results.items():
            if status.load_state == "not-found":
                log.debug(f"Service {svc_name} not present on {self._node_name} (LoadState=not-found); skipping")
                continue
            if status.error is not None:
                log.warning(f"Service {svc_name} probe failed on {self._node_name}, state unknown: {status.error}")
                metrics.check_errors.labels("services").inc()
                continue
            metrics.nvidia_service_up.labels(self._node_name, svc_name).set(1 if status.active else 0)
            if not status.active and not self._in_grace_period():
                results.append(
                    CheckResult(
                        check_name="GpuServiceDown",
                        is_healthy=False,
                        is_fatal=False,
                        error_codes=["GPU_SERVICE_NOT_RUNNING"],
                        message=f"Service {svc_name} is {status.sub_state} on {self._node_name}",
                        entities_impacted=[{"entityType": "NODE", "entityValue": self._node_name}],
                        metadata={"service_name": svc_name, "sub_state": status.sub_state},
                    )
                )
            elif status.active:
                results.append(
                    CheckResult(
                        check_name="GpuServiceDown",
                        is_healthy=True,
                        is_fatal=False,
                        error_codes=[],
                        message=f"Service {svc_name} is running on {self._node_name}",
                        entities_impacted=[{"entityType": "NODE", "entityValue": self._node_name}],
                        metadata={"service_name": svc_name},
                    )
                )

        return results

    def _run_fabric_state_checks(self) -> List[CheckResult]:
        """Query per-GPU fabric state via nvidia-smi."""
        statuses = self._fabric_state_checker.check()

        # Update per-GPU Prometheus metrics
        for gpu in statuses:
            if gpu.fabric_state == "N/A":
                continue
            is_healthy = gpu.fabric_state == "Completed" and gpu.fabric_status == "Success"
            metrics.fabric_state_healthy.labels(self._node_name, str(gpu.gpu_index)).set(1 if is_healthy else 0)
            if not is_healthy:
                log.warning(
                    f"Fabric state unhealthy on {self._node_name} GPU {gpu.gpu_index}: "
                    f"state={gpu.fabric_state}, status={gpu.fabric_status}"
                )

        fabric_results = self._fabric_state_checker.to_check_results(statuses, self._node_name)
        if self._in_grace_period():
            # Same suppression the service checks apply during node startup:
            # drop unhealthy results, keep healthy ones (they prime the
            # transition cache without triggering remediation).
            fabric_results = [r for r in fabric_results if r.is_healthy]
        return fabric_results
