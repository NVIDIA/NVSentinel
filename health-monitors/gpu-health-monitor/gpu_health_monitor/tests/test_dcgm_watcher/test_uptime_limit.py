# Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

"""Tests for GpuUptimeLimitWatch, the opt-in reboot before a known uptime-limited defect."""

from collections.abc import Iterator
from pathlib import Path
from threading import Event
from types import SimpleNamespace
from unittest.mock import MagicMock

import pytest

from gpu_health_monitor.dcgm_watcher import dcgm

AFFECTED_DRIVER = "570.133.20"
FIXED_DRIVER = "590.10.01"
AFFECTED_VBIOS = "97.00.82.00.4B"
OTHER_VBIOS = "97.00.90.00.01"
LIMIT_HOURS = 1512.0
HOUR = 3600.0


def _make_watcher(
    enabled: bool = True,
    driver_versions: frozenset[str] = frozenset({AFFECTED_DRIVER}),
    vbios_versions: frozenset[str] = frozenset(),
    max_uptime_hours: float = LIMIT_HOURS,
) -> dcgm.DCGMWatcher:
    return dcgm.DCGMWatcher(
        dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            uptime_limit_enabled=enabled,
            uptime_limit_affected_driver_versions=driver_versions,
            uptime_limit_affected_vbios_versions=vbios_versions,
            uptime_limit_max_uptime_hours=max_uptime_hours,
        ),
        callbacks=[],
    )


@pytest.fixture
def watchers() -> Iterator[list[dcgm.DCGMWatcher]]:
    created: list[dcgm.DCGMWatcher] = []
    yield created
    for watcher in created:
        watcher._callback_thread_pool.shutdown()


@pytest.fixture
def uptime(monkeypatch: pytest.MonkeyPatch) -> MagicMock:
    reader = MagicMock(return_value=(LIMIT_HOURS + 1) * HOUR)
    monkeypatch.setattr(dcgm, "_read_host_uptime_seconds", reader)
    return reader


def _versions(driver: str = AFFECTED_DRIVER, vbios: str = OTHER_VBIOS) -> dcgm.GpuVersions:
    return dcgm.GpuVersions(driver=driver, vbios=vbios)


def test_disabled_check_reads_nothing_and_returns_none(watchers, uptime) -> None:
    watcher = _make_watcher(enabled=False)
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions()}

    assert watcher._evaluate_uptime_limit([0]) is None
    uptime.assert_not_called()


def test_affected_driver_past_limit_fails_gpu(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions()}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.FAIL
    assert result.evaluated_gpu_ids == {0}
    (failure,) = result.entity_failures[0]
    assert failure.code == dcgm.UPTIME_LIMIT_ERROR_CODE
    assert f"driver {AFFECTED_DRIVER}" in failure.message
    assert "1513.0 hours" in failure.message
    assert "1512 hours" in failure.message


def test_affected_vbios_past_limit_fails_gpu(watchers, uptime) -> None:
    watcher = _make_watcher(driver_versions=frozenset(), vbios_versions=frozenset({AFFECTED_VBIOS}))
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions(driver=FIXED_DRIVER, vbios=AFFECTED_VBIOS)}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.FAIL
    assert f"vBIOS {AFFECTED_VBIOS}" in result.entity_failures[0][0].message


@pytest.mark.parametrize("uptime_hours", [0.1, LIMIT_HOURS - 1, LIMIT_HOURS])
def test_affected_gpu_within_limit_is_healthy(watchers, uptime, uptime_hours: float) -> None:
    """An affected GPU must not fail before the node passes the limit."""
    uptime.return_value = uptime_hours * HOUR
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions()}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.entity_failures == {}
    assert result.evaluated_gpu_ids == {0}


def test_unaffected_gpu_past_limit_is_healthy(watchers, uptime) -> None:
    """A long uptime alone must not fail a GPU that runs no listed version."""
    uptime.return_value = 10 * LIMIT_HOURS * HOUR
    watcher = _make_watcher(vbios_versions=frozenset({AFFECTED_VBIOS}))
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions(driver=FIXED_DRIVER, vbios=OTHER_VBIOS)}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.entity_failures == {}
    assert result.evaluated_gpu_ids == {0}


def test_version_match_ignores_case_and_surrounding_space(watchers, uptime) -> None:
    watcher = _make_watcher(
        driver_versions=frozenset({f"  {AFFECTED_DRIVER} "}), vbios_versions=frozenset({AFFECTED_VBIOS.lower()})
    )
    watchers.append(watcher)
    watcher._gpu_versions = {
        0: _versions(driver=AFFECTED_DRIVER, vbios=OTHER_VBIOS),
        1: _versions(driver=FIXED_DRIVER, vbios=AFFECTED_VBIOS),
    }

    result = watcher._evaluate_uptime_limit([0, 1])

    assert set(result.entity_failures) == {0, 1}


def test_version_prefix_does_not_match(watchers, uptime) -> None:
    """Matching is exact, so one listed version does not cover a longer one."""
    watcher = _make_watcher(driver_versions=frozenset({"570.133"}))
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions(driver=AFFECTED_DRIVER)}

    assert watcher._evaluate_uptime_limit([0]).entity_failures == {}


def test_only_affected_gpus_fail_on_a_mixed_node(watchers, uptime) -> None:
    watcher = _make_watcher(driver_versions=frozenset(), vbios_versions=frozenset({AFFECTED_VBIOS}))
    watchers.append(watcher)
    watcher._gpu_versions = {
        0: _versions(driver=FIXED_DRIVER, vbios=AFFECTED_VBIOS),
        1: _versions(driver=FIXED_DRIVER, vbios=OTHER_VBIOS),
    }

    result = watcher._evaluate_uptime_limit([0, 1])

    assert result.status == dcgm.types.HealthStatus.FAIL
    assert set(result.entity_failures) == {0}
    assert result.evaluated_gpu_ids == {0, 1}


def test_gpu_with_unknown_versions_is_not_evaluated(watchers, uptime) -> None:
    """A GPU without versions is neither failed nor reported healthy."""
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions(), 2: _versions(driver="", vbios="")}

    result = watcher._evaluate_uptime_limit([0, 1, 2])

    assert result.evaluated_gpu_ids == {0}
    assert set(result.entity_failures) == {0}


def test_blank_version_with_a_configured_list_is_not_evaluated(watchers, uptime) -> None:
    """Without a match, a blank vBIOS could still be a listed one, so the GPU is not known to be healthy."""
    watcher = _make_watcher(vbios_versions=frozenset({AFFECTED_VBIOS}))
    watchers.append(watcher)
    watcher._gpu_versions = {
        0: _versions(driver=FIXED_DRIVER, vbios=""),
        1: _versions(driver=AFFECTED_DRIVER, vbios=""),
    }

    result = watcher._evaluate_uptime_limit([0, 1])

    assert result.evaluated_gpu_ids == {1}
    assert set(result.entity_failures) == {1}


def test_blank_version_without_a_configured_list_is_evaluated(watchers, uptime) -> None:
    """A blank vBIOS does not matter when no vBIOS version is listed."""
    watcher = _make_watcher(driver_versions=frozenset({AFFECTED_DRIVER}), vbios_versions=frozenset())
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions(driver=FIXED_DRIVER, vbios="")}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.evaluated_gpu_ids == {0}


def test_no_known_versions_returns_none(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_versions = {}

    assert watcher._evaluate_uptime_limit([0, 1]) is None


@pytest.mark.parametrize("error", [OSError("no procfs"), ValueError("bad uptime"), IndexError("empty")])
def test_unreadable_uptime_returns_none(watchers, uptime, error: Exception) -> None:
    """Without the uptime the check cannot decide, so it neither fails nor clears a GPU."""
    uptime.side_effect = error
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_versions = {0: _versions()}

    assert watcher._evaluate_uptime_limit([0]) is None


def test_read_host_uptime_seconds_reads_first_field(tmp_path: Path) -> None:
    uptime_file = tmp_path / "uptime"
    uptime_file.write_text("5745600.42 45000000.10\n")

    assert dcgm._read_host_uptime_seconds(str(uptime_file)) == pytest.approx(5745600.42)


def test_read_host_uptime_seconds_rejects_empty_file(tmp_path: Path) -> None:
    uptime_file = tmp_path / "uptime"
    uptime_file.write_text("")

    with pytest.raises(IndexError):
        dcgm._read_host_uptime_seconds(str(uptime_file))


def _dcgm_handle(identifiers_by_gpu: dict[int, SimpleNamespace]) -> MagicMock:
    handle = MagicMock()
    system = handle.GetSystem.return_value
    system.discovery.GetEntityGroupEntities.return_value = list(identifiers_by_gpu)
    system.discovery.GetGpuAttributes.side_effect = lambda gpu: SimpleNamespace(identifiers=identifiers_by_gpu[gpu])
    return handle


def test_get_gpu_versions_decodes_dcgm_identifiers(watchers) -> None:
    """DCGM identifiers are ctypes char arrays, which read back as bytes."""
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle(
        {
            0: SimpleNamespace(driverVersion=AFFECTED_DRIVER.encode(), vbios=AFFECTED_VBIOS.encode()),
            1: SimpleNamespace(driverVersion=f" {AFFECTED_DRIVER}\x00", vbios=""),
        }
    )

    assert watcher._get_gpu_versions(handle) == {
        0: dcgm.GpuVersions(driver=AFFECTED_DRIVER, vbios=AFFECTED_VBIOS),
        1: dcgm.GpuVersions(driver=AFFECTED_DRIVER, vbios=""),
    }


def test_get_gpu_versions_keeps_gpus_read_before_and_after_a_failure(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    good = SimpleNamespace(driverVersion=AFFECTED_DRIVER.encode(), vbios=AFFECTED_VBIOS.encode())

    def attributes(gpu: int) -> SimpleNamespace:
        if gpu == 1:
            raise RuntimeError("attributes unavailable")
        return SimpleNamespace(identifiers=good)

    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0, 1, 2]
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = attributes
    before = dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get()

    versions = watcher._get_gpu_versions(handle)

    assert set(versions) == {0, 2}
    assert dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get() == before + 1


def test_get_gpu_versions_discovery_failure_returns_empty_map(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.side_effect = RuntimeError("no discovery")

    assert watcher._get_gpu_versions(handle) == {}


def test_get_gpu_versions_failure_returns_empty_map(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = (
        dcgm.dcgm_structs.DCGMError_FunctionNotFound()
    )
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0]
    before = dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get()

    assert watcher._get_gpu_versions(handle) == {}
    assert dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get() == before + 1


@pytest.mark.parametrize("enabled", [True, False])
def test_initialize_reads_versions_only_when_enabled(watchers, enabled: bool) -> None:
    watcher = _make_watcher(enabled=enabled)
    watchers.append(watcher)
    dcgm_group = MagicMock()
    dcgm_group.GetGpuIds.return_value = [0]
    watcher._create_dcgm_group_with_all_entities = MagicMock(return_value=(dcgm_group, []))
    watcher._get_gpu_serial_numbers = MagicMock(return_value={})
    versions = {0: _versions()}
    watcher._get_gpu_versions = MagicMock(return_value=versions)

    watcher._initialize_dcgm_monitoring(MagicMock())

    assert watcher._gpu_versions == (versions if enabled else {})
    assert watcher._get_gpu_versions.called is enabled


def test_poll_loop_publishes_uptime_limit_watch(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)

    def initialize(_handle: object) -> tuple[MagicMock, list[int], list[int], dict[int, str]]:
        watcher._gpu_versions = {0: _versions()}
        return MagicMock(), [0], [], {}

    watcher._get_dcgm_handle = MagicMock(return_value=MagicMock())
    watcher._initialize_dcgm_monitoring = MagicMock(side_effect=initialize)
    watcher._perform_health_check = MagicMock(return_value=({}, True))
    watcher._cleanup_dcgm_resources = MagicMock()
    watcher._fire_callback_funcs = MagicMock()
    exit = MagicMock(spec=Event)
    exit.is_set.side_effect = [False, False, True]
    exit.wait.return_value = False

    watcher.start([], exit)

    func_name, (health_status, gpu_ids, _switch_ids) = watcher._fire_callback_funcs.call_args.args
    assert func_name == "health_event_occurred"
    assert gpu_ids == [0]
    details = health_status[dcgm.types.UPTIME_LIMIT_WATCH_NAME]
    assert details.status == dcgm.types.HealthStatus.FAIL
    assert details.entity_failures[0][0].code == dcgm.UPTIME_LIMIT_ERROR_CODE
