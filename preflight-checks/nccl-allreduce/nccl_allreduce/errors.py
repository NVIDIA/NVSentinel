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

"""NCCL All-Reduce error definitions.

Each error has:
- exit_code: Shell exit code returned by the process
- error_code: Mnemonic string for health events (None if no event should be sent)
- is_fatal: Whether this triggers remediation
- recommended_action: What action to recommend in health event
"""

from dataclasses import dataclass
from enum import Enum

from .protos import health_event_pb2 as pb


@dataclass(frozen=True)
class ErrorDef:
    """Definition of an error type."""

    exit_code: int
    error_code: str | None  # None means no health event
    is_fatal: bool
    recommended_action: int


class NCCLError(Enum):
    """NCCL All-Reduce errors with their definitions."""

    # Success
    SUCCESS = ErrorDef(
        exit_code=0,
        error_code=None,
        is_fatal=False,
        recommended_action=pb.RecommendedAction.NONE,
    )

    # Hardware-related errors
    ALLREDUCE_BW_DEGRADED = ErrorDef(
        exit_code=1,
        error_code="NCCL_ALLREDUCE_BW_DEGRADED",
        is_fatal=True,
        recommended_action=pb.RecommendedAction.CONTACT_SUPPORT,
    )
    ALLREDUCE_TIMEOUT = ErrorDef(
        exit_code=1,
        error_code="NCCL_ALLREDUCE_TIMEOUT",
        is_fatal=True,
        recommended_action=pb.RecommendedAction.CONTACT_SUPPORT,
    )
    ALLREDUCE_INIT_FAILED = ErrorDef(
        exit_code=1,
        error_code="NCCL_ALLREDUCE_INIT_FAILED",
        is_fatal=True,
        recommended_action=pb.RecommendedAction.CONTACT_SUPPORT,
    )

    # Infrastructure-related errors (no remediation)
    GANG_CONFIG_ERROR = ErrorDef(
        exit_code=2,
        error_code="NCCL_GANG_CONFIG_ERROR",
        is_fatal=False,
        recommended_action=pb.RecommendedAction.NONE,
    )
    GANG_TIMEOUT = ErrorDef(
        exit_code=3,
        error_code="NCCL_GANG_TIMEOUT",
        is_fatal=False,
        recommended_action=pb.RecommendedAction.NONE,
    )

    # Health reporting failed - no health event can be sent
    HEALTH_REPORT_FAILED = ErrorDef(
        exit_code=4,
        error_code=None,  # Can't send event about this!
        is_fatal=False,
        recommended_action=pb.RecommendedAction.NONE,
    )
