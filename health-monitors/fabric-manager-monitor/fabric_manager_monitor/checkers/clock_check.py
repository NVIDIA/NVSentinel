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

"""Clock and throttle detection.

Detects silent GPU throttling by comparing current clocks against maximum
and querying active throttle reasons. Catches performance degradation
that doesn't generate XID errors.

GPU Idle false-positive fix: the throttle reason bitmask 0x0000000000000001
(GPU Idle) and the string "Not Active" are treated as benign -- low clock
ratio when no workload is running is expected and not flagged as throttling.
"""

import logging as log
import subprocess
from dataclasses import dataclass
from typing import List, Optional

from .types import CheckResult


@dataclass
class ClockStatus:
    """Clock and throttle status for a single GPU."""

    gpu_index: int
    graphics_clock_current: int  # MHz
    graphics_clock_max: int  # MHz
    mem_clock_current: int  # MHz
    mem_clock_max: int  # MHz
    clock_ratio: float  # current/max (graphics)
    throttled: bool
    throttle_reasons: str = ""
    error: Optional[str] = None


class ClockChecker:
    """Detects GPU clock throttling via nsenter nvidia-smi."""

    def __init__(self, throttle_ratio: float = 0.85):
        self._throttle_ratio = throttle_ratio

    # Throttle reasons that are benign (not actual degradation)
    _BENIGN_REASONS = {
        "Not Active",
        "0x0000000000000000",  # No throttle
        "0x0000000000000001",  # GPU Idle -- normal when no workload running
    }

    def check(self) -> List[ClockStatus]:
        """Query clocks and throttle reasons for all GPUs."""
        clocks = self._query_clocks()
        reasons = self._query_throttle_reasons()

        # Merge throttle reasons into clock results
        reason_map = {r["gpu_index"]: r["reasons"] for r in reasons}
        for status in clocks:
            reason_str = reason_map.get(status.gpu_index, "")
            status.throttle_reasons = reason_str

            # GPU Idle causes low clock ratio but isn't a real throttle.
            # Only flag as throttled for non-benign reasons.
            if reason_str in self._BENIGN_REASONS:
                status.throttled = False
            elif reason_str:
                status.throttled = True

        return clocks

    def _query_clocks(self) -> List[ClockStatus]:
        """Get current vs max clocks from nvidia-smi."""
        try:
            result = subprocess.run(
                [
                    "nsenter",
                    "-t",
                    "1",
                    "-m",
                    "--",
                    "nvidia-smi",
                    "--query-gpu=index,clocks.current.graphics,clocks.max.graphics,"
                    "clocks.current.memory,clocks.max.memory",
                    "--format=csv,noheader,nounits",
                ],
                capture_output=True,
                text=True,
                timeout=15,
            )

            if result.returncode != 0:
                log.error(f"nvidia-smi clock query failed: {result.stderr.strip()}")
                return []

            return self._parse_clocks(result.stdout)

        except subprocess.TimeoutExpired:
            log.error("nvidia-smi clock query timed out")
            return []
        except FileNotFoundError:
            log.error("nvidia-smi not found")
            return []
        except Exception as e:
            log.error(f"Clock check failed: {e}")
            return []

    def _parse_clocks(self, output: str) -> List[ClockStatus]:
        results = []
        for line in output.strip().splitlines():
            parts = [p.strip() for p in line.split(",")]
            if len(parts) != 5:
                continue
            try:
                idx = int(parts[0])
                gfx_cur = int(parts[1])
                gfx_max = int(parts[2])
                mem_cur = int(parts[3])
                mem_max = int(parts[4])

                ratio = gfx_cur / gfx_max if gfx_max > 0 else 0.0
                throttled = ratio < self._throttle_ratio

                results.append(
                    ClockStatus(
                        gpu_index=idx,
                        graphics_clock_current=gfx_cur,
                        graphics_clock_max=gfx_max,
                        mem_clock_current=mem_cur,
                        mem_clock_max=mem_max,
                        clock_ratio=round(ratio, 3),
                        throttled=throttled,
                    )
                )
            except (ValueError, IndexError, ZeroDivisionError) as e:
                log.warning(f"Failed to parse clock line '{line}': {e}")

        return results

    def _query_throttle_reasons(self) -> List[dict]:
        """Get active throttle reasons from nvidia-smi."""
        try:
            result = subprocess.run(
                [
                    "nsenter",
                    "-t",
                    "1",
                    "-m",
                    "--",
                    "nvidia-smi",
                    "--query-gpu=index,clocks_throttle_reasons.active",
                    "--format=csv,noheader",
                ],
                capture_output=True,
                text=True,
                timeout=15,
            )

            if result.returncode != 0:
                return []

            reasons = []
            for line in result.stdout.strip().splitlines():
                parts = [p.strip() for p in line.split(",", 1)]
                if len(parts) == 2:
                    try:
                        reasons.append({
                            "gpu_index": int(parts[0]),
                            "reasons": parts[1],
                        })
                    except ValueError:
                        pass
            return reasons

        except Exception:
            return []

    def to_check_results(self, statuses: List[ClockStatus], node_name: str) -> List[CheckResult]:
        """Convert ClockStatus list to CheckResult list for the watcher."""
        results = []
        for clk in statuses:
            if clk.throttled:
                results.append(
                    CheckResult(
                        check_name="GpuClockThrottled",
                        is_healthy=False,
                        is_fatal=False,
                        error_codes=["GPU_CLOCK_THROTTLED"],
                        message=(
                            f"GPU {clk.gpu_index} throttled on {node_name}: "
                            f"{clk.graphics_clock_current}/{clk.graphics_clock_max} MHz "
                            f"(ratio={clk.clock_ratio:.2f}, reasons={clk.throttle_reasons})"
                        ),
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(clk.gpu_index)}],
                        metadata={"throttle_reasons": clk.throttle_reasons, "clock_ratio": str(clk.clock_ratio)},
                    )
                )
            else:
                results.append(
                    CheckResult(
                        check_name="GpuClockThrottled",
                        is_healthy=True,
                        is_fatal=False,
                        error_codes=[],
                        message=f"GPU {clk.gpu_index} clocks healthy on {node_name}",
                        entities_impacted=[{"entityType": "GPU", "entityValue": str(clk.gpu_index)}],
                    )
                )
        return results
