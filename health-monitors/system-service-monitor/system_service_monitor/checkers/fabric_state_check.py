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

"""Per-GPU fabric state query via nvidia-smi.

nvidia-smi reports fabric.state and fabric.status per GPU. This checker
queries those fields to detect Fabric Manager orchestration problems at
the individual GPU level rather than as a single node-level event.

Example nvidia-smi output (csv):
    0, Completed, Success
    1, Completed, Success
    ...
    7, In Progress, In Progress

fabric.state values (mapped to nvidia-smi query):
    Completed  -- FM has finished configuring this GPU's fabric
    In Progress -- FM is still configuring (may be stuck)
    Not Started -- FM has not begun configuring this GPU
    N/A -- fabric state unavailable (non-NVSwitch topology)

State classification (splits the former FM_UNRESPONSIVE into 3 states):
    FM_NOT_STARTED       -- fabric.state == "Not Started"
    FM_REGISTRATION_STUCK -- fabric.state == "In Progress"
    FM_FABRIC_ERROR      -- fabric.state == "Completed" and fabric.status != "Success"
"""

import logging as log
import subprocess
from dataclasses import dataclass
from enum import Enum
from typing import List, Optional

from .types import CheckResult


class FabricFailureState(Enum):
    """Granular FM failure states replacing the monolithic FM_UNRESPONSIVE."""

    FM_NOT_STARTED = "FM_NOT_STARTED"
    FM_REGISTRATION_STUCK = "FM_REGISTRATION_STUCK"
    FM_FABRIC_ERROR = "FM_FABRIC_ERROR"


@dataclass
class GpuFabricState:
    """Fabric Manager orchestration state for a single GPU."""

    gpu_index: int
    fabric_state: str  # e.g. "Completed", "In Progress", "Not Started", "N/A"
    fabric_status: str  # e.g. "Success", "In Progress", error string
    error: Optional[str] = None


class FabricStateChecker:
    """Queries per-GPU Fabric Manager state via nsenter nvidia-smi."""

    def check(self) -> List[GpuFabricState]:
        """Query nvidia-smi for fabric state on all GPUs via nsenter."""
        try:
            result = subprocess.run(
                [
                    "nsenter",
                    "-t",
                    "1",
                    "-m",
                    "--",
                    "nvidia-smi",
                    "--query-gpu=index,fabric.state,fabric.status",
                    "--format=csv,noheader",
                ],
                capture_output=True,
                text=True,
                timeout=15,
            )

            if result.returncode != 0:
                log.error(f"nvidia-smi fabric state query failed: {result.stderr.strip()}")
                return []

            return self._parse_output(result.stdout)

        except subprocess.TimeoutExpired:
            log.error("nvidia-smi fabric state query timed out")
            return []
        except FileNotFoundError:
            log.error("nvidia-smi not found")
            return []
        except Exception as e:
            log.error(f"Fabric state check failed: {e}")
            return []

    def _parse_output(self, output: str) -> List[GpuFabricState]:
        """Parse nvidia-smi CSV output into GpuFabricState objects."""
        results = []
        for line in output.strip().splitlines():
            parts = [p.strip() for p in line.split(",")]
            if len(parts) < 3:
                continue
            try:
                idx = int(parts[0])
                state = parts[1]
                status = parts[2]
                results.append(
                    GpuFabricState(
                        gpu_index=idx,
                        fabric_state=state,
                        fabric_status=status,
                    )
                )
            except (ValueError, IndexError) as e:
                log.warning(f"Failed to parse fabric state line '{line}': {e}")
        return results

    @staticmethod
    def classify_failure(gpu: GpuFabricState) -> Optional[FabricFailureState]:
        """Classify a GPU's fabric state into a specific failure mode.

        Returns None if the GPU is healthy (Completed/Success) or N/A.
        """
        if gpu.fabric_state == "N/A":
            return None
        if gpu.fabric_state == "Completed" and gpu.fabric_status == "Success":
            return None

        if gpu.fabric_state == "Not Started":
            return FabricFailureState.FM_NOT_STARTED
        if gpu.fabric_state == "In Progress":
            return FabricFailureState.FM_REGISTRATION_STUCK
        if gpu.fabric_state == "Completed" and gpu.fabric_status != "Success":
            return FabricFailureState.FM_FABRIC_ERROR

        # Fallback for any unexpected combination
        return FabricFailureState.FM_FABRIC_ERROR

    def to_check_results(self, statuses: List[GpuFabricState], node_name: str) -> List[CheckResult]:
        """Convert GpuFabricState list to CheckResult list.

        Healthy: fabric_state == "Completed" and fabric_status == "Success"
        Also healthy: fabric_state == "N/A" (non-NVSwitch topology)

        Unhealthy states (replaces monolithic FM_UNRESPONSIVE):
          FM_NOT_STARTED        -- FM has not begun configuring this GPU
          FM_REGISTRATION_STUCK -- FM configuration in progress (may be hung)
          FM_FABRIC_ERROR       -- FM completed but with error status
        """
        results = []
        for gpu in statuses:
            # N/A means non-NVSwitch topology -- skip
            if gpu.fabric_state == "N/A":
                continue

            failure = self.classify_failure(gpu)

            if failure is not None:
                results.append(
                    CheckResult(
                        check_name="FabricStateUnhealthy",
                        is_healthy=False,
                        is_fatal=True,
                        error_codes=[failure.value],
                        message=(
                            f"{failure.value} on {node_name} GPU {gpu.gpu_index}: "
                            f"state={gpu.fabric_state}, status={gpu.fabric_status}"
                        ),
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(gpu.gpu_index)}],
                        metadata={
                            "gpu_index": str(gpu.gpu_index),
                            "fabric_state": gpu.fabric_state,
                            "fabric_status": gpu.fabric_status,
                            "failure_class": failure.value,
                        },
                    )
                )
            else:
                results.append(
                    CheckResult(
                        check_name="FabricStateUnhealthy",
                        is_healthy=True,
                        is_fatal=False,
                        error_codes=[],
                        message=f"Fabric state healthy on {node_name} GPU {gpu.gpu_index}",
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(gpu.gpu_index)}],
                        metadata={"gpu_index": str(gpu.gpu_index)},
                    )
                )
        return results
