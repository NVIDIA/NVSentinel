"""Check 4: NVLink fabric health via DCGM metrics.

Queries the DCGM exporter's Prometheus endpoint for NVLink bandwidth and
error counters. False-positive mitigation: NVLink bandwidth is normally
zero when no multi-GPU workload is running, so this check alone doesn't
flag unhealthy -- monitor.py correlates with Fabric Manager status.
"""

import logging
from dataclasses import dataclass
from typing import Dict, Optional

import requests

logger = logging.getLogger(__name__)


@dataclass
class NVLinkStatus:
    """NVLink fabric health summary for the node."""
    healthy: bool
    total_tx_bytes: float = 0.0
    total_rx_bytes: float = 0.0
    crc_error_count: float = 0.0
    bandwidth_zero: bool = True
    error: Optional[str] = None


class NVLinkFabricChecker:
    """Checks NVLink fabric health via DCGM exporter metrics."""

    # DCGM metric names we care about
    _TX_METRIC = "DCGM_FI_PROF_NVLINK_TX_BYTES"
    _RX_METRIC = "DCGM_FI_PROF_NVLINK_RX_BYTES"
    _BW_METRIC = "DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL"
    _CRC_METRIC = "DCGM_FI_DEV_NVLINK_CRC_FLIT_ERROR_COUNT_TOTAL"

    def __init__(self, dcgm_url: str = "http://localhost:9400"):
        self._dcgm_url = dcgm_url.rstrip("/")

    def check(self) -> NVLinkStatus:
        """Query DCGM exporter and assess NVLink health."""
        try:
            metrics = self._fetch_metrics()
        except Exception as e:
            return NVLinkStatus(
                healthy=True,  # can't determine -- assume healthy
                error=f"Failed to fetch DCGM metrics: {e}",
            )

        tx = self._sum_metric(metrics, self._TX_METRIC)
        rx = self._sum_metric(metrics, self._RX_METRIC)
        bw = self._sum_metric(metrics, self._BW_METRIC)
        crc = self._sum_metric(metrics, self._CRC_METRIC)

        bandwidth_zero = (tx + rx + bw) == 0.0
        has_errors = crc > 0

        # NVLink bandwidth being zero is normal when idle.
        # We only flag unhealthy if CRC errors are accumulating.
        # The correlation with Fabric Manager down is done in monitor.py.
        healthy = not has_errors

        return NVLinkStatus(
            healthy=healthy,
            total_tx_bytes=tx,
            total_rx_bytes=rx,
            crc_error_count=crc,
            bandwidth_zero=bandwidth_zero,
        )

    def _fetch_metrics(self) -> Dict[str, float]:
        """Fetch and parse Prometheus text format from DCGM exporter."""
        resp = requests.get(
            f"{self._dcgm_url}/metrics",
            timeout=10,
        )
        resp.raise_for_status()
        return self._parse_prometheus_text(resp.text)

    def _parse_prometheus_text(self, text: str) -> Dict[str, float]:
        """Parse Prometheus exposition format into {metric_name: [values]}."""
        metrics: Dict[str, list] = {}
        for line in text.splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            # Lines look like: DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL{gpu="0",...} 1234.0
            try:
                name_and_labels, value_str = line.rsplit(" ", 1)
                name = name_and_labels.split("{")[0]
                value = float(value_str)
                metrics.setdefault(name, []).append(value)
            except (ValueError, IndexError):
                continue
        return metrics

    def _sum_metric(self, metrics: Dict[str, list], name: str) -> float:
        """Sum all values for a given metric name across GPUs."""
        values = metrics.get(name, [])
        return sum(values)
