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

"""Tests for the gpu-health-monitor CLI, focused on the metrics server binding."""

from unittest.mock import MagicMock, patch

from click.testing import CliRunner

from gpu_health_monitor.cli import cli


def _find_option(param_name):
    for param in cli.params:
        if param.name == param_name:
            return param
    return None


def test_metrics_addr_option_defaults_to_ipv4():
    """--metrics-addr exists and defaults to 0.0.0.0 (no behavior change by default)."""
    option = _find_option("metrics_addr")
    assert option is not None
    assert option.default == "0.0.0.0"
    assert option.required is False


def _write_config(tmp_path):
    config_file = tmp_path / "config.ini"
    config_file.write_text(
        "[logging]\n"
        "[dcgm]\n"
        "PollIntervalSeconds = 60\n"
        "[cli]\n"
        "EnabledEventProcessors = PlatformConnectorEventProcessor\n"
        "[eventprocessors.platformconnector]\n"
        "SocketPath = /tmp/does-not-matter.sock\n"
    )
    mapping_file = tmp_path / "dcgmerrors.csv"
    mapping_file.write_text("0,DCGM_FR_UNKNOWN\n")
    return config_file, mapping_file


def _run_cli(tmp_path, extra_args):
    config_file, mapping_file = _write_config(tmp_path)
    args = [
        "--dcgm-addr",
        "localhost:5555",
        "--dcgm-error-mapping-config-file",
        str(mapping_file),
        "--config-file",
        str(config_file),
        "--port",
        "2112",
        "--state-file",
        str(tmp_path / "statefile"),
        "--dcgm-k8s-service-enabled",
        "false",
        *extra_args,
    ]
    with patch("gpu_health_monitor.cli.start_health_server") as mock_start, patch(
        "gpu_health_monitor.cli._init_event_processor"
    ), patch("gpu_health_monitor.cli.dcgm.DCGMWatcher") as mock_watcher:
        mock_start.return_value = (MagicMock(), MagicMock())
        runner = CliRunner()
        result = runner.invoke(cli, args, env={"NODE_NAME": "test-node"})
    return result, mock_start, mock_watcher


def test_health_server_binds_explicit_metrics_addr(tmp_path):
    """--metrics-addr :: is passed through to the health server as addr='::'."""
    result, mock_start, _ = _run_cli(tmp_path, ["--metrics-addr", "::"])
    assert result.exit_code == 0, result.output
    mock_start.assert_called_once()
    assert mock_start.call_args.args[0] == 2112
    assert mock_start.call_args.kwargs["addr"] == "::"


def test_health_server_defaults_to_ipv4(tmp_path):
    """Without --metrics-addr the server still binds 0.0.0.0 (backward compatible)."""
    result, mock_start, _ = _run_cli(tmp_path, [])
    assert result.exit_code == 0, result.output
    mock_start.assert_called_once()
    assert mock_start.call_args.args[0] == 2112
    assert mock_start.call_args.kwargs["addr"] == "0.0.0.0"
