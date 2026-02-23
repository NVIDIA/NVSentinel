# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

"""Configuration for NCCL all-reduce preflight check."""

import os
from dataclasses import dataclass

from .protos import health_event_pb2 as pb

# Default values
DEFAULT_GANG_CONFIG_DIR = "/etc/preflight"
DEFAULT_BW_THRESHOLD_GBPS = 100.0
DEFAULT_GANG_TIMEOUT_SECONDS = 600
DEFAULT_MESSAGE_SIZES = "4G,8G"
DEFAULT_BENCHMARK_ITERS = 20
DEFAULT_WARMUP_ITERS = 5
DEFAULT_REDUCE_OP = "sum"


@dataclass
class Config:
    """Configuration for NCCL all-reduce preflight check.

    Attributes:
        gang_config_dir: Directory where gang ConfigMap is mounted.
        bw_threshold_gbps: Minimum acceptable bus bandwidth in GB/s.
        skip_bandwidth_check: Skip bandwidth threshold validation; pass if benchmark completes.
        gang_timeout_seconds: Timeout for gang formation in seconds.
        message_sizes: Comma-separated message sizes to test (e.g., "4G,8G").
        benchmark_iters: Number of benchmark iterations per size.
        warmup_iters: Number of warmup iterations before timing.
        connector_socket: Unix socket path for Platform Connector.
        node_name: Kubernetes node name for health events.
        pod_name: Pod name (used to determine rank).
        processing_strategy: How downstream modules handle the event.
        reduce_op: Reduction operation (sum/prod/min/max/avg).
    """

    gang_config_dir: str
    bw_threshold_gbps: float
    skip_bandwidth_check: bool
    gang_timeout_seconds: int
    message_sizes: str
    benchmark_iters: int
    warmup_iters: int
    reduce_op: str
    connector_socket: str
    node_name: str
    pod_name: str
    processing_strategy: int

    @classmethod
    def from_env(cls) -> "Config":
        """Load configuration from environment variables.

        Returns:
            Config instance populated from environment.

        Raises:
            ValueError: If required environment variables are missing or invalid.
        """
        gang_config_dir = os.getenv("GANG_CONFIG_DIR", DEFAULT_GANG_CONFIG_DIR)
        bw_threshold_gbps = _parse_float(
            "BW_THRESHOLD_GBPS",
            DEFAULT_BW_THRESHOLD_GBPS,
        )
        skip_bandwidth_check = os.getenv(
            "SKIP_BANDWIDTH_CHECK",
            "false",
        ).lower() in ("true", "1", "yes")
        gang_timeout_seconds = _parse_int(
            "GANG_TIMEOUT_SECONDS",
            DEFAULT_GANG_TIMEOUT_SECONDS,
        )
        message_sizes = os.getenv("MESSAGE_SIZES", DEFAULT_MESSAGE_SIZES)
        benchmark_iters = _parse_int("BENCHMARK_ITERS", DEFAULT_BENCHMARK_ITERS)
        warmup_iters = _parse_int("WARMUP_ITERS", DEFAULT_WARMUP_ITERS, min_value=0)
        reduce_op = os.getenv("NCCL_REDUCE_OP", DEFAULT_REDUCE_OP)

        connector_socket = os.getenv("PLATFORM_CONNECTOR_SOCKET", "")
        node_name = os.getenv("NODE_NAME", "")
        pod_name = os.getenv("POD_NAME", "")

        if not connector_socket:
            raise ValueError("PLATFORM_CONNECTOR_SOCKET is required")
        if not node_name:
            raise ValueError("NODE_NAME is required")
        if not pod_name:
            raise ValueError("POD_NAME is required")

        strategy_str = os.getenv("PROCESSING_STRATEGY", "EXECUTE_REMEDIATION")
        try:
            processing_strategy = pb.ProcessingStrategy.Value(strategy_str)
        except ValueError as err:
            raise ValueError(f"Invalid PROCESSING_STRATEGY: {strategy_str}") from err

        return cls(
            gang_config_dir=gang_config_dir,
            bw_threshold_gbps=bw_threshold_gbps,
            skip_bandwidth_check=skip_bandwidth_check,
            gang_timeout_seconds=gang_timeout_seconds,
            message_sizes=message_sizes,
            benchmark_iters=benchmark_iters,
            warmup_iters=warmup_iters,
            reduce_op=reduce_op,
            connector_socket=connector_socket,
            node_name=node_name,
            pod_name=pod_name,
            processing_strategy=processing_strategy,
        )


def _parse_float(env_key: str, default: float) -> float:
    """Parse a float from environment variable.

    Args:
        env_key: Environment variable name.
        default: Default value if not set.

    Returns:
        Parsed float value.

    Raises:
        ValueError: If the value cannot be parsed as a positive float.
    """
    value = os.getenv(env_key, "")
    if not value:
        return default

    try:
        parsed = float(value)
    except ValueError as err:
        raise ValueError(f"Invalid {env_key}: {value}") from err

    if parsed <= 0:
        raise ValueError(f"{env_key} must be positive, got {parsed}")

    return parsed


def _parse_int(env_key: str, default: int, *, min_value: int = 1) -> int:
    """Parse an integer from environment variable.

    Args:
        env_key: Environment variable name.
        default: Default value if not set.
        min_value: Minimum acceptable value (default: 1).

    Returns:
        Parsed integer value.

    Raises:
        ValueError: If the value is invalid or below min_value.
    """
    value = os.getenv(env_key, "")
    if not value:
        return default

    try:
        parsed = int(value)
    except ValueError as err:
        raise ValueError(f"Invalid {env_key}: {value}") from err

    if parsed < min_value:
        raise ValueError(f"{env_key} must be >= {min_value}, got {parsed}")

    return parsed
