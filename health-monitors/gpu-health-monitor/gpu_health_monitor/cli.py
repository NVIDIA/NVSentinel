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
from importlib.metadata import version as get_package_version
from threading import Event
from gpu_health_monitor.healthz import start_server as start_health_server
import csv
from .dcgm_watcher import dcgm
from .platform_connector import platform_connector
from . import metrics
from gpu_health_monitor.metadata import MetadataReader
from gpu_health_monitor.protos import health_event_pb2 as platformconnector_pb2
from gpu_health_monitor.logger import set_default_structured_logger_with_level


def _parse_min_consecutive_polls(raw: str) -> dict[str, int]:
    """Parse ``CODE=N`` pairs from the INI value into a per-error-code threshold map.

    INI has no nested sections, so the chart renders the values map as a comma
    separated list. A malformed entry is logged and skipped rather than taken as a
    reason to refuse to start, since the fail-open outcome is today's behaviour for
    that one code.
    """
    thresholds: dict[str, int] = {}

    for entry in raw.split(","):
        entry = entry.strip()
        if not entry:
            continue

        code, separator, polls = entry.partition("=")
        code, polls = code.strip(), polls.strip()

        # isascii guards the isdigit/int mismatch: "²".isdigit() is True but int("²")
        # raises, which would crash startup instead of skipping the entry.
        if not separator or not code or not (polls.isascii() and polls.isdigit()):
            log.warning(f"Ignoring malformed MinConsecutivePolls entry {entry!r}; expected CODE=N")
            continue

        thresholds[code] = int(polls)

    return thresholds


def _init_event_processor(
    event_processor_name: str,
    config: configparser.ConfigParser,
    node_name: str,
    exit: Event,
    dcgm_errors_info_dict: dict[str, str],
    state_file_path: str,
    metadata_path: str,
    processing_strategy: platformconnector_pb2.ProcessingStrategy,
    store_only_checks: frozenset[str],
    connectivity_failure_escalation_threshold: int,
    platform_connector_token_path: str,
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
                metadata_path=metadata_path,
                processing_strategy=processing_strategy,
                store_only_checks=store_only_checks,
                connectivity_failure_escalation_threshold=connectivity_failure_escalation_threshold,
                token_path=platform_connector_token_path or None,
            )
        case _:
            log.fatal(f"Unknown event processor {event_processor_name}")
            sys.exit(1)


@click.command()
@click.option("--dcgm-addr", type=str, help="Host:Port where DCGM is running", required=True)
@click.option(
    "--dcgm-mode",
    type=click.Choice(["remote", "local-managed"]),
    default="remote",
    show_default=True,
    help="DCGM connection mode. remote connects to a remote hostengine; local-managed runs an in-process embedded hostengine with a loopback listener.",
)
@click.option(
    "--dcgm-error-mapping-config-file", type=click.Path(), help="Path to dcgm errors mapping config file", required=True
)
@click.option("--config-file", type=click.Path(), help="Path to config file", required=True)
@click.option("--port", type=int, help="Port to use for metrics server", required=True)
@click.option("--verbose", type=bool, default=False, help="Enable debug logging", required=False)
@click.option("--state-file", type=click.Path(), help="gpu health monitor state file path", required=True)
@click.option("--dcgm-k8s-service-enabled", type=bool, help="Is DCGM K8s service Enabled", required=True)
@click.option(
    "--metadata-path",
    type=click.Path(),
    default="/var/lib/nvsentinel/gpu_metadata.json",
    help="Path to GPU metadata JSON file",
    required=False,
)
@click.option(
    "--processing-strategy",
    type=str,
    default="EXECUTE_REMEDIATION",
    help="Event processing strategy: EXECUTE_REMEDIATION or STORE_ONLY",
    required=False,
)
@click.option(
    "--platform-connector-token-path",
    type=click.Path(),
    default="",
    envvar="PLATFORM_CONNECTOR_TOKEN_PATH",
    help=(
        "Path to a projected ServiceAccount token presented as a bearer credential "
        "when publishing health events to platform-connector. Defaults to the "
        "PLATFORM_CONNECTOR_TOKEN_PATH environment variable; empty disables it."
    ),
    required=False,
)
@click.option(
    "--suppress-nvlink-down-unbridged-pcie",
    type=bool,
    default=False,
    required=False,
    help=(
        "Operator assertion that bridge-capable PCIe GPUs (A100/H100 PCIe) in this fleet "
        "run without NVLink bridges by design. When true, DCGM_FR_NVLINK_DOWN is suppressed "
        "on PCIe-named GPUs whose metadata shows zero active NVLink links. Leave false if "
        "any pool uses NVLink bridges: an unbridged card and a card whose bridge was dead at "
        "metadata-collection time are indistinguishable, so enabling this could mask a "
        "bridge failure present at boot. GPUs with no NVLink silicon (L40, A40) are always "
        "suppressed regardless of this flag."
    ),
)
def cli(
    dcgm_addr,
    dcgm_mode,
    dcgm_error_mapping_config_file,
    config_file,
    port,
    verbose,
    state_file,
    dcgm_k8s_service_enabled,
    metadata_path,
    processing_strategy,
    platform_connector_token_path,
    suppress_nvlink_down_unbridged_pcie,
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

    # Initialize structured JSON logging
    # Version is read from package metadata (set at build time via poetry version)
    version = get_package_version("gpu-health-monitor")
    log_level = "debug" if verbose else os.getenv("LOG_LEVEL", "info")
    set_default_structured_logger_with_level("gpu-health-monitor", version, log_level)

    with open(dcgm_error_mapping_config_file, mode="r") as file:
        csv_reader = csv.reader(file)
        for row in csv_reader:
            dcgm_errors_info_dict[row[0]] = row[1]
            log.debug(
                f"dcgm error {row[0]} dcgm_error_name {dcgm_errors_info_dict[row[0]]} dcgm_error_recommended_action {row[1]}"
            )

    try:
        processing_strategy_value = platformconnector_pb2.ProcessingStrategy.Value(processing_strategy)
    except ValueError:
        valid_strategies = list(platformconnector_pb2.ProcessingStrategy.keys())
        log.fatal(f"Invalid processing_strategy '{processing_strategy}'. " f"Valid options are: {valid_strategies}")
        sys.exit(1)

    log.info(f"Event handling strategy configured to: {processing_strategy_value}")
    log.info(
        "Platform-connector token auth: %s",
        f"enabled (path={platform_connector_token_path})" if platform_connector_token_path else "disabled",
    )

    metrics.set_flag("store_only_mode", processing_strategy == "STORE_ONLY")
    metrics.set_flag("dcgm_k8s_service_enabled", dcgm_k8s_service_enabled)
    metrics.set_flag("dcgm_local_managed", dcgm_mode == "local-managed")

    log.info("Initialization completed")

    thermal_margin_enabled = False
    thermal_margin_store_only = False
    power_brake_enabled = False
    power_brake_store_only = False
    power_brake_min_consecutive_polls = 1
    if config.has_section("dcgmfieldsmonitoring"):
        fields_monitoring_config = config["dcgmfieldsmonitoring"]
        thermal_margin_enabled = fields_monitoring_config.getboolean("gputemplimitmonitoringenabled", fallback=False)
        thermal_margin_store_only = fields_monitoring_config.getboolean("gputemplimitstoreonly", fallback=False)
        log.info(
            "GpuThermalMarginWatch field monitor: enabled=%s store_only=%s",
            thermal_margin_enabled,
            thermal_margin_store_only,
        )

        power_brake_enabled = fields_monitoring_config.getboolean("gpupowerbrakemonitoringenabled", fallback=False)
        power_brake_store_only = fields_monitoring_config.getboolean("gpupowerbrakestoreonly", fallback=False)
        power_brake_min_consecutive_polls = fields_monitoring_config.getint(
            "gpupowerbrakeminconsecutivepolls", fallback=1
        )
        log.info(
            "GpuPowerBrakeWatch field monitor: enabled=%s store_only=%s min_consecutive_polls=%s",
            power_brake_enabled,
            power_brake_store_only,
            power_brake_min_consecutive_polls,
        )

    # Per-check observe-only set: when store-only is enabled the new
    # GpuThermalMarginWatch emits STORE_ONLY events (persisted + exported as
    # metrics but excluded from the remediation pipeline, so no node condition
    # or cordon) while every other DCGM check keeps the process-wide strategy.
    store_only_checks = set()
    if thermal_margin_store_only:
        store_only_checks.add("GpuThermalMarginWatch")
    if power_brake_store_only:
        store_only_checks.add("GpuPowerBrakeWatch")

    # GpuDcgmUnresponsive recommends a node reboot, so it ships observe-only
    # and has to be turned on deliberately per fleet.
    probe_store_only = dcgm_config.getboolean("ProbeStoreOnly", fallback=True)
    if probe_store_only:
        store_only_checks.add("GpuDcgmUnresponsive")
    log.info("GpuDcgmUnresponsive check: store_only=%s", probe_store_only)

    store_only_checks = frozenset(store_only_checks)

    suppressed_error_codes = frozenset()
    connectivity_failure_escalation_threshold = 0
    health_check_min_consecutive_polls: dict[str, int] = {}
    if config.has_section("dcgmhealthcheck"):
        health_check_config = config["dcgmhealthcheck"]
        suppressed_error_codes_raw = health_check_config.get("SuppressedErrorCodes", fallback="")
        suppressed_error_codes = frozenset(
            code.strip() for code in suppressed_error_codes_raw.split(",") if code.strip()
        )
        if suppressed_error_codes:
            log.info(f"DCGM error codes suppressed via config: {sorted(suppressed_error_codes)}")

        connectivity_failure_escalation_threshold = health_check_config.getint(
            "ConnectivityFailureEscalationThreshold", fallback=0
        )
        if connectivity_failure_escalation_threshold > 0:
            log.info(
                "DCGM connectivity failures escalate to RESTART_BM after %d consecutive cycles",
                connectivity_failure_escalation_threshold,
            )

        health_check_min_consecutive_polls = _parse_min_consecutive_polls(
            health_check_config.get("MinConsecutivePolls", fallback="")
        )
        if health_check_min_consecutive_polls:
            log.info(f"DCGM incident debounce thresholds: {health_check_min_consecutive_polls}")

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
                metadata_path,
                processing_strategy_value,
                store_only_checks,
                connectivity_failure_escalation_threshold,
                platform_connector_token_path,
            )
        )

    metadata_reader = MetadataReader(metadata_path)

    poll_interval = int(dcgm_config["PollIntervalSeconds"])
    # Defaults to the /healthz staleness window, so the watchdog reports at the
    # same moment the loop is declared stale. Critical event delivery is bounded
    # separately so it remains inside the liveness restart budget. DCGM does not
    # expose a documented fixed RPC timeout, so fleets should validate this
    # deadline in STORE_ONLY mode before enabling remediation. Set to 0 to disable.
    probe_deadline_seconds = dcgm_config.getfloat("ProbeDeadlineSeconds", fallback=poll_interval * 3)
    prom_server, t = start_health_server(port, staleness_seconds=poll_interval * 3)

    def process_exit_signal(signum, frame):
        exit.set()
        prom_server.shutdown()
        t.join()

    signal.signal(signal.SIGTERM, process_exit_signal)
    signal.signal(signal.SIGINT, process_exit_signal)

    dcgm_watcher = dcgm.DCGMWatcher(
        addr=dcgm_addr,
        poll_interval_seconds=poll_interval,
        callbacks=enabled_event_processors,
        dcgm_k8s_service_enabled=dcgm_k8s_service_enabled,
        thermal_margin_enabled=thermal_margin_enabled,
        metadata_reader=metadata_reader,
        dcgm_mode=dcgm_mode,
        suppressed_error_codes=suppressed_error_codes,
        suppress_unbridged_pcie_nvlink_down=suppress_nvlink_down_unbridged_pcie,
        probe_deadline_seconds=probe_deadline_seconds,
        power_brake_enabled=power_brake_enabled,
        power_brake_min_consecutive_polls=power_brake_min_consecutive_polls,
        health_check_min_consecutive_polls=health_check_min_consecutive_polls,
    )
    dcgm_watcher.start([], exit)


if __name__ == "__main__":
    cli()
