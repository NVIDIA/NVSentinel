"""GPU Node Health Validator — main entry point.

Runs a periodic check loop across all enabled health checks, exposes
Prometheus metrics on the configured port, and computes overall node health.

Scope (per ADR-049): this demo covers non-DCGM signals only. PCIe
downtraining, NVLink bandwidth/CRC, and GPU clock throttling are already
surfaced by NVSentinel's gpu-health-monitor via DCGM and are intentionally
NOT duplicated here.
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
    health_check_duration_seconds,
    health_check_errors_total,
)
from checks.service_check import ServiceChecker

logger = logging.getLogger(__name__)


class SystemServicesMonitor:
    """Orchestrates system-service health checks and exposes Prometheus metrics."""

    def __init__(self, config: MonitorConfig):
        self.config = config
        self._shutdown = Event()
        self._start_time = time.monotonic()

        # Initialize checkers
        self._service_checker = ServiceChecker(
            flap_window=config.flap_window,
            flap_threshold=config.flap_threshold,
        )

        # Track state for cross-check correlation
        self._fabric_manager_down = False

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

        # GPU context/memory validation is intentionally not polled from this
        # daemon (it would contend for GPU memory with running workloads). It
        # runs as a preflight init-container instead — see the demo README.

        # --- Overall health ---
        if self._in_grace_period():
            gpu_node_health_up.labels(node).set(1)
            logger.debug("In boot grace period, reporting healthy")
        else:
            gpu_node_health_up.labels(node).set(1 if overall_healthy else 0)


# Backwards-compat alias
FabricManagerMonitor = SystemServicesMonitor


def main():
    config = MonitorConfig.from_env()
    monitor = SystemServicesMonitor(config)
    monitor.run()


if __name__ == "__main__":
    main()
