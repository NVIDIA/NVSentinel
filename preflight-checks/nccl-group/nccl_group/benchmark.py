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

"""NCCL group collectives benchmark implementation.

This module validates all-reduce, reduce-scatter, and all-gather within
contiguous node groups. Jobs with eight or fewer nodes use one group; larger
jobs are split into eight-node groups to match node-sanity coverage.
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
class CollectiveResult:
    """Result of one collective benchmark.

    Attributes:
        group_id: Group index.
        op: Collective operation name.
        size_bytes: Message size in bytes.
        size_human: Human-readable size string.
        bus_bw_gbps: Bus bandwidth in GB/s.
        passed: Whether the test met the bandwidth threshold.
    """

    group_id: int
    op: str
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
        collectives: Results for each collective tested.
        passed: Overall pass/fail status.
        min_bus_bw: Minimum bus bandwidth observed.
    """

    world_size: int
    threshold_gbps: float
    collectives: list[CollectiveResult]
    passed: bool
    min_bus_bw: float


@dataclass(frozen=True)
class GroupSpec:
    """Ranks participating in one grouped collective phase."""

    group_id: int
    start_node: int
    end_node: int
    process_group: dist.ProcessGroup

    def contains_node(self, node_id: int) -> bool:
        """Return whether the node participates in this group."""
        return self.start_node <= node_id <= self.end_node


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
    """NCCL Group Collectives benchmark runner."""

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
                "Starting NCCL Group Collectives benchmark",
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

        collectives: list[CollectiveResult] = []
        min_bus_bw = float("inf")
        all_passed = True
        groups = _create_groups(num_nodes, gpus_per_node)
        node_id = dist.get_rank() // gpus_per_node

        for size in message_sizes:
            for group in groups:
                for op in ("all_reduce", "reduce_scatter", "all_gather"):
                    participating = group.contains_node(node_id)
                    local_bus_bw = -1.0
                    if participating:
                        local_bus_bw = self._run_collective(
                            op,
                            size,
                            local_rank,
                            group.process_group,
                        )

                    bus_bw = _global_max(local_bus_bw, local_rank)
                    result = CollectiveResult(
                        group_id=group.group_id,
                        op=op,
                        size_bytes=size,
                        size_human=format_size(size),
                        bus_bw_gbps=bus_bw,
                        passed=bus_bw >= self._threshold,
                    )
                    collectives.append(result)
                    min_bus_bw = min(min_bus_bw, bus_bw)
                    all_passed = all_passed and result.passed
                    if dist.get_rank() == 0:
                        log.info(
                            "Group collective result",
                            extra={
                                "group_id": group.group_id,
                                "op": op,
                                "size": result.size_human,
                                "bus_bw_gbps": round(bus_bw, 2),
                                "passed": result.passed,
                            },
                        )
                    dist.barrier()

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
            collectives=collectives,
            passed=all_passed,
            min_bus_bw=min_bus_bw if min_bus_bw != float("inf") else 0.0,
        )

    def _run_collective(
        self,
        op: str,
        size_bytes: int,
        local_rank: int,
        group: dist.ProcessGroup,
    ) -> float:
        """Run a collective and return median bus bandwidth.

        Args:
            op: Collective operation name.
            size_bytes: Message size in bytes.
            local_rank: Local GPU index.
            group: Process group for this grouped phase.

        Returns:
            Median bus bandwidth across participating ranks.
        """
        group_size = dist.get_world_size(group)
        num_elements = size_bytes // 2  # bfloat16 = 2 bytes
        num_elements = (num_elements // group_size) * group_size
        tensor = torch.randn(num_elements, dtype=torch.bfloat16, device=f"cuda:{local_rank}")

        if op == "all_reduce":
            collective_fn = lambda: dist.all_reduce(tensor, op=self._reduce_op, group=group)
            bw_factor = 2 * (group_size - 1) / group_size
        elif op == "reduce_scatter":
            out = torch.empty(num_elements // group_size, dtype=torch.bfloat16, device=f"cuda:{local_rank}")
            collective_fn = lambda: dist.reduce_scatter_tensor(out, tensor, group=group)
            bw_factor = (group_size - 1) / group_size
        elif op == "all_gather":
            inp = torch.randn(num_elements // group_size, dtype=torch.bfloat16, device=f"cuda:{local_rank}")
            out = torch.empty(num_elements, dtype=torch.bfloat16, device=f"cuda:{local_rank}")
            collective_fn = lambda: dist.all_gather_into_tensor(out, inp, group=group)
            bw_factor = (group_size - 1) / group_size
        else:
            raise ValueError(f"Unknown collective op: {op}")

        for _ in range(self._warmup):
            collective_fn()
        torch.cuda.synchronize()

        dist.barrier(group)
        start = torch.cuda.Event(enable_timing=True)
        end = torch.cuda.Event(enable_timing=True)
        start.record()
        for _ in range(self._iters):
            collective_fn()
        end.record()
        end.synchronize()

        elapsed_ms = start.elapsed_time(end)
        bytes_transferred = size_bytes * bw_factor * self._iters
        local_bus_bw = bytes_transferred / (elapsed_ms / 1000) / 1e9
        return _group_median(local_bus_bw, group, local_rank)


def _create_groups(num_nodes: int, gpus_per_node: int) -> list[GroupSpec]:
    """Create contiguous process groups for grouped collective checks."""
    if num_nodes <= 0:
        return []
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

    groups: list[GroupSpec] = []
    for group_id, (start_node, end_node) in enumerate(node_ranges):
        ranks = list(range(start_node * gpus_per_node, (end_node + 1) * gpus_per_node))
        groups.append(
            GroupSpec(
                group_id=group_id,
                start_node=start_node,
                end_node=end_node,
                process_group=dist.new_group(ranks=ranks),
            )
        )
    return groups


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
