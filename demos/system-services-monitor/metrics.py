"""Prometheus metric definitions for GPU Node Health Validator.

All metrics are defined in one place for consistency.
Port 9101 to avoid conflict with NVSentinel's 2112.
"""

from prometheus_client import Gauge, Counter, Histogram


# --- Overall node health ---
gpu_node_health_up = Gauge(
    "gpu_node_health_up",
    "Overall GPU node health (1=healthy, 0=unhealthy)",
    ["node"],
)

# --- Fabric Manager ---
fabric_manager_up = Gauge(
    "fabric_manager_up",
    "Fabric Manager service status (1=running, 0=down)",
    ["node"],
)
fabric_manager_restarts_total = Counter(
    "fabric_manager_restarts_total",
    "Total Fabric Manager restart count observed",
    ["node"],
)
fabric_manager_last_healthy_seconds = Gauge(
    "fabric_manager_last_healthy_seconds",
    "Unix timestamp of last healthy Fabric Manager observation",
    ["node"],
)

# --- GPU systemd services ---
nvidia_service_up = Gauge(
    "nvidia_service_up",
    "NVIDIA systemd service status (1=running, 0=down)",
    ["node", "service_name"],
)

# --- Check infrastructure ---
health_check_duration_seconds = Histogram(
    "health_check_duration_seconds",
    "Duration of health check execution",
    ["check_name"],
    buckets=(0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0),
)
health_check_errors_total = Counter(
    "health_check_errors_total",
    "Total errors encountered during health checks",
    ["check_name"],
)
