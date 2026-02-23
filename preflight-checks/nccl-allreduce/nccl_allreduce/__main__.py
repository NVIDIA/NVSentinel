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

"""NCCL All-Reduce preflight check entry point.

This module is invoked by torchrun after the entrypoint script has:
1. Waited for gang formation
2. Set up the distributed environment

The flow is:
    entrypoint.py → torchrun → nccl_allreduce.__main__

Environment variables set by torchrun:
    - RANK: Global rank of this process
    - LOCAL_RANK: Local GPU index on this node
    - WORLD_SIZE: Total number of processes
    - MASTER_ADDR: Address of rank 0 node
    - MASTER_PORT: Port for distributed communication

Environment variables set by entrypoint/container:
    - BW_THRESHOLD_GBPS: Minimum acceptable bus bandwidth
    - MESSAGE_SIZES: Comma-separated message sizes to test
    - SKIP_BANDWIDTH_CHECK: Skip bandwidth threshold; pass if benchmark completes
    - PLATFORM_CONNECTOR_SOCKET: gRPC socket for health events
    - NODE_NAME: Kubernetes node name
"""

import logging
import os
import sys
from importlib.metadata import PackageNotFoundError, version

import torch.distributed as dist

from .benchmark import Benchmark, BenchmarkResult, parse_size
from .config import Config
from .errors import NCCLError
from .health import HealthReporter
from .logger import set_default_structured_logger

log = logging.getLogger(__name__)


def get_version() -> str:
    """Get package version."""
    try:
        return version("nvsentinel-nccl-allreduce")
    except PackageNotFoundError:
        return "dev"


def main() -> None:
    """Main entry point for the NCCL all-reduce benchmark.

    This is called by torchrun on each process. Only rank 0 sends
    health events and determines the final exit code.
    """
    log_level = os.getenv("LOG_LEVEL", "info")
    set_default_structured_logger("preflight-nccl-allreduce", get_version(), log_level)

    exit_code = run()
    sys.exit(exit_code)


def run() -> int:
    """Run the multi-node NCCL all-reduce bandwidth benchmark.

    Initialises a NCCL process group (one process per GPU across all gang
    members), executes all-reduce operations at the configured message sizes,
    and compares the measured bus bandwidth against the threshold.

    On rank 0 the result is reported to the platform-connector via gRPC so
    that NVSentinel can quarantine unhealthy nodes.  Non-zero ranks still
    return an appropriate exit code but do not send health events.

    Returns:
        Exit code: 0 when all message sizes meet the bandwidth threshold,
        or a non-zero :class:`NCCLError` exit code on failure.
    """
    try:
        cfg = Config.from_env()
    except ValueError as err:
        log.error("Configuration error", extra={"error": str(err)})
        return NCCLError.GANG_CONFIG_ERROR.value.exit_code

    # Set NCCL defaults if not already set by the container env.
    if "NCCL_DEBUG" not in os.environ:
        os.environ["NCCL_DEBUG"] = "INFO"

    try:
        log.info("Initializing NCCL process group", extra={"backend": "nccl"})
        dist.init_process_group(backend="nccl")
        rank = dist.get_rank()
        log.info(
            "NCCL process group initialized",
            extra={
                "rank": rank,
                "world_size": dist.get_world_size(),
            },
        )
    except Exception as err:
        log.error(
            "Failed to initialize distributed",
            extra={"error": str(err)},
        )
        return _handle_failure(
            cfg,
            NCCLError.ALLREDUCE_INIT_FAILED,
            f"Failed to initialize NCCL: {err}",
        )

    try:
        return _run_benchmark(cfg, rank)
    except RuntimeError as err:
        error_str = str(err)
        if "NCCL" in error_str or "nccl" in error_str.lower():
            log.error(
                "NCCL error during benchmark execution",
                extra={"error": error_str},
            )
            return _handle_failure(
                cfg,
                NCCLError.ALLREDUCE_TIMEOUT,
                f"NCCL communication error: {error_str}",
            )
        raise
    finally:
        try:
            dist.destroy_process_group()
        except Exception:
            pass  # Ignore cleanup errors


def _run_benchmark(cfg: Config, rank: int) -> int:
    """Run the benchmark and report results.

    Args:
        cfg: Configuration object.
        rank: This process's global rank.

    Returns:
        Exit code.
    """
    try:
        message_sizes = [parse_size(s) for s in cfg.message_sizes.split(",")]
    except ValueError as err:
        log.error("Invalid message sizes", extra={"error": str(err)})
        return NCCLError.GANG_CONFIG_ERROR.value.exit_code

    log.info(
        "Running NCCL all-reduce benchmark",
        extra={
            "rank": rank,
            "world_size": dist.get_world_size(),
            "threshold_gbps": cfg.bw_threshold_gbps,
            "skip_bandwidth_check": cfg.skip_bandwidth_check,
            "message_sizes": cfg.message_sizes,
        },
    )

    benchmark = Benchmark(
        threshold_gbps=cfg.bw_threshold_gbps,
        iters=cfg.benchmark_iters,
        warmup=cfg.warmup_iters,
        reduce_op=cfg.reduce_op,
    )

    try:
        result = benchmark.run(message_sizes)
    except Exception as err:
        log.error("Benchmark execution failed", extra={"error": str(err)})
        if rank == 0:
            return _handle_failure(
                cfg,
                NCCLError.ALLREDUCE_TIMEOUT,
                f"NCCL all-reduce benchmark failed: {err}",
            )
        return NCCLError.ALLREDUCE_TIMEOUT.value.exit_code

    passed = result.passed or cfg.skip_bandwidth_check

    # Only rank 0 reports results and determines exit code
    if rank != 0:
        return NCCLError.SUCCESS.value.exit_code if passed else NCCLError.ALLREDUCE_BW_DEGRADED.value.exit_code

    if cfg.skip_bandwidth_check and not result.passed:
        log.info(
            "NCCL all-reduce check PASSED (bandwidth check skipped)",
            extra={"measured_gbps": round(result.min_bus_bw, 2)},
        )

    if passed:
        return _handle_success(cfg, result)

    return _handle_failure(
        cfg,
        NCCLError.ALLREDUCE_BW_DEGRADED,
        f"NCCL all-reduce bus bandwidth {result.min_bus_bw:.2f} GB/s "
        f"is below threshold {cfg.bw_threshold_gbps:.2f} GB/s",
    )


def _handle_success(cfg: Config, result: BenchmarkResult) -> int:
    """Handle successful benchmark completion.

    Args:
        cfg: Configuration object.
        result: Benchmark result.

    Returns:
        Exit code.
    """
    message = (
        f"NCCL all-reduce bus bandwidth {result.min_bus_bw:.2f} GB/s "
        f"meets threshold {cfg.bw_threshold_gbps:.2f} GB/s"
    )

    log.info("NCCL all-reduce check PASSED", extra={"details": message})

    try:
        reporter = HealthReporter(
            socket_path=cfg.connector_socket,
            node_name=cfg.node_name,
            processing_strategy=cfg.processing_strategy,
        )
        reporter.send_success(message)
    except RuntimeError as err:
        log.error(
            "Failed to send health event",
            extra={"error": str(err)},
        )
        return NCCLError.HEALTH_REPORT_FAILED.value.exit_code

    return NCCLError.SUCCESS.value.exit_code


def _handle_failure(cfg: Config, error: NCCLError, message: str) -> int:
    """Handle benchmark failure.

    Args:
        cfg: Configuration object.
        error: The NCCL error that occurred.
        message: Error message.

    Returns:
        Exit code.
    """
    log.error("NCCL all-reduce check FAILED", extra={"details": message})

    try:
        reporter = HealthReporter(
            socket_path=cfg.connector_socket,
            node_name=cfg.node_name,
            processing_strategy=cfg.processing_strategy,
        )
        reporter.send_failure(error, message)
    except RuntimeError as err:
        log.error(
            "Failed to send health event",
            extra={"error": str(err)},
        )
        return NCCLError.HEALTH_REPORT_FAILED.value.exit_code

    return error.value.exit_code


if __name__ == "__main__":
    main()
