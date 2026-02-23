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

"""Structured JSON logging using structlog.

Usage:
    from nccl_allreduce.logger import set_default_structured_logger
    import logging as log

    # At application startup
    set_default_structured_logger("preflight-nccl-allreduce", "v0.1.0", "info")

    # Use standard logging
    log.info("Application started")
    log.info("Processing GPUs", extra={"gpu_count": 8})
"""

import logging
import sys
from typing import Any, Callable, Final

import structlog

_LEVEL_MAP: Final[dict[str, int]] = {
    "debug": logging.DEBUG,
    "info": logging.INFO,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "error": logging.ERROR,
}


def _parse_log_level(level: str) -> int:
    """Convert a string log level to a logging constant."""
    return _LEVEL_MAP.get(level.lower().strip(), logging.INFO)


def _make_module_version_injector(
    module: str, version: str
) -> Callable[[logging.Logger | None, str, dict[str, Any]], dict[str, Any]]:
    """Create a processor that injects module and version into every log entry."""

    def inject_module_version(
        logger: logging.Logger | None,
        method_name: str,
        event_dict: dict[str, Any],
    ) -> dict[str, Any]:
        event_dict["module"] = module
        event_dict["version"] = version
        return event_dict

    return inject_module_version


def set_default_structured_logger(module: str, version: str, level: str) -> None:
    """Initialize the structured logger with the specified log level.

    Args:
        module: The name of the module/application using the logger.
        version: The version of the module/application (e.g., "v1.0.0").
        level: The log level as a string (e.g., "debug", "info", "warn", "error").
    """
    log_level = _parse_log_level(level)

    inject_module_version = _make_module_version_injector(module, version)

    root_logger = logging.getLogger()
    root_logger.setLevel(log_level)

    for handler in root_logger.handlers[:]:
        root_logger.removeHandler(handler)

    handler = logging.StreamHandler(sys.stderr)
    handler.setLevel(log_level)

    handler.setFormatter(
        structlog.stdlib.ProcessorFormatter(
            foreign_pre_chain=[
                structlog.stdlib.add_log_level,
                structlog.stdlib.ExtraAdder(),
                structlog.processors.TimeStamper(fmt="iso"),
                inject_module_version,
            ],
            processors=[
                structlog.stdlib.ProcessorFormatter.remove_processors_meta,
                structlog.processors.add_log_level,
                structlog.processors.JSONRenderer(),
            ],
        )
    )

    root_logger.addHandler(handler)
