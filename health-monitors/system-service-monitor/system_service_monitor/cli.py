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

import os
import signal
import sys
import logging as log
from importlib.metadata import version as get_package_version
from threading import Event

import click
from prometheus_client import start_http_server

from system_service_monitor.checkers.watcher import FabricManagerWatcher
from system_service_monitor.logger import set_default_structured_logger_with_level
from system_service_monitor.platform_connector.event_processor import PlatformConnectorEventProcessor
from system_service_monitor.protos import health_event_pb2 as platformconnector_pb2


@click.command()
@click.option(
    "--platform-connector-socket",
    type=str,
    required=True,
    help="Unix socket path for gRPC connection to platform-connector",
)
@click.option("--port", type=int, default=9101, help="Prometheus metrics HTTP server port")
@click.option("--poll-interval", type=int, default=30, help="Seconds between check cycles")
@click.option(
    "--node-name",
    type=str,
    default=None,
    help="Node name (defaults to NODE_NAME or HOSTNAME env var)",
)
@click.option("--boot-grace-period", type=int, default=300, help="Seconds after startup to suppress unhealthy alerts")
@click.option("--flap-window", type=int, default=600, help="Seconds window for counting service restarts")
@click.option("--flap-threshold", type=int, default=3, help="Restart count within flap window to flag flapping")
@click.option("--enable-fabric-check/--disable-fabric-check", default=True, help="Enable Fabric Manager service check")
@click.option(
    "--enable-cuda-validation/--disable-cuda-validation",
    default=False,
    help="Enable CUDA context validation (resource intensive, disabled by default)",
)
@click.option(
    "--processing-strategy",
    type=str,
    default="EXECUTE_REMEDIATION",
    help="Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY",
)
@click.option("--verbose", is_flag=True, default=False, help="Enable debug logging")
def cli(
    platform_connector_socket,
    port,
    poll_interval,
    node_name,
    boot_grace_period,
    flap_window,
    flap_threshold,
    enable_fabric_check,
    enable_cuda_validation,
    processing_strategy,
    verbose,
):
    exit = Event()

    # Resolve node name from CLI or environment
    if node_name is None:
        node_name = os.getenv("NODE_NAME", os.getenv("HOSTNAME", ""))
    if not node_name:
        log.fatal("Failed to determine node name from --node-name, NODE_NAME, or HOSTNAME")
        sys.exit(1)

    # Initialize structured JSON logging
    # Version is read from package metadata (set at build time via poetry version)
    version = get_package_version("system-service-monitor")
    log_level = "debug" if verbose else os.getenv("LOG_LEVEL", "info")
    set_default_structured_logger_with_level("system-service-monitor", version, log_level)

    # Validate processing strategy
    try:
        processing_strategy_value = platformconnector_pb2.ProcessingStrategy.Value(processing_strategy)
    except ValueError:
        valid_strategies = list(platformconnector_pb2.ProcessingStrategy.keys())
        log.fatal(f"Invalid processing_strategy '{processing_strategy}'. Valid options are: {valid_strategies}")
        sys.exit(1)

    log.info(f"Event handling strategy configured to: {processing_strategy_value}")
    log.info("Initialization completed")

    # Create event processor (platform-connector gRPC client)
    event_processor = PlatformConnectorEventProcessor(
        socket_path=platform_connector_socket,
        node_name=node_name,
        processing_strategy=processing_strategy_value,
    )

    # Start Prometheus HTTP server
    prom_server, t = start_http_server(port)

    def process_exit_signal(signum, frame):
        exit.set()
        prom_server.shutdown()
        t.join()

    signal.signal(signal.SIGTERM, process_exit_signal)
    signal.signal(signal.SIGINT, process_exit_signal)

    # Create watcher with enabled checks (scoped to non-DCGM signals per ADR-030)
    watcher = FabricManagerWatcher(
        poll_interval=poll_interval,
        callbacks=[event_processor],
        node_name=node_name,
        boot_grace_period=boot_grace_period,
        flap_window=flap_window,
        flap_threshold=flap_threshold,
        enable_fabric_check=enable_fabric_check,
        enable_cuda_validation=enable_cuda_validation,
    )

    watcher.start(exit)


if __name__ == "__main__":
    cli()
