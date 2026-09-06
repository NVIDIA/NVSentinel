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

"""Tests for the CLI entrypoint (arg/env wiring and startup validation)."""

from contextlib import contextmanager
from unittest.mock import MagicMock, patch

import pytest
from click.testing import CliRunner

import system_services_monitor.cli as climod


@contextmanager
def _mock_runtime():
    """Patch out every side-effecting dependency the CLI touches at startup.

    Yields (WatcherMock, EventProcessorMock) so tests can assert on the values
    the CLI threaded through from args/env.
    """
    with patch.object(climod, "FabricManagerWatcher") as watcher, patch.object(
        climod, "PlatformConnectorEventProcessor"
    ) as processor, patch.object(climod, "start_http_server", return_value=(MagicMock(), MagicMock())), patch.object(
        climod, "set_default_structured_logger_with_level"
    ), patch(
        "signal.signal"
    ):
        yield watcher, processor


@pytest.fixture
def runner() -> CliRunner:
    return CliRunner()


class TestArgumentWiring:
    def test_socket_from_cli_arg_happy_path(self, runner: CliRunner) -> None:
        """The socket path passed as a CLI flag reaches the event processor."""
        with _mock_runtime() as (watcher, processor):
            result = runner.invoke(
                climod.cli,
                ["--platform-connector-socket", "/run/pc.sock", "--node-name", "node-a"],
            )

        assert result.exit_code == 0
        assert processor.call_args.kwargs["socket_path"] == "/run/pc.sock"
        assert processor.call_args.kwargs["node_name"] == "node-a"
        watcher.return_value.start.assert_called_once()


class TestEnvVarWiring:
    def test_config_from_env_only(self, runner: CliRunner) -> None:
        """The Helm chart passes config via env only (no args) -- this is the
        CrashLoop regression fix: env vars must satisfy the required socket
        option and override option defaults."""
        env = {
            "PLATFORM_CONNECTOR_SOCKET": "/run/from-env.sock",
            "NODE_NAME": "node-env",
            "CHECK_INTERVAL": "15",
            "METRICS_PORT": "9300",
            "BOOT_GRACE_PERIOD": "120",
            "FLAP_WINDOW": "300",
            "FLAP_THRESHOLD": "5",
        }
        with _mock_runtime() as (watcher, processor):
            result = runner.invoke(climod.cli, [], env=env)

        assert result.exit_code == 0
        assert processor.call_args.kwargs["socket_path"] == "/run/from-env.sock"
        wk = watcher.call_args.kwargs
        assert wk["poll_interval"] == 15  # CHECK_INTERVAL
        assert wk["boot_grace_period"] == 120  # BOOT_GRACE_PERIOD
        assert wk["flap_window"] == 300  # FLAP_WINDOW
        assert wk["flap_threshold"] == 5  # FLAP_THRESHOLD

    def test_cli_arg_overrides_env(self, runner: CliRunner) -> None:
        """An explicit CLI flag wins over the corresponding env var."""
        with _mock_runtime() as (_watcher, processor):
            result = runner.invoke(
                climod.cli,
                ["--platform-connector-socket", "/run/from-cli.sock", "--node-name", "node-a"],
                env={"PLATFORM_CONNECTOR_SOCKET": "/run/from-env.sock"},
            )

        assert result.exit_code == 0
        assert processor.call_args.kwargs["socket_path"] == "/run/from-cli.sock"


class TestStartupValidation:
    def test_missing_socket_fails(self, runner: CliRunner) -> None:
        """With neither --platform-connector-socket nor its env var, Click errors."""
        with _mock_runtime():
            result = runner.invoke(climod.cli, ["--node-name", "node-a"], env={})

        assert result.exit_code != 0
        assert "platform-connector-socket" in result.output

    def test_missing_node_name_exits_nonzero(self, runner: CliRunner) -> None:
        """No node name from flag/NODE_NAME/HOSTNAME is a fatal startup error."""
        with _mock_runtime() as (watcher, _processor):
            result = runner.invoke(
                climod.cli,
                ["--platform-connector-socket", "/run/pc.sock"],
                env={"NODE_NAME": "", "HOSTNAME": ""},
            )

        assert result.exit_code == 1
        watcher.return_value.start.assert_not_called()

    def test_invalid_processing_strategy_exits_nonzero(self, runner: CliRunner) -> None:
        """An unknown --processing-strategy is rejected before the watcher starts."""
        with _mock_runtime() as (watcher, _processor):
            result = runner.invoke(
                climod.cli,
                [
                    "--platform-connector-socket",
                    "/run/pc.sock",
                    "--node-name",
                    "node-a",
                    "--processing-strategy",
                    "NOT_A_STRATEGY",
                ],
            )

        assert result.exit_code == 1
        watcher.return_value.start.assert_not_called()
