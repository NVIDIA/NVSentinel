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

import os
import re
from collections.abc import Mapping
from dataclasses import dataclass

from .protos import health_event_pb2 as pb

DEFAULT_STATUS_RETRY_MAX_ATTEMPTS = 10
DEFAULT_STATUS_RETRY_INTERVAL_SECONDS = 10.0
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
    diag_level: int
    hostengine_addr: str
    connector_socket: str
    node_name: str
    processing_strategy: pb.ProcessingStrategy
    status_retry_max_attempts: int
    status_retry_interval_seconds: float
    # Optional file path of a projected ServiceAccount token presented as a
    # bearer credential on Platform Connector calls. None leaves the calls
    # unauthenticated (current behavior).
    token_path: str | None = None
    # Set when the check publishes straight to the deployment platform
    # connector (HEALTH_PUBLISH_TARGET). None keeps the node-local socket.
    publish: DirectPublisherConfig | None = None

    @classmethod
    def from_env(cls) -> "Config":
        diag_level = int(os.getenv("DCGM_DIAG_LEVEL", "2"))
        hostengine_addr = os.getenv("DCGM_HOSTENGINE_ADDR", "")
        connector_socket = os.getenv("PLATFORM_CONNECTOR_SOCKET", "")
        node_name = os.getenv("NODE_NAME", "")
        # Optional: set by the preflight injection webhook when token
        # authentication to the Platform Connector is configured.
        token_path = os.getenv("PLATFORM_CONNECTOR_TOKEN_PATH") or None
        strategy_str = os.getenv("PROCESSING_STRATEGY", "EXECUTE_REMEDIATION")
        status_retry_max_attempts = int(
            os.getenv(
                "DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS",
                str(DEFAULT_STATUS_RETRY_MAX_ATTEMPTS),
            )
        )
        status_retry_interval_seconds = float(
            os.getenv(
                "DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS",
                str(DEFAULT_STATUS_RETRY_INTERVAL_SECONDS),
            )
        )

        if not hostengine_addr:
            raise ValueError("DCGM_HOSTENGINE_ADDR is required")

        if not connector_socket:
            raise ValueError("PLATFORM_CONNECTOR_SOCKET is required")

        if not node_name:
            raise ValueError("NODE_NAME is required")

        if diag_level < 1 or diag_level > 4:
            raise ValueError(f"DCGM_DIAG_LEVEL must be 1-4, got {diag_level}")

        if status_retry_max_attempts < 1:
            raise ValueError(f"DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS must be >= 1, got {status_retry_max_attempts}")

        if status_retry_interval_seconds <= 0:
            raise ValueError(
                f"DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS must be > 0, got {status_retry_interval_seconds}"
            )

        try:
            processing_strategy = pb.ProcessingStrategy.Value(strategy_str)
        except ValueError:
            raise ValueError(f"Invalid PROCESSING_STRATEGY: {strategy_str}")

        publish = DirectPublisherConfig.from_env()

        return cls(
            diag_level=diag_level,
            hostengine_addr=hostengine_addr,
            connector_socket=connector_socket,
            node_name=node_name,
            processing_strategy=processing_strategy,
            status_retry_max_attempts=status_retry_max_attempts,
            status_retry_interval_seconds=status_retry_interval_seconds,
            token_path=token_path,
            publish=publish,
        )
