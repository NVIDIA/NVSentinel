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

"""PCIe link health check -- detects link downtraining.

Compares current PCIe link generation and width against maximum values.
On P5.48xlarge with H100 GPUs, expected: Gen5 x16 for all 8 GPUs.
On P4d.24xlarge with A100 GPUs, expected: Gen4 x16 for all 8 GPUs.
A drop (e.g. Gen5->Gen3 or x16->x8) indicates hardware degradation.
"""

import logging as log
import subprocess
from dataclasses import dataclass
from typing import List, Optional

from .types import CheckResult


@dataclass
class PCIeStatus:
    """PCIe link status for a single GPU."""

    gpu_index: int
    link_gen_current: int
    link_gen_max: int
    link_width_current: int
    link_width_max: int
    degraded: bool
    error: Optional[str] = None


class PCIeChecker:
    """Checks PCIe link width and generation for all GPUs via nsenter nvidia-smi."""

    def check(self) -> List[PCIeStatus]:
        """Query nvidia-smi for PCIe link status on all GPUs via nsenter."""
        try:
            result = subprocess.run(
                [
                    "nsenter",
                    "-t",
                    "1",
                    "-m",
                    "--",
                    "nvidia-smi",
                    "--query-gpu=index,pcie.link.gen.current,pcie.link.gen.max,"
                    "pcie.link.width.current,pcie.link.width.max",
                    "--format=csv,noheader,nounits",
                ],
                capture_output=True,
                text=True,
                timeout=15,
            )

            if result.returncode != 0:
                log.error(f"nvidia-smi PCIe query failed: {result.stderr.strip()}")
                return []

            return self._parse_output(result.stdout)

        except subprocess.TimeoutExpired:
            log.error("nvidia-smi PCIe query timed out")
            return []
        except FileNotFoundError:
            log.error("nvidia-smi not found")
            return []
        except Exception as e:
            log.error(f"PCIe check failed: {e}")
            return []

    def _parse_output(self, output: str) -> List[PCIeStatus]:
        """Parse nvidia-smi CSV output into PCIeStatus objects."""
        results = []
        for line in output.strip().splitlines():
            parts = [p.strip() for p in line.split(",")]
            if len(parts) != 5:
                continue
            try:
                idx = int(parts[0])
                gen_cur = int(parts[1])
                gen_max = int(parts[2])
                width_cur = int(parts[3])
                width_max = int(parts[4])

                degraded = (gen_cur < gen_max) or (width_cur < width_max)

                results.append(
                    PCIeStatus(
                        gpu_index=idx,
                        link_gen_current=gen_cur,
                        link_gen_max=gen_max,
                        link_width_current=width_cur,
                        link_width_max=width_max,
                        degraded=degraded,
                    )
                )
            except (ValueError, IndexError) as e:
                log.warning(f"Failed to parse PCIe line '{line}': {e}")

        return results

    def to_check_results(self, statuses: List[PCIeStatus], node_name: str) -> List[CheckResult]:
        """Convert PCIeStatus list to CheckResult list for the watcher."""
        results = []
        for pcie in statuses:
            if pcie.degraded:
                results.append(
                    CheckResult(
                        check_name="PcieLinkDegraded",
                        is_healthy=False,
                        is_fatal=True,
                        error_codes=["PCIE_LINK_DEGRADED"],
                        message=(
                            f"PCIe link degraded on {node_name} GPU {pcie.gpu_index}: "
                            f"Gen{pcie.link_gen_current} x{pcie.link_width_current} "
                            f"(max Gen{pcie.link_gen_max} x{pcie.link_width_max})"
                        ),
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(pcie.gpu_index)}],
                    )
                )
            else:
                results.append(
                    CheckResult(
                        check_name="PcieLinkDegraded",
                        is_healthy=True,
                        is_fatal=False,
                        error_codes=[],
                        message=f"PCIe link healthy on {node_name} GPU {pcie.gpu_index}",
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(pcie.gpu_index)}],
                    )
                )
        return results
