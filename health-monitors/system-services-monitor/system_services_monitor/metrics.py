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

"""Prometheus metric definitions for health checks."""

from prometheus_client import Counter, Gauge, Histogram

# --- Check infrastructure ---
check_duration = Histogram(
    "fabric_monitor_check_duration_seconds",
    "Duration of individual health check execution",
    labelnames=["check_name"],
    buckets=(0.1, 0.25, 0.5, 1.0, 2.5, 5.0, 10.0, 30.0),
)

check_errors = Counter(
    "fabric_monitor_check_errors_total",
    "Total errors encountered during health checks",
    labelnames=["check_name"],
)

overall_reconcile_loop_time = Histogram(
    "fabric_monitor_reconcile_time",
    "Amount of time spent running a single reconcile loop",
)

callback_failures = Counter(
    "fabric_monitor_callback_failures",
    "Number of times a callback function has thrown an exception",
    labelnames=["class_name", "func_name"],
)

callback_success = Counter(
    "fabric_monitor_callback_success",
    "Number of times a callback function has successfully completed",
    labelnames=["class_name", "func_name"],
)

# --- Fabric Manager ---
fabric_manager_up = Gauge(
    "fabric_manager_up",
    "Fabric Manager service status (1=running, 0=down)",
    labelnames=["node"],
)

fabric_manager_last_healthy_seconds = Gauge(
    "fabric_manager_last_healthy_seconds",
    "Unix timestamp of last healthy Fabric Manager observation",
    labelnames=["node"],
)

# --- GPU systemd services ---
nvidia_service_up = Gauge(
    "nvidia_service_up",
    "NVIDIA systemd service status (1=running, 0=down)",
    labelnames=["node", "service_name"],
)

# --- Per-GPU fabric state ---
fabric_state_healthy = Gauge(
    "fabric_state_healthy",
    "Per-GPU fabric orchestration state (1=completed, 0=unhealthy)",
    labelnames=["node", "gpu_index"],
)

# --- CUDA validation ---
cuda_validation_passed = Gauge(
    "cuda_validation_passed",
    "CUDA validation result (1=passed, 0=failed)",
    labelnames=["node"],
)

# --- Overall node health ---
gpu_node_health_up = Gauge(
    "gpu_node_health_up",
    "Overall GPU node health (1=healthy, 0=unhealthy)",
    labelnames=["node"],
)

# --- Active health events ---
active_health_events = Gauge(
    "fabric_monitor_active_health_events",
    "Number of active health events by type and severity",
    labelnames=["event_type", "gpu_id", "severity"],
)
