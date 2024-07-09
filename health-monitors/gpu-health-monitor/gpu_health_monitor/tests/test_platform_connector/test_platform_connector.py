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

socket_path = "/tmp/nvsentinel.sock"


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
        watcher = dcgm.DCGMWatcher(addr="localhost:5555", poll_interval_seconds=10, callbacks=[])
        exit = Event()
        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(socket_path, exit)
        dcgm_health_events = watcher._get_health_status_dict()
        platform_connector_test.health_event_occurred(dcgm_health_events)
        health_events = healthEventProcessor.health_events
        for event in health_events:
            assert event.isHealthy == True
            assert event.checkName != ""
            assert len(dcgm_health_events) == len(health_events)
        platform_connector_test.xid_event_occurred("0", "79")
        health_events = healthEventProcessor.health_events
        health_event = health_events[0]
        assert health_event.checkName == "XidError"
        assert health_event.errorCode == "79"
        assert health_event.entitiesImpacted == ["0"]
        platform_connector_test.clear_all_xid_errors()
        health_events = healthEventProcessor.health_events
        health_event = health_events[0]
        assert health_event.checkName == "XidError"
        assert health_event.errorCode == ""
        assert health_event.entitiesImpacted == []
        server.stop(0)
