from gpu_health_monitor.dcgm_watcher import dcgm
from unittest.mock import MagicMock, patch
import dcgm_structs, dcgm_errors, dcgm_fields, dcgm_field_helpers
from threading import Event, Thread
from ctypes import *


class FakeEventProcessorInTest(dcgm.types.CallbackInterface):
    def __init__(self) -> None:
        self.health_details = None
        self.gpu_id = None
        self.error_num = None
        self.fields_changes = None

    def health_event_occurred(self, health_details: dict[str, dcgm.types.HealthDetails]):
        self.health_details = health_details

    def xid_event_occurred(self, gpu_id: str, error_num: int):
        self.gpu_id = gpu_id
        self.error_num = error_num

    def clear_all_xid_errors(self):
        pass


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
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        health_watches = watcher._get_available_health_watches()
        assert len(health_watches) == 12

    def test_get_available_error_codes(self):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        error_codes = watcher._get_available_error_codes()
        assert len(error_codes) == 112

    def test_get_available_fields(self):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        dcgm_fields = watcher._get_available_fields()
        assert len(dcgm_fields) == 320

    def test_get_health_status_dict(self):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        health_status_dict = watcher._get_health_status_dict()
        assert len(health_status_dict) == 12
        for _, val in health_status_dict.items():
            assert val.status == dcgm.types.HealthStatus.PASS
            assert val.entity_failures == {}

    @patch("pydcgm.DcgmGroup.__new__")
    def test_dcgm_create_group(self, mock_dcgm_group):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
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
                raise "unknown entityGroupId"

        dcgm_system_mock.discovery.GetEntityGroupEntities = MagicMock(side_effect=GetEntityGroupEntities_mock)
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        dcgm_group = watcher._create_dcgm_group_with_all_entities(dcgm_handle_mock)
        for gpu in supported_gpus:
            dcgm_group.AddEntity.assert_any_call(dcgm_fields.DCGM_FE_GPU, gpu)
        for switch in supported_switches:
            dcgm_group.AddEntity.assert_any_call(dcgm_fields.DCGM_FE_SWITCH, switch)

    def test_perform_health_check_all_watch_pass(self):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_DIAG_RESULT_PASS
        mock_response.incidentCount = 0
        mock_response.incidents = dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS
        dcgm_group_mock.health.Check.return_value = mock_response()

        response = watcher._perform_health_check(dcgm_group_mock)
        expected_response = watcher._get_health_status_dict()
        assert response == expected_response

    def test_perform_health_check_one_watch_fail_single_entity_failure(self):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        mock_response.incidentCount = 1
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        mock_response.incidents[0] = self._get_pcie_incident(0, 1)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response = watcher._perform_health_check(dcgm_group_mock)
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

    def test_perform_health_check_one_watch_fail_multiple_entity_failure(self):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        dcgm_group_mock = MagicMock()
        mock_response = dcgm_structs.c_dcgmHealthResponse_v4
        mock_response.version = dcgm_structs.dcgmHealthResponse_version4
        mock_response.overallHealth = dcgm_structs.DCGM_HEALTH_RESULT_WARN
        mock_response.incidentCount = 2
        mock_response.incidents = (dcgm_structs.c_dcgmIncidentInfo_t * dcgm_structs.DCGM_HEALTH_WATCH_MAX_INCIDENTS)()
        mock_response.incidents[0] = self._get_pcie_incident(0, 1)
        mock_response.incidents[1] = self._get_pcie_incident(0, 2)
        dcgm_group_mock.health.Check.return_value = mock_response()

        response = watcher._perform_health_check(dcgm_group_mock)
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

    @patch("pydcgm.DcgmGroup.__new__")
    def test_register_xid_callback_on_all_gpus(self, mock_dcgm_group):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        dcgm_handle_mock = MagicMock()
        dcgm_system_mock = MagicMock()
        dcgm_group_mock = MagicMock()
        mock_dcgm_group.return_value = dcgm_group_mock
        supported_gpus = [0, 1, 2, 3, 4, 5, 6, 7]
        dcgm_system_mock.discovery.GetEntityGroupEntities.return_value = supported_gpus
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        dcgm_groups = watcher._register_xid_callbacks_on_all_gpus(dcgm_handle_mock)

        assert len(dcgm_groups) == len(supported_gpus)
        for dcgm_group in dcgm_groups:
            assert dcgm_group.AddGpu.called
        assert dcgm_group_mock.policy.Register.call_count == len(supported_gpus)

    @patch("pydcgm.DcgmGroup.__new__")
    def test_un_register_xid_callback_on_all_gpus(self, mock_dcgm_group):
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        dcgm_handle_mock = MagicMock()
        dcgm_system_mock = MagicMock()
        dcgm_group_mock = MagicMock()
        mock_dcgm_group.return_value = dcgm_group_mock
        supported_gpus = [0, 1, 2, 3, 4, 5, 6, 7]
        dcgm_system_mock.discovery.GetEntityGroupEntities.return_value = supported_gpus
        dcgm_handle_mock.GetSystem.return_value = dcgm_system_mock

        dcgm_groups = watcher._register_xid_callbacks_on_all_gpus(dcgm_handle_mock)
        watcher._unregister_xid_callbacks(dcgm_groups)

        assert dcgm_group_mock.policy.Unregister.call_count == len(supported_gpus)

    @patch("pydcgm.DcgmHandle.__new__")
    @patch("pydcgm.DcgmGroup.__new__")
    def test_start(self, mock_dcgm_group, mock_dcgm_handle):
        event_processor_test = FakeEventProcessorInTest()
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[event_processor_test])
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

        xid_callback_data = dcgm_structs.c_dcgmPolicyCallbackResponse_v1
        xid_callback_data.xid = dcgm_structs.c_dcgmPolicyConditionXID_t
        xid_callback_data.xid.errnum = 13

        watcher_thread = Thread(target=watcher.start, args=([], exit))
        expected_response = watcher._get_health_status_dict()
        watcher_thread.start()
        exit.wait(5)  # wait for the watcher to enter the event loop
        watcher._xid_event_callback_func(0, pointer(xid_callback_data()))
        exit.wait(4)
        exit.wait(3)
        exit.wait(2)
        exit.wait(1)
        exit.set()
        watcher_thread.join()
        assert event_processor_test.health_details == expected_response
        assert event_processor_test.gpu_id == 0
        assert event_processor_test.error_num == 13
