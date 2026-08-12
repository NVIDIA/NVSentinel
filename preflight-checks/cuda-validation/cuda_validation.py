#!/usr/bin/env python3
# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

"""CUDA validation preflight check (one-shot init-container probe).

Validates that the CUDA runtime can create a context, allocate device
memory, run a trivial kernel, and read the result back on every visible
GPU. Designed to run as a Pod init-container so a failed probe gates the
workload from starting; once the workload runs, this probe has already
exited and consumes no GPU memory.

Behaviour
---------
* Iterates over every CUDA device exposed to the container.
* For each device: sets the device, allocates a small float tensor,
  computes a sum on-device, calls ``torch.cuda.synchronize()``, and
  verifies the result is finite.
* Releases the allocation between devices to keep peak memory minimal
  (``torch.cuda.empty_cache()``).

Exit codes
----------
* ``0``  — every visible GPU passed the probe.
* ``1``  — at least one GPU failed the probe (allocation, kernel, or
  numeric check); error detail printed to stderr.
* ``2``  — environment problem: PyTorch unavailable, CUDA runtime
  unavailable, or zero GPUs visible. Treated as infrastructure rather
  than a per-GPU fault.

Environment variables
---------------------
* ``CUDA_VALIDATION_TENSOR_SIZE``: number of float32 elements to allocate
  per device (default ``1024``). Probe is intentionally tiny so it
  cannot OOM the workload that follows.
* ``LOG_LEVEL``: one of ``debug|info|warn|error`` (default ``info``).

Diagnostics
-----------
All output goes to stdout/stderr — init-container logs are how operators
diagnose preflight failures.
"""

from __future__ import annotations

import logging
import os
import sys
import types
from dataclasses import dataclass


EXIT_OK = 0
EXIT_GPU_FAIL = 1
EXIT_ENV_FAIL = 2

DEFAULT_TENSOR_SIZE = 1024


@dataclass
class GPUResult:
    """Outcome of the probe on a single GPU."""

    index: int
    name: str
    passed: bool
    detail: str


def _setup_logging() -> logging.Logger:
    level_name = os.getenv("LOG_LEVEL", "info").lower().strip()
    level = {
        "debug": logging.DEBUG,
        "info": logging.INFO,
        "warn": logging.WARNING,
        "warning": logging.WARNING,
        "error": logging.ERROR,
    }.get(level_name, logging.INFO)
    logging.basicConfig(
        level=level,
        format="%(asctime)s %(levelname)s preflight-cuda-validation: %(message)s",
        stream=sys.stderr,
    )
    return logging.getLogger("preflight-cuda-validation")


def _tensor_size() -> int:
    raw = os.getenv("CUDA_VALIDATION_TENSOR_SIZE", str(DEFAULT_TENSOR_SIZE))
    try:
        size = int(raw)
    except ValueError:
        return DEFAULT_TENSOR_SIZE
    return size if size > 0 else DEFAULT_TENSOR_SIZE


def _probe_device(torch: types.ModuleType, index: int, size: int) -> GPUResult:
    """Run the per-device probe; never raises — failures are returned."""
    try:
        name = torch.cuda.get_device_name(index)
    except Exception as exc:  # noqa: BLE001 - intentional broad catch
        return GPUResult(index=index, name="?", passed=False, detail=f"get_device_name failed: {exc}")

    try:
        torch.cuda.set_device(index)
        # Allocate a tiny tensor, run a kernel, sync, verify the result.
        # Small allocation keeps probe footprint negligible — the init
        # container exits before the workload starts.
        tensor = torch.randn(size, device=f"cuda:{index}")
        total = tensor.sum()
        torch.cuda.synchronize(index)
        if not total.isfinite().item():
            return GPUResult(index=index, name=name, passed=False, detail="non-finite sum")
        del tensor
        torch.cuda.empty_cache()
        return GPUResult(index=index, name=name, passed=True, detail="ok")
    except Exception as exc:  # noqa: BLE001 - intentional broad catch
        return GPUResult(index=index, name=name, passed=False, detail=str(exc))


def main() -> int:
    log = _setup_logging()

    try:
        import torch  # imported here so missing torch maps to EXIT_ENV_FAIL
    except ImportError as exc:
        log.error("PyTorch is not available in this image: %s", exc)
        return EXIT_ENV_FAIL

    if not torch.cuda.is_available():
        log.error("torch.cuda.is_available() returned False; CUDA runtime not usable")
        return EXIT_ENV_FAIL

    gpu_count = torch.cuda.device_count()
    if gpu_count == 0:
        log.error("torch.cuda.device_count() returned 0; no GPUs visible to container")
        return EXIT_ENV_FAIL

    size = _tensor_size()
    log.info("Probing %d CUDA device(s) with tensor size %d", gpu_count, size)

    results: list[GPUResult] = [_probe_device(torch, i, size) for i in range(gpu_count)]

    failures = [r for r in results if not r.passed]
    for r in results:
        if r.passed:
            log.info("GPU %d (%s): pass", r.index, r.name)
        else:
            log.error("GPU %d (%s): FAIL — %s", r.index, r.name, r.detail)

    if failures:
        log.error("CUDA validation FAILED on %d/%d GPU(s)", len(failures), gpu_count)
        return EXIT_GPU_FAIL

    log.info("CUDA validation passed on %d/%d GPU(s)", gpu_count, gpu_count)
    return EXIT_OK


if __name__ == "__main__":
    sys.exit(main())
