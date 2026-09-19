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

from pathlib import Path
from threading import Event
from unittest.mock import MagicMock

import pytest
from click.testing import CliRunner

from gpu_health_monitor import cli as cli_module
from gpu_health_monitor.cli import _parse_min_consecutive_polls


class TestParseMinConsecutivePolls:
    @pytest.mark.parametrize("raw", ["", "   ", ",", " , "])
    def test_empty_value_yields_no_thresholds(self, raw: str) -> None:
        assert _parse_min_consecutive_polls(raw) == {}

    def test_single_pair(self) -> None:
        assert _parse_min_consecutive_polls("DCGM_FR_NVLINK_DOWN=2") == {"DCGM_FR_NVLINK_DOWN": 2}

    def test_multiple_pairs_with_surrounding_whitespace(self) -> None:
        raw = " DCGM_FR_NVLINK_DOWN = 2 , DCGM_FR_PCI_REPLAY_RATE=3 "

        assert _parse_min_consecutive_polls(raw) == {
            "DCGM_FR_NVLINK_DOWN": 2,
            "DCGM_FR_PCI_REPLAY_RATE": 3,
        }

    @pytest.mark.parametrize(
        "raw",
        [
            "DCGM_FR_NVLINK_DOWN",  # no separator
            "DCGM_FR_NVLINK_DOWN=",  # no value
            "=2",  # no code
            "DCGM_FR_NVLINK_DOWN=two",  # not a number
            "DCGM_FR_NVLINK_DOWN=2.5",  # not an integer
            "DCGM_FR_NVLINK_DOWN=-2",  # negative
        ],
    )
    def test_malformed_entry_is_skipped_rather_than_fatal(self, raw: str) -> None:
        assert _parse_min_consecutive_polls(raw) == {}

    def test_malformed_entry_does_not_discard_the_valid_ones(self) -> None:
        raw = "DCGM_FR_NVLINK_DOWN=2,garbage,DCGM_FR_PCI_REPLAY_RATE=3"

        assert _parse_min_consecutive_polls(raw) == {
            "DCGM_FR_NVLINK_DOWN": 2,
            "DCGM_FR_PCI_REPLAY_RATE": 3,
        }

    def test_last_value_wins_on_a_duplicated_code(self) -> None:
        assert _parse_min_consecutive_polls("DCGM_FR_NVLINK_DOWN=2,DCGM_FR_NVLINK_DOWN=5") == {"DCGM_FR_NVLINK_DOWN": 5}


@pytest.mark.parametrize("custom_settings", [False, True], ids=["defaults", "configured"])
def test_cli_passes_configuration_to_watcher_and_processor(
    tmp_path: Path, monkeypatch: pytest.MonkeyPatch, custom_settings: bool
) -> None:
    config_file = tmp_path / "monitor.ini"
    mapping_file = tmp_path / "mapping.csv"
    state_file = tmp_path / "state"
    metadata_file = tmp_path / "metadata.json"
    socket_path = str(tmp_path / "connector.sock")
    token_path = str(tmp_path / "token") if custom_settings else ""
    state_file.write_text("test-boot-id")
    mapping_file.write_text("DCGM_FR_NVLINK_DOWN,CONTACT_SUPPORT\n")
    dcgm_options = "ProbeDeadlineSeconds=23\nProbeStoreOnly=false\n" if custom_settings else ""
    config_text = (
        "[logging]\n[dcgm]\nPollIntervalSeconds=17\n"
        f"{dcgm_options}"
        "[cli]\nEnabledEventProcessors=PlatformConnectorEventProcessor\n"
        f"[eventprocessors.platformconnector]\nSocketPath={socket_path}\n"
    )
    if custom_settings:
        config_text += (
            "[dcgmfieldsmonitoring]\n"
            "gputemplimitmonitoringenabled=true\ngputemplimitstoreonly=true\n"
            "gpupowerbrakemonitoringenabled=true\ngpupowerbrakeminconsecutivepolls=4\n"
            "[dcgmhealthcheck]\nImexMonitoringEnabled=true\n"
            "SuppressedErrorCodes=DCGM_FR_CONTAINED_ERROR\n"
            "MinConsecutivePolls=DCGM_FR_NVLINK_DOWN=3\n"
            "ConnectivityFailureEscalationThreshold=9\n"
            "[dcgmconnectivity]\nFailureThreshold=4\nSuccessThreshold=2\n"
        )
    config_file.write_text(config_text)
    started: list[cli_module.dcgm.DCGMWatcher] = []

    def capture_start(watcher: cli_module.dcgm.DCGMWatcher, fields: list[str], exit: Event) -> None:
        assert fields == []
        assert watcher._callbacks[0]._exit is exit
        started.append(watcher)
        watcher._callback_thread_pool.shutdown()

    monkeypatch.setattr(cli_module.dcgm.DCGMWatcher, "start", capture_start)
    monkeypatch.setattr(cli_module, "start_health_server", MagicMock(return_value=(MagicMock(), MagicMock())))
    monkeypatch.setattr(cli_module.signal, "signal", MagicMock())
    monkeypatch.setattr(cli_module, "set_default_structured_logger_with_level", MagicMock())
    monkeypatch.setattr(cli_module, "get_package_version", lambda _: "test")
    result = CliRunner().invoke(
        cli_module.cli,
        [
            "--dcgm-addr",
            "localhost:5555",
            "--dcgm-mode",
            "local-managed" if custom_settings else "remote",
            "--dcgm-error-mapping-config-file",
            str(mapping_file),
            "--config-file",
            str(config_file),
            "--port",
            "8080",
            "--state-file",
            str(state_file),
            "--dcgm-k8s-service-enabled",
            str(custom_settings),
            "--metadata-path",
            str(metadata_file),
            "--processing-strategy",
            "STORE_ONLY" if custom_settings else "EXECUTE_REMEDIATION",
            "--platform-connector-token-path",
            token_path,
            "--suppress-nvlink-down-unbridged-pcie",
            str(custom_settings),
        ],
        env={"NODE_NAME": "test-node"},
    )
    assert result.exit_code == 0, result.output or str(result.exception)
    (watcher,) = started
    assert watcher._addr == "localhost:5555"
    assert watcher._poll_interval_seconds == 17
    assert watcher._dcgm_mode == ("local-managed" if custom_settings else "remote")
    assert watcher._dcgm_k8s_service_enabled is custom_settings
    assert watcher._metadata_reader._path == str(metadata_file)
    assert watcher._thermal_margin_enabled is custom_settings
    assert watcher._power_brake_enabled is custom_settings
    assert watcher._power_brake_min_consecutive_polls == (4 if custom_settings else 1)
    assert watcher._imex_monitoring_enabled is custom_settings
    assert watcher._suppress_unbridged_pcie_nvlink_down is custom_settings
    assert watcher._suppressed_error_codes == (
        frozenset({"DCGM_FR_CONTAINED_ERROR"}) if custom_settings else frozenset()
    )
    assert watcher._health_check_min_consecutive_polls == ({"DCGM_FR_NVLINK_DOWN": 3} if custom_settings else {})
    assert watcher._probe_watchdog._deadline_seconds == (23 if custom_settings else 51)
    (processor,) = watcher._callbacks
    assert processor._node_name == "test-node"
    assert processor._socket_path == socket_path
    assert processor.state_file_path == str(state_file)
    assert processor._metadata_reader._path == str(metadata_file)
    assert processor.dcgm_errors_info_dict == {"DCGM_FR_NVLINK_DOWN": "CONTACT_SUPPORT"}
    assert processor._token_path == (token_path or None)
    assert processor._processing_strategy == (
        cli_module.platformconnector_pb2.STORE_ONLY
        if custom_settings
        else cli_module.platformconnector_pb2.EXECUTE_REMEDIATION
    )
    assert processor._store_only_checks == frozenset(
        {"GpuThermalMarginWatch"} if custom_settings else {"GpuDcgmUnresponsive"}
    )
    assert processor._connectivity_failure_escalation_threshold == (9 if custom_settings else 0)
    assert processor._connectivity_failure_threshold == (4 if custom_settings else 1)
    assert processor._connectivity_success_threshold == (2 if custom_settings else 1)
