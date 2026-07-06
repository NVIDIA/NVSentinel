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
from unittest.mock import MagicMock, patch
import dcgm_structs, dcgm_errors, dcgm_fields
from threading import Event
from ctypes import pointer
import copy
import json
import pytest


class FakeEventProcessorInTest(dcgm.types.CallbackInterface):
    def __init__(self) -> None:
        self.health_details = None
        self.gpu_id = None
        self.error_num = None
        self.serial = None
        self.fields_changes = None
        self.connectivity_failed_called = False

    def health_event_occurred(self, health_details: dict[str, dcgm.types.HealthDetails], gpu_ids: list[int]):
        self.health_details = health_details

    def dcgm_connectivity_failed(self):
        self.connectivity_failed_called = True


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
        assert len(error_codes) == 113

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
        """_perform_health_check itself never suppresses; suppression is applied later
        against the fully assembled health_status (see _suppress_configured_error_codes)."""
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
