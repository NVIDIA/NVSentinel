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

import logging as log
import multiprocessing
import signal
import sys
import time
from collections.abc import Callable
from types import FrameType
from typing import NoReturn

import click
import dcgm_fields
import dcgm_structs
import pydcgm

_LOG_FORMAT = "%(asctime)s %(levelname)s %(message)s"


def _configure_logging() -> None:
    """Configure logging for the init process and readiness probe children."""
    log.basicConfig(level=log.INFO, format=_LOG_FORMAT)


def dcgm_is_ready(dcgm_addr: str) -> bool:
    """Return true after a DCGM connection and GPU discovery both succeed."""
    dcgm_handle = None
    try:
        dcgm_handle = pydcgm.DcgmHandle(
            ipAddress=dcgm_addr,
            opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO,
        )
        dcgm_system = dcgm_handle.GetSystem()
        gpu_ids = dcgm_system.discovery.GetEntityGroupEntities(dcgm_fields.DCGM_FE_GPU, True)
        log.info("DCGM is ready at %s; discovered %d supported GPU(s)", dcgm_addr, len(gpu_ids))
        return True
    except Exception as error:
        log.warning("DCGM is not ready at %s: %s", dcgm_addr, error)
        return False
    finally:
        if dcgm_handle is not None:
            try:
                dcgm_handle.Shutdown()
            except Exception as error:
                log.warning("Failed to close DCGM readiness handle: %s", error)


def _probe_process_entrypoint(dcgm_addr: str) -> None:
    """Run one DCGM readiness attempt in an independently killable process."""
    _configure_logging()
    sys.exit(0 if dcgm_is_ready(dcgm_addr) else 1)


def _stop_process(process: multiprocessing.Process) -> None:
    """Stop a probe process without allowing cleanup to block the init gate."""
    if process.pid is None:
        return
    if process.is_alive():
        process.terminate()
        process.join(timeout=1)
    if process.is_alive():
        process.kill()
        process.join(timeout=1)
    if process.is_alive():
        log.error("DCGM readiness probe process %s did not stop after terminate and kill", process.pid)
        return

    process.close()


def dcgm_is_ready_with_timeout(dcgm_addr: str, connect_timeout_seconds: float) -> bool:
    """Run a functional DCGM readiness check with a hard process timeout."""
    process = multiprocessing.get_context("spawn").Process(
        target=_probe_process_entrypoint,
        args=(dcgm_addr,),
        name="dcgm-readiness-probe",
    )
    try:
        process.start()
        process.join(timeout=connect_timeout_seconds)
        if process.is_alive():
            log.warning(
                "DCGM readiness check at %s exceeded %.1f seconds",
                dcgm_addr,
                connect_timeout_seconds,
            )
            return False
        return process.exitcode == 0
    except Exception as error:
        log.warning("Failed to run DCGM readiness check at %s: %s", dcgm_addr, error)
        return False
    finally:
        _stop_process(process)


def wait_for_dcgm(
    dcgm_addr: str,
    retry_interval_seconds: float,
    connect_timeout_seconds: float,
    sleep: Callable[[float], None] = time.sleep,
) -> None:
    """Wait indefinitely for DCGM to accept a functional API request."""
    while not dcgm_is_ready_with_timeout(dcgm_addr, connect_timeout_seconds):
        sleep(retry_interval_seconds)


def _exit_on_sigterm(_signum: int, _frame: FrameType | None) -> NoReturn:
    """Allow finally blocks to terminate an active probe during pod shutdown."""
    raise SystemExit(0)


@click.command()
@click.option("--dcgm-addr", required=True, help="Host:Port where DCGM is running")
@click.option(
    "--retry-interval-seconds",
    type=click.FloatRange(min=0.0, min_open=True),
    default=5.0,
    show_default=True,
    help="Seconds to wait between DCGM readiness attempts.",
)
@click.option(
    "--connect-timeout-seconds",
    type=click.FloatRange(min=0.0, min_open=True),
    default=10.0,
    show_default=True,
    help="Maximum seconds allowed for one functional DCGM readiness check.",
)
def cli(dcgm_addr: str, retry_interval_seconds: float, connect_timeout_seconds: float) -> None:
    """Block startup until the configured DCGM endpoint is functional."""
    _configure_logging()
    signal.signal(signal.SIGTERM, _exit_on_sigterm)
    log.info(
        "Waiting for DCGM at %s (retry interval %.1fs, connection timeout %.1fs)",
        dcgm_addr,
        retry_interval_seconds,
        connect_timeout_seconds,
    )
    wait_for_dcgm(dcgm_addr, retry_interval_seconds, connect_timeout_seconds)
