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

AFFECTED_VBIOS = "97.00.82.00.4B"
OTHER_VBIOS = "97.00.90.00.01"
LIMIT_HOURS = 1512.0
HOUR = 3600.0
STAGGER_HOURS = 72.0
# int.from_bytes(sha256(name.encode()).digest()[:8], "big") / 2**64 for each node
# name: the share of staggerHours by which that node reboots early.
NODE_A, NODE_A_FRACTION = "node-a", 0.3997659646794334
NODE_B, NODE_B_FRACTION = "node-b", 0.5778689249613692


def _make_watcher(
    enabled: bool = True,
    vbios_versions: frozenset[str] = frozenset({AFFECTED_VBIOS}),
    max_uptime_hours: float = LIMIT_HOURS,
    stagger_hours: float = 0.0,
    node_name: str = NODE_A,
) -> dcgm.DCGMWatcher:
    return dcgm.DCGMWatcher(
        dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            uptime_limit_enabled=enabled,
            uptime_limit_affected_vbios_versions=vbios_versions,
            uptime_limit_max_uptime_hours=max_uptime_hours,
            uptime_limit_stagger_hours=stagger_hours,
            node_name=node_name,
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
    monkeypatch.setattr(dcgm, "_read_node_uptime_seconds", reader)
    return reader


def test_disabled_check_reads_nothing_and_returns_none(watchers, uptime) -> None:
    watcher = _make_watcher(enabled=False)
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}

    assert watcher._evaluate_uptime_limit([0]) is None
    uptime.assert_not_called()


def test_affected_vbios_past_limit_fails_gpu(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.FAIL
    assert result.evaluated_gpu_ids == {0}
    (failure,) = result.entity_failures[0]
    assert failure.code == dcgm.UPTIME_LIMIT_ERROR_CODE
    assert f"vBIOS {AFFECTED_VBIOS}" in failure.message
    assert "1513.0 hours" in failure.message
    assert "1512 hours" in failure.message


@pytest.mark.parametrize("uptime_hours", [0.1, LIMIT_HOURS - 1, LIMIT_HOURS])
def test_affected_gpu_within_limit_is_healthy(watchers, uptime, uptime_hours: float) -> None:
    """An affected GPU must not fail before the node passes the limit."""
    uptime.return_value = uptime_hours * HOUR
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.entity_failures == {}
    assert result.evaluated_gpu_ids == {0}


def test_unaffected_gpu_past_limit_is_healthy(watchers, uptime) -> None:
    """A long uptime alone must not fail a GPU that runs no listed version."""
    uptime.return_value = 10 * LIMIT_HOURS * HOUR
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: OTHER_VBIOS}

    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.entity_failures == {}
    assert result.evaluated_gpu_ids == {0}


def test_version_match_ignores_case_and_surrounding_space(watchers, uptime) -> None:
    watcher = _make_watcher(vbios_versions=frozenset({f"  {AFFECTED_VBIOS.lower()} "}))
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS, 1: AFFECTED_VBIOS.lower()}

    result = watcher._evaluate_uptime_limit([0, 1])

    assert set(result.entity_failures) == {0, 1}


def test_version_prefix_does_not_match(watchers, uptime) -> None:
    """Matching is exact, so one listed version does not cover a longer one."""
    watcher = _make_watcher(vbios_versions=frozenset({"97.00.82"}))
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}

    assert watcher._evaluate_uptime_limit([0]).entity_failures == {}


def test_only_affected_gpus_fail_on_a_mixed_node(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS, 1: OTHER_VBIOS}

    result = watcher._evaluate_uptime_limit([0, 1])

    assert result.status == dcgm.types.HealthStatus.FAIL
    assert set(result.entity_failures) == {0}
    assert result.evaluated_gpu_ids == {0, 1}


def test_gpu_with_unknown_vbios_is_not_evaluated(watchers, uptime) -> None:
    """Past the limit, a GPU without a vBIOS version is neither failed nor reported healthy."""
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS, 2: ""}

    result = watcher._evaluate_uptime_limit([0, 1, 2])

    assert result.evaluated_gpu_ids == {0}
    assert set(result.entity_failures) == {0}


def test_no_known_versions_returns_none(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {}

    assert watcher._evaluate_uptime_limit([0, 1]) is None


@pytest.mark.parametrize("error", [OSError("clock unavailable"), AttributeError("no CLOCK_BOOTTIME")])
def test_unreadable_uptime_returns_none(watchers, uptime, error: Exception) -> None:
    """Without the uptime the check cannot decide, so it neither fails nor clears a GPU."""
    uptime.side_effect = error
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}

    assert watcher._evaluate_uptime_limit([0]) is None


def test_read_node_uptime_seconds_uses_boottime(monkeypatch: pytest.MonkeyPatch) -> None:
    """The uptime comes from CLOCK_BOOTTIME, which a per-container /proc/uptime does not change."""
    monkeypatch.setattr(dcgm.time, "CLOCK_BOOTTIME", 7, raising=False)
    clock = MagicMock(return_value=5745600.42)
    monkeypatch.setattr(dcgm.time, "clock_gettime", clock)

    assert dcgm._read_node_uptime_seconds() == pytest.approx(5745600.42)
    clock.assert_called_once_with(7)


def test_gpus_with_unknown_vbios_are_healthy_within_the_limit(watchers, uptime) -> None:
    """Within the limit the versions do not matter, so a fault from before a reboot can clear."""
    uptime.return_value = 0.5 * HOUR
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: ""}

    result = watcher._evaluate_uptime_limit([0, 1])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.evaluated_gpu_ids == {0, 1}


def test_failed_gpu_clears_after_reconnect_with_a_fixed_vbios(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}
    assert set(watcher._evaluate_uptime_limit([0]).entity_failures) == {0}

    watcher._gpu_vbios = {0: OTHER_VBIOS}
    result = watcher._evaluate_uptime_limit([0])

    assert result.status == dcgm.types.HealthStatus.PASS
    assert result.evaluated_gpu_ids == {0}


def test_unevaluated_gpus_past_the_limit_are_logged_once(watchers, uptime, caplog) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}

    with caplog.at_level("WARNING"):
        watcher._evaluate_uptime_limit([0, 1])
        watcher._evaluate_uptime_limit([0, 1])

    warnings = [r for r in caplog.records if "not evaluated" in r.getMessage()]
    assert len(warnings) == 1
    assert "[1]" in warnings[0].getMessage()


def _limit_hours(watcher: dcgm.DCGMWatcher) -> float:
    return watcher._uptime_limit_max_uptime_seconds / HOUR


def test_stagger_offset_is_fixed_for_a_node_name() -> None:
    offset = dcgm._uptime_limit_offset_hours(NODE_A, STAGGER_HOURS)

    assert dcgm._uptime_limit_offset_hours(NODE_A, STAGGER_HOURS) == offset
    assert offset == pytest.approx(STAGGER_HOURS * NODE_A_FRACTION)


def test_stagger_gives_different_nodes_different_limits_within_the_stagger(watchers) -> None:
    node_a = _make_watcher(stagger_hours=STAGGER_HOURS, node_name=NODE_A)
    node_b = _make_watcher(stagger_hours=STAGGER_HOURS, node_name=NODE_B)
    watchers.extend([node_a, node_b])

    assert _limit_hours(node_a) == pytest.approx(LIMIT_HOURS - STAGGER_HOURS * NODE_A_FRACTION)
    assert _limit_hours(node_b) == pytest.approx(LIMIT_HOURS - STAGGER_HOURS * NODE_B_FRACTION)
    assert _limit_hours(node_a) != _limit_hours(node_b)


def test_stagger_never_moves_a_limit_past_max_uptime_hours() -> None:
    offsets = [dcgm._uptime_limit_offset_hours(f"gpu-node-{i:04d}", STAGGER_HOURS) for i in range(1000)]

    assert all(0 <= offset <= STAGGER_HOURS for offset in offsets)
    # The offsets spread over the whole stagger, so the reboots spread too.
    assert min(offsets) < STAGGER_HOURS * 0.01
    assert max(offsets) > STAGGER_HOURS * 0.99


@pytest.mark.parametrize("node_name", [NODE_A, NODE_B, ""])
def test_zero_stagger_gives_every_node_max_uptime_hours(watchers, node_name: str) -> None:
    watcher = _make_watcher(stagger_hours=0.0, node_name=node_name)
    watchers.append(watcher)

    assert watcher._uptime_limit_max_uptime_seconds == LIMIT_HOURS * HOUR


def test_gpu_fails_past_the_staggered_limit_of_its_node_and_not_before(watchers, uptime) -> None:
    """With a stagger, an affected GPU fails before maxUptimeHours, but not before the limit of its node."""
    watcher = _make_watcher(stagger_hours=STAGGER_HOURS, node_name=NODE_A)
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS}
    node_limit_hours = LIMIT_HOURS - STAGGER_HOURS * NODE_A_FRACTION

    uptime.return_value = (node_limit_hours - 0.01) * HOUR
    within = watcher._evaluate_uptime_limit([0])
    uptime.return_value = (node_limit_hours + 0.01) * HOUR
    past = watcher._evaluate_uptime_limit([0])

    assert within.status == dcgm.types.HealthStatus.PASS
    assert within.entity_failures == {}
    assert past.status == dcgm.types.HealthStatus.FAIL
    assert set(past.entity_failures) == {0}
    assert f"limit of {node_limit_hours:g} hours" in past.entity_failures[0][0].message


def test_enabled_check_logs_the_limit_of_the_node_once(watchers, caplog) -> None:
    with caplog.at_level("INFO"):
        watcher = _make_watcher(stagger_hours=STAGGER_HOURS, node_name=NODE_A)
    watchers.append(watcher)

    messages = [r.getMessage() for r in caplog.records if "GpuUptimeLimitWatch enabled" in r.getMessage()]
    assert len(messages) == 1
    assert f"limit {LIMIT_HOURS - STAGGER_HOURS * NODE_A_FRACTION:g} hour(s) for node {NODE_A}" in messages[0]


def test_refresh_reads_missing_versions_after_the_retry_interval(watchers, monkeypatch: pytest.MonkeyPatch) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS, 2: ""}
    watcher._get_gpu_identifiers = MagicMock(return_value=({}, {0: OTHER_VBIOS, 1: AFFECTED_VBIOS, 2: OTHER_VBIOS}))
    now = 1000.0
    monkeypatch.setattr(dcgm.time, "monotonic", lambda: now)
    watcher._gpu_vbios_retry_at = now + 1

    watcher._refresh_gpu_vbios(MagicMock(), [0, 1, 2])
    watcher._get_gpu_identifiers.assert_not_called()

    now += 2
    handle = MagicMock()
    watcher._refresh_gpu_vbios(handle, [0, 1, 2])

    # The refresh reads only the versions; the serials are read only on connect.
    watcher._get_gpu_identifiers.assert_called_once_with(handle, read_serials=False)

    # Only the GPUs that were missing are updated; GPU 0 keeps the version it had.
    assert watcher._gpu_vbios == {0: AFFECTED_VBIOS, 1: AFFECTED_VBIOS, 2: OTHER_VBIOS}
    assert watcher._gpu_vbios_retry_at == now + dcgm.GPU_VERSIONS_RETRY_SECONDS


def test_refresh_does_nothing_when_versions_are_complete(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    watcher._gpu_vbios = {0: AFFECTED_VBIOS, 1: OTHER_VBIOS}
    watcher._get_gpu_identifiers = MagicMock()

    watcher._refresh_gpu_vbios(MagicMock(), [0, 1])

    watcher._get_gpu_identifiers.assert_not_called()


def _read_versions(watcher: dcgm.DCGMWatcher, handle: MagicMock) -> dict[int, str]:
    """Read only the vBIOS versions, as the refresh in the poll loop does."""
    serials, versions = watcher._get_gpu_identifiers(handle, read_serials=False)
    assert serials == {}
    return versions


def _dcgm_handle(identifiers_by_gpu: dict[int, object]) -> MagicMock:
    handle = MagicMock()
    system = handle.GetSystem.return_value
    system.discovery.GetEntityGroupEntities.return_value = list(identifiers_by_gpu)
    system.discovery.GetGpuAttributes.side_effect = lambda gpu: SimpleNamespace(identifiers=identifiers_by_gpu[gpu])
    return handle


def test_version_read_uses_dcgm_identifiers(watchers) -> None:
    """The DCGM bindings return the identifiers as str; raw bytes are accepted too."""
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle(
        {
            0: SimpleNamespace(vbios=AFFECTED_VBIOS),
            1: SimpleNamespace(vbios=f" {AFFECTED_VBIOS}\x00".encode()),
            2: SimpleNamespace(vbios=b""),
        }
    )

    assert _read_versions(watcher, handle) == {0: AFFECTED_VBIOS, 1: AFFECTED_VBIOS, 2: ""}


def test_version_read_does_not_read_the_driver_version(watchers) -> None:
    """The defect is in the vBIOS, so a driver version that cannot be read does not matter."""

    class DriverUnreadable:
        vbios = AFFECTED_VBIOS

        @property
        def driverVersion(self) -> str:
            raise AssertionError("the driver version must not be read")

    watcher = _make_watcher()
    watchers.append(watcher)

    assert _read_versions(watcher, _dcgm_handle({0: DriverUnreadable()})) == {0: AFFECTED_VBIOS}


@pytest.mark.parametrize("marker", ["<<<NULL>>>", "<<<NOT_FOUND>>>", "<<<NOT_SUPPORTED>>>", "<<<NOT_PERM>>>"])
def test_dcgm_unavailable_markers_read_as_unknown(marker: str) -> None:
    assert dcgm._identifier_text(marker) == ""
    assert dcgm._identifier_text(marker.encode()) == ""


def test_unsupported_vbios_is_not_evaluated(watchers, uptime) -> None:
    """A vBIOS that DCGM cannot report could be a listed one, so the GPU is not known to be healthy."""
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle({0: SimpleNamespace(vbios="<<<NOT_SUPPORTED>>>")})
    watcher._gpu_vbios = _read_versions(watcher, handle)

    assert watcher._gpu_vbios == {0: ""}
    assert watcher._evaluate_uptime_limit([0]) is None


def test_version_read_keeps_gpus_read_before_and_after_a_failure(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    good = SimpleNamespace(vbios=AFFECTED_VBIOS)

    def attributes(gpu: int) -> SimpleNamespace:
        if gpu == 1:
            raise RuntimeError("attributes unavailable")
        return SimpleNamespace(identifiers=good)

    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0, 1, 2]
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = attributes
    before = dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get()

    versions = _read_versions(watcher, handle)

    assert set(versions) == {0, 2}
    assert dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get() == before + 1


def test_version_read_skips_a_gpu_whose_identifiers_lack_the_vbios(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle({0: SimpleNamespace(), 1: SimpleNamespace(vbios=AFFECTED_VBIOS)})

    assert set(_read_versions(watcher, handle)) == {1}


def test_version_read_skips_a_gpu_whose_identifiers_fail_to_decode(watchers) -> None:
    """The bindings decode on attribute access, so a bad byte raises there; only that GPU is skipped."""

    class Undecodable:
        @property
        def vbios(self) -> str:
            return b"\xff97".decode("utf-8")

    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle({0: Undecodable(), 1: SimpleNamespace(vbios=AFFECTED_VBIOS)})

    assert set(_read_versions(watcher, handle)) == {1}


def test_version_read_discovery_failure_returns_empty_map(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.side_effect = RuntimeError("no discovery")

    assert _read_versions(watcher, handle) == {}


def test_version_read_failure_returns_empty_map(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = (
        dcgm.dcgm_structs.DCGMError_FunctionNotFound()
    )
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0]
    before = dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get()

    assert _read_versions(watcher, handle) == {}
    assert dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get() == before + 1


def test_initialize_reads_serials_and_versions_in_one_walk(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    dcgm_group = MagicMock()
    dcgm_group.GetGpuIds.return_value = [0]
    watcher._create_dcgm_group_with_all_entities = MagicMock(return_value=(dcgm_group, []))
    handle = MagicMock()
    watcher._get_gpu_identifiers = MagicMock(return_value=({0: "SERIAL0"}, {0: AFFECTED_VBIOS}))

    _group, _gpu_ids, _switch_ids, gpu_serials = watcher._initialize_dcgm_monitoring(handle)

    watcher._get_gpu_identifiers.assert_called_once_with(handle)
    assert gpu_serials == {0: "SERIAL0"}
    assert watcher._gpu_vbios == {0: AFFECTED_VBIOS}


def _identifiers(serial: str, vbios: str = AFFECTED_VBIOS) -> SimpleNamespace:
    return SimpleNamespace(serial=serial, vbios=vbios)


def test_connect_reads_the_attributes_of_each_gpu_once(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle({0: _identifiers("SERIAL0"), 1: _identifiers("SERIAL1", vbios=OTHER_VBIOS)})

    serials, versions = watcher._get_gpu_identifiers(handle)

    assert serials == {0: "SERIAL0", 1: "SERIAL1"}
    assert versions == {0: AFFECTED_VBIOS, 1: OTHER_VBIOS}
    discovery = handle.GetSystem.return_value.discovery
    assert discovery.GetEntityGroupEntities.call_count == 1
    assert [c.args for c in discovery.GetGpuAttributes.call_args_list] == [(0,), (1,)]


def test_connect_with_the_check_disabled_reads_only_serials(watchers) -> None:
    """Without the check, identifiers that lack the vBIOS field are not an error."""
    watcher = _make_watcher(enabled=False)
    watchers.append(watcher)
    handle = _dcgm_handle({0: SimpleNamespace(serial="SERIAL0")})

    assert watcher._get_gpu_identifiers(handle) == ({0: "SERIAL0"}, {})


def test_connect_without_attribute_support_continues_without_serials_and_versions(watchers, caplog) -> None:
    """An older 4.x server raises DCGMError_FunctionNotFound; the connection still comes up."""
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0, 1]
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = (
        dcgm.dcgm_structs.DCGMError_FunctionNotFound()
    )
    before = dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get()

    with caplog.at_level("WARNING"):
        assert watcher._get_gpu_identifiers(handle) == ({}, {})

    assert dcgm.GPU_ATTRIBUTES_UNSUPPORTED_MESSAGE in caplog.text
    assert dcgm.metrics.dcgm_api_failures.labels("get_gpu_versions")._value.get() == before + 2


def test_connect_drops_serials_read_before_an_unsupported_attribute_request(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)

    def attributes(gpu: int) -> SimpleNamespace:
        if gpu == 1:
            raise dcgm.dcgm_structs.DCGMError_FunctionNotFound()
        return SimpleNamespace(identifiers=_identifiers(f"SERIAL{gpu}"))

    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0, 1, 2]
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = attributes

    serials, versions = watcher._get_gpu_identifiers(handle)

    assert serials == {}
    assert set(versions) == {0, 2}


@pytest.mark.parametrize("enabled", [True, False])
def test_connect_attribute_failure_propagates(watchers, enabled: bool) -> None:
    """A failure other than an unsupported request fails the connect, so the monitor reconnects."""
    watcher = _make_watcher(enabled=enabled)
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.return_value = [0]
    handle.GetSystem.return_value.discovery.GetGpuAttributes.side_effect = RuntimeError("attributes unavailable")

    with pytest.raises(RuntimeError, match="attributes unavailable"):
        watcher._get_gpu_identifiers(handle)


def test_connect_listing_failure_propagates(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = MagicMock()
    handle.GetSystem.return_value.discovery.GetEntityGroupEntities.side_effect = RuntimeError("no discovery")

    with pytest.raises(RuntimeError, match="no discovery"):
        watcher._get_gpu_identifiers(handle)


def test_connect_keeps_the_serial_of_a_gpu_whose_vbios_fails(watchers) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)
    handle = _dcgm_handle({0: SimpleNamespace(serial="SERIAL0"), 1: _identifiers("SERIAL1")})

    serials, versions = watcher._get_gpu_identifiers(handle)

    assert serials == {0: "SERIAL0", 1: "SERIAL1"}
    assert set(versions) == {1}


def test_poll_loop_publishes_uptime_limit_watch(watchers, uptime) -> None:
    watcher = _make_watcher()
    watchers.append(watcher)

    def initialize(_handle: object) -> tuple[MagicMock, list[int], list[int], dict[int, str]]:
        watcher._gpu_vbios = {0: AFFECTED_VBIOS}
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
