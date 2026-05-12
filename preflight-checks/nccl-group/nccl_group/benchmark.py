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

"""NCCL group benchmark implementation.

This module validates all-reduce, reduce-scatter, and all-gather within
contiguous node groups. Jobs with eight or fewer nodes use one group; larger
jobs are split into groups to match node-sanity coverage. It also supports
DeepEP internode benchmarks over the same grouped process topology.
"""

import importlib
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

_DEEPEP_NUM_TOKENS: Final = 8192
_DEEPEP_HIDDEN_DIM: Final = 3072
_DEEPEP_NUM_EXPERTS: Final = 128
_DEEPEP_TOPK: Final = 8
_DEEPEP_WARMUP_ITERATIONS: Final = 10
_DEEPEP_ITERATIONS: Final = 50


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


@dataclass
class DeepEPResult:
    """Result of one grouped DeepEP benchmark."""

    group_id: int
    total_bw_gbps: float
    dispatch_bw_gbps: float
    combine_bw_gbps: float
    layout_ms: float
    dispatch_ms: float
    combine_ms: float
    rdma_bytes_per_iter: int
    passed: bool
    error: str | None = None


@dataclass
class DeepEPBenchmarkResult:
    """Result of the complete DeepEP benchmark run."""

    world_size: int
    dispatch_threshold_gbps: float
    combine_threshold_gbps: float
    total_threshold_gbps: float
    groups: list[DeepEPResult]
    passed: bool
    min_total_bw: float
    min_dispatch_bw: float
    min_combine_bw: float


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

    def run_deepep(
        self,
        *,
        dispatch_threshold_gbps: float,
        combine_threshold_gbps: float,
        total_threshold_gbps: float,
        per_node: bool = False,
    ) -> DeepEPBenchmarkResult:
        """Run grouped DeepEP internode benchmarks."""
        if not dist.is_initialized():
            raise RuntimeError("Distributed not initialized")

        rank = dist.get_rank()
        world_size = dist.get_world_size()
        local_rank = int(os.environ.get("LOCAL_RANK", 0))
        gpus_per_node = int(os.environ.get("NPROCS_PER_NODE", 8))
        num_nodes = world_size // gpus_per_node if gpus_per_node > 0 else 1

        torch.cuda.set_device(local_rank)
        dist.barrier()

        if rank == 0:
            log.info(
                "Starting DeepEP group benchmark",
                extra={
                    "num_nodes": num_nodes,
                    "gpus_per_node": gpus_per_node,
                    "world_size": world_size,
                    "per_node": per_node,
                    "dispatch_threshold_gbps": dispatch_threshold_gbps,
                    "combine_threshold_gbps": combine_threshold_gbps,
                    "total_threshold_gbps": total_threshold_gbps,
                },
            )

        groups = _create_groups(num_nodes, gpus_per_node, nodes_per_group_override=1 if per_node else None)
        node_id = rank // gpus_per_node
        results: list[DeepEPResult] = []
        all_passed = True
        min_total_bw = float("inf")
        min_dispatch_bw = float("inf")
        min_combine_bw = float("inf")

        for group in groups:
            participating = group.contains_node(node_id)
            local_result = _LocalDeepEPResult()
            if participating:
                local_result = _benchmark_deepep(group.process_group, local_rank, per_node=per_node)

            error_flag = _global_max_int(1 if local_result.error else 0, local_rank)
            total_bw = _global_max(local_result.total_bw_gbps, local_rank)
            dispatch_bw = _global_max(local_result.dispatch_bw_gbps, local_rank)
            combine_bw = _global_max(local_result.combine_bw_gbps, local_rank)
            layout_ms = _global_max(local_result.layout_ms, local_rank)
            dispatch_ms = _global_max(local_result.dispatch_ms, local_rank)
            combine_ms = _global_max(local_result.combine_ms, local_rank)
            rdma_bytes = _global_max_int(local_result.rdma_bytes_per_iter, local_rank)
            error = "error" if error_flag else None

            passed = (
                error_flag == 0
                and dispatch_bw >= dispatch_threshold_gbps
                and combine_bw >= combine_threshold_gbps
                and total_bw >= total_threshold_gbps
            )
            result = DeepEPResult(
                group_id=group.group_id,
                total_bw_gbps=total_bw,
                dispatch_bw_gbps=dispatch_bw,
                combine_bw_gbps=combine_bw,
                layout_ms=layout_ms,
                dispatch_ms=dispatch_ms,
                combine_ms=combine_ms,
                rdma_bytes_per_iter=rdma_bytes,
                passed=passed,
                error=error,
            )
            results.append(result)
            all_passed = all_passed and result.passed
            min_total_bw = min(min_total_bw, total_bw)
            min_dispatch_bw = min(min_dispatch_bw, dispatch_bw)
            min_combine_bw = min(min_combine_bw, combine_bw)

            if rank == 0:
                log.info(
                    "DeepEP group result",
                    extra={
                        "group_id": result.group_id,
                        "total_bw_gbps": round(result.total_bw_gbps, 2),
                        "dispatch_bw_gbps": round(result.dispatch_bw_gbps, 2),
                        "combine_bw_gbps": round(result.combine_bw_gbps, 2),
                        "rdma_bytes_per_iter": result.rdma_bytes_per_iter,
                        "passed": result.passed,
                        "error": result.error,
                    },
                )
            dist.barrier()

        return DeepEPBenchmarkResult(
            world_size=world_size,
            dispatch_threshold_gbps=dispatch_threshold_gbps,
            combine_threshold_gbps=combine_threshold_gbps,
            total_threshold_gbps=total_threshold_gbps,
            groups=results,
            passed=all_passed,
            min_total_bw=min_total_bw if min_total_bw != float("inf") else 0.0,
            min_dispatch_bw=min_dispatch_bw if min_dispatch_bw != float("inf") else 0.0,
            min_combine_bw=min_combine_bw if min_combine_bw != float("inf") else 0.0,
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

            def collective_fn() -> None:
                dist.all_reduce(tensor, op=self._reduce_op, group=group)

            bw_factor = 2 * (group_size - 1) / group_size
        elif op == "reduce_scatter":
            out = torch.empty(num_elements // group_size, dtype=torch.bfloat16, device=f"cuda:{local_rank}")

            def collective_fn() -> None:
                dist.reduce_scatter_tensor(out, tensor, group=group)

            bw_factor = (group_size - 1) / group_size
        elif op == "all_gather":
            inp = torch.randn(num_elements // group_size, dtype=torch.bfloat16, device=f"cuda:{local_rank}")
            out = torch.empty(num_elements, dtype=torch.bfloat16, device=f"cuda:{local_rank}")

            def collective_fn() -> None:
                dist.all_gather_into_tensor(out, inp, group=group)

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


def _create_groups(
    num_nodes: int,
    gpus_per_node: int,
    *,
    nodes_per_group_override: int | None = None,
) -> list[GroupSpec]:
    """Create contiguous process groups for grouped collective checks."""
    if num_nodes <= 0:
        return []
    nodes_per_group = nodes_per_group_override or int(os.environ.get("NODES_PER_GROUP", "8"))
    if nodes_per_group not in (1, 2, 4, 8):
        raise ValueError(f"NODES_PER_GROUP must be 1, 2, 4, or 8, got {nodes_per_group}")
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


@dataclass
class _LocalDeepEPResult:
    """Local DeepEP measurement before global aggregation."""

    total_bw_gbps: float = -1.0
    dispatch_bw_gbps: float = -1.0
    combine_bw_gbps: float = -1.0
    layout_ms: float = -1.0
    dispatch_ms: float = -1.0
    combine_ms: float = -1.0
    rdma_bytes_per_iter: int = 0
    error: str | None = None


def _benchmark_deepep(
    group: dist.ProcessGroup,
    local_rank: int,
    *,
    per_node: bool,
) -> _LocalDeepEPResult:
    """Benchmark DeepEP dispatch/combine over one process group."""
    try:
        deep_ep = importlib.import_module("deep_ep")
    except ImportError:
        return _LocalDeepEPResult(error="import")

    try:
        buffer = deep_ep.Buffer(
            group=group,
            num_nvl_bytes=256 * 1024 * 1024 if per_node else 1024 * 1024 * 1024,
            num_rdma_bytes=0 if per_node else 256 * 1024 * 1024,
            low_latency_mode=False,
        )
    except Exception as err:  # noqa: BLE001
        return _LocalDeepEPResult(error=f"buffer: {err}")

    tensor = torch.randn(
        _DEEPEP_NUM_TOKENS,
        _DEEPEP_HIDDEN_DIM,
        device=f"cuda:{local_rank}",
        dtype=torch.bfloat16,
    )
    topk_idx = torch.randint(
        0,
        _DEEPEP_NUM_EXPERTS,
        (_DEEPEP_NUM_TOKENS, _DEEPEP_TOPK),
        device=f"cuda:{local_rank}",
        dtype=torch.int64,
    )
    topk_weights = torch.rand(
        _DEEPEP_NUM_TOKENS,
        _DEEPEP_TOPK,
        device=f"cuda:{local_rank}",
        dtype=torch.float32,
    )

    for _ in range(_DEEPEP_WARMUP_ITERATIONS):
        layout = buffer.get_dispatch_layout(topk_idx, _DEEPEP_NUM_EXPERTS)
        num_tokens_per_rank = layout[0]
        num_tokens_per_rdma_rank = None if per_node else layout[1]
        num_tokens_per_expert = layout[2]
        is_token_in_rank = layout[3]
        recv_x, _, recv_topk_weights, _, handle, _ = buffer.dispatch(
            tensor,
            None,
            num_tokens_per_rank,
            num_tokens_per_rdma_rank,
            is_token_in_rank,
            num_tokens_per_expert,
            topk_idx,
            topk_weights,
        )
        if recv_x is not None and recv_x.size(0) > 0:
            combine_x = torch.randn_like(recv_x)
            _, _, _ = buffer.combine(combine_x, handle, recv_topk_weights)
        torch.cuda.synchronize()

    layout_start = torch.cuda.Event(enable_timing=True)
    layout_end = torch.cuda.Event(enable_timing=True)
    dispatch_start = torch.cuda.Event(enable_timing=True)
    dispatch_end = torch.cuda.Event(enable_timing=True)
    combine_start = torch.cuda.Event(enable_timing=True)
    combine_end = torch.cuda.Event(enable_timing=True)

    total_layout_ms = 0.0
    total_dispatch_ms = 0.0
    total_combine_ms = 0.0
    total_rdma_tokens = 0

    for _ in range(_DEEPEP_ITERATIONS):
        layout_start.record()
        layout = buffer.get_dispatch_layout(topk_idx, _DEEPEP_NUM_EXPERTS)
        num_tokens_per_rank = layout[0]
        num_tokens_per_rdma_rank = None if per_node else layout[1]
        num_tokens_per_expert = layout[2]
        is_token_in_rank = layout[3]
        layout_end.record()
        layout_end.synchronize()
        total_layout_ms += layout_start.elapsed_time(layout_end)

        if per_node:
            total_rdma_tokens += int(num_tokens_per_rank.sum().item())
        elif num_tokens_per_rdma_rank is not None:
            total_rdma_tokens += int(num_tokens_per_rdma_rank.sum().item())

        dispatch_start.record()
        recv_x, _, recv_topk_weights, _, handle, _ = buffer.dispatch(
            tensor,
            None,
            num_tokens_per_rank,
            num_tokens_per_rdma_rank,
            is_token_in_rank,
            num_tokens_per_expert,
            topk_idx,
            topk_weights,
        )
        dispatch_end.record()
        dispatch_end.synchronize()
        total_dispatch_ms += dispatch_start.elapsed_time(dispatch_end)

        if recv_x is not None and recv_x.size(0) > 0:
            combine_x = torch.randn_like(recv_x)
            combine_start.record()
            _, _, _ = buffer.combine(combine_x, handle, recv_topk_weights)
            combine_end.record()
            combine_end.synchronize()
            total_combine_ms += combine_start.elapsed_time(combine_end)

    bytes_per_token = _DEEPEP_HIDDEN_DIM * 2
    rdma_bytes_per_iter = (total_rdma_tokens // _DEEPEP_ITERATIONS) * bytes_per_token
    total_rdma_bytes = total_rdma_tokens * bytes_per_token * 2
    total_ms = total_layout_ms + total_dispatch_ms + total_combine_ms
    dispatch_bw = (total_rdma_tokens * bytes_per_token) / (total_dispatch_ms / 1000) / 1e9 if total_dispatch_ms else 0.0
    combine_bw = (total_rdma_tokens * bytes_per_token) / (total_combine_ms / 1000) / 1e9 if total_combine_ms else 0.0
    total_bw = total_rdma_bytes / (total_ms / 1000) / 1e9 if total_ms else 0.0

    return _LocalDeepEPResult(
        total_bw_gbps=_group_median(total_bw, group, local_rank),
        dispatch_bw_gbps=_group_median(dispatch_bw, group, local_rank),
        combine_bw_gbps=_group_median(combine_bw, group, local_rank),
        layout_ms=_group_median(total_layout_ms, group, local_rank),
        dispatch_ms=_group_median(total_dispatch_ms, group, local_rank),
        combine_ms=_group_median(total_combine_ms, group, local_rank),
        rdma_bytes_per_iter=rdma_bytes_per_iter,
    )


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


def _global_max_int(value: int, local_rank: int) -> int:
    """Return the maximum integer scalar across the global process group."""
    tensor = torch.tensor([value], device=f"cuda:{local_rank}", dtype=torch.int64)
    dist.all_reduce(tensor, op=dist.ReduceOp.MAX)
    return int(tensor.item())
