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

"""NCCL pairwise benchmark implementation.

This module runs an all-reduce between every node pair in the gang. Each pair
uses all GPUs from the two participating nodes, matching the pairwise
node-sanity coverage while keeping the check reusable as an init container.
"""

import logging
import os
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
class PairResult:
    """Result of one node-pair benchmark.

    Attributes:
        src_node: Source node index in the gang.
        dst_node: Destination node index in the gang.
        size_bytes: Message size in bytes.
        size_human: Human-readable size string.
        bus_bw_gbps: Bus bandwidth in GB/s.
        passed: Whether the test met the bandwidth threshold.
    """

    src_node: int
    dst_node: int
    size_bytes: int
    size_human: str
    bus_bw_gbps: float
    passed: bool


@dataclass
class BenchmarkResult:
    """Result of the complete benchmark run.

    Attributes:
        world_size: Total number of distributed processes.
        threshold_gbps: Bandwidth threshold used.
        pairs: Results for each node pair tested.
        passed: Overall pass/fail status.
        min_bus_bw: Minimum bus bandwidth observed.
    """

    world_size: int
    threshold_gbps: float
    pairs: list[PairResult]
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
    """NCCL Pairwise benchmark runner."""

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
            raise ValueError(f"Invalid reduce_op '{reduce_op}'. Supported: {', '.join(REDUCE_OPS)}")
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
                "Starting NCCL Pairwise benchmark",
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

        pair_results: list[PairResult] = []
        min_bus_bw = float("inf")
        all_passed = True

        pairwise_size_mb = os.getenv("PAIRWISE_SIZE_MB", "")
        if pairwise_size_mb:
            message_sizes = [int(pairwise_size_mb) * 1024 * 1024]

        for size_bytes in message_sizes:
            for result in self._run_pairwise_size(size_bytes, num_nodes, gpus_per_node, local_rank):
                pair_results.append(result)
                min_bus_bw = min(min_bus_bw, result.bus_bw_gbps)
                all_passed = all_passed and result.passed

                if rank == 0:
                    log.info(
                        "Pairwise test result",
                        extra={
                            "src_node": result.src_node,
                            "dst_node": result.dst_node,
                            "size": result.size_human,
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
            pairs=pair_results,
            passed=all_passed,
            min_bus_bw=min_bus_bw if min_bus_bw != float("inf") else 0.0,
        )

    def _run_pairwise_size(
        self,
        size_bytes: int,
        num_nodes: int,
        gpus_per_node: int,
        local_rank: int,
    ) -> list[PairResult]:
        """Run all node-pair benchmarks for a message size."""
        rank = dist.get_rank()
        node_id = rank // gpus_per_node
        nodes_per_group = int(os.environ.get("NODES_PER_GROUP", "8"))
        if num_nodes <= nodes_per_group:
            node_ranges = [(0, num_nodes - 1)]
        elif num_nodes % nodes_per_group == 0:
            node_ranges = [
                (group_id * nodes_per_group, (group_id + 1) * nodes_per_group - 1)
                for group_id in range(num_nodes // nodes_per_group)
            ]
        else:
            raise ValueError(f"num_nodes={num_nodes} must be <= {nodes_per_group} or divisible by {nodes_per_group}")

        pair_groups: dict[tuple[int, int], dist.ProcessGroup] = {}

        for start_node, end_node in node_ranges:
            for src_node in range(start_node, end_node + 1):
                for dst_node in range(src_node + 1, end_node + 1):
                    pair_ranks = list(
                        range(
                            src_node * gpus_per_node,
                            (src_node + 1) * gpus_per_node,
                        )
                    )
                    pair_ranks.extend(
                        range(
                            dst_node * gpus_per_node,
                            (dst_node + 1) * gpus_per_node,
                        )
                    )
                    pair_groups[(src_node, dst_node)] = dist.new_group(ranks=pair_ranks)

        results: list[PairResult] = []
        for start_node, end_node in node_ranges:
            for src_node in range(start_node, end_node + 1):
                for dst_node in range(src_node + 1, end_node + 1):
                    group = pair_groups[(src_node, dst_node)]
                    participating = node_id in (src_node, dst_node)
                    local_bus_bw = -1.0

                    if participating:
                        local_bus_bw = self._run_single_pair(size_bytes, local_rank, group)

                    pair_bus_bw = _global_max(local_bus_bw, local_rank)
                    result = PairResult(
                        src_node=src_node,
                        dst_node=dst_node,
                        size_bytes=size_bytes,
                        size_human=format_size(size_bytes),
                        bus_bw_gbps=pair_bus_bw,
                        passed=pair_bus_bw >= self._threshold,
                    )
                    results.append(result)
                    dist.barrier()

        return results

    def _run_single_pair(
        self,
        size_bytes: int,
        local_rank: int,
        group: dist.ProcessGroup,
    ) -> float:
        """Run a single 2-node all-reduce and return median bus bandwidth.

        Args:
            size_bytes: Message size in bytes.
            local_rank: Local GPU index.
            group: Process group containing the two participating nodes.

        Returns:
            Median bus bandwidth across participating ranks.
        """
        group_size = dist.get_world_size(group)
        num_elements = size_bytes // 2  # bfloat16 = 2 bytes
        tensor = torch.randn(
            num_elements,
            dtype=torch.bfloat16,
            device=f"cuda:{local_rank}",
        )

        for _ in range(self._warmup):
            dist.all_reduce(tensor, op=self._reduce_op, group=group)
        torch.cuda.synchronize()

        dist.barrier(group)
        start = torch.cuda.Event(enable_timing=True)
        end = torch.cuda.Event(enable_timing=True)
        start.record()
        for _ in range(self._iters):
            dist.all_reduce(tensor, op=self._reduce_op, group=group)
        end.record()
        end.synchronize()

        elapsed_ms = start.elapsed_time(end)
        bw_factor = 2 * (group_size - 1) / group_size
        bytes_transferred = size_bytes * bw_factor * self._iters
        local_bus_bw = bytes_transferred / (elapsed_ms / 1000) / 1e9
        return _group_median(local_bus_bw, group, local_rank)


def _group_median(
    local_bandwidth_gbps: float,
    group: dist.ProcessGroup,
    local_rank: int,
) -> float:
    """Gather participating-rank samples and return the median."""
    samples = [torch.zeros(1, device=f"cuda:{local_rank}") for _ in range(dist.get_world_size(group))]
    sample = torch.tensor([local_bandwidth_gbps], device=f"cuda:{local_rank}")
    dist.all_gather(samples, sample, group=group)
    values = sorted(float(sample.item()) for sample in samples)
    mid = len(values) // 2
    if len(values) % 2:
        return values[mid]
    return (values[mid - 1] + values[mid]) / 2


def _global_max(value: float, local_rank: int) -> float:
    """Return the maximum scalar across the global process group."""
    tensor = torch.tensor([value], device=f"cuda:{local_rank}")
    dist.all_reduce(tensor, op=dist.ReduceOp.MAX)
    return float(tensor.item())
