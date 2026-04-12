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
"""

import logging as log
import subprocess
from dataclasses import dataclass
from typing import List, Optional

from .types import CheckResult


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

    def to_check_results(self, statuses: List[GpuFabricState], node_name: str) -> List[CheckResult]:
        """Convert GpuFabricState list to CheckResult list.

        Healthy: fabric_state == "Completed" and fabric_status == "Success"
        Also healthy: fabric_state == "N/A" (non-NVSwitch topology)
        Unhealthy: anything else (stuck, error, not started outside grace)
        """
        results = []
        for gpu in statuses:
            # N/A means non-NVSwitch topology -- not a failure
            if gpu.fabric_state == "N/A":
                continue

            is_healthy = gpu.fabric_state == "Completed" and gpu.fabric_status == "Success"

            if not is_healthy:
                results.append(
                    CheckResult(
                        check_name="FabricStateUnhealthy",
                        is_healthy=False,
                        is_fatal=True,
                        error_codes=["FABRIC_STATE_UNHEALTHY"],
                        message=(
                            f"Fabric state unhealthy on {node_name} GPU {gpu.gpu_index}: "
                            f"state={gpu.fabric_state}, status={gpu.fabric_status}"
                        ),
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(gpu.gpu_index)}],
                        metadata={
                            "gpu_index": str(gpu.gpu_index),
                            "fabric_state": gpu.fabric_state,
                            "fabric_status": gpu.fabric_status,
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
