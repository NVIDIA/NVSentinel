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
import click, configparser, signal, sys
import logging as log
from threading import Event
from prometheus_client import start_http_server
import csv
from .dcgm_watcher import dcgm
from .platform_connector import platform_connector
from gpu_health_monitor.protos import health_event_pb2


def _init_event_processor(
    event_processor_name: str,
    config: configparser.ConfigParser,
    node_name: str,
    exit: Event,
    dcgm_errors_info_dict: dict[str, str],
    state_file_path: str,
    dcgm_health_conditions_categorization_mapping_config: dict[str, str],
):
    platform_connector_config = config["eventprocessors.platformconnector"]
    match event_processor_name:
        case platform_connector.PlatformConnectorEventProcessor.__name__:
            return platform_connector.PlatformConnectorEventProcessor(
                socket_path=platform_connector_config["SocketPath"],
                node_name=node_name,
                exit=exit,
                dcgm_errors_info_dict=dcgm_errors_info_dict,
                state_file_path=state_file_path,
                dcgm_health_conditions_categorization_mapping_config=dcgm_health_conditions_categorization_mapping_config,
            )
        case _:
            log.fatal(f"Unknown event processor {event_processor_name}")
            sys.exit(1)


@click.command()
@click.option("--dcgm-addr", type=str, help="Host:Port where DCGM is running", required=True)
@click.option(
    "--dcgm-error-mapping-config-file", type=click.Path(), help="Path to dcgm errors mapping config file", required=True
)
@click.option("--config-file", type=click.Path(), help="Path to config file", required=True)
@click.option("--port", type=int, help="Port to use for metrics server", required=True)
@click.option("--verbose", type=bool, default=False, help="Enable debug logging", required=False)
@click.option("--state-file", type=click.Path(), help="gpu health monitor state file path", required=True)
@click.option("--dcgm-k8s-service-enabled", type=bool, help="Is DCGM K8s service Enabled", required=True)
def cli(
    dcgm_addr,
    dcgm_error_mapping_config_file,
    config_file,
    port,
    verbose,
    state_file,
    dcgm_k8s_service_enabled,
):
    exit = Event()
    config = configparser.ConfigParser()
    # By default, the Python ConfigParser module reads keys case-insensitively and converts them to lowercase.
    # This is because it's designed to parse Windows INI files, which are typically case-insensitive. To overcome that,
    # added the below optionxform config.This will preserve the case of strings.
    config.optionxform = str
    config.read(config_file)
    logging_config = config["logging"]
    dcgm_config = config["dcgm"]
    cli_config = config["cli"]
    state_file_path = state_file
    node_name = os.getenv("NODE_NAME")
    if node_name == "":
        log.fatal("Failed to fetch nodename from environment variable 'NODE_NAME'")
        sys.exit(1)

    dcgm_errors_info_dict: dict[str, str] = {}
    dcgm_health_conditions_categorization_mapping_config = config["DCGMHealthConditionsCategorizationMapping"]
    log.basicConfig(format=logging_config["LogFormat"], datefmt=logging_config["DateTimeFormat"])
    if verbose:
        log.getLogger().setLevel(log.DEBUG)
    else:
        log.getLogger().setLevel(log.INFO)

    with open(dcgm_error_mapping_config_file, mode="r") as file:
        csv_reader = csv.reader(file)
        for row in csv_reader:
            dcgm_errors_info_dict[row[0]] = row[1]
            log.debug(
                f"dcgm error {row[0]} dcgm_error_name {dcgm_errors_info_dict[row[0]]} dcgm_error_recommended_action {row[1]}"
            )

    log.info("Initialization completed")
    enabled_event_processor_names = cli_config["EnabledEventProcessors"].split(",")
    enabled_event_processors = []
    for event_processor in enabled_event_processor_names:
        enabled_event_processors.append(
            _init_event_processor(
                event_processor,
                config,
                node_name,
                exit,
                dcgm_errors_info_dict,
                state_file_path,
                dcgm_health_conditions_categorization_mapping_config,
            )
        )

    prom_server, t = start_http_server(port)

    def process_exit_signal():
        exit.set()
        prom_server.shutdown()
        t.join()

    signal.signal(signal.SIGTERM, process_exit_signal)
    signal.signal(signal.SIGINT, process_exit_signal)

    dcgm_watcher = dcgm.DCGMWatcher(
        addr=dcgm_addr,
        poll_interval_seconds=int(dcgm_config["PollIntervalSeconds"]),
        callbacks=enabled_event_processors,
        dcgm_k8s_service_enabled=dcgm_k8s_service_enabled,
    )
    dcgm_watcher.start([], exit)


if __name__ == "__main__":
    cli()
