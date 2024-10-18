import os
import threading
import click, configparser, signal, sys
import logging as log
from threading import Event
from prometheus_client import start_http_server
import csv
from .dcgm_watcher import dcgm
from .platform_connector import platform_connector
from .platform_connector.protos import platformconnector_pb2
from gpu_health_monitor.nvml_parser.nvml_xid_parser import DummyNvmlXidParser


def _init_event_processor(
    event_processor_name: str,
    config: configparser.ConfigParser,
    node_name: str,
    exit: Event,
    xid_errors_info_dict: dict[str, platform_connector.XidErrorsMappingDetails],
    xid_error_recommend_action_mapping: dict[str, platformconnector_pb2.RecommenedAction],
    xid_errors_batch_processing_interval: int,
    xid_errors_batch_processing_enabled: bool,
    nvml_xid_parser,
    state_file_path: str,
):
    platform_connector_config = config["eventprocessors.platformconnector"]
    match event_processor_name:
        case platform_connector.PlatformConnectorEventProcessor.__name__:
            return platform_connector.PlatformConnectorEventProcessor(
                socket_path=platform_connector_config["SocketPath"],
                node_name=node_name,
                exit=exit,
                xid_errors_info_dict=xid_errors_info_dict,
                xid_errors_recommend_action_mapping=xid_error_recommend_action_mapping,
                xid_errors_batch_processing_interval=xid_errors_batch_processing_interval,
                xid_errors_batch_processing_enabled=xid_errors_batch_processing_enabled,
                nvml_xid_parser=nvml_xid_parser,
                state_file_path=state_file_path,
            )
        case _:
            log.fatal(f"Unknown event processor {event_processor_name}")
            sys.exit(1)


def create_recommend_action_mapping_from_xid_error_to_platform_connector(data):
    xid_error_recommend_action_connector_mapping: dict[str, platformconnector_pb2.RecommenedAction] = {}
    if isinstance(data, dict):
        for key, value in data.items():
            xid_error_recommend_action_connector_mapping[key] = value

    return xid_error_recommend_action_connector_mapping


@click.command()
@click.option("--dcgm-addr", type=str, default="localhost:5555", help="Host:Port where DCGM is running")
@click.option(
    "--xid-error-mapping-config-file", type=click.Path(), help="Path to xid errors mapping config file", required=True
)
@click.option("--config-file", type=click.Path(), help="Path to config file", required=True)
@click.option("--port", type=int, help="Port to use for metrics server", required=True)
@click.option("--verbose", type=bool, default=False, help="Enable debug logging", required=False)
@click.option("--state-file", type=click.Path(), help="gpu health monitor state file path", required=True)
@click.option("--dcgm-k8s-service-enabled", type=bool, help="Is DCGM K8s service Enabled", required=True)
def cli(dcgm_addr, xid_error_mapping_config_file, config_file, port, verbose, state_file, dcgm_k8s_service_enabled):
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

    xid_error_recommend_action_mapping_config = config["xiderrorrecommendactiontoplatformconnectormapping"]
    xid_errors_batch_processing_enabled = config.getboolean("xiderrorsconfig", "XidErrorsBatchProcessingEnabled")
    xid_errors_batch_processing_interval = config.getint("xiderrorsconfig", "XidErrorsBatchProcessingInterval")
    xid_errors_info_dict: dict[str, platform_connector.XidErrorsMappingDetails] = {}
    log.basicConfig(format=logging_config["LogFormat"], datefmt=logging_config["DateTimeFormat"])
    if verbose:
        log.getLogger().setLevel(log.DEBUG)
    else:
        log.getLogger().setLevel(log.INFO)
    with open(xid_error_mapping_config_file, mode="r") as file:
        # Create a CSV reader
        csv_reader = csv.reader(file)
        for row in csv_reader:
            xid_errors_info_dict[row[0]] = platform_connector.XidErrorsMappingDetails(
                name=row[1], recommended_action=row[2], fatal=row[3]
            )
            log.debug(
                f"xid error {row[0]} xid_error_name {xid_errors_info_dict[row[0]].name} xid_error_recommendation {xid_errors_info_dict[row[0]].recommended_action} xid_error_fatal {xid_errors_info_dict[row[0]].fatal}"
            )

    xid_error_recommend_action_mapping: dict[str, platformconnector_pb2.RecommenedAction] = {}
    for key, value in xid_error_recommend_action_mapping_config.items():
        xid_error_recommend_action_mapping[key] = int(value)

    for key, value in xid_error_recommend_action_mapping.items():
        log.debug(f"{key}={value}")

    prom_server, t = start_http_server(port)
    log.info("Initialization completed")
    nvml_xid_parser = DummyNvmlXidParser()
    enabled_event_processor_names = cli_config["EnabledEventProcessors"].split(",")
    enabled_event_processors = []
    for event_processor in enabled_event_processor_names:
        enabled_event_processors.append(
            _init_event_processor(
                event_processor,
                config,
                node_name,
                exit,
                xid_errors_info_dict,
                xid_error_recommend_action_mapping,
                int(xid_errors_batch_processing_interval),
                xid_errors_batch_processing_enabled,
                nvml_xid_parser,
                state_file_path,
            )
        )

    def process_exit_signal(signum, frame):
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
