"""Check 3: PCIe link health — detects link downtraining.

Compares current PCIe link generation and width against maximum values.
On P5.48xlarge with H100 GPUs, expected: Gen5 x16 for all 8 GPUs.
On P4d.24xlarge with A100 GPUs, expected: Gen4 x16 for all 8 GPUs.
A drop (e.g. Gen5->Gen3 or x16->x8) indicates hardware degradation.
"""

import logging
import subprocess
from dataclasses import dataclass
from typing import List, Optional

logger = logging.getLogger(__name__)


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
    """Checks PCIe link width and generation for all GPUs."""

    def check(self) -> List[PCIeStatus]:
        """Query nvidia-smi for PCIe link status on all GPUs via nsenter."""
        try:
            result = subprocess.run(
                [
                    "nsenter", "-t", "1", "-m", "--",
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
                logger.error("nvidia-smi PCIe query failed: %s", result.stderr.strip())
                return []

            return self._parse_output(result.stdout)

        except subprocess.TimeoutExpired:
            logger.error("nvidia-smi PCIe query timed out")
            return []
        except FileNotFoundError:
            logger.error("nvidia-smi not found")
            return []
        except Exception as e:
            logger.error("PCIe check failed: %s", e)
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

                results.append(PCIeStatus(
                    gpu_index=idx,
                    link_gen_current=gen_cur,
                    link_gen_max=gen_max,
                    link_width_current=width_cur,
                    link_width_max=width_max,
                    degraded=degraded,
                ))
            except (ValueError, IndexError) as e:
                logger.warning("Failed to parse PCIe line '%s': %s", line, e)

        return results
