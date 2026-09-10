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
        str(tmp_path / "connector.sock"),
        "node1",
        Event(),
        CODES,
        str(state),
        str(tmp_path / "metadata.json"),
        pb.EXECUTE_REMEDIATION,
    )
    result._metadata_reader = MagicMock()
    result._metadata_reader.get_pci_address.return_value = "0000:17:00.0"
    result._metadata_reader.get_gpu_uuid.return_value = "GPU-test"
    result._metadata_reader.get_chassis_serial.return_value = "CHASSIS-test"
    result.send_health_event_with_retries = MagicMock(return_value=True)
    return result


def incident(code: str, message: str, gpu_id: int = 0) -> SimpleNamespace:
    """Construct the DCGM incident shape without a GPU or DCGM host engine."""
    return SimpleNamespace(
        system=dcgm_structs.DCGM_HEALTH_WATCH_NVLINK,
        health=(
            dcgm_structs.DCGM_HEALTH_RESULT_WARN
            if code == "DCGM_FR_IMEX_UNHEALTHY"
            else dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        ),
        error=SimpleNamespace(code=getattr(dcgm_errors, code), msg=message),
        entityInfo=SimpleNamespace(entityId=gpu_id, entityGroupId=dcgm_structs.DCGM_FE_GPU),
    )


def poll(watcher: dcgm.DCGMWatcher, incidents: list[SimpleNamespace]) -> dict[str, dcgm.types.HealthDetails]:
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
def test_distinct_codes_survive_publication_and_recovery(processor, reverse: bool, send_succeeds: bool) -> None:
    """Each code keeps its own message and action, including after a failed delivery."""
    watcher = dcgm.DCGMWatcher("localhost:5555", 10, [], False)
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
    processor.health_event_occurred(poll(watcher, incidents[:1]), [0])
    processor.send_health_event_with_retries.assert_not_called()
    assert processor.entity_cache[key].active_errors == set(CODES)

    processor.health_event_occurred(poll(watcher, []), [0])
    recovered = processor.send_health_event_with_retries.call_args.args[0]
    assert len(recovered) == 1
    assert recovered[0].isHealthy and not recovered[0].isFatal
    assert recovered[0].recommendedAction == pb.NONE and not recovered[0].errorCode
    assert processor.entity_cache[key].is_healthy
    processor.send_health_event_with_retries.reset_mock()
    processor.health_event_occurred(poll(watcher, []), [0])
    processor.send_health_event_with_retries.assert_not_called()


@pytest.mark.parametrize("reverse", [False, True])
def test_repeated_code_combines_only_its_own_messages(processor, reverse: bool) -> None:
    """Same-code deduplication cannot mix another code's remediation evidence."""
    watcher = dcgm.DCGMWatcher("localhost:5555", 10, [], False)
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
def test_suppression_preserves_the_other_code(processor, suppressed: str, reverse: bool) -> None:
    """Suppressing either code leaves the other's original message and action."""
    watcher = dcgm.DCGMWatcher("localhost:5555", 10, [], False, suppressed_error_codes=frozenset({suppressed}))
    incidents = [incident(code, code) for code in CODES]
    if reverse:
        incidents.reverse()
    processor.health_event_occurred(poll(watcher, incidents), [0])
    events = processor.send_health_event_with_retries.call_args.args[0]
    expected = next(code for code in CODES if code != suppressed)
    assert len(events) == 1 and list(events[0].errorCode) == [expected]
    assert events[0].message == expected
    assert events[0].recommendedAction == pb.RecommendedAction.Value(CODES[expected])


def test_debounced_second_code_is_published_after_its_threshold(processor) -> None:
    """A cached first code cannot conceal a later code when its debounce matures."""
    watcher = dcgm.DCGMWatcher(
        "localhost:5555", 10, [], False, health_check_min_consecutive_polls={"DCGM_FR_FABRIC_PROBE_STATE": 2}
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
