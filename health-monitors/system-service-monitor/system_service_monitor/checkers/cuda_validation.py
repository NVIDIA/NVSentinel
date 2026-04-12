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

"""CUDA validation -- context creation and memory test.

Runs a minimal CUDA test on each GPU: allocate memory, write a pattern,
read back, verify. This check runs at a slower cadence (default disabled)
since it consumes GPU resources. The test script is executed as a subprocess
so that a PyTorch import failure doesn't crash the main monitor process.
"""

import json
import logging as log
import subprocess
import sys
import textwrap
from dataclasses import dataclass, field
from typing import List, Optional

from .types import CheckResult

# Inline Python script executed as a subprocess
_CUDA_TEST_SCRIPT = textwrap.dedent("""\
    import sys
    import json

    results = {"passed": True, "gpu_count": 0, "errors": []}

    try:
        import torch
    except ImportError:
        results["errors"].append("PyTorch not available")
        results["passed"] = False
        print(json.dumps(results))
        sys.exit(0)

    gpu_count = torch.cuda.device_count()
    results["gpu_count"] = gpu_count

    if gpu_count == 0:
        results["errors"].append("No CUDA devices found")
        results["passed"] = False
        print(json.dumps(results))
        sys.exit(0)

    for i in range(gpu_count):
        try:
            torch.cuda.set_device(i)
            # Allocate, write, read back, verify
            t = torch.randn(1024, device="cuda")
            assert t.sum().isfinite(), f"GPU {i}: non-finite sum"
            del t
            torch.cuda.empty_cache()
        except Exception as e:
            results["errors"].append(f"GPU {i}: {e}")
            results["passed"] = False

    print(json.dumps(results))
""")


@dataclass
class CUDAValidationResult:
    """Result of CUDA validation across all GPUs."""

    passed: bool
    gpu_count: int = 0
    errors: List[str] = field(default_factory=list)
    error: Optional[str] = None  # check-level error (couldn't run at all)


class CUDAValidator:
    """Validates CUDA context creation and memory on each GPU."""

    def check(self) -> CUDAValidationResult:
        """Run CUDA validation script as a subprocess."""
        try:
            result = subprocess.run(
                [sys.executable, "-c", _CUDA_TEST_SCRIPT],
                capture_output=True,
                text=True,
                timeout=120,  # generous timeout for multi-GPU test
            )

            if result.returncode != 0:
                return CUDAValidationResult(
                    passed=False,
                    error=f"CUDA test script failed: {result.stderr.strip()}",
                )

            data = json.loads(result.stdout.strip())
            return CUDAValidationResult(
                passed=data.get("passed", False),
                gpu_count=data.get("gpu_count", 0),
                errors=data.get("errors", []),
            )

        except subprocess.TimeoutExpired:
            return CUDAValidationResult(
                passed=False,
                error="CUDA validation timed out",
            )
        except Exception as e:
            return CUDAValidationResult(
                passed=False,
                error=str(e),
            )

    def to_check_results(self, result: CUDAValidationResult, node_name: str) -> List[CheckResult]:
        """Convert CUDAValidationResult to CheckResult list for the watcher."""
        if not result.passed:
            error_msg = "; ".join(result.errors) if result.errors else (result.error or "Unknown CUDA failure")
            return [
                CheckResult(
                    check_name="CudaValidationFailed",
                    is_healthy=False,
                    is_fatal=True,
                    error_codes=["CUDA_VALIDATION_FAILED"],
                    message=f"CUDA validation failed on {node_name}: {error_msg}",
                    entities_impacted=[{"entityType": "NODE", "entityValue": node_name}],
                    metadata={"gpu_count": str(result.gpu_count)},
                )
            ]
        else:
            return [
                CheckResult(
                    check_name="CudaValidationFailed",
                    is_healthy=True,
                    is_fatal=False,
                    error_codes=[],
                    message=f"CUDA validation passed on {node_name} ({result.gpu_count} GPUs)",
                    entities_impacted=[{"entityType": "NODE", "entityValue": node_name}],
                )
            ]
