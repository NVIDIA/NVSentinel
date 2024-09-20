from gpu_health_monitor.dcgm_watcher import dcgm
from unittest.mock import MagicMock, patch
from threading import Event, Thread
from ctypes import *
import grpc
import time
import unittest
from typing import Any
from concurrent import futures
from gpu_health_monitor.dcgm_watcher import types as dcgmtypes
from gpu_health_monitor.platform_connector import platform_connector
from gpu_health_monitor.platform_connector.protos import platformconnector_pb2, platformconnector_pb2_grpc
from gpu_health_monitor.nvml_parser.nvml_xid_parser import DummyNvmlXidParser


socket_path = "/tmp/nvsentinel.sock"
node_name = "node1"


def create_recommend_action_mapping_from_xid_error_to_platform_connector():
    xid_error_recommend_action_connector_mapping: dict[str, platformconnector_pb2.RecommenedAction] = {}
    xid_error_recommend_action_connector_mapping["UNEXPECTED_ERR_REPORT_ISSUE"] = platformconnector_pb2.REPORT_ISSUE
    xid_error_recommend_action_connector_mapping["WORKFLOW_XID_13_31"] = platformconnector_pb2.UNKNOWN
    xid_error_recommend_action_connector_mapping["IGNORE "] = platformconnector_pb2.NONE
    xid_error_recommend_action_connector_mapping["WORKFLOW_ECC_DBE_SRAM"] = platformconnector_pb2.UNKNOWN
    xid_error_recommend_action_connector_mapping["REPORT_ISSUE"] = platformconnector_pb2.REPORT_ISSUE
    xid_error_recommend_action_connector_mapping["RUN_FIELDDIAG"] = platformconnector_pb2.RUN_FIELDDIAG
    xid_error_recommend_action_connector_mapping["WORKFLOW_NVLINK_ERR"] = platformconnector_pb2.UNKNOWN
    xid_error_recommend_action_connector_mapping["RESTART_APP"] = platformconnector_pb2.APPLICATION_RESTART
    xid_error_recommend_action_connector_mapping["RESET_GPU"] = platformconnector_pb2.COMPONENT_RESET
    xid_error_recommend_action_connector_mapping["RESOLUTION_BUCKET_TBD"] = platformconnector_pb2.UNKNOWN
    xid_error_recommend_action_connector_mapping["WORKFLOW_NVLINK5_ERR"] = platformconnector_pb2.UNKNOWN

    return xid_error_recommend_action_connector_mapping


class PlatformConnectorServicer(platformconnector_pb2_grpc.PlatformConnectorServicer):
    def __init__(self) -> None:
        self.health_events: platformconnector_pb2.HealthEvents = None
        self.health_event: platformconnector_pb2.HealthEvent = None

    def HealthEventOccuredV1(self, request: platformconnector_pb2.HealthEvents, context: Any):
        assert isinstance(request, platformconnector_pb2.HealthEvents) == True
        self.health_events = request.events
        return platformconnector_pb2.HealthEvents()


class TestPlatformConnectors(unittest.TestCase):

    def test_health_event_occured(self):
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555", poll_interval_seconds=10, callbacks=[], dcgm_k8s_service_enabled=False
        )
        exit = Event()
        xid_errors_info_dict: dict[str, platform_connector.XidErrorsMappingDetails] = {}
        xid_errors_info_dict["1"] = platform_connector.XidErrorsMappingDetails(
            name="ROBUST_CHANNEL_FIFO_ERROR_FIFO_METHOD",
            recommended_action="UNEXPECTED_ERR_REPORT_ISSUE",
            fatal="FATAL",
        )
        xid_errors_info_dict["13"] = platform_connector.XidErrorsMappingDetails(
            name="ROBUST_CHANNEL_GR_ERROR_SW_NOTIFY",
            recommended_action="WORKFLOW_XID_13_31",
            fatal="FATAL",
        )
        xid_errors_info_dict["43"] = platform_connector.XidErrorsMappingDetails(
            name="ROBUST_CHANNEL_RESETCHANNEL_VERIF_ERROR",
            recommended_action="IGNORE",
            fatal="NONFATAL",
        )
        xid_errors_info_dict["48"] = platform_connector.XidErrorsMappingDetails(
            name="ROBUST_CHANNEL_GPU_ECC_DBE",
            recommended_action="WORKFLOW_ECC_DBE_SRAM",
            fatal="FATAL",
        )
        xid_errors_info_dict["64"] = platform_connector.XidErrorsMappingDetails(
            name="INFOROM_DRAM_RETIREMENT_FAILURE", recommended_action="RUN_FIELDDIAG", fatal="FATAL"
        )

        xid_errors_info_dict["65"] = platform_connector.XidErrorsMappingDetails(
            name="ROBUST_CHANNEL_NVENC1_ERROR", recommended_action="UNEXPECTED_ERR_REPORT_ISSUE", fatal="FATAL"
        )

        xid_errors_info_dict["66"] = platform_connector.XidErrorsMappingDetails(
            name="ROBUST_CHANNEL_FECS_ERR_REG_ACCESS_VIOLATION",
            recommended_action="UNEXPECTED_ERR_REPORT_ISSUE",
            fatal="FATAL",
        )

        xid_errors_info_dict["74"] = platform_connector.XidErrorsMappingDetails(
            name="NVLINK_ERROR", recommended_action="WORKFLOW_NVLINK_ERR", fatal="FATAL"
        )
        xid_errors_info_dict["110"] = platform_connector.XidErrorsMappingDetails(
            name="SEC_FAULT_ERROR", recommended_action="RESOLUTION_BUCKET_TBD", fatal="FATAL"
        )
        xid_error_recommend_action_mapping = create_recommend_action_mapping_from_xid_error_to_platform_connector()
        xid_errors_sliding_window_size = 3
        xid_errors_batch_processing_interval = 4
        xid_errors_batch_processing_enabled = True
        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            xid_errors_info_dict,
            xid_error_recommend_action_mapping,
            xid_errors_batch_processing_interval,
            xid_errors_batch_processing_enabled,
            DummyNvmlXidParser(),
            "statefile",
        )
        dcgm_health_events = watcher._get_health_status_dict()
        platform_connector_test.health_event_occurred(dcgm_health_events)
        health_events = healthEventProcessor.health_events
        for event in health_events:
            assert event.isHealthy == True
            assert event.checkName != ""
            assert len(dcgm_health_events) == len(health_events)
        platform_connector_test.xid_event_occurred("0", 64)
        health_events = healthEventProcessor.health_events
        health_event = health_events[0]
        assert health_event.checkName == "GpuXidError"
        assert health_event.errorCode[0] == "64"
        assert health_event.nodeName == "node1"
        assert health_event.entitiesImpacted == ["0"]
        assert health_event.recommendedAction == platformconnector_pb2.RecommenedAction.RUN_FIELDDIAG

        platform_connector_test.xid_event_occurred("0", 65)
        health_events = healthEventProcessor.health_events
        health_event = health_events[0]
        assert health_event.checkName == "GpuXidError"
        assert health_event.errorCode[0] == "65"
        assert health_event.nodeName == "node1"
        assert health_event.entitiesImpacted == ["0"]
        assert health_event.recommendedAction == platformconnector_pb2.RecommenedAction.REPORT_ISSUE

        platform_connector_test.clear_all_xid_errors()
        health_events = healthEventProcessor.health_events
        health_event = health_events[0]
        assert health_event.checkName == "GpuXidError"
        assert health_event.errorCode == []
        assert health_event.entitiesImpacted == []
        assert health_event.recommendedAction == platformconnector_pb2.RecommenedAction.NONE
        server.stop(0)
