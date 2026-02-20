"""Configuration for GPU Node Health Validator.

All settings are driven by environment variables with sensible defaults.
Designed to be configured via Kubernetes ConfigMap.
"""

import os
from dataclasses import dataclass, field


@dataclass
class MonitorConfig:
    """Monitor configuration loaded from environment variables."""

    # Core settings
    check_interval: int = 30          # seconds between check cycles
    metrics_port: int = 9101          # Prometheus metrics port (avoids NVSentinel 2112)
    log_level: str = "INFO"
    node_name: str = ""               # populated from HOSTNAME or NODE_NAME

    # Boot grace period - don't flag unhealthy during startup
    boot_grace_period: int = 300      # seconds

    # Flap detection
    flap_window: int = 600            # seconds window for counting restarts
    flap_threshold: int = 3           # restarts within window to flag flapping

    # Check toggles
    enable_fabric_check: bool = True
    enable_pcie_check: bool = True
    enable_clock_check: bool = True
    enable_nvlink_check: bool = True
    enable_cuda_validation: bool = False  # off by default (resource intensive)

    # CUDA validation runs at a slower cadence
    cuda_validation_interval: int = 600  # seconds

    # DCGM exporter endpoint for NVLink metrics
    dcgm_exporter_url: str = "http://localhost:9400"

    # Clock throttle threshold (ratio of current/max)
    clock_throttle_ratio: float = 0.85

    # Services to monitor (besides fabric manager)
    gpu_services: list = field(default_factory=lambda: [
        "nvidia-fabricmanager",
        "nvidia-persistenced",
        "nv-hostengine",
    ])

    @classmethod
    def from_env(cls) -> "MonitorConfig":
        """Load configuration from environment variables."""
        def _bool(val: str) -> bool:
            return val.lower() in ("true", "1", "yes")

        config = cls(
            check_interval=int(os.environ.get("CHECK_INTERVAL", "30")),
            metrics_port=int(os.environ.get("METRICS_PORT", "9101")),
            log_level=os.environ.get("LOG_LEVEL", "INFO"),
            node_name=os.environ.get("NODE_NAME", os.environ.get("HOSTNAME", "")),
            boot_grace_period=int(os.environ.get("BOOT_GRACE_PERIOD", "300")),
            flap_window=int(os.environ.get("FLAP_WINDOW", "600")),
            flap_threshold=int(os.environ.get("FLAP_THRESHOLD", "3")),
            enable_fabric_check=_bool(os.environ.get("ENABLE_FABRIC_CHECK", "true")),
            enable_pcie_check=_bool(os.environ.get("ENABLE_PCIE_CHECK", "true")),
            enable_clock_check=_bool(os.environ.get("ENABLE_CLOCK_CHECK", "true")),
            enable_nvlink_check=_bool(os.environ.get("ENABLE_NVLINK_CHECK", "true")),
            enable_cuda_validation=_bool(os.environ.get("ENABLE_CUDA_VALIDATION", "false")),
            cuda_validation_interval=int(os.environ.get("CUDA_VALIDATION_INTERVAL", "600")),
            dcgm_exporter_url=os.environ.get("DCGM_EXPORTER_URL", "http://localhost:9400"),
            clock_throttle_ratio=float(os.environ.get("CLOCK_THROTTLE_RATIO", "0.85")),
        )

        services_env = os.environ.get("GPU_SERVICES")
        if services_env:
            config.gpu_services = [s.strip() for s in services_env.split(",") if s.strip()]

        return config
