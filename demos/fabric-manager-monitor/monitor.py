"""GPU Node Health Validator — main entry point.

Runs a periodic check loop across all enabled health checks, exposes
Prometheus metrics on the configured port, and computes overall node health.
"""

import logging
import signal
import sys
import time
from threading import Event

from prometheus_client import start_http_server

from config import MonitorConfig
from metrics import (
    gpu_node_health_up,
    fabric_manager_up,
    fabric_manager_restarts_total,
    fabric_manager_last_healthy_seconds,
    nvidia_service_up,
    pcie_link_width,
    pcie_link_gen,
    pcie_link_degraded,
    nvlink_fabric_healthy,
    cuda_validation_passed,
    gpu_clock_throttled,
    gpu_clock_ratio,
    health_check_duration_seconds,
    health_check_errors_total,
)
from checks.service_check import ServiceChecker
from checks.pcie_check import PCIeChecker
from checks.clock_check import ClockChecker
from checks.fabric_check import NVLinkFabricChecker
from checks.cuda_validation import CUDAValidator

logger = logging.getLogger(__name__)


class FabricManagerMonitor:
    """Orchestrates all health checks and exposes Prometheus metrics."""

    def __init__(self, config: MonitorConfig):
        self.config = config
        self._shutdown = Event()
        self._start_time = time.monotonic()
        self._last_cuda_check = 0.0

        # Initialize checkers
        self._service_checker = ServiceChecker(
            flap_window=config.flap_window,
            flap_threshold=config.flap_threshold,
        )
        self._pcie_checker = PCIeChecker()
        self._clock_checker = ClockChecker(throttle_ratio=config.clock_throttle_ratio)
        self._nvlink_checker = NVLinkFabricChecker(dcgm_url=config.dcgm_exporter_url)
        self._cuda_validator = CUDAValidator()

        # Track state for cross-check correlation
        self._fabric_manager_down = False
        self._nvlink_bandwidth_zero = False

    def run(self):
        """Start metrics server and enter the check loop."""
        logging.basicConfig(
            level=getattr(logging, self.config.log_level, logging.INFO),
            format="%(asctime)s %(levelname)s %(name)s: %(message)s",
        )

        logger.info("Starting GPU Node Health Validator on node=%s port=%d interval=%ds",
                     self.config.node_name, self.config.metrics_port, self.config.check_interval)

        # Register signal handlers for graceful shutdown
        signal.signal(signal.SIGTERM, self._handle_signal)
        signal.signal(signal.SIGINT, self._handle_signal)

        # Start Prometheus HTTP server
        start_http_server(self.config.metrics_port)
        logger.info("Prometheus metrics server started on :%d", self.config.metrics_port)

        while not self._shutdown.is_set():
            try:
                self.run_check_cycle()
            except Exception:
                logger.exception("Unexpected error in check cycle")
            self._shutdown.wait(timeout=self.config.check_interval)

        logger.info("Shutting down GPU Node Health Validator")

    def _handle_signal(self, signum, frame):
        logger.info("Received signal %d, initiating shutdown", signum)
        self._shutdown.set()

    def _in_grace_period(self) -> bool:
        return (time.monotonic() - self._start_time) < self.config.boot_grace_period

    def run_check_cycle(self):
        """Execute all enabled checks and update metrics."""
        node = self.config.node_name
        overall_healthy = True

        # --- Check 1 & 2: Services ---
        if self.config.enable_fabric_check:
            with health_check_duration_seconds.labels("services").time():
                try:
                    fm_status = self._service_checker.check_fabric_manager()
                    self._fabric_manager_down = not fm_status.active

                    fabric_manager_up.labels(node).set(1 if fm_status.active else 0)
                    if fm_status.active:
                        fabric_manager_last_healthy_seconds.labels(node).set(time.time())

                    # Update restart counter (set to current total — Counter only goes up)
                    if fm_status.n_restarts > 0:
                        # We use _total suffix via Counter; increment by delta
                        pass  # Counter tracks via flap detection instead

                    if fm_status.flapping:
                        logger.warning("Fabric Manager is flapping on %s", node)

                    if fm_status.journal_errors:
                        logger.warning("Fabric Manager journal errors on %s: %s",
                                       node, [e.value for e in fm_status.journal_errors])

                    if not fm_status.active and not self._in_grace_period():
                        logger.error("Fabric Manager DOWN on %s (sub_state=%s)",
                                     node, fm_status.sub_state)
                        overall_healthy = False

                    # All GPU services
                    svc_results = self._service_checker.check_all_gpu_services(
                        self.config.gpu_services
                    )
                    for svc_name, status in svc_results.items():
                        nvidia_service_up.labels(node, svc_name).set(1 if status.active else 0)
                        if not status.active and not self._in_grace_period():
                            logger.error("Service %s DOWN on %s", svc_name, node)
                            overall_healthy = False

                except Exception:
                    logger.exception("Service check failed")
                    health_check_errors_total.labels("services").inc()

        # --- Check 3: PCIe ---
        if self.config.enable_pcie_check:
            with health_check_duration_seconds.labels("pcie").time():
                try:
                    pcie_results = self._pcie_checker.check()
                    for pcie in pcie_results:
                        gpu = str(pcie.gpu_index)
                        pcie_link_width.labels(node, gpu).set(pcie.link_width_current)
                        pcie_link_gen.labels(node, gpu).set(pcie.link_gen_current)
                        pcie_link_degraded.labels(node, gpu).set(1 if pcie.degraded else 0)
                        if pcie.degraded:
                            logger.warning(
                                "PCIe degraded on %s GPU %s: Gen%d x%d (max Gen%d x%d)",
                                node, gpu, pcie.link_gen_current, pcie.link_width_current,
                                pcie.link_gen_max, pcie.link_width_max,
                            )
                except Exception:
                    logger.exception("PCIe check failed")
                    health_check_errors_total.labels("pcie").inc()

        # --- Check 6: Clocks ---
        if self.config.enable_clock_check:
            with health_check_duration_seconds.labels("clocks").time():
                try:
                    clock_results = self._clock_checker.check()
                    for clk in clock_results:
                        gpu = str(clk.gpu_index)
                        gpu_clock_throttled.labels(node, gpu).set(1 if clk.throttled else 0)
                        gpu_clock_ratio.labels(node, gpu).set(clk.clock_ratio)
                        if clk.throttled:
                            logger.warning(
                                "GPU %s throttled on %s: %d/%d MHz (ratio=%.2f, reasons=%s)",
                                gpu, node, clk.graphics_clock_current,
                                clk.graphics_clock_max, clk.clock_ratio, clk.throttle_reasons,
                            )
                except Exception:
                    logger.exception("Clock check failed")
                    health_check_errors_total.labels("clocks").inc()

        # --- Check 4: NVLink ---
        if self.config.enable_nvlink_check:
            with health_check_duration_seconds.labels("nvlink").time():
                try:
                    nvlink_status = self._nvlink_checker.check()
                    self._nvlink_bandwidth_zero = nvlink_status.bandwidth_zero

                    # False-positive mitigation: only flag unhealthy when
                    # NVLink has CRC errors OR bandwidth is zero AND FM is down
                    fabric_nvlink_degraded = (
                        not nvlink_status.healthy
                        or (nvlink_status.bandwidth_zero and self._fabric_manager_down)
                    )
                    nvlink_fabric_healthy.labels(node).set(0 if fabric_nvlink_degraded else 1)

                    if fabric_nvlink_degraded and not self._in_grace_period():
                        logger.error(
                            "NVLink fabric degraded on %s (crc_errors=%.0f, bw_zero=%s, fm_down=%s)",
                            node, nvlink_status.crc_error_count,
                            nvlink_status.bandwidth_zero, self._fabric_manager_down,
                        )
                        overall_healthy = False

                except Exception:
                    logger.exception("NVLink check failed")
                    health_check_errors_total.labels("nvlink").inc()

        # --- Check 5: CUDA validation (slower cadence) ---
        if self.config.enable_cuda_validation:
            now = time.monotonic()
            if (now - self._last_cuda_check) >= self.config.cuda_validation_interval:
                self._last_cuda_check = now
                with health_check_duration_seconds.labels("cuda").time():
                    try:
                        cuda_result = self._cuda_validator.check()
                        cuda_validation_passed.labels(node).set(1 if cuda_result.passed else 0)
                        if not cuda_result.passed:
                            logger.error("CUDA validation FAILED on %s: %s",
                                         node, cuda_result.errors or cuda_result.error)
                            overall_healthy = False
                    except Exception:
                        logger.exception("CUDA validation failed")
                        health_check_errors_total.labels("cuda").inc()

        # --- Overall health ---
        if self._in_grace_period():
            gpu_node_health_up.labels(node).set(1)
            logger.debug("In boot grace period, reporting healthy")
        else:
            gpu_node_health_up.labels(node).set(1 if overall_healthy else 0)


def main():
    config = MonitorConfig.from_env()
    monitor = FabricManagerMonitor(config)
    monitor.run()


if __name__ == "__main__":
    main()
