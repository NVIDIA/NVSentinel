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

"""DCGM error code to RecommendedAction mapping."""

import csv
import logging
import os
from functools import lru_cache

from .protos import health_event_pb2 as pb

log = logging.getLogger(__name__)


DEFAULT_ERROR_MAPPING_PATH = "/etc/dcgm/dcgmerrorsmapping.csv"


def _load_code_to_name() -> dict[int, str]:
    """Load DCGM error code → name mapping from dcgm_errors module."""
    try:
        import dcgm_errors

        return {v: k for k, v in vars(dcgm_errors).items() if k.startswith("DCGM_FR_") and isinstance(v, int)}
    except ImportError:
        log.warning("dcgm_errors module not available")
        return {}


def _load_name_to_action() -> dict[str, int]:
    """Load DCGM error name → action mapping from CSV file."""
    path = os.getenv("DCGM_ERROR_MAPPING_PATH", DEFAULT_ERROR_MAPPING_PATH)
    if not os.path.exists(path):
        log.warning(f"Error mapping file not found at {path}, using CONTACT_SUPPORT fallback")
        return {}

    mapping = {}
    with open(path) as f:
        for row in csv.reader(f):
            if len(row) >= 2 and row[0] and row[1] in pb.RecommendedAction.keys():
                mapping[row[0].strip()] = pb.RecommendedAction.Value(row[1].strip())

    log.info(f"Loaded {len(mapping)} DCGM error mappings from {path}")
    return mapping


def get_error_name(error_code: int) -> str:
    """Get DCGM error mnemonic name for an error code."""
    return _load_code_to_name().get(error_code, "")


def get_recommended_action(error_code: int) -> int:
    """Get recommended action for a DCGM error code."""
    name = get_error_name(error_code)
    if name:
        return _load_name_to_action().get(name, pb.RecommendedAction.CONTACT_SUPPORT)
    return pb.RecommendedAction.CONTACT_SUPPORT
