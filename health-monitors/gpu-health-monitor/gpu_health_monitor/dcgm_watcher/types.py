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


@dataclasses.dataclass
class HealthDetails:
    status: HealthStatus
    entity_failures: dict[int, ErrorDetails]


@dataclasses.dataclass(order=True)
class FieldDetails:
    field_id: str
    value: str


class CallbackInterface(abc.ABC):
    @abc.abstractmethod
    def health_event_occurred(self, health_details: dict[str, HealthDetails], gpu_ids: list[int]):
        pass

    @abc.abstractmethod
    def dcgm_connectivity_failed(self):
        """Called when DCGM connectivity fails during health check."""
        pass
