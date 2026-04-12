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

import abc
import dataclasses
from typing import Dict, List, Optional


@dataclasses.dataclass
class CheckResult:
    """Result of a single health check.

    Attributes:
        check_name: Identifier for the check type, e.g. "FabricManagerServiceDown", "PcieLinkDegraded".
        is_healthy: True if the check passed without issues.
        is_fatal: True if the failure warrants immediate remediation (e.g. node restart).
        error_codes: Machine-readable error codes for downstream processing.
        message: Human-readable description of the check result.
        entities_impacted: List of impacted entities, e.g. [{"entityType": "GPU", "entityValue": "0"}].
        metadata: Optional key-value metadata attached to the health event.
    """

    check_name: str
    is_healthy: bool
    is_fatal: bool
    error_codes: List[str]
    message: str
    entities_impacted: List[Dict[str, str]]
    metadata: Optional[Dict[str, str]] = None


class CallbackInterface(abc.ABC):
    @abc.abstractmethod
    def health_check_completed(self, results: List[CheckResult]) -> None:
        """Called after each check cycle with the aggregated results from all checkers."""
        pass
