"""Check 6: Clock and throttle detection.

Detects silent GPU throttling by comparing current clocks against maximum
and querying active throttle reasons. Catches performance degradation
that doesn't generate XID errors.
"""

import logging
import subprocess
from dataclasses import dataclass
from typing import List, Optional

logger = logging.getLogger(__name__)


@dataclass
class ClockStatus:
    """Clock and throttle status for a single GPU."""
    gpu_index: int
    graphics_clock_current: int   # MHz
    graphics_clock_max: int       # MHz
    mem_clock_current: int        # MHz
    mem_clock_max: int            # MHz
    clock_ratio: float            # current/max (graphics)
    throttled: bool
    throttle_reasons: str = ""
    error: Optional[str] = None


class ClockChecker:
    """Detects GPU clock throttling."""

    def __init__(self, throttle_ratio: float = 0.85):
        self._throttle_ratio = throttle_ratio

    # Throttle reasons that are benign (not actual degradation)
    _BENIGN_REASONS = {
        "Not Active",
        "0x0000000000000000",  # No throttle
        "0x0000000000000001",  # GPU Idle — normal when no workload running
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
                    "nsenter", "-t", "1", "-m", "--",
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
                logger.error("nvidia-smi clock query failed: %s", result.stderr.strip())
                return []

            return self._parse_clocks(result.stdout)

        except subprocess.TimeoutExpired:
            logger.error("nvidia-smi clock query timed out")
            return []
        except FileNotFoundError:
            logger.error("nvidia-smi not found")
            return []
        except Exception as e:
            logger.error("Clock check failed: %s", e)
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

                results.append(ClockStatus(
                    gpu_index=idx,
                    graphics_clock_current=gfx_cur,
                    graphics_clock_max=gfx_max,
                    mem_clock_current=mem_cur,
                    mem_clock_max=mem_max,
                    clock_ratio=round(ratio, 3),
                    throttled=throttled,
                ))
            except (ValueError, IndexError, ZeroDivisionError) as e:
                logger.warning("Failed to parse clock line '%s': %s", line, e)

        return results

    def _query_throttle_reasons(self) -> List[dict]:
        """Get active throttle reasons from nvidia-smi."""
        try:
            result = subprocess.run(
                [
                    "nsenter", "-t", "1", "-m", "--",
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
