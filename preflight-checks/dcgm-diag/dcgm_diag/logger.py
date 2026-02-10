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

"""Structured JSON logging using structlog."""

import logging
import sys

import structlog


def setup_logging(module: str, version: str, level: str = "info") -> None:
    """Initialize structured JSON logging."""
    log_level = getattr(logging, level.upper(), logging.INFO)

    def add_module_version(logger, method_name, event_dict):
        event_dict["module"] = module
        event_dict["version"] = version
        return event_dict

    root = logging.getLogger()
    root.setLevel(log_level)

    for handler in root.handlers[:]:
        root.removeHandler(handler)

    handler = logging.StreamHandler(sys.stderr)
    handler.setLevel(log_level)
    handler.setFormatter(
        structlog.stdlib.ProcessorFormatter(
            foreign_pre_chain=[
                structlog.stdlib.add_log_level,
                structlog.stdlib.ExtraAdder(),  # Include extra={} fields in output
                structlog.processors.TimeStamper(fmt="iso"),
                add_module_version,
            ],
            processors=[
                structlog.stdlib.ProcessorFormatter.remove_processors_meta,
                structlog.processors.JSONRenderer(),
            ],
        )
    )

    root.addHandler(handler)
