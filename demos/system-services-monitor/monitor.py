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
import time
from threading import Event
from typing import Optional

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

        # Last-known FM down/up state; kept for observability (tests assert
        # on it) — no production consumer today.
        self._fabric_manager_down = False
        # Last-observed cumulative FM restart count (systemd NRestarts), used
        # to increment fabric_manager_restarts_total by the per-cycle delta.
        # None until the first successful observation: the counter must only
        # count restarts that happen while this monitor is watching, not the
        # NRestarts total accumulated before it was deployed (which would
        # false-fire the FabricManagerFlapping alert right after rollout).
        self._last_fm_restarts: Optional[int] = None

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
        # Snapshot the grace state once per cycle: the checks report absolute
        # health and this single snapshot gates both their DOWN logging and
        # the final gauge, so a grace period expiring mid-cycle can't leave a
        # down service published as healthy.
        in_grace = self._in_grace_period()
        overall_healthy = True

        # --- Check 1: Fabric Manager service ---
        if self.config.enable_fabric_check:
            with health_check_duration_seconds.labels("fabric_manager").time():
                try:
                    overall_healthy &= self._check_fabric_manager(node, in_grace)
                except Exception:
                    # Could not inspect Fabric Manager at all: past boot grace
                    # the node must not keep reporting healthy on no evidence.
                    logger.exception("Fabric Manager check failed")
                    health_check_errors_total.labels("fabric_manager").inc()
                    overall_healthy = False

        # --- Check 2: generic GPU services (besides Fabric Manager) ---
        if self.config.enable_gpu_services_check:
            with health_check_duration_seconds.labels("services").time():
                try:
                    overall_healthy &= self._check_gpu_services(node, in_grace)
                except Exception:
                    logger.exception("GPU service check failed")
                    health_check_errors_total.labels("services").inc()
                    overall_healthy = False

        # GPU context/memory validation is intentionally not polled from this
        # daemon (it would contend for GPU memory with running workloads). It
        # runs as a preflight init-container instead — see the demo README.

        # --- Overall health ---
        if in_grace:
            gpu_node_health_up.labels(node).set(1)
            logger.debug("In boot grace period, reporting healthy")
        else:
            gpu_node_health_up.labels(node).set(1 if overall_healthy else 0)

    def _check_fabric_manager(self, node: str, in_grace: bool) -> bool:
        """Check Fabric Manager; return its contribution to overall health."""
        fm_status = self._service_checker.check_fabric_manager()

        if fm_status.load_state == "not-found":
            # Unit not present on this host (e.g. a platform without
            # nvidia-fabricmanager installed). Not a failure.
            logger.debug("nvidia-fabricmanager not present on %s (LoadState=not-found); skipping",
                         node)
            self._fabric_manager_down = False
            return True

        if fm_status.error is not None:
            # The probe itself failed (nsenter/systemctl error or timeout):
            # FM state is UNKNOWN, which is not the same as FM down. Leave the
            # fabric_manager_up gauge untouched (no false FabricManagerDown
            # alert) but count the node unhealthy past boot grace — it cannot
            # be called healthy on no evidence.
            logger.warning("Fabric Manager probe failed on %s, state unknown: %s",
                           node, fm_status.error)
            health_check_errors_total.labels("fabric_manager").inc()
            return False

        self._fabric_manager_down = not fm_status.active

        fabric_manager_up.labels(node).set(1 if fm_status.active else 0)
        if fm_status.active:
            fabric_manager_last_healthy_seconds.labels(node).set(time.time())

        # Increment the restart counter by the delta in systemd NRestarts
        # since the last observation. Backs the FabricManagerFlapping alert.
        # An unobserved NRestarts (None) leaves the baseline untouched.
        if fm_status.n_restarts is not None:
            if self._last_fm_restarts is not None:
                restart_delta = fm_status.n_restarts - self._last_fm_restarts
                if restart_delta > 0:
                    fabric_manager_restarts_total.labels(node).inc(restart_delta)
                elif restart_delta < 0:
                    # NRestarts reset (reset-failed / unit re-creation /
                    # reboot): count the restart that caused it, keeping the
                    # exported counter in step with flap tracking, which
                    # records one sample for the same event.
                    fabric_manager_restarts_total.labels(node).inc(1)
            self._last_fm_restarts = fm_status.n_restarts

        if fm_status.flapping:
            logger.warning("Fabric Manager is flapping on %s", node)

        if fm_status.journal_probe_failed:
            # Journal state is UNKNOWN — do not treat as "no errors found".
            logger.warning("Fabric Manager journal probe failed on %s; journal state unknown",
                           node)
            health_check_errors_total.labels("journal").inc()
        elif fm_status.journal_errors:
            logger.warning("Fabric Manager journal errors on %s: %s",
                           node, [e.value for e in fm_status.journal_errors])

        if not fm_status.active:
            if not in_grace:
                logger.error("Fabric Manager DOWN on %s (sub_state=%s)",
                             node, fm_status.sub_state)
            return False

        return True

    def _check_gpu_services(self, node: str, in_grace: bool) -> bool:
        """Check the configured GPU services; return their health contribution."""
        healthy = True
        svc_results = self._service_checker.check_all_gpu_services(
            self.config.gpu_services
        )
        for svc_name, status in svc_results.items():
            if status.load_state == "not-found":
                logger.debug("Service %s not present on %s (LoadState=not-found); skipping",
                             svc_name, node)
                continue
            if status.error is not None:
                logger.warning("Service %s probe failed on %s, state unknown: %s",
                               svc_name, node, status.error)
                health_check_errors_total.labels("services").inc()
                healthy = False
                continue
            nvidia_service_up.labels(node, svc_name).set(1 if status.active else 0)
            if not status.active:
                if not in_grace:
                    logger.error("Service %s DOWN on %s", svc_name, node)
                healthy = False
        return healthy


# Backwards-compat alias
FabricManagerMonitor = SystemServicesMonitor


def main():
    config = MonitorConfig.from_env()
    monitor = SystemServicesMonitor(config)
    monitor.run()


if __name__ == "__main__":
    main()
