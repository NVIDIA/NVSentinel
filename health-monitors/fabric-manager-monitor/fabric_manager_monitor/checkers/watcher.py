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

"""Main polling loop for fabric-manager-monitor.

Runs all enabled health checks on a configurable interval and fires
callbacks (e.g. PlatformConnectorEventProcessor) with the aggregated
results. Mirrors the DCGMWatcher pattern from gpu-health-monitor.
"""

import logging as log
import time
from concurrent.futures import ThreadPoolExecutor
from functools import partial
from threading import Event
from typing import List

from fabric_manager_monitor import metrics
from .clock_check import ClockChecker
from .cuda_validation import CUDAValidator
from .fabric_check import NVLinkFabricChecker
from .pcie_check import PCIeChecker
from .service_check import ServiceChecker
from .types import CallbackInterface, CheckResult


class FabricManagerWatcher:
    """Orchestrates all health checks and fires callbacks with results.

    Follows the same callback pattern as DCGMWatcher: a list of CallbackInterface
    implementations are invoked after each check cycle.
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
        enable_pcie_check: bool = True,
        enable_clock_check: bool = True,
        enable_nvlink_check: bool = True,
        enable_cuda_validation: bool = False,
        dcgm_exporter_url: str = "http://localhost:9400",
        clock_throttle_ratio: float = 0.85,
    ) -> None:
        self._poll_interval = poll_interval
        self._callbacks = callbacks
        self._node_name = node_name
        self._boot_grace_period = boot_grace_period
        self._start_time = time.monotonic()
        self._callback_thread_pool = ThreadPoolExecutor()

        # Track cross-check state for correlation
        self._fabric_manager_down = False

        # Initialize checkers and build the check list based on enabled flags
        self._checkers: List[tuple[str, callable]] = []

        if enable_fabric_check:
            self._service_checker = ServiceChecker(
                flap_window=flap_window,
                flap_threshold=flap_threshold,
            )
            self._checkers.append(("services", self._run_service_checks))

        if enable_pcie_check:
            self._pcie_checker = PCIeChecker()
            self._checkers.append(("pcie", self._run_pcie_checks))

        if enable_clock_check:
            self._clock_checker = ClockChecker(throttle_ratio=clock_throttle_ratio)
            self._checkers.append(("clocks", self._run_clock_checks))

        if enable_nvlink_check:
            self._nvlink_checker = NVLinkFabricChecker(dcgm_url=dcgm_exporter_url)
            self._checkers.append(("nvlink", self._run_nvlink_checks))

        if enable_cuda_validation:
            self._cuda_validator = CUDAValidator()
            self._checkers.append(("cuda", self._run_cuda_checks))

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
        self._fabric_manager_down = not fm.active

        # Update Prometheus metrics
        metrics.fabric_manager_up.labels(self._node_name).set(1 if fm.active else 0)
        if fm.active:
            metrics.fabric_manager_last_healthy_seconds.labels(self._node_name).set(time.time())

        if fm.flapping:
            log.warning(f"Fabric Manager is flapping on {self._node_name}")

        if fm.journal_errors:
            log.warning(f"Fabric Manager journal errors on {self._node_name}: {[e.value for e in fm.journal_errors]}")

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

    def _run_pcie_checks(self) -> List[CheckResult]:
        """Check PCIe link health for all GPUs."""
        statuses = self._pcie_checker.check()

        # Update Prometheus metrics
        for pcie in statuses:
            gpu = str(pcie.gpu_index)
            metrics.pcie_link_width.labels(self._node_name, gpu).set(pcie.link_width_current)
            metrics.pcie_link_gen.labels(self._node_name, gpu).set(pcie.link_gen_current)
            metrics.pcie_link_degraded.labels(self._node_name, gpu).set(1 if pcie.degraded else 0)
            if pcie.degraded:
                log.warning(
                    f"PCIe degraded on {self._node_name} GPU {gpu}: "
                    f"Gen{pcie.link_gen_current} x{pcie.link_width_current} "
                    f"(max Gen{pcie.link_gen_max} x{pcie.link_width_max})"
                )

        return self._pcie_checker.to_check_results(statuses, self._node_name)

    def _run_clock_checks(self) -> List[CheckResult]:
        """Check GPU clock throttling."""
        statuses = self._clock_checker.check()

        # Update Prometheus metrics
        for clk in statuses:
            gpu = str(clk.gpu_index)
            metrics.gpu_clock_throttled.labels(self._node_name, gpu).set(1 if clk.throttled else 0)
            metrics.gpu_clock_ratio.labels(self._node_name, gpu).set(clk.clock_ratio)
            if clk.throttled:
                log.warning(
                    f"GPU {gpu} throttled on {self._node_name}: "
                    f"{clk.graphics_clock_current}/{clk.graphics_clock_max} MHz "
                    f"(ratio={clk.clock_ratio:.2f}, reasons={clk.throttle_reasons})"
                )

        return self._clock_checker.to_check_results(statuses, self._node_name)

    def _run_nvlink_checks(self) -> List[CheckResult]:
        """Check NVLink fabric health."""
        status = self._nvlink_checker.check()

        # False-positive mitigation: only flag unhealthy when NVLink has CRC errors
        # OR bandwidth is zero AND Fabric Manager is down
        fabric_nvlink_degraded = not status.healthy or (status.bandwidth_zero and self._fabric_manager_down)
        metrics.nvlink_fabric_healthy.labels(self._node_name).set(0 if fabric_nvlink_degraded else 1)

        if fabric_nvlink_degraded and not self._in_grace_period():
            log.error(
                f"NVLink fabric degraded on {self._node_name} "
                f"(crc_errors={status.crc_error_count:.0f}, "
                f"bw_zero={status.bandwidth_zero}, fm_down={self._fabric_manager_down})"
            )

        return self._nvlink_checker.to_check_results(status, self._node_name, self._fabric_manager_down)

    def _run_cuda_checks(self) -> List[CheckResult]:
        """Run CUDA validation."""
        result = self._cuda_validator.check()

        metrics.cuda_validation_passed.labels(self._node_name).set(1 if result.passed else 0)
        if not result.passed:
            log.error(f"CUDA validation FAILED on {self._node_name}: {result.errors or result.error}")

        return self._cuda_validator.to_check_results(result, self._node_name)
