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
from unittest.mock import MagicMock, patch
import dcgm_structs, dcgm_errors, dcgm_fields, dcgm_field_helpers
from threading import Event, Thread
from ctypes import pointer


class FakeEventProcessorInTest(dcgm.types.CallbackInterface):
    def __init__(self) -> None:
        self.health_details = None
        self.gpu_id = None
        self.error_num = None
        self.serial = None
        self.fields_changes = None
        self.connectivity_failed_called = False

    def health_event_occurred(
        self, health_details: dict[str, dcgm.types.HealthDetails], gpu_ids: list[int], serials: dict[int, str]
    ):
        self.health_details = health_details
        self.serials = serials

    def dcgm_connectivity_failed(self):
        self.connectivity_failed_called = True


class TestDCGMHealthChecks:

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

    def test_get_available_health_watches(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        health_watches = watcher._get_available_health_watches()
        assert len(health_watches) == 12

    def test_get_available_error_codes(self):
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        error_codes = watcher._get_available_error_codes()
        assert len(error_codes) == 112

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
        assert len(health_status_dict) == 12
        for _, val in health_status_dict.items():
            assert val.status == dcgm.types.HealthStatus.PASS
            assert val.entity_failures == {}

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
        exit = Event()
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

        watcher_thread = Thread(target=watcher.start, args=([], exit))
        expected_response = watcher._get_health_status_dict()
        watcher_thread.start()
        exit.wait(5)  # wait for the watcher to enter the event loop
        exit.wait(4)
        exit.wait(3)
        exit.wait(2)
        exit.wait(1)
        exit.set()
        watcher_thread.join()
        assert event_processor_test.health_details == expected_response

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
