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

import abc, dataclasses, enum, dcgm_structs


class HealthStatus(enum.Enum):
    PASS = dcgm_structs.DCGM_HEALTH_RESULT_PASS
    WARN = dcgm_structs.DCGM_HEALTH_RESULT_WARN
    FAIL = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
    UNKNOWN = -1


@dataclasses.dataclass
class ErrorDetails:
    code: str
    message: str


# GPU keys remain integer IDs for compatibility. NVSwitch keys are
# (entityGroupId, entityId) tuples because DCGM IDs are group-local.
EntityKey = int | tuple[int, int]


@dataclasses.dataclass
class DCGMWatcherConfig:
    addr: str
    poll_interval_seconds: int
    dcgm_k8s_service_enabled: bool
    thermal_margin_enabled: bool = False
    dcgm_mode: str = "remote"
    suppressed_error_codes: frozenset[str] | None = None
    suppress_unbridged_pcie_nvlink_down: bool = False
    probe_deadline_seconds: float = 0.0
    power_brake_enabled: bool = False
    power_brake_min_consecutive_polls: int = 1
    health_check_min_consecutive_polls: dict[str, int] | None = None
    imex_monitoring_enabled: bool = False


@dataclasses.dataclass
class HealthDetails:
    status: HealthStatus
    entity_failures: dict[EntityKey, list[ErrorDetails]]
    # None retains the native DCGM-watch behavior: every monitored GPU has
    # been evaluated. Field-based watches can instead identify only GPUs with
    # a valid sample so an unavailable sample cannot clear an active
    # error for that GPU.
    evaluated_gpu_ids: set[int] | None = None
    # Native DCGM health responses have a fixed incident array. A response at
    # capacity can omit additional incidents, so it is not safe to infer that
    # an absent error code has recovered.
    is_complete: bool = True


@dataclasses.dataclass(frozen=True)
class DCGMFieldMonitor:
    """Configuration for a DCGM field-based health monitor."""

    field_id: int
    watch_name: str
    violation_code: str


@dataclasses.dataclass(order=True)
class FieldDetails:
    field_id: str
    value: str


class CallbackInterface(abc.ABC):
    @abc.abstractmethod
    def health_event_occurred(
        self,
        health_details: dict[str, HealthDetails],
        gpu_ids: list[int],
        switch_ids: list[int] | None = None,
    ) -> None:
        pass

    @abc.abstractmethod
    def dcgm_connectivity_failed(self) -> bool | None:
        """Called when DCGM connectivity fails during health check.

        Return False when delivery failed. True/None means the event was
        delivered or is already active.
        """
        pass

    @abc.abstractmethod
    def dcgm_probe_unresponsive(
        self,
        operation: str,
        elapsed_seconds: float,
        dcgm_mode: str,
    ) -> bool | None:
        """Called when a DCGM probe exceeds its deadline without returning.

        Invoked from the watchdog thread, not the poll loop, because the poll
        loop is by definition blocked when this fires. dcgm_mode distinguishes
        an in-process driver call from a remote service/network call.

        Return False to ask the watchdog to retry delivery on the next poll.
        Return True (or None) once the hang has been recorded.
        """
        pass
