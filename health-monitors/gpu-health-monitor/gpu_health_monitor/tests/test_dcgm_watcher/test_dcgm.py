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

from gpu_health_monitor.dcgm_watcher import dcgm
from gpu_health_monitor.metadata import MetadataReader
from gpu_health_monitor.tests.nvlink_fixtures import (
    A100_PCIE_UNBRIDGED,
    H100_SXM_TRAINED,
    L40_NO_NVLINK,
    make_metadata_reader,
)
from unittest.mock import MagicMock, patch
import dcgm_structs, dcgm_errors, dcgm_fields, dcgmvalue
from pathlib import Path
from threading import Event, Thread
from concurrent.futures import ThreadPoolExecutor
from typing import Optional
from ctypes import pointer
import copy
import json
import pytest
import time


class FakeEventProcessorInTest(dcgm.types.CallbackInterface):
    def __init__(self) -> None:
        self.health_details = None
        self.gpu_id = None
        self.error_num = None
        self.serial = None
        self.fields_changes = None
        self.connectivity_failed_called = False
        self.probe_unresponsive_calls: list[tuple[str, float, str]] = []

    def health_event_occurred(self, health_details: dict[str, dcgm.types.HealthDetails], gpu_ids: list[int]) -> None:
        self.health_details = health_details

    def dcgm_connectivity_failed(self) -> bool:
        self.connectivity_failed_called = True
        return True

    def dcgm_probe_unresponsive(
        self,
        operation: str,
        elapsed_seconds: float,
        dcgm_mode: str,
    ) -> bool:
        self.probe_unresponsive_calls.append((operation, elapsed_seconds, dcgm_mode))
        return True


class TestDCGMHealthChecks:
    def _make_thermal_margin_watcher(self, metadata_reader: MetadataReader) -> dcgm.DCGMWatcher:
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            thermal_margin_enabled=True,
            metadata_reader=metadata_reader,
        )
        watcher._field_group = MagicMock()
        return watcher

    def _make_power_brake_watcher(self, min_consecutive_polls: int = 1) -> dcgm.DCGMWatcher:
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            power_brake_enabled=True,
            power_brake_min_consecutive_polls=min_consecutive_polls,
        )
        watcher._field_group = MagicMock()
        return watcher

    @staticmethod
    def _brake_samples(mask_by_gpu: dict[int, int]) -> MagicMock:
        field_id = dcgm.DCGM_FIELDS_MONITORING["gpupowerbrakemonitoringenabled"].field_id
        return MagicMock(values={gpu: {field_id: [MagicMock(value=mask)]} for gpu, mask in mask_by_gpu.items()})

    def _get_pcie_incident(self, group_id, entity_id):
        incident = dcgm_structs.c_dcgmIncidentInfo_t()
        incident.system = dcgm_structs.DCGM_HEALTH_WATCH_PCIE
        incident.health = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        incident.error = dcgm_structs.c_dcgmDiagErrorDetail_t()
        incident.error.msg = "Detected more than 8 PCIe replays per minute for GPU 1 : 99999 Reconnect PCIe card. Run system side PCIE diagnostic utilities to verify hops off the GPU board. If issue is on the board, run the field diagnostic."
        incident.error.code = dcgm_errors.DCGM_FR_PCI_REPLAY_RATE
        incident.entityInfo = dcgm_structs.c_dcgmGroupEntityPair_t()
        incident.entityInfo.entityGroupId = group_id
        incident.entityInfo.entityId = entity_id
        return incident

    def test_unsupported_thermal_margin_field_is_disabled(self, monkeypatch):
        monkeypatch.delitem(dcgm.DCGM_FIELDS_MONITORING, "gputemplimitmonitoringenabled")
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            thermal_margin_enabled=True,
            metadata_reader=MagicMock(),
        )
        dcgm_group = MagicMock()
        dcgm_group.GetGpuIds.return_value = [0]
        watcher._create_dcgm_group_with_all_entities = MagicMock(return_value=dcgm_group)
        watcher._get_gpu_serial_numbers = MagicMock(return_value={})

        watcher._initialize_dcgm_monitoring(MagicMock())

        assert watcher._thermal_margin_enabled is False
        dcgm_group.health.Set.assert_called_once_with(dcgm_structs.DCGM_HEALTH_WATCH_ALL)
        dcgm_group.samples.WatchFields.assert_not_called()

    def test_unsupported_power_brake_field_is_disabled(self, monkeypatch: pytest.MonkeyPatch) -> None:
        """No clocks-event-reasons field in this DCGM build → monitor disables itself."""
        monkeypatch.delitem(dcgm.DCGM_FIELDS_MONITORING, "gpupowerbrakemonitoringenabled")
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            power_brake_enabled=True,
        )
        assert watcher._power_brake_enabled is False

    def test_power_brake_disabled_returns_none(self) -> None:
        """Watch off → nothing published, even with the bit set."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        watcher._field_group = MagicMock()
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgm.HW_POWER_BRAKE_REASON_BIT})

        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]) is None

    def test_evaluate_gpu_power_brake_detects_brake_bit(self) -> None:
        """Brake bit set, threshold of 1 → FAIL carrying the violation code."""
        watcher = self._make_power_brake_watcher()
        dcgm_group_mock = MagicMock()
        # 0x8c = SW power cap | HW slowdown | HW power brake, as seen on real hardware.
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: 0x8C})

        result = watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0])

        assert result is not None
        assert result.status == dcgm.types.HealthStatus.FAIL
        assert result.entity_failures[0].code == "GPU_HW_POWER_BRAKE_VIOLATION"

    def test_evaluate_gpu_power_brake_ignores_sw_power_cap(self) -> None:
        """SW power cap alone is normal capping under load and must not fail."""
        watcher = self._make_power_brake_watcher()
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: 0x04})

        result = watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0])

        assert result is not None
        assert result.status == dcgm.types.HealthStatus.PASS
        assert result.entity_failures == {}

    def test_evaluate_gpu_power_brake_requires_consecutive_polls(self) -> None:
        """With a threshold of 3, only the third consecutive assertion fails."""
        watcher = self._make_power_brake_watcher(min_consecutive_polls=3)
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgm.HW_POWER_BRAKE_REASON_BIT})

        first = watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0])
        second = watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0])
        third = watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0])

        assert first.status == dcgm.types.HealthStatus.PASS
        assert second.status == dcgm.types.HealthStatus.PASS
        assert third.status == dcgm.types.HealthStatus.FAIL
        assert third.entity_failures[0].code == "GPU_HW_POWER_BRAKE_VIOLATION"

    def test_evaluate_gpu_power_brake_streak_resets_when_cleared(self) -> None:
        """A clear resets the streak, so a transient never accumulates to a failure."""
        watcher = self._make_power_brake_watcher(min_consecutive_polls=2)
        dcgm_group_mock = MagicMock()

        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgm.HW_POWER_BRAKE_REASON_BIT})
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]).status == dcgm.types.HealthStatus.PASS

        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: 0x00})
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]).status == dcgm.types.HealthStatus.PASS
        assert watcher._power_brake_streaks == {}

        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgm.HW_POWER_BRAKE_REASON_BIT})
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]).status == dcgm.types.HealthStatus.PASS

    def test_evaluate_gpu_power_brake_mixed_gpus(self) -> None:
        """Only the braked GPU is failed; the other is left clean."""
        watcher = self._make_power_brake_watcher()
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples(
            {0: 0x01, 1: dcgm.HW_POWER_BRAKE_REASON_BIT}
        )

        result = watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0, 1])

        assert result.status == dcgm.types.HealthStatus.FAIL
        assert set(result.entity_failures) == {1}

    def test_evaluate_gpu_power_brake_returns_none_without_samples(self) -> None:
        """A DCGM data gap must neither raise nor clear a finding."""
        watcher = self._make_power_brake_watcher()
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = MagicMock(values={})

        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]) is None

    def test_evaluate_gpu_power_brake_ignores_blank_sentinel(self) -> None:
        """DCGM blank sentinels have bit 0x80 set in their low byte, so an
        unchecked blank would be indistinguishable from an asserted brake."""
        watcher = self._make_power_brake_watcher()
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgmvalue.DCGM_INT64_BLANK})

        # Nothing was evaluated, so the watch is not published at all.
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]) is None

    def test_evaluate_gpu_power_brake_blank_does_not_accumulate_streak(self) -> None:
        """Repeated blanks must not accumulate to a failure, and must not clear
        a streak built from real assertions either."""
        watcher = self._make_power_brake_watcher(min_consecutive_polls=2)
        dcgm_group_mock = MagicMock()

        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgm.HW_POWER_BRAKE_REASON_BIT})
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]).status == dcgm.types.HealthStatus.PASS
        assert watcher._power_brake_streaks == {0: 1}

        # A blank in the middle is skipped: the streak survives rather than
        # being cleared or advanced.
        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgmvalue.DCGM_INT64_BLANK})
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]) is None
        assert watcher._power_brake_streaks == {0: 1}

        dcgm_group_mock.samples.GetLatest.return_value = self._brake_samples({0: dcgm.HW_POWER_BRAKE_REASON_BIT})
        assert watcher._evaluate_gpu_power_brake(dcgm_group_mock, [0]).status == dcgm.types.HealthStatus.FAIL

    def test_get_available_health_watches(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        health_watches = watcher._get_available_health_watches()
        assert len(health_watches) == 13

    def test_get_available_error_codes(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        error_codes = watcher._get_available_error_codes()
        assert len(error_codes) == 114

    def test_get_available_fields(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_fields = watcher._get_available_fields()
        assert len(dcgm_fields) == 320

    def test_get_health_status_dict(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        health_status_dict = watcher._get_health_status_dict()
        assert len(health_status_dict) == 13
        for _, val in health_status_dict.items():
            assert val.status == dcgm.types.HealthStatus.PASS
            assert val.entity_failures == {}

    def test_evaluate_gpu_thermal_margin_skips_when_no_threshold(self):
        """No GPU has threshold → returns None (watch not published)."""
        metadata_reader = MagicMock()
        metadata_reader.get_slowdown_tlimit_c.return_value = None
        watcher = self._make_thermal_margin_watcher(metadata_reader)
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.samples.GetLatest.return_value = MagicMock(
            values={0: {dcgm.DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].field_id: [MagicMock(value=43)]}}
        )

        assert watcher._evaluate_gpu_thermal_margin(dcgm_group_mock, [0]) is None

    def test_evaluate_gpu_thermal_margin_lifecycle(self, tmp_path):
        """Covers: PASS → FAIL → PASS lifecycle.

        Uses realistic negative offset values matching actual field semantics:
        - slowdown_tlimit_c=-2 is a signed negative offset (HW slowdown kicks in at T.Max - 2°C)
        - margin=-1 means GPU is 1°C below T.Max (near but not at slowdown)
        - violation occurs when margin < slowdown_tlimit_c (i.e., -1 < -2 is False, but -3 < -2 is True)
        """
        field_id = dcgm.DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].field_id
        violation_code = dcgm.DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].violation_code

        metadata_path = tmp_path / "gpu_metadata.json"
        metadata_path.write_text(
            json.dumps(
                {
                    "version": "1.0",
                    "gpus": [
                        {"gpu_id": 0, "uuid": "GPU-0", "pci_address": "0000:01:00.0", "slowdown_tlimit_c": -2},
                    ],
                }
            )
        )
        reader = MetadataReader(str(metadata_path))
        watcher = self._make_thermal_margin_watcher(reader)
        dcgm_group_mock = MagicMock()

        # Phase 1: Healthy (margin=-1 > threshold=-2) → PASS
        dcgm_group_mock.samples.GetLatest.return_value = MagicMock(values={0: {field_id: [MagicMock(value=-1)]}})
        healthy = watcher._evaluate_gpu_thermal_margin(dcgm_group_mock, [0])
        assert healthy.status == dcgm.types.HealthStatus.PASS
        assert healthy.entity_failures == {}

        # Phase 2: Violation (margin=-3 < threshold=-2) → FAIL
        dcgm_group_mock.samples.GetLatest.return_value = MagicMock(values={0: {field_id: [MagicMock(value=-3)]}})
        triggered = watcher._evaluate_gpu_thermal_margin(dcgm_group_mock, [0])
        assert triggered.status == dcgm.types.HealthStatus.FAIL
        assert triggered.entity_failures[0].code == violation_code

        # Phase 3: Adjust threshold to clear violation → PASS
        reader._metadata["gpus"][0]["slowdown_tlimit_c"] = -4
        cleared = watcher._evaluate_gpu_thermal_margin(dcgm_group_mock, [0])
        assert cleared.status == dcgm.types.HealthStatus.PASS
        assert cleared.entity_failures == {}

    def test_evaluate_gpu_thermal_margin_mixed_gpus(self, tmp_path):
        """Test mixed scenario: GPU 0 passes, GPU 1 fails."""
        field_id = dcgm.DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].field_id
        violation_code = dcgm.DCGM_FIELDS_MONITORING["gputemplimitmonitoringenabled"].violation_code

        metadata_path = tmp_path / "gpu_metadata.json"
        metadata_path.write_text(
            json.dumps(
                {
                    "version": "1.0",
                    "gpus": [
                        {"gpu_id": 0, "uuid": "GPU-0", "pci_address": "0000:01:00.0", "slowdown_tlimit_c": -2},
                        {"gpu_id": 1, "uuid": "GPU-1", "pci_address": "0000:02:00.0", "slowdown_tlimit_c": -2},
                    ],
                }
            )
        )
        reader = MetadataReader(str(metadata_path))
        watcher = self._make_thermal_margin_watcher(reader)
        dcgm_group_mock = MagicMock()

        # GPU 0: margin=-1 (healthy), GPU 1: margin=-3 (violation)
        dcgm_group_mock.samples.GetLatest.return_value = MagicMock(
            values={
                0: {field_id: [MagicMock(value=-1)]},
                1: {field_id: [MagicMock(value=-3)]},
            }
        )
        result = watcher._evaluate_gpu_thermal_margin(dcgm_group_mock, [0, 1])

        assert result.status == dcgm.types.HealthStatus.FAIL
        assert 0 not in result.entity_failures  # GPU 0 passed
        assert 1 in result.entity_failures  # GPU 1 failed
        assert result.entity_failures[1].code == violation_code

    @patch("pydcgm.DcgmGroup.__new__")
    def test_dcgm_create_group(self, mock_dcgm_group):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_handle_mock = MagicMock()
        dcgm_system_mock = MagicMock()
        dcgm_group_mock = MagicMock()
        mock_dcgm_group.return_value = dcgm_group_mock
        supported_gpus = [0, 1, 2, 3, 4, 5, 6, 7]
        supported_switches = [10, 11, 12, 13, 14]

        def GetEntityGroupEntities_mock(entityGroupId, onlySupported):
            if entityGroupId == dcgm_fields.DCGM_FE_GPU:
                return supported_gpus
            elif entityGroupId == dcgm_fields.DCGM_FE_SWITCH:
                return supported_switches
            else:
                raise ValueError("unknown entityGroupId")

        dcgm_system_mock.discovery.GetEntityGroupEntities = MagicMock(side_effect=GetEntityGroupEntities_mock)
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        dcgm_group = watcher._create_dcgm_group_with_all_entities(dcgm_handle_mock)
        for gpu in supported_gpus:
            dcgm_group.AddEntity.assert_any_call(dcgm_fields.DCGM_FE_GPU, gpu)
        for switch in supported_switches:
            dcgm_group.AddEntity.assert_any_call(dcgm_fields.DCGM_FE_SWITCH, switch)

    def test_perform_health_check_all_watch_pass(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_DIAG_RESULT_PASS
        mock_response.incidentCount = 0
        mock_response.incidents = dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()
        assert response == expected_response
        assert connectivity_success == True

    def test_perform_health_check_one_watch_fail_single_entity_failure(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        mock_response.incidentCount = 1
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        mock_response.incidents[0] = self._get_pcie_incident(0, 1)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()
        expected_response["DCGM_HEALTH_WATCH_PCIE"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_PCI_REPLAY_RATE",
                    message="Detected more than 8 PCIe replays per minute for GPU 1 : 99999 Reconnect PCIe card. Run system side PCIE diagnostic utilities to verify hops off the GPU board. If issue is on the board, run the field diagnostic.",
                )
            },
        )
        assert response == expected_response
        assert connectivity_success == True

    def test_perform_health_check_one_watch_fail_multiple_entity_failure(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        mock_response.incidentCount = 2
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        mock_response.incidents[0] = self._get_pcie_incident(0, 1)
        mock_response.incidents[1] = self._get_pcie_incident(0, 2)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()
        expected_response["DCGM_HEALTH_WATCH_PCIE"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_PCI_REPLAY_RATE",
                    message="Detected more than 8 PCIe replays per minute for GPU 1 : 99999 Reconnect PCIe card. Run system side PCIE diagnostic utilities to verify hops off the GPU board. If issue is on the board, run the field diagnostic.",
                ),
                2: dcgm.types.ErrorDetails(
                    code="DCGM_FR_PCI_REPLAY_RATE",
                    message="Detected more than 8 PCIe replays per minute for GPU 1 : 99999 Reconnect PCIe card. Run system side PCIE diagnostic utilities to verify hops off the GPU board. If issue is on the board, run the field diagnostic.",
                ),
            },
        )

        assert response == expected_response
        assert connectivity_success == True

    def _get_power_throttle_incident(self, group_id, entity_id):
        """Helper to create a DCGM_FR_CLOCK_THROTTLE_POWER incident for testing."""
        incident = dcgm_structs.c_dcgmIncidentInfo_t()
        incident.system = dcgm_structs.DCGM_HEALTH_WATCH_POWER
        incident.health = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        incident.error = dcgm_structs.c_dcgmDiagErrorDetail_t()
        incident.error.msg = f"ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU:{entity_id} Recommended Action=NONE;"
        incident.error.code = dcgm_errors.DCGM_FR_CLOCK_THROTTLE_POWER
        incident.entityInfo = dcgm_structs.c_dcgmGroupEntityPair_t()
        incident.entityInfo.entityGroupId = group_id
        incident.entityInfo.entityId = entity_id
        return incident

    def test_perform_health_check_reports_clock_throttle_power(self):
        """_perform_health_check never suppresses configured error codes; that
        suppression is applied later against the fully assembled health_status
        (see _suppress_configured_error_codes). Only NVLINK_DOWN false positives
        on non-NVLink GPUs are filtered per-incident during the check itself."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        mock_response.incidentCount = 1
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        mock_response.incidents[0] = self._get_power_throttle_incident(0, 1)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)

        expected_response = watcher._get_health_status_dict()
        expected_response["DCGM_HEALTH_WATCH_POWER"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_CLOCK_THROTTLE_POWER",
                    message="ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU:1 Recommended Action=NONE;",
                )
            },
        )
        assert response == expected_response
        assert connectivity_success == True

    def test_suppress_configured_error_codes_noop_by_default(self):
        """With no suppressed_error_codes configured, health_status is left untouched."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        health_status = watcher._get_health_status_dict()
        health_status["DCGM_HEALTH_WATCH_POWER"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_CLOCK_THROTTLE_POWER",
                    message="ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU:1 Recommended Action=NONE;",
                )
            },
        )
        expected = copy.deepcopy(health_status)

        watcher._suppress_configured_error_codes(health_status)

        assert health_status == expected

    def test_suppress_configured_error_codes_clears_matching_dcgm_watch(self):
        """A DCGM health-watch incident (e.g. GpuPowerWatch) matching a suppressed
        error code is dropped and the watch reverts to PASS."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            suppressed_error_codes=frozenset({"DCGM_FR_CLOCK_THROTTLE_POWER"}),
        )
        health_status = watcher._get_health_status_dict()
        health_status["DCGM_HEALTH_WATCH_POWER"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_CLOCK_THROTTLE_POWER",
                    message="ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU:1 Recommended Action=NONE;",
                )
            },
        )

        watcher._suppress_configured_error_codes(health_status)

        expected_response = watcher._get_health_status_dict()
        assert health_status == expected_response

    def test_suppress_configured_error_codes_only_suppresses_matching_entities(self):
        """A genuine (non-suppressed) incident on another GPU/watch must still be reported."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            suppressed_error_codes=frozenset({"DCGM_FR_CLOCK_THROTTLE_POWER"}),
        )
        health_status = watcher._get_health_status_dict()
        health_status["DCGM_HEALTH_WATCH_POWER"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_CLOCK_THROTTLE_POWER",
                    message="ErrorCode:DCGM_FR_CLOCK_THROTTLE_POWER GPU:1 Recommended Action=NONE;",
                )
            },
        )
        health_status["DCGM_HEALTH_WATCH_PCIE"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                2: dcgm.types.ErrorDetails(
                    code="DCGM_FR_PCI_REPLAY_RATE",
                    message="Detected more than 8 PCIe replays per minute for GPU 1.",
                )
            },
        )

        watcher._suppress_configured_error_codes(health_status)

        expected_response = watcher._get_health_status_dict()
        expected_response["DCGM_HEALTH_WATCH_PCIE"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                2: dcgm.types.ErrorDetails(
                    code="DCGM_FR_PCI_REPLAY_RATE",
                    message="Detected more than 8 PCIe replays per minute for GPU 1.",
                )
            },
        )
        assert health_status == expected_response

    def test_suppress_configured_error_codes_applies_to_custom_field_monitors(self):
        """Suppression is generalized: it also applies to non-DCGM-health-watch entries
        such as GpuThermalMarginWatch (custom field monitoring), not just native
        DCGM health check incidents."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            suppressed_error_codes=frozenset({"GPU_TEMP_HW_SLOWDOWN_VIOLATION"}),
        )
        health_status = watcher._get_health_status_dict()
        health_status["DCGM_HEALTH_WATCH_THERMAL_MARGIN"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.WARN,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="GPU_TEMP_HW_SLOWDOWN_VIOLATION",
                    message="GPU 0 thermal margin below HW slowdown T.Limit",
                )
            },
        )

        watcher._suppress_configured_error_codes(health_status)

        expected_response = watcher._get_health_status_dict()
        expected_response["DCGM_HEALTH_WATCH_THERMAL_MARGIN"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.PASS, entity_failures={}
        )
        assert health_status == expected_response

    def _get_nvlink_incident(self, group_id, entity_id, link_id):
        """Helper to create NvLink down incident for testing."""
        incident = dcgm_structs.c_dcgmIncidentInfo_t()
        incident.system = dcgm_structs.DCGM_HEALTH_WATCH_NVLINK
        incident.health = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        incident.error = dcgm_structs.c_dcgmDiagErrorDetail_t()
        incident.error.msg = f"GPU {entity_id}'s NvLink link {link_id} is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        incident.error.code = dcgm_errors.DCGM_FR_NVLINK_DOWN
        incident.entityInfo = dcgm_structs.c_dcgmGroupEntityPair_t()
        incident.entityInfo.entityGroupId = group_id
        incident.entityInfo.entityId = entity_id
        return incident

    def _make_debounce_watcher(self, thresholds: dict[str, int]) -> dcgm.DCGMWatcher:
        return dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            health_check_min_consecutive_polls=thresholds,
        )

    @staticmethod
    def _health_response(incidents: list) -> dcgm_structs.c_dcgmHealthResponse_v4:
        """Build a health check response carrying the given incidents."""
        response = dcgm_structs.c_dcgmHealthResponse_v4()
        response.version = dcgm_structs.dcgmHealthResponse_version4
        response.overallHealth = (
            dcgm_structs.DCGM_HEALTH_RESULT_FAIL if incidents else dcgm_structs.DCGM_DIAG_RESULT_PASS
        )
        response.incidentCount = len(incidents)
        # Allocated per response rather than on the class, so one test cannot leak
        # incidents into the next.
        response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        for index, incident in enumerate(incidents):
            response.incidents[index] = incident
        return response

    def _poll(self, watcher: dcgm.DCGMWatcher, dcgm_group_mock: MagicMock, incidents: list) -> dict:
        dcgm_group_mock.health.Check.return_value = self._health_response(incidents)
        health_status, _ = watcher._perform_health_check(dcgm_group_mock)
        return health_status

    def test_incident_debounce_publishes_first_observation_by_default(self) -> None:
        """With no thresholds configured, an incident is published on the poll it appears."""
        watcher = self._make_debounce_watcher({})

        health_status = self._poll(watcher, MagicMock(), [self._get_nvlink_incident(0, 1, 16)])

        assert health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[1].code == "DCGM_FR_NVLINK_DOWN"
        # Unconfigured codes are never tracked, so the dict stays empty.
        assert watcher._incident_streaks == {}

    def test_incident_debounce_withholds_until_threshold_is_reached(self) -> None:
        """A threshold of 2 withholds the first observation and publishes the second."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        dcgm_group_mock = MagicMock()
        incidents = [self._get_nvlink_incident(0, 1, 16)]

        first = self._poll(watcher, dcgm_group_mock, incidents)
        assert first["DCGM_HEALTH_WATCH_NVLINK"] == dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.PASS, entity_failures={}
        )

        second = self._poll(watcher, dcgm_group_mock, incidents)
        assert second["DCGM_HEALTH_WATCH_NVLINK"].status == dcgm.types.HealthStatus.FAIL
        assert second["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[1].code == "DCGM_FR_NVLINK_DOWN"

    def test_incident_debounce_keeps_publishing_past_the_threshold(self) -> None:
        """A sustained fault keeps publishing once the threshold is met."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        dcgm_group_mock = MagicMock()
        incidents = [self._get_nvlink_incident(0, 1, 16)]

        self._poll(watcher, dcgm_group_mock, incidents)

        for _ in range(3):
            health_status = self._poll(watcher, dcgm_group_mock, incidents)
            assert health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[1].code == "DCGM_FR_NVLINK_DOWN"

    def test_incident_debounce_counts_one_streak_per_gpu_per_poll(self) -> None:
        """DCGM reports one incident per down link, so a GPU with several down links
        yields several records for the same code in a single poll. The streak must
        advance once per poll, or the threshold is reached within the first poll and
        the debounce is defeated."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        dcgm_group_mock = MagicMock()
        two_links_one_gpu = [self._get_nvlink_incident(0, 3, 16), self._get_nvlink_incident(0, 3, 17)]

        first = self._poll(watcher, dcgm_group_mock, two_links_one_gpu)
        assert first["DCGM_HEALTH_WATCH_NVLINK"].entity_failures == {}
        assert watcher._incident_streaks == {("DCGM_FR_NVLINK_DOWN", 3): 1}

        # Both records are published together once the threshold is genuinely met.
        second = self._poll(watcher, dcgm_group_mock, two_links_one_gpu)
        failure = second["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[3]
        assert failure.code == "DCGM_FR_NVLINK_DOWN"
        assert "link 16" in failure.message
        assert "link 17" in failure.message

    def test_incident_debounce_streak_resets_when_incident_is_absent(self) -> None:
        """A link down on alternate polls never reaches its threshold."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        dcgm_group_mock = MagicMock()
        down = [self._get_nvlink_incident(0, 1, 16)]

        for incidents in (down, [], down, [], down):
            health_status = self._poll(watcher, dcgm_group_mock, incidents)
            assert health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures == {}

        assert watcher._incident_streaks == {("DCGM_FR_NVLINK_DOWN", 1): 1}

    def test_incident_debounce_is_tracked_per_gpu(self) -> None:
        """Reaching the threshold on one GPU does not publish another GPU's first observation."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        dcgm_group_mock = MagicMock()

        self._poll(watcher, dcgm_group_mock, [self._get_nvlink_incident(0, 1, 16)])
        health_status = self._poll(
            watcher,
            dcgm_group_mock,
            [self._get_nvlink_incident(0, 1, 16), self._get_nvlink_incident(0, 2, 16)],
        )

        failures = health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures
        assert 1 in failures
        assert 2 not in failures

    def test_incident_debounce_only_applies_to_configured_codes(self) -> None:
        """An unconfigured code in the same poll is published immediately."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})

        health_status = self._poll(
            watcher,
            MagicMock(),
            [self._get_nvlink_incident(0, 1, 16), self._get_pcie_incident(0, 1)],
        )

        assert health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures == {}
        assert health_status["DCGM_HEALTH_WATCH_PCIE"].entity_failures[1].code == "DCGM_FR_PCI_REPLAY_RATE"

    def test_incident_debounce_failed_poll_does_not_reset_the_streak(self) -> None:
        """A poll that observed nothing must not clear a streak, so a fault spanning a
        DCGM timeout still publishes on its next observation."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        dcgm_group_mock = MagicMock()
        incidents = [self._get_nvlink_incident(0, 1, 16)]

        self._poll(watcher, dcgm_group_mock, incidents)

        dcgm_group_mock.health.Check.side_effect = dcgm_structs.DCGMError_Timeout()
        _, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        assert connectivity_success is False
        assert watcher._incident_streaks == {("DCGM_FR_NVLINK_DOWN", 1): 1}

        dcgm_group_mock.health.Check.side_effect = None
        health_status = self._poll(watcher, dcgm_group_mock, incidents)
        assert health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[1].code == "DCGM_FR_NVLINK_DOWN"

    def test_incident_debounce_counts_withheld_incidents(self) -> None:
        """Withheld incidents are observable via the debounced counter."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 2})
        counter = dcgm.metrics.dcgm_health_check_debounced_incidents.labels("DCGM_FR_NVLINK_DOWN", "1")
        before = counter._value.get()

        # Two links on GPU 1 in one poll must count once, not twice.
        self._poll(watcher, MagicMock(), [self._get_nvlink_incident(0, 1, 16), self._get_nvlink_incident(0, 1, 17)])

        assert counter._value.get() == before + 1

    def test_incident_debounce_threshold_of_one_is_not_tracked(self) -> None:
        """A configured threshold of 1 is today's behaviour, so no streak is kept."""
        watcher = self._make_debounce_watcher({"DCGM_FR_NVLINK_DOWN": 1})

        health_status = self._poll(watcher, MagicMock(), [self._get_nvlink_incident(0, 1, 16)])

        assert health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[1].code == "DCGM_FR_NVLINK_DOWN"
        assert watcher._health_check_min_consecutive_polls == {}

    def test_perform_health_check_multiple_failures_same_gpu(self):
        """Test that multiple failures for the same GPU are aggregated into a single error message."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        mock_response.incidentCount = 4
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()

        # Simulate 4 NvLink failures for GPU 0 (links 8, 9, 14, 15)
        mock_response.incidents[0] = self._get_nvlink_incident(0, 0, 8)
        mock_response.incidents[1] = self._get_nvlink_incident(0, 0, 9)
        mock_response.incidents[2] = self._get_nvlink_incident(0, 0, 14)
        mock_response.incidents[3] = self._get_nvlink_incident(0, 0, 15)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()

        # Expected: All 4 NvLink failures should be aggregated into a single message
        expected_message = (
            "GPU 0's NvLink link 8 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 9 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 14 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        )

        expected_response["DCGM_HEALTH_WATCH_NVLINK"] = dcgm.types.HealthDetails(
            status=dcgm.types.HealthStatus.FAIL,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=expected_message,
                )
            },
        )

        assert response == expected_response
        assert connectivity_success == True

        # Verify that all 4 failures are captured in the message
        assert "link 8" in response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[0].message
        assert "link 9" in response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[0].message
        assert "link 14" in response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[0].message
        assert "link 15" in response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[0].message

        # Verify messages are separated by semicolons
        assert response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[0].message.count(";") == 3

    def test_perform_health_check_multiple_gpus_multiple_failures_each(self):
        """Test that multiple failures across multiple GPUs are properly handled."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        mock_response.incidentCount = 8
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()

        # Simulate 4 NvLink failures for GPU 0 and 4 for GPU 1
        mock_response.incidents[0] = self._get_nvlink_incident(0, 0, 8)
        mock_response.incidents[1] = self._get_nvlink_incident(0, 0, 9)
        mock_response.incidents[2] = self._get_nvlink_incident(0, 0, 14)
        mock_response.incidents[3] = self._get_nvlink_incident(0, 0, 15)
        mock_response.incidents[4] = self._get_nvlink_incident(0, 1, 8)
        mock_response.incidents[5] = self._get_nvlink_incident(0, 1, 9)
        mock_response.incidents[6] = self._get_nvlink_incident(0, 1, 12)
        mock_response.incidents[7] = self._get_nvlink_incident(0, 1, 13)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)

        # Verify both GPUs have entries
        assert 0 in response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures
        assert 1 in response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures

        # Verify GPU 0 has all 4 link failures
        gpu0_message = response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[0].message
        assert "link 8" in gpu0_message
        assert "link 9" in gpu0_message
        assert "link 14" in gpu0_message
        assert "link 15" in gpu0_message
        assert gpu0_message.count(";") == 3

        # Verify GPU 1 has all 4 link failures
        gpu1_message = response["DCGM_HEALTH_WATCH_NVLINK"].entity_failures[1].message
        assert "link 8" in gpu1_message
        assert "link 9" in gpu1_message
        assert "link 12" in gpu1_message
        assert "link 13" in gpu1_message
        assert gpu1_message.count(";") == 3

        assert connectivity_success == True

    @patch("pydcgm.DcgmHandle.__new__")
    @patch("pydcgm.DcgmGroup.__new__")
    def test_start(self, mock_dcgm_group, mock_dcgm_handle):
        event_processor_test = FakeEventProcessorInTest()
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[event_processor_test],
            dcgm_k8s_service_enabled=False,
        )
        exit = MagicMock(spec=Event)
        exit.is_set.side_effect = [False, False, False, True]
        exit.wait.side_effect = [False, False, True]
        dcgm_handle_mock = MagicMock()
        mock_dcgm_handle.return_value = dcgm_handle_mock

        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_DIAG_RESULT_PASS
        mock_response.incidentCount = 0
        mock_response.incidents = dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS
        dcgm_group_mock.health.Check.return_value = mock_response()

        mock_dcgm_group.return_value = dcgm_group_mock

        expected_response = watcher._get_health_status_dict()
        watcher.start([], exit)

        assert event_processor_test.health_details == expected_response
        assert dcgm_group_mock.health.Check.call_count == 1

    @patch("gpu_health_monitor.dcgm_watcher.dcgm._run_dcgm_server")
    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmHandle")
    def test_local_managed_exposes_in_process_embedded_handle(self, mock_handle, mock_run_server):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            dcgm_mode="local-managed",
        )

        handle = watcher._create_dcgm_handle()

        mock_handle.assert_called_once_with(opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO)
        mock_run_server.assert_called_once_with(5555, "127.0.0.1")
        assert handle == mock_handle.return_value

    @patch("gpu_health_monitor.dcgm_watcher.dcgm._run_dcgm_server", side_effect=RuntimeError("bind failed"))
    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmHandle")
    def test_local_managed_stops_embedded_handle_when_server_start_fails(self, mock_handle, _mock_run_server):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            dcgm_mode="local-managed",
        )

        with pytest.raises(RuntimeError, match="bind failed"):
            watcher._create_dcgm_handle()

        mock_handle.return_value.Shutdown.assert_called_once()

    @patch("gpu_health_monitor.dcgm_watcher.dcgm._run_dcgm_server")
    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmHandle")
    def test_local_managed_rejects_non_loopback_address(self, mock_handle, mock_run_server):
        watcher = dcgm.DCGMWatcher(
            addr="dcgm-hostengine.nvsentinel.svc:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            dcgm_mode="local-managed",
        )

        with pytest.raises(ValueError, match="requires a loopback DCGM address"):
            watcher._create_dcgm_handle()

        mock_handle.assert_not_called()
        mock_run_server.assert_not_called()

    def test_dcgm_3_server_run_compatibility_fallback(self, monkeypatch):
        engine_run = MagicMock(return_value=0)
        check_return = MagicMock()
        agent = MagicMock(spec=["dcgmFP"])
        agent.dcgmFP.return_value = engine_run
        monkeypatch.setattr(dcgm, "dcgm_agent", agent)
        monkeypatch.setattr(dcgm.dcgm_structs, "_dcgmCheckReturn", check_return, raising=False)

        dcgm._run_dcgm_server(5555, "127.0.0.1")

        agent.dcgmFP.assert_called_once_with("dcgmEngineRun")
        engine_run.assert_called_once_with(5555, b"127.0.0.1", dcgm.DCGM_CONNECTION_TYPE_TCP)
        check_return.assert_called_once_with(0)

    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmHandle")
    def test_remote_mode_connects_to_addr(self, mock_handle):
        """remote mode connects to the configured DCGM address over the network."""
        watcher = dcgm.DCGMWatcher(
            addr="dcgm-hostengine.nvsentinel.svc:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=True,
            dcgm_mode="remote",
        )

        handle = watcher._create_dcgm_handle()

        mock_handle.assert_called_once_with(
            ipAddress="dcgm-hostengine.nvsentinel.svc:5555", opMode=dcgm_structs.DCGM_OPERATION_MODE_AUTO
        )
        assert handle == mock_handle.return_value

    def test_get_dcgm_handle_returns_none_on_error(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            dcgm_mode="local-managed",
        )
        watcher._create_dcgm_handle = MagicMock(side_effect=Exception("boom"))

        assert watcher._get_dcgm_handle() is None

    def test_perform_health_check_connectivity_failure_timeout(self):
        """Test that connectivity failure is detected when DCGM health check times out."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        # Simulate timeout exception - DCGMError_Timeout doesn't take message parameter
        dcgm_group_mock.health.Check.side_effect = dcgm_structs.DCGMError_Timeout()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()

        assert response == expected_response  # Should return empty health status
        assert connectivity_success == False  # Should indicate connectivity failure

    def test_perform_health_check_connectivity_failure_generic_error(self):
        """Test that connectivity failure is detected when DCGM health check raises generic exception."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        # Simulate generic exception
        dcgm_group_mock.health.Check.side_effect = Exception("Connection refused")

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()

        assert response == expected_response  # Should return empty health status
        assert connectivity_success == False  # Should indicate connectivity failure

    def test_perform_health_check_watch_all_incident(self):
        """Test that DCGM_HEALTH_WATCH_ALL incidents are processed correctly."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        mock_response.incidentCount = 1
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()

        incident = dcgm_structs.c_dcgmIncidentInfo_t()
        incident.system = dcgm_structs.DCGM_HEALTH_WATCH_ALL
        incident.health = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        incident.error = dcgm_structs.c_dcgmDiagErrorDetail_t()
        incident.error.msg = "XID 95 detected on GPU 0"
        incident.error.code = dcgm_errors.DCGM_FR_PCI_REPLAY_RATE
        incident.entityInfo = dcgm_structs.c_dcgmGroupEntityPair_t()
        incident.entityInfo.entityGroupId = 0
        incident.entityInfo.entityId = 0
        mock_response.incidents[0] = incident
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)

        assert connectivity_success == True
        assert response["DCGM_HEALTH_WATCH_ALL"].status == dcgm.types.HealthStatus.FAIL
        assert 0 in response["DCGM_HEALTH_WATCH_ALL"].entity_failures
        assert response["DCGM_HEALTH_WATCH_ALL"].entity_failures[0].message == "XID 95 detected on GPU 0"

    def test_perform_health_check_unknown_error_code(self):
        """Test that incidents with unknown error codes use DCGM_FR_UNKNOWN fallback."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        mock_response.incidentCount = 1
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()

        incident = dcgm_structs.c_dcgmIncidentInfo_t()
        incident.system = dcgm_structs.DCGM_HEALTH_WATCH_PCIE
        incident.health = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        incident.error = dcgm_structs.c_dcgmDiagErrorDetail_t()
        incident.error.msg = "Some future error"
        incident.error.code = 99999
        incident.entityInfo = dcgm_structs.c_dcgmGroupEntityPair_t()
        incident.entityInfo.entityGroupId = 0
        incident.entityInfo.entityId = 1
        mock_response.incidents[0] = incident
        dcgm_group_mock.health.Check.return_value = mock_response()

        response, connectivity_success = watcher._perform_health_check(dcgm_group_mock)

        assert connectivity_success == True
        assert response["DCGM_HEALTH_WATCH_PCIE"].status == dcgm.types.HealthStatus.WARN
        assert 1 in response["DCGM_HEALTH_WATCH_PCIE"].entity_failures
        assert response["DCGM_HEALTH_WATCH_PCIE"].entity_failures[1].code == "DCGM_FR_UNKNOWN"

    @patch("pydcgm.DcgmHandle")
    @patch("pydcgm.DcgmGroup")
    def test_initialize_dcgm_monitoring(self, mock_dcgm_group, mock_dcgm_handle):
        """Test that _initialize_dcgm_monitoring properly sets up monitoring components."""
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        # Setup mocks
        dcgm_handle_mock = MagicMock()
        dcgm_group_mock = MagicMock()
        dcgm_group_mock.GetGpuIds.return_value = [0, 1, 2, 3]
        mock_dcgm_group.return_value = dcgm_group_mock

        # Mock system and discovery
        dcgm_system_mock = MagicMock()
        dcgm_system_mock.discovery.GetEntityGroupEntities.return_value = [0, 1, 2, 3]
        dcgm_system_mock.discovery.GetGpuAttributes.return_value = MagicMock(
            identifiers=MagicMock(serial="TEST_SERIAL")
        )
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        # Call the method
        group, gpu_ids, gpu_serials = watcher._initialize_dcgm_monitoring(dcgm_handle_mock)

        # Verify results
        # Note: group will be the conftest.py mock object, not our dcgm_group_mock
        assert group is not None
        assert hasattr(group, "health")
        assert hasattr(group, "GetGpuIds")
        assert gpu_ids == [0, 1, 2, 3]
        assert len(gpu_serials) == 4
        # Verify that health.Set was called on the actual group object
        group.health.Set.assert_called_once()


class TestDCGMHandleLeakFix:
    """Tests for the DCGM handle/connection leak fix (issue #1078).

    Covers: split try-blocks in cleanup and init rollback.
    """

    def _make_watcher(self):
        return dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

    @pytest.mark.parametrize(
        "group_is_none, delete_raises",
        [
            (False, True),
            (True, False),
        ],
        ids=["delete_throws", "none_group"],
    )
    def test_cleanup_dcgm_resources(self, group_is_none, delete_raises):
        """Shutdown() always runs regardless of Delete() outcome."""
        watcher = self._make_watcher()
        dcgm_handle_mock = MagicMock()

        dcgm_group_mock = None
        if not group_is_none:
            dcgm_group_mock = MagicMock()
            if delete_raises:
                dcgm_group_mock.Delete.side_effect = Exception("Delete failed")

        watcher._cleanup_dcgm_resources(dcgm_group_mock, dcgm_handle_mock)

        dcgm_handle_mock.Shutdown.assert_called_once()
        if not group_is_none:
            dcgm_group_mock.Delete.assert_called_once()

    @patch("pydcgm.DcgmGroup.__new__")
    def test_init_monitoring_rolls_back_group_on_failure(self, mock_dcgm_group):
        """Group must be deleted if initialization fails after group creation."""
        watcher = self._make_watcher()
        dcgm_handle_mock = MagicMock()
        dcgm_group_mock = MagicMock()
        mock_dcgm_group.return_value = dcgm_group_mock

        dcgm_system_mock = MagicMock()
        dcgm_system_mock.discovery.GetEntityGroupEntities.return_value = [0, 1]
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        # health.Set() fails after group is created
        dcgm_group_mock.health.Set.side_effect = Exception("DCGM connection lost")

        with pytest.raises(Exception, match="DCGM connection lost"):
            watcher._initialize_dcgm_monitoring(dcgm_handle_mock)

        dcgm_group_mock.Delete.assert_called_once()


class TestSuppressNvlinkDownOnPcieGpus:
    """Tests for per-incident suppression of false positive DCGM_FR_NVLINK_DOWN.

    Two expected-down cases: GPUs with no NVLink silicon are always
    suppressed; unbridged bridge-capable PCIe cards are suppressed only with
    explicit operator opt-in (suppress_unbridged_pcie_nvlink_down), because
    metadata alone cannot distinguish them from a card whose bridge was dead
    at collection time.

    Suppression happens inside _perform_health_check at incident granularity,
    before incidents are aggregated per (watch, GPU), so genuine co-occurring
    incidents on the same GPU and watch are always preserved.
    """

    def _make_watcher(self):
        return dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

    @pytest.mark.parametrize(
        "group_is_none, delete_raises",
        [
            (False, True),
            (True, False),
        ],
        ids=["delete_throws", "none_group"],
    )
    def test_cleanup_dcgm_resources(self, group_is_none, delete_raises):
        """Shutdown() always runs regardless of Delete() outcome."""
        watcher = self._make_watcher()
        dcgm_handle_mock = MagicMock()

        dcgm_group_mock = None
        if not group_is_none:
            dcgm_group_mock = MagicMock()
            if delete_raises:
                dcgm_group_mock.Delete.side_effect = Exception("Delete failed")

        watcher._cleanup_dcgm_resources(dcgm_group_mock, dcgm_handle_mock)

        dcgm_handle_mock.Shutdown.assert_called_once()
        if not group_is_none:
            dcgm_group_mock.Delete.assert_called_once()

    @patch("pydcgm.DcgmGroup.__new__")
    def test_init_monitoring_rolls_back_group_on_failure(self, mock_dcgm_group):
        """Group must be deleted if initialization fails after group creation."""
        watcher = self._make_watcher()
        dcgm_handle_mock = MagicMock()
        dcgm_group_mock = MagicMock()
        mock_dcgm_group.return_value = dcgm_group_mock

        dcgm_system_mock = MagicMock()
        dcgm_system_mock.discovery.GetEntityGroupEntities.return_value = [0, 1]
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        # health.Set() fails after group is created
        dcgm_group_mock.health.Set.side_effect = Exception("DCGM connection lost")

        with pytest.raises(Exception, match="DCGM connection lost"):
            watcher._initialize_dcgm_monitoring(dcgm_handle_mock)

        dcgm_group_mock.Delete.assert_called_once()


class TestSuppressNvlinkDownOnPcieGpus:
    """Tests for per-incident suppression of false positive DCGM_FR_NVLINK_DOWN.

    Two expected-down cases: GPUs with no NVLink silicon are always
    suppressed; unbridged bridge-capable PCIe cards are suppressed only with
    explicit operator opt-in (suppress_unbridged_pcie_nvlink_down), because
    metadata alone cannot distinguish them from a card whose bridge was dead
    at collection time.

    Suppression happens inside _perform_health_check at incident granularity,
    before incidents are aggregated per (watch, GPU), so genuine co-occurring
    incidents on the same GPU and watch are always preserved.
    """

    def _make_watcher(
        self,
        metadata_reader: Optional[MetadataReader] = None,
        suppress_unbridged_pcie: bool = False,
    ) -> dcgm.DCGMWatcher:
        return dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
            metadata_reader=metadata_reader,
            suppress_unbridged_pcie_nvlink_down=suppress_unbridged_pcie,
        )

    def _make_nvlink_incident(self, entity_id: int, msg: str, error_code: int) -> "dcgm_structs.c_dcgmIncidentInfo_t":
        incident = dcgm_structs.c_dcgmIncidentInfo_t()
        incident.system = dcgm_structs.DCGM_HEALTH_WATCH_NVLINK
        incident.health = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        incident.error = dcgm_structs.c_dcgmDiagErrorDetail_t()
        incident.error.msg = msg
        incident.error.code = error_code
        incident.entityInfo = dcgm_structs.c_dcgmGroupEntityPair_t()
        incident.entityInfo.entityGroupId = 0
        incident.entityInfo.entityId = entity_id
        return incident

    def _nvlink_down_incident(self, entity_id: int, link_id: int) -> "dcgm_structs.c_dcgmIncidentInfo_t":
        return self._make_nvlink_incident(
            entity_id,
            f"GPU {entity_id}'s NvLink link {link_id} is currently down",
            dcgm_errors.DCGM_FR_NVLINK_DOWN,
        )

    def _nvlink_threshold_incident(self, entity_id: int) -> "dcgm_structs.c_dcgmIncidentInfo_t":
        return self._make_nvlink_incident(
            entity_id,
            f"GPU {entity_id} NVLink error threshold exceeded",
            dcgm_errors.DCGM_FR_NVLINK_ERROR_THRESHOLD,
        )

    def _run_health_check(self, watcher: dcgm.DCGMWatcher, incidents: list) -> dict[str, dcgm.types.HealthDetails]:
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
        mock_response.incidentCount = len(incidents)
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        for i, incident in enumerate(incidents):
            mock_response.incidents[i] = incident
        dcgm_group_mock.health.Check.return_value = mock_response()

        health_status, connectivity_success = watcher._perform_health_check(dcgm_group_mock)
        assert connectivity_success is True
        return health_status

    def _assert_not_suppressed(self, health_status: dict[str, dcgm.types.HealthDetails]) -> None:
        details = health_status["DCGM_HEALTH_WATCH_NVLINK"]
        assert details.status == dcgm.types.HealthStatus.FAIL
        assert details.entity_failures[0].code == "DCGM_FR_NVLINK_DOWN"

    def test_suppress_no_nvlink_silicon_without_opt_in(self, tmp_path: Path) -> None:
        """L40-class GPU (zero hardware links): suppressed unconditionally —
        no operator opt-in required for the unambiguous case."""
        reader = make_metadata_reader(tmp_path, [L40_NO_NVLINK])
        watcher = self._make_watcher(metadata_reader=reader)

        health_status = self._run_health_check(watcher, [self._nvlink_down_incident(0, 0)])

        assert health_status["DCGM_HEALTH_WATCH_NVLINK"].status == dcgm.types.HealthStatus.PASS
        assert len(health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures) == 0

    def test_no_suppress_unbridged_pcie_by_default(self, tmp_path: Path) -> None:
        """Unbridged A100 PCIe WITHOUT operator opt-in: NOT suppressed.

        Metadata alone cannot distinguish an unbridged card from one whose
        bridge was dead at collection time, so the default is fail-safe."""
        reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED])
        watcher = self._make_watcher(metadata_reader=reader)

        health_status = self._run_health_check(watcher, [self._nvlink_down_incident(0, 8)])

        self._assert_not_suppressed(health_status)

    def test_suppress_unbridged_pcie_with_opt_in(self, tmp_path: Path) -> None:
        """Unbridged A100 PCIe WITH operator opt-in (the live-repro shape):
        all NVLINK_DOWN incidents suppressed, watch stays PASS."""
        reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED])
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(
            watcher,
            [self._nvlink_down_incident(0, link) for link in range(12)],
        )

        assert health_status == watcher._get_health_status_dict()
        assert health_status["DCGM_HEALTH_WATCH_NVLINK"].status == dcgm.types.HealthStatus.PASS
        assert len(health_status["DCGM_HEALTH_WATCH_NVLINK"].entity_failures) == 0

    def test_no_suppress_sxm_gpu_with_active_nvlink(self, tmp_path: Path) -> None:
        """SXM GPU with trained links: incident NOT suppressed even with the
        opt-in enabled."""
        reader = make_metadata_reader(tmp_path, [H100_SXM_TRAINED])
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(watcher, [self._nvlink_down_incident(0, 5)])

        self._assert_not_suppressed(health_status)

    def test_no_suppress_sxm_gpu_zero_active_links(self, tmp_path: Path) -> None:
        """SXM GPU whose metadata shows zero active links (fabric manager may
        not have trained links at collection time): NOT suppressed even with
        the opt-in enabled — the expectation is UNKNOWN, so we fail closed."""
        gpu = dict(H100_SXM_TRAINED, nvlink_active_link_count=0)
        reader = make_metadata_reader(tmp_path, [gpu])
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(watcher, [self._nvlink_down_incident(0, 5)])

        self._assert_not_suppressed(health_status)

    def test_mixed_topology_only_expected_down_suppressed(self, tmp_path: Path) -> None:
        """Mixed node with opt-in: unbridged PCIe GPU suppressed, trained SXM
        GPU kept."""
        gpu1 = dict(H100_SXM_TRAINED, gpu_id=1, uuid="GPU-1")
        reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED, gpu1])
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(
            watcher,
            [self._nvlink_down_incident(0, 3), self._nvlink_down_incident(1, 5)],
        )

        details = health_status["DCGM_HEALTH_WATCH_NVLINK"]
        assert details.status == dcgm.types.HealthStatus.FAIL
        assert 0 not in details.entity_failures
        assert details.entity_failures[1].code == "DCGM_FR_NVLINK_DOWN"

    def test_mixed_codes_same_gpu_preserves_genuine_incident(self, tmp_path: Path) -> None:
        """Regression: a genuine non-NVLINK_DOWN incident aggregated on the same
        GPU and watch survives suppression of the NVLINK_DOWN false positives.

        NVLINK_DOWN arrives first, so aggregate-level suppression (keyed on the
        first incident's code) would have deleted the whole entry and dropped
        the genuine threshold incident with it."""
        reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED])
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(
            watcher,
            [
                self._nvlink_down_incident(0, 8),
                self._nvlink_threshold_incident(0),
                self._nvlink_down_incident(0, 9),
            ],
        )

        details = health_status["DCGM_HEALTH_WATCH_NVLINK"]
        assert details.status == dcgm.types.HealthStatus.FAIL
        assert details.entity_failures[0].code == "DCGM_FR_NVLINK_ERROR_THRESHOLD"
        assert details.entity_failures[0].message == "GPU 0 NVLink error threshold exceeded"
        assert "currently down" not in details.entity_failures[0].message

    def test_no_suppress_when_metadata_unavailable(self) -> None:
        """Metadata file missing: incident NOT suppressed (fail closed)."""
        reader = MetadataReader("/nonexistent/file.json")
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(watcher, [self._nvlink_down_incident(0, 8)])

        self._assert_not_suppressed(health_status)

    def test_no_suppress_when_no_metadata_reader(self) -> None:
        """No metadata reader configured: incident NOT suppressed (fail closed)."""
        watcher = self._make_watcher(metadata_reader=None, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(watcher, [self._nvlink_down_incident(0, 8)])

        self._assert_not_suppressed(health_status)

    def test_other_nvlink_error_code_not_suppressed(self, tmp_path: Path) -> None:
        """Non-NVLINK_DOWN codes on an expected-down GPU are never suppressed."""
        reader = make_metadata_reader(tmp_path, [A100_PCIE_UNBRIDGED])
        watcher = self._make_watcher(metadata_reader=reader, suppress_unbridged_pcie=True)

        health_status = self._run_health_check(watcher, [self._nvlink_threshold_incident(0)])

        details = health_status["DCGM_HEALTH_WATCH_NVLINK"]
        assert details.status == dcgm.types.HealthStatus.FAIL
        assert details.entity_failures[0].code == "DCGM_FR_NVLINK_ERROR_THRESHOLD"


class TestProbeWatchdog:
    """A wedged driver never returns, so the poll loop cannot report its own hang."""

    def _collector(self, succeed: bool = True):
        """Returns (recorded_hangs, on_hang_callback).

        ``succeed`` controls whether on_hang reports delivery success. False
        models a failed UDS publish that the watchdog must retry.
        """
        hangs: list[tuple[str, float]] = []

        def on_hang(operation: str, elapsed: float) -> bool:
            hangs.append((operation, elapsed))
            return succeed

        return hangs, on_hang

    def test_probe_within_deadline_is_not_reported(self):
        hangs, on_hang = self._collector()
        watchdog = dcgm.ProbeWatchdog(10.0, on_hang)

        with watchdog.probe("dcgm_health_check"):
            assert watchdog.poll_once() is False

        assert hangs == []

    def test_probe_past_deadline_is_reported_once(self):
        hangs, on_hang = self._collector()
        watchdog = dcgm.ProbeWatchdog(0.01, on_hang)

        with watchdog.probe("dcgm_health_check"):
            time.sleep(0.05)
            assert watchdog.poll_once() is True
            # Delivered successfully: further polls must not spam.
            assert watchdog.poll_once() is False

        assert len(hangs) == 1
        operation, elapsed = hangs[0]
        assert operation == "dcgm_health_check"
        assert elapsed >= 0.01

    @patch("gpu_health_monitor.dcgm_watcher.dcgm.metrics.dcgm_probe_hangs")
    def test_failed_delivery_is_retried_until_success(self, probe_hangs_metric):
        """A hung poll loop has no next cycle, so a failed publish must retry."""
        attempts: list[int] = []

        def on_hang(operation: str, elapsed: float) -> bool:
            attempts.append(1)
            # Fail the first publish (e.g. platform-connector socket missing),
            # then succeed — the pattern seen when the connector starts after us.
            return len(attempts) >= 2

        watchdog = dcgm.ProbeWatchdog(0.01, on_hang)

        with watchdog.probe("dcgm_health_check"):
            time.sleep(0.05)
            assert watchdog.poll_once() is False
            assert watchdog.poll_once() is True
            assert watchdog.poll_once() is False

        assert len(attempts) == 2
        # Detection is observable immediately and counted once, independent of
        # how many delivery attempts the event needs.
        probe_hangs_metric.labels.assert_called_once_with("dcgm_health_check")
        probe_hangs_metric.labels.return_value.inc.assert_called_once_with()

    def test_completed_probe_is_never_reported(self):
        hangs, on_hang = self._collector()
        watchdog = dcgm.ProbeWatchdog(0.01, on_hang)

        with watchdog.probe("dcgm_connect"):
            pass

        time.sleep(0.05)

        assert watchdog.poll_once() is False
        assert hangs == []

    def test_probe_completion_waits_for_bounded_delivery(self):
        """Recovery cannot overtake the unhealthy event publication."""
        entered_probe = Event()
        finish_probe = Event()
        probe_returned = Event()
        delivery_started = Event()
        release_delivery = Event()
        poll_result = []

        def on_hang(operation: str, elapsed: float) -> bool:
            delivery_started.set()
            assert release_delivery.wait(1)
            return True

        watchdog = dcgm.ProbeWatchdog(0.01, on_hang)

        def run_probe():
            with watchdog.probe("dcgm_health_check"):
                entered_probe.set()
                finish_probe.wait()
            probe_returned.set()

        probe_thread = Thread(target=run_probe, daemon=True)
        probe_thread.start()
        assert entered_probe.wait(1)
        time.sleep(0.05)

        report_thread = Thread(target=lambda: poll_result.append(watchdog.poll_once()), daemon=True)
        report_thread.start()
        assert delivery_started.wait(1)

        finish_probe.set()
        assert not probe_returned.wait(0.05)

        release_delivery.set()
        report_thread.join(1)
        probe_thread.join(1)

        assert poll_result == [True]
        assert probe_returned.is_set()

    def test_second_hang_episode_is_reported_again(self):
        hangs, on_hang = self._collector()
        watchdog = dcgm.ProbeWatchdog(0.01, on_hang)

        for _ in range(2):
            with watchdog.probe("dcgm_health_check"):
                time.sleep(0.05)
                watchdog.poll_once()

        assert len(hangs) == 2

    def test_run_returns_when_exit_is_set(self):
        hangs, on_hang = self._collector()
        watchdog = dcgm.ProbeWatchdog(10.0, on_hang)
        exit_event = Event()
        exit_event.set()

        watchdog.run(exit_event, interval_seconds=0.01)

        assert hangs == []


class TestDCGMWatcherProbeWatchdog:
    def _make_watcher(self, probe_deadline_seconds: float, callbacks=None) -> dcgm.DCGMWatcher:
        return dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=callbacks if callbacks is not None else [],
            dcgm_k8s_service_enabled=False,
            probe_deadline_seconds=probe_deadline_seconds,
        )

    def test_watchdog_disabled_when_deadline_not_positive(self):
        watcher = self._make_watcher(probe_deadline_seconds=0)

        assert watcher._probe_watchdog is None
        # Probe tracking must degrade to a no-op rather than failing.
        with watcher._probe("dcgm_health_check"):
            pass

    def test_watchdog_enabled_tracks_probes(self):
        watcher = self._make_watcher(probe_deadline_seconds=30)

        assert watcher._probe_watchdog is not None
        with watcher._probe("dcgm_health_check"):
            assert watcher._probe_watchdog._operation == "dcgm_health_check"
        assert watcher._probe_watchdog._operation is None

    def test_hang_is_delivered_to_callbacks(self):
        fake = FakeEventProcessorInTest()
        watcher = self._make_watcher(probe_deadline_seconds=30, callbacks=[fake])

        assert watcher._report_probe_unresponsive("dcgm_health_check", 42.0) is True

        assert fake.probe_unresponsive_calls == [("dcgm_health_check", 42.0, "remote")]

    def test_cleanup_is_probe_tracked(self):
        watcher = self._make_watcher(probe_deadline_seconds=30)
        dcgm_handle_mock = MagicMock()
        observed = {}

        # Mid-loop cleanup after connectivity failure still reaches the driver
        # and must stay tracked.
        dcgm_handle_mock.Shutdown.side_effect = lambda: observed.update(operation=watcher._probe_watchdog._operation)

        watcher._cleanup_dcgm_resources(None, dcgm_handle_mock)

        assert observed["operation"] == "dcgm_cleanup"
        assert watcher._probe_watchdog._operation is None

    def test_teardown_cleanup_skips_probe_tracking(self):
        watcher = self._make_watcher(probe_deadline_seconds=30)
        dcgm_handle_mock = MagicMock()
        observed = {}

        # Intentional loop teardown must not publish GpuDcgmUnresponsive when
        # Shutdown() is merely slow (rolling upgrades / DCGM restarts).
        dcgm_handle_mock.Shutdown.side_effect = lambda: observed.update(operation=watcher._probe_watchdog._operation)

        watcher._cleanup_dcgm_resources(None, dcgm_handle_mock, track_probe=False)

        assert observed["operation"] is None
        assert watcher._probe_watchdog._operation is None


class TestDCGMWatcherHangSafeOrdering:
    """The loop must publish findings before making further DCGM calls.

    An unresponsive driver blocks every call including Shutdown(), so anything
    published only after cleanup is never published at all.
    """

    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmGroup")
    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmHandle")
    def test_connectivity_failure_is_published_before_cleanup(
        self, mock_dcgm_handle: MagicMock, mock_dcgm_group: MagicMock
    ) -> None:
        published = Event()
        observed = {}

        class SignallingProcessor(FakeEventProcessorInTest):
            def dcgm_connectivity_failed(self) -> bool:
                delivered = super().dcgm_connectivity_failed()
                published.set()
                return delivered

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[SignallingProcessor()],
            dcgm_k8s_service_enabled=False,
        )

        # Saturate the shared callback executor. Critical connectivity delivery
        # must bypass it or the event remains queued behind these workers.
        release_workers = Event()
        saturated_workers = 8
        watcher._callback_thread_pool = ThreadPoolExecutor(max_workers=saturated_workers)
        for _ in range(saturated_workers):
            watcher._callback_thread_pool.submit(release_workers.wait, 10)

        dcgm_handle_mock = MagicMock()

        def cleanup() -> None:
            observed["published_before_cleanup"] = published.is_set()
            release_workers.set()

        dcgm_handle_mock.Shutdown.side_effect = cleanup
        mock_dcgm_handle.return_value = dcgm_handle_mock

        dcgm_group_mock = MagicMock()
        # A timeout from the health check is what flags connectivity failure.
        dcgm_group_mock.health.Check.side_effect = dcgm_structs.DCGMError_Timeout()
        mock_dcgm_group.return_value = dcgm_group_mock

        # The first cycle only connects; the health check runs on the second.
        stop_event = MagicMock(spec=Event)
        stop_event.is_set.side_effect = [False, False, False, True]
        stop_event.wait.side_effect = [False, False, True]
        watcher.start([], stop_event)

        assert observed["published_before_cleanup"] is True

    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmGroup")
    @patch("gpu_health_monitor.dcgm_watcher.dcgm.pydcgm.DcgmHandle")
    def test_thermal_margin_evaluation_is_probe_tracked(self, mock_dcgm_handle, mock_dcgm_group):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[FakeEventProcessorInTest()],
            dcgm_k8s_service_enabled=False,
            probe_deadline_seconds=30,
        )
        observed = {}
        watcher._evaluate_gpu_thermal_margin = lambda *_: observed.update(operation=watcher._probe_watchdog._operation)

        mock_dcgm_handle.return_value = MagicMock()
        dcgm_group_mock = MagicMock()
        health_response = dcgm_structs.c_dcgmHealthResponse_v4()
        health_response.version = dcgm_structs.dcgmHealthResponse_version4
        health_response.overallHealth = dcgm_structs.DCGM_DIAG_RESULT_PASS
        health_response.incidentCount = 0
        dcgm_group_mock.health.Check.return_value = health_response
        mock_dcgm_group.return_value = dcgm_group_mock

        # The first cycle only connects; the health check runs on the second.
        stop_event = MagicMock(spec=Event)
        stop_event.is_set.side_effect = [False, False, False, True]
        stop_event.wait.side_effect = [False, False, True]
        watcher.start([], stop_event)

        assert observed["operation"] == "dcgm_thermal_margin"
