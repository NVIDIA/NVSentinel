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
import re
from collections.abc import Mapping
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
# How long a health event is retried against the deployment platform connector
# before the send is given up. Same default as the shared Go client.
DEFAULT_PUBLISH_RETRY_WINDOW_SECONDS = 300.0

# Environment contract (identical names in the shared Go client).
TARGET_ENV = "HEALTH_PUBLISH_TARGET"
INSECURE_ENV = "HEALTH_PUBLISH_INSECURE"
TLS_CA_FILE_ENV = "HEALTH_PUBLISH_TLS_CA_FILE"
TLS_SERVER_NAME_ENV = "HEALTH_PUBLISH_TLS_SERVER_NAME"
TOKEN_PATH_ENV = "HEALTH_PUBLISH_TOKEN_PATH"
RETRY_WINDOW_ENV = "HEALTH_PUBLISH_RETRY_WINDOW"

# Go-style durations ("5m", "1m30s", "500ms"); bare numbers are rejected the
# same way Go's time.ParseDuration rejects them, so both clients read the
# same value the same way.
_DURATION_PATTERN = re.compile(r"^(?:\d+(?:\.\d+)?(?:ms|s|m|h))+$")
_DURATION_COMPONENT = re.compile(r"(\d+(?:\.\d+)?)(ms|s|m|h)")
_UNIT_SECONDS = {"ms": 0.001, "s": 1.0, "m": 60.0, "h": 3600.0}


def _parse_duration(text: str) -> float:
    """Parse a Go-style duration string into seconds."""
    candidate = text.strip()
    if not _DURATION_PATTERN.match(candidate):
        raise ValueError(f"invalid duration {text!r}: expected a Go-style duration such as '5m' or '90s'")
    return sum(float(value) * _UNIT_SECONDS[unit] for value, unit in _DURATION_COMPONENT.findall(candidate))


def _bool_env(environ: Mapping[str, str], name: str) -> bool:
    """A boolean setting in the vocabulary Go's strconv.ParseBool accepts; anything else is refused."""
    raw = environ.get(name, "").strip()
    if not raw:
        return False
    lowered = raw.lower()
    if lowered in ("1", "t", "true"):
        return True
    if lowered in ("0", "f", "false"):
        return False
    raise ValueError(f"invalid {name} {raw!r}: must be true or false")


@dataclass(frozen=True)
class DirectPublisherConfig:
    """HEALTH_PUBLISH_* settings for publishing straight to the deployment platform connector."""

    target: str
    insecure: bool
    ca_file: str | None
    server_name_override: str | None
    token_path: str
    retry_window_seconds: float

    @classmethod
    def from_env(cls, environ: Mapping[str, str] | None = None) -> "DirectPublisherConfig | None":
        """The direct mode settings, or None when HEALTH_PUBLISH_TARGET is unset or blank.

        A set target with missing or invalid companion settings raises
        ValueError: a misconfigured check must fail as a config error, not
        silently fall back to the socket.
        """
        if environ is None:
            environ = os.environ
        target = environ.get(TARGET_ENV, "").strip()
        if not target:
            return None

        insecure = _bool_env(environ, INSECURE_ENV)
        ca_file = environ.get(TLS_CA_FILE_ENV, "").strip() or None
        if not insecure and not ca_file:
            raise ValueError(f"{TLS_CA_FILE_ENV} is required when {TARGET_ENV} is set unless {INSECURE_ENV}=true")

        token_path = environ.get(TOKEN_PATH_ENV, "").strip()
        if not token_path:
            # The server authenticates every publish; there is no token-less mode.
            raise ValueError(f"{TOKEN_PATH_ENV} is required when {TARGET_ENV} is set")

        retry_window_raw = environ.get(RETRY_WINDOW_ENV, "").strip()
        retry_window_seconds = (
            _parse_duration(retry_window_raw) if retry_window_raw else DEFAULT_PUBLISH_RETRY_WINDOW_SECONDS
        )
        if retry_window_seconds <= 0:
            raise ValueError(f"invalid {RETRY_WINDOW_ENV} {retry_window_raw!r}: must be a positive duration")

        return cls(
            target=target,
            insecure=insecure,
            ca_file=ca_file,
            server_name_override=environ.get(TLS_SERVER_NAME_ENV, "").strip() or None,
            token_path=token_path,
            retry_window_seconds=retry_window_seconds,
        )


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
        token_path: Optional file path of a projected ServiceAccount token
            presented as a bearer credential on Platform Connector calls.
            None leaves the calls unauthenticated (current behavior).
        publish: Set when the check publishes straight to the deployment
            platform connector (HEALTH_PUBLISH_TARGET). None keeps the
            node-local socket.
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
    token_path: str | None = None
    publish: DirectPublisherConfig | None = None

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
        # Optional: set by the preflight injection webhook when token
        # authentication to the Platform Connector is configured.
        token_path = os.getenv("PLATFORM_CONNECTOR_TOKEN_PATH") or None

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

        publish = DirectPublisherConfig.from_env()

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
            token_path=token_path,
            publish=publish,
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
