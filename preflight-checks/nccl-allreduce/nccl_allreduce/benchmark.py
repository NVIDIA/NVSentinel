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

"""NCCL All-Reduce benchmark implementation.

This module runs NCCL all-reduce benchmarks using PyTorch distributed.
It measures bus bandwidth and compares against a configurable threshold.
"""

import logging
import os
import time
from dataclasses import dataclass
from typing import Final

import torch
import torch.distributed as dist

log = logging.getLogger(__name__)

# Supported reduction operations, matching nccl-tests -o flag.
# See: https://github.com/nvidia/nccl-tests#arguments
REDUCE_OPS: Final[dict[str, dist.ReduceOp]] = {
    "sum": dist.ReduceOp.SUM,
    "prod": dist.ReduceOp.PRODUCT,
    "min": dist.ReduceOp.MIN,
    "max": dist.ReduceOp.MAX,
    "avg": dist.ReduceOp.AVG,
}


@dataclass
class TestResult:
    """Result of a single all-reduce test.

    Attributes:
        size_bytes: Message size in bytes.
        size_human: Human-readable size string.
        time_ms: Average time per iteration in milliseconds.
        algo_bw_gbps: Algorithm bandwidth in GB/s.
        bus_bw_gbps: Bus bandwidth in GB/s.
        passed: Whether the test met the bandwidth threshold.
    """

    size_bytes: int
    size_human: str
    time_ms: float
    algo_bw_gbps: float
    bus_bw_gbps: float
    passed: bool


@dataclass
class BenchmarkResult:
    """Result of the complete benchmark run.

    Attributes:
        world_size: Total number of distributed processes.
        threshold_gbps: Bandwidth threshold used.
        tests: Results for each message size tested.
        passed: Overall pass/fail status.
        min_bus_bw: Minimum bus bandwidth observed.
    """

    world_size: int
    threshold_gbps: float
    tests: list[TestResult]
    passed: bool
    min_bus_bw: float


def parse_size(size_str: str) -> int:
    """Parse a size string to bytes.

    Args:
        size_str: Size string like "4G", "4GB", "512M", or "512MB".

    Returns:
        Size in bytes.

    Raises:
        ValueError: If the size string is invalid.
    """
    size_str = size_str.strip().upper()

    if size_str.endswith("GB"):
        return int(float(size_str[:-2]) * 1024**3)
    if size_str.endswith("G"):
        return int(float(size_str[:-1]) * 1024**3)
    if size_str.endswith("MB"):
        return int(float(size_str[:-2]) * 1024**2)
    if size_str.endswith("M"):
        return int(float(size_str[:-1]) * 1024**2)

    raise ValueError(f"Invalid size format: {size_str}. Use G/GB or M/MB suffix.")


def format_size(size_bytes: int) -> str:
    """Format bytes to human-readable string.

    Args:
        size_bytes: Size in bytes.

    Returns:
        Human-readable size string (MB or GB).
    """
    if size_bytes >= 1024**3:
        return f"{size_bytes / 1024**3:.2f} GB"
    return f"{size_bytes / 1024**2:.2f} MB"


class Benchmark:
    """NCCL All-Reduce benchmark runner."""

    def __init__(
        self,
        threshold_gbps: float,
        iters: int = 20,
        warmup: int = 5,
        reduce_op: str = "sum",
    ) -> None:
        """Initialize the benchmark.

        Args:
            threshold_gbps: Minimum acceptable bus bandwidth in GB/s.
            iters: Number of timed iterations per test.
            warmup: Number of warmup iterations before timing.
            reduce_op: Reduction operation name (sum/prod/min/max/avg).
        """
        if iters < 1:
            raise ValueError(f"iters must be >= 1, got {iters}")
        self._threshold = threshold_gbps
        self._iters = iters
        self._warmup = warmup
        op_name = reduce_op.lower().strip()
        if op_name not in REDUCE_OPS:
            raise ValueError(f"Invalid reduce_op '{reduce_op}'. " f"Supported: {', '.join(REDUCE_OPS)}")
        self._reduce_op = REDUCE_OPS[op_name]
        self._reduce_op_name = op_name

    def run(self, message_sizes: list[int]) -> BenchmarkResult:
        """Run the benchmark with the given message sizes.

        Must be called after dist.init_process_group().

        Args:
            message_sizes: List of message sizes in bytes to test.

        Returns:
            BenchmarkResult with all test results.

        Raises:
            RuntimeError: If distributed is not initialized.
        """
        if not dist.is_initialized():
            raise RuntimeError("Distributed not initialized")

        if not message_sizes:
            raise ValueError("message_sizes must be non-empty")

        rank = dist.get_rank()
        world_size = dist.get_world_size()
        local_rank = int(os.environ.get("LOCAL_RANK", 0))
        gpus_per_node = int(os.environ.get("NPROCS_PER_NODE", 8))
        num_nodes = world_size // gpus_per_node if gpus_per_node > 0 else 1

        torch.cuda.set_device(local_rank)

        # Synchronize all processes before starting benchmark
        if rank == 0:
            log.info("Synchronizing all processes before benchmark")
        dist.barrier()
        if rank == 0:
            log.info("All processes synchronized, starting benchmark")

        if rank == 0:
            log.info(
                "Starting NCCL All-Reduce benchmark",
                extra={
                    "reduce_op": self._reduce_op_name,
                    "num_nodes": num_nodes,
                    "gpus_per_node": gpus_per_node,
                    "world_size": world_size,
                    "threshold_gbps": self._threshold,
                    "iters": self._iters,
                    "warmup": self._warmup,
                },
            )

        tests: list[TestResult] = []
        min_bus_bw = float("inf")
        all_passed = True

        for size in message_sizes:
            result = self._run_single(size, world_size, local_rank)
            tests.append(result)

            if result.bus_bw_gbps < min_bus_bw:
                min_bus_bw = result.bus_bw_gbps

            if not result.passed:
                all_passed = False

            if rank == 0:
                log.info(
                    "Test result",
                    extra={
                        "size": result.size_human,
                        "time_ms": round(result.time_ms, 2),
                        "algo_bw_gbps": round(result.algo_bw_gbps, 2),
                        "bus_bw_gbps": round(result.bus_bw_gbps, 2),
                        "passed": result.passed,
                    },
                )

        if rank == 0:
            status = "PASSED" if all_passed else "FAILED"
            log.info(
                f"Benchmark {status}",
                extra={
                    "passed": all_passed,
                    "min_bus_bw_gbps": round(min_bus_bw, 2),
                    "threshold_gbps": self._threshold,
                },
            )

        return BenchmarkResult(
            world_size=world_size,
            threshold_gbps=self._threshold,
            tests=tests,
            passed=all_passed,
            min_bus_bw=min_bus_bw if min_bus_bw != float("inf") else 0.0,
        )

    def _run_single(
        self,
        size_bytes: int,
        world_size: int,
        local_rank: int,
    ) -> TestResult:
        """Run a single all-reduce test.

        Args:
            size_bytes: Message size in bytes.
            world_size: Total number of distributed processes.
            local_rank: Local GPU index.

        Returns:
            TestResult for this message size.
        """
        rank = dist.get_rank()
        num_elements = size_bytes // 4  # float32 = 4 bytes
        tensor = torch.randn(
            num_elements,
            dtype=torch.float32,
            device=f"cuda:{local_rank}",
        )

        # Warmup iterations (not timed)
        if rank == 0:
            log.info(
                "Starting warmup iterations",
                extra={
                    "size_bytes": size_bytes,
                    "warmup_iters": self._warmup,
                },
            )
        for _ in range(self._warmup):
            dist.all_reduce(tensor, op=self._reduce_op)
        torch.cuda.synchronize()
        if rank == 0:
            log.info("Warmup iterations complete")

        # Timed iterations
        if rank == 0:
            log.info(
                "Starting timed iterations",
                extra={"iters": self._iters},
            )
        start = time.perf_counter()
        for _ in range(self._iters):
            dist.all_reduce(tensor, op=self._reduce_op)
        torch.cuda.synchronize()
        elapsed = time.perf_counter() - start
        if rank == 0:
            log.info(
                "Timed iterations complete",
                extra={"elapsed_seconds": round(elapsed, 2)},
            )

        # Calculate bandwidth metrics
        avg_time = elapsed / self._iters
        algo_bw = size_bytes / avg_time / 1e9  # GB/s

        # Bus bandwidth accounts for ring/tree algorithm overhead
        bus_bw = algo_bw * (2 * (world_size - 1) / world_size)

        passed = bus_bw >= self._threshold

        return TestResult(
            size_bytes=size_bytes,
            size_human=format_size(size_bytes),
            time_ms=avg_time * 1000,
            algo_bw_gbps=algo_bw,
            bus_bw_gbps=bus_bw,
            passed=passed,
        )
