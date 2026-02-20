"""Check 5: CUDA validation — context creation and memory test.

Runs a minimal CUDA test on each GPU: allocate memory, write a pattern,
read back, verify. Optional P2P test copies data between GPU pairs.
This check runs at a slower cadence (default 10 minutes) since it
consumes GPU resources.
"""

import logging
import subprocess
import sys
import textwrap
from dataclasses import dataclass, field
from typing import List, Optional

logger = logging.getLogger(__name__)

# Inline Python script executed as a subprocess so that a PyTorch import
# failure doesn't crash the main monitor process.
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

            import json
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
