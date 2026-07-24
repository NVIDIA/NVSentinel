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

    # Services to monitor (besides fabric manager).
    # nv-hostengine is monitored by gpu-health-monitor via
    # GpuDcgmConnectivityFailure; not duplicated here.
    gpu_services: list = field(default_factory=lambda: [
        "nvidia-fabricmanager",
        "nvidia-persistenced",
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
        )

        services_env = os.environ.get("GPU_SERVICES")
        if services_env:
            config.gpu_services = [s.strip() for s in services_env.split(",") if s.strip()]

        return config
