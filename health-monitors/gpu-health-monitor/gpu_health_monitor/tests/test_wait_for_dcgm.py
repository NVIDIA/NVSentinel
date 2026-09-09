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

from unittest.mock import MagicMock, call, patch

import dcgm_fields
from click.testing import CliRunner

from gpu_health_monitor.wait_for_dcgm import cli, dcgm_is_ready, dcgm_is_ready_with_timeout, wait_for_dcgm


@patch("gpu_health_monitor.wait_for_dcgm.pydcgm.DcgmHandle")
def test_dcgm_is_ready_requires_gpu_discovery(dcgm_handle_factory: MagicMock) -> None:
    """Readiness requires a functional supported-GPU discovery request."""
    dcgm_handle = MagicMock()
    dcgm_handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0, 1]
    dcgm_handle_factory.return_value = dcgm_handle

    assert dcgm_is_ready("dcgm.example:5555") is True

    dcgm_handle_factory.assert_called_once()
    dcgm_handle.GetSystem.return_value.discovery.GetEntityGroupEntities.assert_called_once_with(
        dcgm_fields.DCGM_FE_GPU, True
    )
    dcgm_handle.Shutdown.assert_called_once_with()


@patch("gpu_health_monitor.wait_for_dcgm.pydcgm.DcgmHandle")
def test_dcgm_is_ready_accepts_an_empty_supported_gpu_list(dcgm_handle_factory: MagicMock) -> None:
    """A successful discovery request is ready even when it returns no GPUs."""
    dcgm_handle = MagicMock()
    dcgm_handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = []
    dcgm_handle_factory.return_value = dcgm_handle

    assert dcgm_is_ready("dcgm.example:5555") is True
    dcgm_handle.Shutdown.assert_called_once_with()


@patch("gpu_health_monitor.wait_for_dcgm.pydcgm.DcgmHandle")
def test_dcgm_is_ready_returns_false_and_closes_handle_after_discovery_failure(dcgm_handle_factory: MagicMock) -> None:
    """A failed discovery closes its DCGM handle and reports not ready."""
    dcgm_handle = MagicMock()
    dcgm_handle.GetSystem.return_value.discovery.GetEntityGroupEntities.side_effect = RuntimeError("not ready")
    dcgm_handle_factory.return_value = dcgm_handle

    assert dcgm_is_ready("dcgm.example:5555") is False
    dcgm_handle.Shutdown.assert_called_once_with()


@patch("gpu_health_monitor.wait_for_dcgm.dcgm_is_ready", side_effect=[False, False, True])
def test_probe_process_entrypoint_uses_readiness_result(is_ready: MagicMock) -> None:
    """The child process exit code reflects the DCGM readiness result."""
    from gpu_health_monitor.wait_for_dcgm import _probe_process_entrypoint

    with patch("gpu_health_monitor.wait_for_dcgm.sys.exit", side_effect=SystemExit) as exit_mock:
        for expected_exit_code in [1, 1, 0]:
            try:
                _probe_process_entrypoint("dcgm.example:5555")
            except SystemExit:
                pass
            exit_mock.assert_called_with(expected_exit_code)


@patch("gpu_health_monitor.wait_for_dcgm.multiprocessing.get_context")
def test_dcgm_is_ready_with_timeout_returns_child_result(get_context: MagicMock) -> None:
    """A completed child result is returned and its process is closed."""
    process = MagicMock(pid=123, exitcode=0)
    process.is_alive.return_value = False
    get_context.return_value.Process.return_value = process

    assert dcgm_is_ready_with_timeout("dcgm.example:5555", 4) is True

    get_context.assert_called_once_with("spawn")
    process.join.assert_called_once_with(timeout=4)
    process.close.assert_called_once_with()


@patch("gpu_health_monitor.wait_for_dcgm.multiprocessing.get_context")
def test_dcgm_is_ready_with_timeout_terminates_a_hung_probe(get_context: MagicMock) -> None:
    """A timed-out probe is terminated before another attempt can begin."""
    process = MagicMock(pid=123)
    process.is_alive.side_effect = [True, True, False, False]
    get_context.return_value.Process.return_value = process

    assert dcgm_is_ready_with_timeout("dcgm.example:5555", 4) is False

    process.terminate.assert_called_once_with()
    process.join.assert_has_calls([call(timeout=4), call(timeout=1)])
    process.kill.assert_not_called()
    process.close.assert_called_once_with()


@patch("gpu_health_monitor.wait_for_dcgm.multiprocessing.get_context")
def test_dcgm_is_ready_with_timeout_bounds_cleanup_after_kill(get_context: MagicMock) -> None:
    """Cleanup remains bounded and does not close a child that is still alive."""
    process = MagicMock(pid=123)
    process.is_alive.side_effect = [True, True, True, True]
    get_context.return_value.Process.return_value = process

    assert dcgm_is_ready_with_timeout("dcgm.example:5555", 4) is False

    process.terminate.assert_called_once_with()
    process.kill.assert_called_once_with()
    process.join.assert_has_calls([call(timeout=4), call(timeout=1), call(timeout=1)])
    process.close.assert_not_called()


@patch("gpu_health_monitor.wait_for_dcgm.dcgm_is_ready_with_timeout", side_effect=[False, False, True])
def test_wait_for_dcgm_retries_until_ready(is_ready: MagicMock) -> None:
    """The gate retries failed probes and returns after the first success."""
    sleep = MagicMock()

    wait_for_dcgm("dcgm.example:5555", 2.5, 4, sleep=sleep)

    assert is_ready.call_args_list == [call("dcgm.example:5555", 4)] * 3
    assert sleep.call_args_list == [call(2.5), call(2.5)]


@patch("gpu_health_monitor.wait_for_dcgm.wait_for_dcgm")
def test_cli_uses_documented_timeout_defaults(wait_for_dcgm_mock: MagicMock) -> None:
    """The CLI defaults leave time for DCGM's own connection error to surface."""
    result = CliRunner().invoke(cli, ["--dcgm-addr", "dcgm.example:5555"])

    assert result.exit_code == 0
    wait_for_dcgm_mock.assert_called_once_with("dcgm.example:5555", 5.0, 10.0)


@patch("gpu_health_monitor.wait_for_dcgm.wait_for_dcgm")
def test_cli_accepts_positive_subsecond_intervals(wait_for_dcgm_mock: MagicMock) -> None:
    """Any value greater than zero is accepted for both timing options."""
    result = CliRunner().invoke(
        cli,
        [
            "--dcgm-addr",
            "dcgm.example:5555",
            "--retry-interval-seconds",
            "0.01",
            "--connect-timeout-seconds",
            "0.02",
        ],
    )

    assert result.exit_code == 0
    wait_for_dcgm_mock.assert_called_once_with("dcgm.example:5555", 0.01, 0.02)


def test_cli_rejects_non_positive_retry_interval() -> None:
    """The CLI rejects retry intervals that could create a busy loop."""
    result = CliRunner().invoke(
        cli,
        ["--dcgm-addr", "dcgm.example:5555", "--retry-interval-seconds", "0"],
    )

    assert result.exit_code == 2
    assert "not in the range" in result.output


def test_cli_rejects_non_positive_connect_timeout() -> None:
    """The CLI rejects non-positive per-attempt timeouts."""
    result = CliRunner().invoke(
        cli,
        ["--dcgm-addr", "dcgm.example:5555", "--connect-timeout-seconds", "0"],
    )

    assert result.exit_code == 2
    assert "not in the range" in result.output
