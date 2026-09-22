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

"""Keep incident codes distinct from polling through event publication and recovery."""

from dataclasses import dataclass
from pathlib import Path
from threading import Event
from types import SimpleNamespace
from unittest.mock import MagicMock

import dcgm_errors
import dcgm_structs
import pytest

from gpu_health_monitor.dcgm_watcher import dcgm
from gpu_health_monitor.platform_connector import platform_connector
from gpu_health_monitor.protos import health_event_pb2 as pb

WATCH = "DCGM_HEALTH_WATCH_NVLINK"
CHECK = "GpuNvlinkWatch"
CODES = {"DCGM_FR_IMEX_UNHEALTHY": "NONE", "DCGM_FR_FABRIC_PROBE_STATE": "RESTART_VM"}


@pytest.fixture
def processor(tmp_path: Path) -> platform_connector.PlatformConnectorEventProcessor:
    """Use the real event builder and cache with an isolated, controllable transport."""
    state = tmp_path / "state"
    state.write_text("test-boot-id")
    result = platform_connector.PlatformConnectorEventProcessor(
        config=platform_connector.PlatformConnectorConfig(
            socket_path=str(tmp_path / "connector.sock"),
            node_name="node1",
            dcgm_errors_info_dict=dict(CODES),
            state_file_path=str(state),
            metadata_path=str(tmp_path / "metadata.json"),
            processing_strategy=pb.EXECUTE_REMEDIATION,
        ),
        exit=Event(),
    )
    result._metadata_reader = MagicMock()
    result._metadata_reader.get_pci_address.return_value = "0000:17:00.0"
    result._metadata_reader.get_gpu_uuid.return_value = "GPU-test"
    result._metadata_reader.get_chassis_serial.return_value = "CHASSIS-test"
    result.send_health_event_with_retries = MagicMock(return_value=True)
    return result


@dataclass(frozen=True)
class IncidentError:
    code: int
    msg: str


@dataclass(frozen=True)
class IncidentEntityInfo:
    entityId: int
    entityGroupId: int


@dataclass(frozen=True)
class Incident:
    system: int
    health: int
    error: IncidentError
    entityInfo: IncidentEntityInfo


def incident(code: str, message: str, gpu_id: int = 0) -> Incident:
    """Construct the DCGM incident shape without a GPU or DCGM host engine."""
    return Incident(
        system=dcgm_structs.DCGM_HEALTH_WATCH_NVLINK,
        health=(
            dcgm_structs.DCGM_HEALTH_RESULT_WARN
            if code == "DCGM_FR_IMEX_UNHEALTHY"
            else dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        ),
        error=IncidentError(code=getattr(dcgm_errors, code), msg=message),
        entityInfo=IncidentEntityInfo(entityId=gpu_id, entityGroupId=dcgm_structs.DCGM_FE_GPU),
    )


def poll(watcher: dcgm.DCGMWatcher, incidents: list[Incident]) -> dict[str, dcgm.types.HealthDetails]:
    """Run incident suppression, debounce and accumulation in the real watcher."""
    group = MagicMock()
    group.health.Check.return_value = SimpleNamespace(
        overallHealth=dcgm_structs.DCGM_HEALTH_RESULT_FAIL,
        incidentCount=len(incidents),
        incidents=incidents,
    )
    health, connected = watcher._perform_health_check(group)
    assert connected
    watcher._suppress_configured_error_codes(health)
    return {WATCH: health[WATCH]}


@pytest.mark.parametrize("reverse", [False, True])
@pytest.mark.parametrize("send_succeeds", [False, True])
def test_distinct_codes_survive_publication_and_recovery(
    processor: platform_connector.PlatformConnectorEventProcessor, reverse: bool, send_succeeds: bool
) -> None:
    """Each code keeps its own message and action, including after a failed delivery."""
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    incidents = [
        incident("DCGM_FR_IMEX_UNHEALTHY", "IMEX warning"),
        incident("DCGM_FR_FABRIC_PROBE_STATE", "fabric failure"),
    ]
    if reverse:
        incidents.reverse()
    health = poll(watcher, incidents)
    assert health[WATCH].status == dcgm.types.HealthStatus.FAIL
    processor.send_health_event_with_retries.return_value = send_succeeds
    processor.health_event_occurred(health, [0])
    events = processor.send_health_event_with_retries.call_args.args[0]
    events = pb.HealthEvents.FromString(pb.HealthEvents(events=events).SerializeToString()).events
    assert [(list(e.errorCode), e.message, e.recommendedAction, e.isFatal) for e in events] == [
        (["DCGM_FR_FABRIC_PROBE_STATE"], "fabric failure", pb.RESTART_VM, True),
        (["DCGM_FR_IMEX_UNHEALTHY"], "IMEX warning", pb.NONE, False),
    ]
    assert all(not e.isHealthy and e.processingStrategy == pb.EXECUTE_REMEDIATION for e in events)
    key = processor._build_cache_key(CHECK, "GPU", "0")
    if not send_succeeds:
        assert key not in processor.entity_cache
        processor.send_health_event_with_retries.reset_mock()
        processor.send_health_event_with_retries.return_value = True
        processor.health_event_occurred(health, [0])
        assert len(processor.send_health_event_with_retries.call_args.args[0]) == 2
    assert processor.entity_cache[key].active_errors == set(CODES)

    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(health, [0])
    processor.send_health_event_with_retries.assert_not_called()

    # The warning remains, but the fatal incident disappeared. Recover only the
    # disappeared code; the warning must stay active in the local cache.
    remaining_incidents = [
        current_incident
        for current_incident in incidents
        if current_incident.error.code == dcgm_errors.DCGM_FR_IMEX_UNHEALTHY
    ]
    processor.health_event_occurred(poll(watcher, remaining_incidents), [0])
    partial_recovery = processor.send_health_event_with_retries.call_args.args[0]
    assert len(partial_recovery) == 1
    assert partial_recovery[0].isHealthy and not partial_recovery[0].isFatal
    assert list(partial_recovery[0].errorCode) == ["DCGM_FR_FABRIC_PROBE_STATE"]
    assert partial_recovery[0].recommendedAction == pb.NONE
    assert processor.entity_cache[key].active_errors == {"DCGM_FR_IMEX_UNHEALTHY"}

    # A recovered code that returns while another code remains must be published
    # again and returned to the active cache.
    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(health, [0])
    recurrence = processor.send_health_event_with_retries.call_args.args[0]
    assert len(recurrence) == 1
    assert list(recurrence[0].errorCode) == ["DCGM_FR_FABRIC_PROBE_STATE"]
    assert not recurrence[0].isHealthy and recurrence[0].isFatal
    assert processor.entity_cache[key].active_errors == set(CODES)

    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(poll(watcher, []), [0])
    recovered = processor.send_health_event_with_retries.call_args.args[0]
    assert len(recovered) == 1
    assert recovered[0].isHealthy and not recovered[0].isFatal
    assert recovered[0].recommendedAction == pb.NONE and not recovered[0].errorCode
    assert processor.entity_cache[key].is_healthy
    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(poll(watcher, []), [0])
    processor.send_health_event_with_retries.assert_not_called()


def test_partial_recovery_and_new_code_are_cache_atomic(
    processor: platform_connector.PlatformConnectorEventProcessor,
) -> None:
    """A failed mixed recovery/new-fault batch leaves the last delivered cache intact."""
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    new_code = "DCGM_FR_NVLINK_DOWN"
    processor.dcgm_errors_info_dict[new_code] = "CONTACT_SUPPORT"
    initial = [
        incident("DCGM_FR_FABRIC_PROBE_STATE", "first fatal"),
        incident(new_code, "second fatal"),
    ]
    remaining_and_new = [
        incident(new_code, "second fatal"),
        incident("DCGM_FR_IMEX_UNHEALTHY", "new warning"),
    ]
    key = processor._build_cache_key(CHECK, "GPU", "0")

    processor.health_event_occurred(poll(watcher, initial), [0])
    assert processor.entity_cache[key].active_errors == {"DCGM_FR_FABRIC_PROBE_STATE", new_code}

    processor.send_health_event_with_retries.reset_mock()
    processor.send_health_event_with_retries.return_value = False
    processor.health_event_occurred(poll(watcher, remaining_and_new), [0])
    pending = processor.send_health_event_with_retries.call_args.args[0]
    assert [(event.isHealthy, tuple(event.errorCode)) for event in pending] == [
        (False, ("DCGM_FR_IMEX_UNHEALTHY",)),
        (True, ("DCGM_FR_FABRIC_PROBE_STATE",)),
    ]
    assert processor.entity_cache[key].active_errors == {"DCGM_FR_FABRIC_PROBE_STATE", new_code}

    processor.send_health_event_with_retries.reset_mock()
    processor.send_health_event_with_retries.return_value = True
    processor.health_event_occurred(poll(watcher, remaining_and_new), [0])
    assert processor.entity_cache[key].active_errors == {new_code, "DCGM_FR_IMEX_UNHEALTHY"}

    # Ordering also applies when a fault replaces a different GPU's fault. The
    # full healthy event for GPU 0 must follow GPU 1's newly observed fault.
    processor.entity_cache.clear()
    cross_watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    processor.health_event_occurred(
        poll(
            cross_watcher,
            [
                incident("DCGM_FR_FABRIC_PROBE_STATE", "GPU 0 fault", gpu_id=0),
                incident(new_code, "GPU 1 existing fault", gpu_id=1),
            ],
        ),
        [0, 1],
    )
    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(
        poll(
            cross_watcher,
            [
                incident(new_code, "GPU 1 existing fault", gpu_id=1),
                incident("DCGM_FR_IMEX_UNHEALTHY", "GPU 1 new fault", gpu_id=1),
            ],
        ),
        [0, 1],
    )
    cross_entity_events = processor.send_health_event_with_retries.call_args.args[0]
    assert [
        (event.isHealthy, event.entitiesImpacted[0].entityValue, tuple(event.errorCode))
        for event in cross_entity_events
    ] == [
        (False, "1", ("DCGM_FR_IMEX_UNHEALTHY",)),
        (True, "0", ()),
    ]


@pytest.mark.parametrize("reverse", [False, True])
def test_repeated_code_combines_only_its_own_messages(
    processor: platform_connector.PlatformConnectorEventProcessor, reverse: bool
) -> None:
    """Same-code deduplication cannot mix another code's remediation evidence."""
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    incidents = [
        incident("DCGM_FR_IMEX_UNHEALTHY", "warning one"),
        incident("DCGM_FR_FABRIC_PROBE_STATE", "failure"),
        incident("DCGM_FR_IMEX_UNHEALTHY", "warning two"),
    ]
    if reverse:
        incidents.reverse()
    processor.health_event_occurred(poll(watcher, incidents), [0])
    events = processor.send_health_event_with_retries.call_args.args[0]
    assert len(events) == 2
    by_code = {event.errorCode[0]: event for event in events}
    assert by_code["DCGM_FR_FABRIC_PROBE_STATE"].message == "failure"
    assert set(by_code["DCGM_FR_IMEX_UNHEALTHY"].message.split("; ")) == {"warning one", "warning two"}


@pytest.mark.parametrize("suppressed", list(CODES))
@pytest.mark.parametrize("reverse", [False, True])
def test_suppression_preserves_the_other_code(
    processor: platform_connector.PlatformConnectorEventProcessor, suppressed: str, reverse: bool
) -> None:
    """Suppressing either code leaves the other's original message and action."""
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            suppressed_error_codes=frozenset({suppressed}),
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    incidents = [incident(code, code) for code in CODES]
    if reverse:
        incidents.reverse()
    processor.health_event_occurred(poll(watcher, incidents), [0])
    events = processor.send_health_event_with_retries.call_args.args[0]
    expected = next(code for code in CODES if code != suppressed)
    assert len(events) == 1 and list(events[0].errorCode) == [expected]
    assert events[0].message == expected
    assert events[0].recommendedAction == pb.RecommendedAction.Value(CODES[expected])


def test_debounced_second_code_is_published_after_its_threshold(
    processor: platform_connector.PlatformConnectorEventProcessor,
) -> None:
    """A cached first code cannot conceal a later code when its debounce matures."""
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            health_check_min_consecutive_polls={"DCGM_FR_FABRIC_PROBE_STATE": 2},
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    incidents = [incident(code, code) for code in CODES]
    processor.health_event_occurred(poll(watcher, incidents), [0])
    first = processor.send_health_event_with_retries.call_args.args[0]
    assert len(first) == 1 and list(first[0].errorCode) == ["DCGM_FR_IMEX_UNHEALTHY"]
    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(poll(watcher, incidents), [0])
    second = processor.send_health_event_with_retries.call_args.args[0]
    assert len(second) == 1 and list(second[0].errorCode) == ["DCGM_FR_FABRIC_PROBE_STATE"]
    key = processor._build_cache_key(CHECK, "GPU", "0")
    assert processor.entity_cache[key].active_errors == set(CODES)


@pytest.mark.parametrize("connectx_binding_available", [False, True])
def test_connectx_is_not_enabled_or_published(
    processor: platform_connector.PlatformConnectorEventProcessor,
    monkeypatch: pytest.MonkeyPatch,
    connectx_binding_available: bool,
) -> None:
    """GPU monitoring must not claim NIC coverage, with either old or new bindings."""
    if connectx_binding_available:
        monkeypatch.setattr(dcgm_structs, "DCGM_HEALTH_WATCH_CONNECTX", 0x1000, raising=False)
    else:
        monkeypatch.delattr(dcgm_structs, "DCGM_HEALTH_WATCH_CONNECTX", raising=False)
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            imex_monitoring_enabled=True,
        ),
        callbacks=[],
    )
    group = MagicMock()
    group.GetGpuIds.return_value = [0]
    watcher._create_dcgm_group_with_all_entities = MagicMock(return_value=(group, []))
    watcher._get_gpu_serial_numbers = MagicMock(return_value={})

    _, gpu_ids, switch_ids, _ = watcher._initialize_dcgm_monitoring(MagicMock())
    group.health.Set.assert_called_once_with(dcgm_structs.DCGM_HEALTH_WATCH_ALL & ~0x1000)
    processor.health_event_occurred(poll(watcher, []), gpu_ids, switch_ids)

    events = processor.send_health_event_with_retries.call_args.args[0]
    assert any(event.checkName == "GpuNvlinkWatch" for event in events)
    assert all(event.checkName != "GpuConnectxWatch" for event in events)
    watcher._callback_thread_pool.shutdown()


@pytest.mark.parametrize("system", [dcgm_structs.DCGM_HEALTH_WATCH_NVLINK, 0x2000])
@pytest.mark.parametrize("enabled", [False, True])
def test_global_imex_incident_reaches_event_publisher(processor, system, enabled):
    """Global IMEX faults keep their existing event identity across DCGM watch versions."""
    watcher = dcgm.DCGMWatcher(
        config=dcgm.types.DCGMWatcherConfig(
            addr="localhost:5555",
            poll_interval_seconds=10,
            dcgm_k8s_service_enabled=False,
            imex_monitoring_enabled=enabled,
        ),
        callbacks=[],
    )
    watcher._health_watches[0x2000] = "DCGM_HEALTH_WATCH_IMEX"
    group = MagicMock()
    global_incident = Incident(
        system=system,
        health=dcgm_structs.DCGM_HEALTH_RESULT_FAIL,
        error=IncidentError(dcgm_errors.DCGM_FR_IMEX_UNHEALTHY, "IMEX daemon not READY"),
        entityInfo=IncidentEntityInfo(entityId=0, entityGroupId=dcgm_structs.DCGM_FE_NONE),
    )
    group.health.Check.return_value = SimpleNamespace(
        overallHealth=dcgm_structs.DCGM_HEALTH_RESULT_FAIL, incidentCount=1, incidents=[global_incident]
    )
    health, connected = watcher._perform_health_check(group)
    assert connected
    processor.health_event_occurred(health, [0])
    events = processor.send_health_event_with_retries.call_args.args[0]
    imex_events = [event for event in events if "DCGM_FR_IMEX_UNHEALTHY" in event.errorCode]
    assert len(imex_events) == int(enabled)
    if enabled:
        assert not imex_events[0].isHealthy
        assert imex_events[0].entitiesImpacted[0].entityValue == "0"
    watcher._callback_thread_pool.shutdown()


@pytest.mark.parametrize(
    ("entity_key", "gpu_ids", "switch_ids", "component_class"),
    [
        (0, [0], None, "GPU"),
        ((dcgm.dcgm_fields.DCGM_FE_SWITCH, 0), [], [0], "NVSWITCH"),
    ],
)
def test_incomplete_observation_does_not_recover_absent_codes(
    processor: platform_connector.PlatformConnectorEventProcessor,
    entity_key: dcgm.types.EntityKey,
    gpu_ids: list[int],
    switch_ids: list[int] | None,
    component_class: str,
) -> None:
    """An incomplete native response may add faults but must not clear an unseen one."""
    first_code = "DCGM_FR_FABRIC_PROBE_STATE"
    remaining_code = "DCGM_FR_NVLINK_DOWN"
    new_code = "DCGM_FR_IMEX_UNHEALTHY"
    processor.dcgm_errors_info_dict[remaining_code] = "CONTACT_SUPPORT"
    watch = "DCGM_HEALTH_WATCH_NVLINK"
    check = "GpuNvlinkWatch"
    entity_type = "GPU" if component_class == "GPU" else "NVSWITCH"
    key = processor._build_cache_key(check, entity_type, "0")

    def details(codes: list[str], complete: bool) -> dcgm.types.HealthDetails:
        result = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.FAIL,
            entity_failures=(
                {entity_key: [dcgm.types.ErrorDetails(code=code, message=code) for code in codes]} if codes else {}
            ),
            is_complete=complete,
        )
        return result

    processor.health_event_occurred({watch: details([first_code, remaining_code], True)}, gpu_ids, switch_ids)
    assert processor.entity_cache[key].active_errors == {first_code, remaining_code}

    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred({watch: details([remaining_code, new_code], False)}, gpu_ids, switch_ids)
    incomplete_events = processor.send_health_event_with_retries.call_args.args[0]
    assert [(event.isHealthy, tuple(event.errorCode)) for event in incomplete_events] == [
        (False, (new_code,)),
    ]
    assert processor.entity_cache[key].active_errors == {first_code, remaining_code, new_code}

    # A truncated response with no incident for this entity still cannot prove
    # that its previous faults recovered.
    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred({watch: details([], False)}, gpu_ids, switch_ids)
    processor.send_health_event_with_retries.assert_not_called()
    assert processor.entity_cache[key].active_errors == {first_code, remaining_code, new_code}

    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred({watch: details([remaining_code, new_code], True)}, gpu_ids, switch_ids)
    completed_events = processor.send_health_event_with_retries.call_args.args[0]
    assert [(event.isHealthy, tuple(event.errorCode)) for event in completed_events] == [
        (True, (first_code,)),
    ]
    assert processor.entity_cache[key].active_errors == {remaining_code, new_code}
