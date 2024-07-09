import click, configparser, signal, sys
import logging as log
from threading import Event
from prometheus_client import start_http_server

from .dcgm_watcher import dcgm
from .platform_connector import platform_connector


def _init_event_processor(event_processor_name: str, config: configparser.ConfigParser, exit: Event):
    platform_connector_config = config["eventprocessors.platformconnector"]
    match event_processor_name:
        case platform_connector.PlatformConnectorEventProcessor.__name__:
            return platform_connector.PlatformConnectorEventProcessor(
                socket_path=platform_connector_config["SocketPath"],
                exit=exit,
            )
        case _:
            log.fatal(f"Unknown event processor {event_processor_name}")
            sys.exit(1)


@click.command()
@click.option("--dcgm-addr", type=str, default="localhost:5555", help="Host:Port where DCGM is running")
@click.option("--config-file", type=click.Path(), help="Path to config file", required=True)
@click.option("--port", type=int, default=8000, help="Port to use for metrics server", required=True)
@click.option("-v", "--verbose", type=bool, default=False, is_flag=True, help="Enable debug logging")
def cli(dcgm_addr, config_file, port, verbose):
    exit = Event()
    config = configparser.ConfigParser()
    config.read(config_file)
    logging_config = config["logging"]
    dcgm_config = config["dcgm"]
    cli_config = config["cli"]

    log.basicConfig(format=logging_config["LogFormat"], datefmt=logging_config["DateTimeFormat"])
    if verbose:
        log.getLogger().setLevel(log.DEBUG)
    else:
        log.getLogger().setLevel(log.INFO)
    prom_server, t = start_http_server(port)
    log.info("Initialization completed")

    enabled_event_processor_names = cli_config["EnabledEventProcessors"].split(",")
    enabled_event_processors = []
    for event_processor in enabled_event_processor_names:
        enabled_event_processors.append(_init_event_processor(event_processor, config, exit))

    def process_exit_signal(signum, frame):
        exit.set()
        prom_server.shutdown()
        t.join()

    signal.signal(signal.SIGTERM, process_exit_signal)
    signal.signal(signal.SIGINT, process_exit_signal)

    fields_to_monitor = [x for x in dcgm_config["FieldsToMonitor"].split(",")]
    dcgm_watcher = dcgm.DCGMWatcher(
        addr=dcgm_addr,
        poll_interval_seconds=int(dcgm_config["PollIntervalSeconds"]),
        callbacks=enabled_event_processors,
    )
    dcgm_watcher.start(fields_to_monitor, exit)


if __name__ == "__main__":
    cli()
