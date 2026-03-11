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
from threading import Event
import grpc
import time
import unittest
import json
import os
import tempfile
from typing import Any
from concurrent import futures
from gpu_health_monitor.dcgm_watcher import types as dcgmtypes
from gpu_health_monitor.platform_connector import platform_connector

from gpu_health_monitor.protos import (
    health_event_pb2 as platformconnector_pb2,
    health_event_pb2_grpc as platformconnector_pb2_grpc,
)
from google.protobuf.timestamp_pb2 import Timestamp

socket_path = "/tmp/nvsentinel.sock"
node_name = "node1"


def sample_metadata():
    """Sample GPU metadata for testing."""
    return {
        "version": "1.0",
        "timestamp": "2025-11-07T10:00:00Z",
        "node_name": "test-node",
        "chassis_serial": "CHASSIS-12345",
        "gpus": [
            {
                "gpu_id": 0,
                "uuid": "GPU-00000000-0000-0000-0000-000000000000",
                "pci_address": "0000:17:00.0",
                "serial_number": "SN-GPU-0",
                "device_name": "NVIDIA A100",
                "nvlinks": [],
            }
        ],
        "nvswitches": [],
    }


def metadata_file():
    """Create a temporary metadata file for testing."""
    f = tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json")
    json.dump(sample_metadata(), f)
    return f.name


class PlatformConnectorServicer(platformconnector_pb2_grpc.PlatformConnectorServicer):
    def __init__(self) -> None:
        self.health_events: platformconnector_pb2.HealthEvents = None
        self.health_event: platformconnector_pb2.HealthEvent = None

    def HealthEventOccurredV1(self, request: platformconnector_pb2.HealthEvents, context: Any):
        assert isinstance(request, platformconnector_pb2.HealthEvents) == True
        self.health_events = request.events
        return platformconnector_pb2.HealthEvents()


class TestPlatformConnectors(unittest.TestCase):

    def test_health_event_occurred(self):
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()
        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )
        gpu_serials = {
            0: "1650924060039",
            1: "1650924060040",
            2: "1650924060041",
            3: "1650924060042",
            4: "1650924060043",
            5: "1650924060044",
            6: "1650924060045",
            7: "1650924060046",
        }
        exit = Event()

        dcgm_errors_info_dict = {}
        dcgm_errors_info_dict["DCGM_FR_UNKNOWN"] = "CONTACT_SUPPORT"
        dcgm_errors_info_dict["DCGM_FR_UNRECOGNIZED"] = "CONTACT_SUPPORT"
        dcgm_errors_info_dict["DCGM_FR_PCI_REPLAY_RATE"] = "CONTACT_SUPPORT"
        dcgm_errors_info_dict["DCGM_FR_VOLATILE_DBE_DETECTED"] = "COMPONENT_RESET"
        dcgm_errors_info_dict["DCGM_FR_VOLATILE_SBE_DETECTED"] = "NONE"
        dcgm_errors_info_dict["DCGM_FR_PENDING_PAGE_RETIREMENTS"] = "NONE"
        dcgm_errors_info_dict["DCGM_FR_RETIRED_PAGES_LIMIT"] = "CONTECT_SUPPORT"
        dcgm_errors_info_dict["DCGM_FR_CORRUPT_INFOROM"] = "COMPONENT_RESET"
        dcgm_errors_info_dict["DCGM_HEALTH_WATCH_INFOROM"] = "NONE"

        temp_file_path = metadata_file()

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            temp_file_path,
            platformconnector_pb2.STORE_ONLY,
        )
        dcgm_health_events = watcher._get_health_status_dict()
        dcgm_health_events["DCGM_HEALTH_WATCH_INFOROM"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.FAIL,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="DCGM_FR_CORRUPT_INFOROM",
                    message="A corrupt InfoROM has been detected in GPU 0. Flash the InfoROM to clear this corruption.",
                )
            },
        )

        gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
        health_events = healthEventProcessor.health_events
        for event in health_events:
            if event.checkName == "GpuInforomWatch" and event.isHealthy == False:
                assert event.errorCode[0] == "DCGM_FR_CORRUPT_INFOROM"
                assert event.entitiesImpacted[0].entityValue == "0"
                assert event.recommendedAction == platformconnector_pb2.RecommendedAction.COMPONENT_RESET
            else:
                assert event.isHealthy == True
                assert event.checkName != ""
        assert len(dcgm_health_events) * len(gpu_ids) == len(health_events)

        # check if cache is not updated with change no in event
        dcgm_health_events["DCGM_HEALTH_WATCH_INFOROM"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.FAIL,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="DCGM_FR_CORRUPT_INFOROM",
                    message="A corrupt InfoROM has been detected in GPU 0. Flash the InfoROM to clear this corruption.",
                )
            },
        )

        check_name = platform_connector_test._convert_dcgm_watch_name_to_check_name("DCGM_HEALTH_WATCH_INFOROM")
        dcgm_health_event_key = platform_connector_test._build_cache_key(check_name, "GPU", "0")
        before_insertion_cache_value = platform_connector_test.entity_cache[dcgm_health_event_key]
        cache_length = len(platform_connector_test.entity_cache)
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
        health_events = healthEventProcessor.health_events
        assert len(platform_connector_test.entity_cache) == cache_length
        assert (
            platform_connector_test.entity_cache[dcgm_health_event_key].active_errors
            == before_insertion_cache_value.active_errors
        )

        # check if cache is updated with change in event
        dcgm_health_events["DCGM_HEALTH_WATCH_INFOROM"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.PASS, entity_failures={}
        )

        check_name = platform_connector_test._convert_dcgm_watch_name_to_check_name("DCGM_HEALTH_WATCH_INFOROM")
        cache_length = len(platform_connector_test.entity_cache)
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)

        dcgm_health_event_key = platform_connector_test._build_cache_key(check_name, "GPU", "0")
        assert dcgm_health_event_key in platform_connector_test.entity_cache
        assert platform_connector_test.entity_cache[dcgm_health_event_key].is_healthy

        server.stop(0)
        os.unlink(temp_file_path)

    def test_health_event_multiple_failures_same_gpu(self):
        """Test that multiple NvLink failures for the same GPU are properly published to gRPC."""
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        gpu_serials = {
            0: "1650924060039",
            1: "1650924060040",
            2: "1650924060041",
            3: "1650924060042",
            4: "1650924060043",
            5: "1650924060044",
            6: "1650924060045",
            7: "1650924060046",
        }

        exit = Event()
        dcgm_errors_info_dict = {}
        dcgm_errors_info_dict["DCGM_FR_NVLINK_DOWN"] = "COMPONENT_RESET"

        temp_file_path = metadata_file()

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            temp_file_path,
            platformconnector_pb2.STORE_ONLY,
        )

        # Simulate multiple NvLink failures for GPU 0 (4 links down: 8, 9, 14, 15)
        # This mimics the aggregated message from dcgm.py after the fix
        dcgm_health_events = watcher._get_health_status_dict()
        aggregated_message = (
            "GPU 0's NvLink link 8 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 9 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 14 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        )

        dcgm_health_events["DCGM_HEALTH_WATCH_NVLINK"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.FAIL,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=aggregated_message,
                )
            },
        )

        gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)

        health_events = healthEventProcessor.health_events

        # Find the NvLink failure event for GPU 0
        nvlink_failure_event = None
        for event in health_events:
            if (
                event.checkName == "GpuNvlinkWatch"
                and not event.isHealthy
                and event.entitiesImpacted[0].entityValue == "0"
            ):
                nvlink_failure_event = event
                break

        # Verify the event was published
        assert nvlink_failure_event is not None, "NvLink failure event for GPU 0 not found"
        assert nvlink_failure_event.errorCode[0] == "DCGM_FR_NVLINK_DOWN"
        assert nvlink_failure_event.isFatal == True
        assert nvlink_failure_event.isHealthy == False
        assert nvlink_failure_event.entitiesImpacted[0].entityValue == "0"
        assert nvlink_failure_event.recommendedAction == platformconnector_pb2.RecommendedAction.COMPONENT_RESET

        # Verify all 4 NvLink failures are in the message
        assert "link 8" in nvlink_failure_event.message, "Link 8 failure missing from message"
        assert "link 9" in nvlink_failure_event.message, "Link 9 failure missing from message"
        assert "link 14" in nvlink_failure_event.message, "Link 14 failure missing from message"
        assert "link 15" in nvlink_failure_event.message, "Link 15 failure missing from message"

        # Verify messages are properly separated
        assert nvlink_failure_event.message.count(";") == 3, "Expected 3 semicolons separating 4 messages"

        # Verify the complete aggregated message is preserved
        assert nvlink_failure_event.message == aggregated_message

        assert nvlink_failure_event.processingStrategy == platformconnector_pb2.STORE_ONLY

        server.stop(0)
        os.unlink(temp_file_path)

    def test_health_event_multiple_gpus_multiple_failures_each(self):
        """Test full lifecycle of NvLink failures across multiple GPUs:
        Phase 1: Both GPU 0 and GPU 1 fail -> both events published, cache tracks each independently
        Phase 2: Same errors persist -> no re-publish (idempotent)
        Phase 3: GPU 0 recovers, GPU 1 stays unhealthy -> healthy event only for GPU 0
        Phase 4: GPU 1 recovers -> healthy event only for GPU 1
        Phase 5: All healthy, steady state -> no events sent
        """
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        gpu_serials = {
            0: "1650924060039",
            1: "1650924060040",
            2: "1650924060041",
            3: "1650924060042",
            4: "1650924060043",
            5: "1650924060044",
            6: "1650924060045",
            7: "1650924060046",
        }

        exit = Event()
        dcgm_errors_info_dict = {}
        dcgm_errors_info_dict["DCGM_FR_NVLINK_DOWN"] = "COMPONENT_RESET"

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            "/tmp/test_metadata.json",
            platformconnector_pb2.STORE_ONLY,
        )

        # Simulate multiple NvLink failures for GPU 0 and GPU 1
        dcgm_health_events = watcher._get_health_status_dict()

        gpu0_message = (
            "GPU 0's NvLink link 8 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 9 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 14 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        )

        gpu1_message = (
            "GPU 1's NvLink link 8 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 1's NvLink link 9 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 1's NvLink link 12 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 1's NvLink link 13 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        )

        dcgm_health_events["DCGM_HEALTH_WATCH_NVLINK"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.FAIL,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=gpu0_message,
                ),
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=gpu1_message,
                ),
            },
        )

        gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)

        health_events = healthEventProcessor.health_events

        # Find NvLink failure events for both GPUs
        gpu0_event = None
        gpu1_event = None
        for event in health_events:
            if event.checkName == "GpuNvlinkWatch" and not event.isHealthy:
                if event.entitiesImpacted[0].entityValue == "0":
                    gpu0_event = event
                elif event.entitiesImpacted[0].entityValue == "1":
                    gpu1_event = event

        # Verify GPU 0 event
        assert gpu0_event is not None, "NvLink failure event for GPU 0 not found"
        assert gpu0_event.errorCode[0] == "DCGM_FR_NVLINK_DOWN"
        assert gpu0_event.isFatal == True
        assert gpu0_event.isHealthy == False

        # Verify all 4 NvLink failures for GPU 0
        assert "link 8" in gpu0_event.message
        assert "link 9" in gpu0_event.message
        assert "link 14" in gpu0_event.message
        assert "link 15" in gpu0_event.message
        assert gpu0_event.message.count(";") == 3
        assert gpu0_event.message == gpu0_message
        assert gpu0_event.processingStrategy == platformconnector_pb2.STORE_ONLY

        # Verify GPU 1 event
        assert gpu1_event is not None, "NvLink failure event for GPU 1 not found"
        assert gpu1_event.errorCode[0] == "DCGM_FR_NVLINK_DOWN"
        assert gpu1_event.isFatal == True
        assert gpu1_event.isHealthy == False

        # Verify all 4 NvLink failures for GPU 1
        assert "link 8" in gpu1_event.message
        assert "link 9" in gpu1_event.message
        assert "link 12" in gpu1_event.message
        assert "link 13" in gpu1_event.message
        assert gpu1_event.message.count(";") == 3
        assert gpu1_event.message == gpu1_message
        assert gpu1_event.processingStrategy == platformconnector_pb2.STORE_ONLY

        gpu0_cache_key = platform_connector_test._build_cache_key("GpuNvlinkWatch", "GPU", "0")
        gpu1_cache_key = platform_connector_test._build_cache_key("GpuNvlinkWatch", "GPU", "1")
        assert "DCGM_FR_NVLINK_DOWN" in platform_connector_test.entity_cache[gpu0_cache_key].active_errors
        assert not platform_connector_test.entity_cache[gpu0_cache_key].is_healthy
        assert "DCGM_FR_NVLINK_DOWN" in platform_connector_test.entity_cache[gpu1_cache_key].active_errors
        assert not platform_connector_test.entity_cache[gpu1_cache_key].is_healthy

        # --- Phase 2: Same errors persist -> no re-publish ---
        healthEventProcessor.health_events = None
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
        assert healthEventProcessor.health_events is None, "Duplicate errors should not be re-published"

        # --- Phase 3: GPU 0 recovers, GPU 1 stays unhealthy ---
        dcgm_health_events["DCGM_HEALTH_WATCH_NVLINK"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.FAIL,
            entity_failures={
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=gpu1_message,
                ),
            },
        )

        healthEventProcessor.health_events = None
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
        recovery_events = healthEventProcessor.health_events

        gpu0_recovery = None
        gpu1_unexpected_recovery = None
        gpu1_unexpected_failure = None
        for event in recovery_events:
            if event.checkName == "GpuNvlinkWatch":
                if event.entitiesImpacted[0].entityValue == "0" and event.isHealthy:
                    gpu0_recovery = event
                elif event.entitiesImpacted[0].entityValue == "1" and event.isHealthy:
                    gpu1_unexpected_recovery = event
                elif event.entitiesImpacted[0].entityValue == "1" and not event.isHealthy:
                    gpu1_unexpected_failure = event

        assert gpu0_recovery is not None, "Healthy event should be sent for GPU 0 recovery"
        assert gpu0_recovery.isHealthy == True
        assert gpu0_recovery.isFatal == False
        assert gpu1_unexpected_recovery is None, "GPU 1 should NOT get a healthy event (still failing)"
        assert gpu1_unexpected_failure is None, "GPU 1 should NOT get a duplicate failure event"

        assert platform_connector_test.entity_cache[gpu0_cache_key].is_healthy
        assert not platform_connector_test.entity_cache[gpu1_cache_key].is_healthy
        assert "DCGM_FR_NVLINK_DOWN" in platform_connector_test.entity_cache[gpu1_cache_key].active_errors

        # --- Phase 4: GPU 1 also recovers ---
        dcgm_health_events["DCGM_HEALTH_WATCH_NVLINK"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.PASS,
            entity_failures={},
        )

        healthEventProcessor.health_events = None
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
        final_recovery_events = healthEventProcessor.health_events

        gpu1_recovery = None
        gpu0_dup_recovery = None
        for event in final_recovery_events:
            if event.checkName == "GpuNvlinkWatch" and event.isHealthy:
                if event.entitiesImpacted[0].entityValue == "1":
                    gpu1_recovery = event
                elif event.entitiesImpacted[0].entityValue == "0":
                    gpu0_dup_recovery = event

        assert gpu1_recovery is not None, "Healthy event should be sent for GPU 1 recovery"
        assert gpu1_recovery.isHealthy == True
        assert gpu0_dup_recovery is None, "GPU 0 should NOT get a duplicate healthy event (already recovered)"

        assert platform_connector_test.entity_cache[gpu0_cache_key].is_healthy
        assert platform_connector_test.entity_cache[gpu1_cache_key].is_healthy

        # --- Phase 5: Steady state -> no events ---
        healthEventProcessor.health_events = None
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
        assert healthEventProcessor.health_events is None, "No events should be sent when all cached as healthy"

        server.stop(0)

    def test_health_event_multiple_recommended_action_override(self):
        """Test that only events with PCI and GPU_UUID are published with the COMPONENT_RESET action"""
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        gpu_serials = {
            0: "1650924060039",
            1: "1650924060040",
            2: "1650924060041",
            3: "1650924060042",
            4: "1650924060043",
            5: "1650924060044",
            6: "1650924060045",
            7: "1650924060046",
        }

        exit = Event()
        dcgm_errors_info_dict = {}
        dcgm_errors_info_dict["DCGM_FR_NVLINK_DOWN"] = "COMPONENT_RESET"

        temp_file_path = metadata_file()

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            temp_file_path,
            platformconnector_pb2.STORE_ONLY,
        )

        # Simulate multiple NvLink failures for GPU 0 and GPU 1
        dcgm_health_events = watcher._get_health_status_dict()

        gpu0_message = (
            "GPU 0's NvLink link 8 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 9 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 14 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 0's NvLink link 15 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        )

        gpu1_message = (
            "GPU 1's NvLink link 8 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 1's NvLink link 9 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 1's NvLink link 12 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics.; "
            "GPU 1's NvLink link 13 is currently down Check DCGM and system logs for errors. Reset GPU. Restart DCGM. Rerun diagnostics."
        )

        dcgm_health_events["DCGM_HEALTH_WATCH_NVLINK"] = dcgmtypes.HealthDetails(
            status=dcgmtypes.HealthStatus.FAIL,
            entity_failures={
                0: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=gpu0_message,
                ),
                1: dcgm.types.ErrorDetails(
                    code="DCGM_FR_NVLINK_DOWN",
                    message=gpu1_message,
                ),
            },
        )

        gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
        platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)

        health_events = healthEventProcessor.health_events

        # Find NvLink failure events for both GPUs
        gpu0_event = None
        gpu1_event = None
        for event in health_events:
            if event.checkName == "GpuNvlinkWatch" and not event.isHealthy:
                if event.entitiesImpacted[0].entityValue == "0":
                    gpu0_event = event
                elif event.entitiesImpacted[0].entityValue == "1":
                    gpu1_event = event

        # Verify GPU 0 event
        assert gpu0_event is not None, "NvLink failure event for GPU 0 not found"
        assert gpu0_event.errorCode[0] == "DCGM_FR_NVLINK_DOWN"
        assert gpu0_event.isFatal == True
        assert gpu0_event.isHealthy == False

        # Verify all 4 NvLink failures for GPU 0
        assert "link 8" in gpu0_event.message
        assert "link 9" in gpu0_event.message
        assert "link 14" in gpu0_event.message
        assert "link 15" in gpu0_event.message
        assert gpu0_event.message.count(";") == 3
        assert gpu0_event.message == gpu0_message
        assert gpu0_event.processingStrategy == platformconnector_pb2.STORE_ONLY
        # Metadata collector includes PCI and GPU_UUID only for GPU 0.
        # Since the PCI and GPU_UUID are present in entitiesImpacted, the
        # recommendedAction will not be overridden from COMPONENT_RESET.
        assert gpu0_event.recommendedAction == platformconnector_pb2.COMPONENT_RESET
        assert len(gpu0_event.entitiesImpacted) == 3

        # Verify GPU 1 event
        assert gpu1_event is not None, "NvLink failure event for GPU 1 not found"
        assert gpu1_event.errorCode[0] == "DCGM_FR_NVLINK_DOWN"
        assert gpu1_event.isFatal == True
        assert gpu1_event.isHealthy == False
        # Since the PCI and GPU_UUID are not present in entitiesImpacted for
        # GPU 1, the recommendedAction will be overridden from COMPONENT_RESET
        # to RESTART_VM.
        assert gpu1_event.recommendedAction == platformconnector_pb2.RESTART_VM
        assert len(gpu1_event.entitiesImpacted) == 1

        # Verify all 4 NvLink failures for GPU 1
        assert "link 8" in gpu1_event.message
        assert "link 9" in gpu1_event.message
        assert "link 12" in gpu1_event.message
        assert "link 13" in gpu1_event.message
        assert gpu1_event.message.count(";") == 3
        assert gpu1_event.message == gpu1_message
        assert gpu1_event.processingStrategy == platformconnector_pb2.STORE_ONLY

        server.stop(0)
        os.unlink(temp_file_path)

    def test_dcgm_connectivity_failed(self):
        """Test that GpuDcgmConnectivityFailure health event is sent when DCGM connectivity fails."""
        import tempfile
        import os

        # Create a temporary state file with test boot ID
        with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix="_test_state") as f:
            f.write("test_boot_id")
            state_file_path = f.name

        try:
            healthEventProcessor = PlatformConnectorServicer()
            server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
            platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
            server.add_insecure_port(f"unix://{socket_path}")
            server.start()

            exit = Event()

            dcgm_errors_info_dict = {}

            platform_connector_processor = platform_connector.PlatformConnectorEventProcessor(
                socket_path=socket_path,
                node_name=node_name,
                exit=exit,
                dcgm_errors_info_dict=dcgm_errors_info_dict,
                state_file_path=state_file_path,
                metadata_path="/tmp/test_metadata.json",
                processing_strategy=platformconnector_pb2.STORE_ONLY,
            )

            # Trigger connectivity failure
            platform_connector_processor.dcgm_connectivity_failed()
            time.sleep(1)  # Allow time for event to be sent

            # Verify the health events
            health_events = healthEventProcessor.health_events
            assert len(health_events) == 1

            for i, event in enumerate(health_events):
                assert event.checkName == "GpuDcgmConnectivityFailure"
                assert event.isFatal == True
                assert event.isHealthy == False
                assert event.errorCode == ["DCGM_CONNECTIVITY_ERROR"]
                assert event.message == "Failed to connect to DCGM for health check"
                assert event.recommendedAction == platformconnector_pb2.CONTACT_SUPPORT
                assert event.nodeName == node_name
                assert event.entitiesImpacted == []
                assert event.processingStrategy == platformconnector_pb2.STORE_ONLY

            server.stop(0)
        finally:
            # Clean up the temporary state file
            if os.path.exists(state_file_path):
                os.unlink(state_file_path)

    def test_dcgm_connectivity_restored(self):
        """Test that connectivity restored event is sent when DCGM connectivity is restored."""
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()
        exit = Event()

        dcgm_errors_info_dict = {}

        platform_connector_processor = platform_connector.PlatformConnectorEventProcessor(
            socket_path=socket_path,
            node_name=node_name,
            exit=exit,
            dcgm_errors_info_dict=dcgm_errors_info_dict,
            state_file_path="statefile",
            metadata_path="/tmp/test_metadata.json",
            processing_strategy=platformconnector_pb2.EXECUTE_REMEDIATION,
        )

        timestamp = Timestamp()
        timestamp.GetCurrentTime()
        platform_connector_processor.clear_dcgm_connectivity_failure(timestamp)
        time.sleep(1)

        # The events should include connectivity restored
        health_events = healthEventProcessor.health_events

        # Find the connectivity restored event
        restored_event = None
        for event in health_events:
            if event.checkName == "GpuDcgmConnectivityFailure" and event.isHealthy == True:
                restored_event = event
                break

        assert (
            restored_event is not None
        ), f"No restored event found. Events: {[f'{e.checkName}:{e.isHealthy}' for e in health_events]}"
        assert restored_event.isFatal == False
        assert restored_event.isHealthy == True
        assert restored_event.errorCode == []
        assert restored_event.message == "DCGM connectivity reported no errors"
        assert restored_event.recommendedAction == platformconnector_pb2.NONE
        assert restored_event.processingStrategy == platformconnector_pb2.EXECUTE_REMEDIATION

        server.stop(0)

    def test_nonfatal_event_lifecycle_downgrade_and_native(self):
        """Test the full lifecycle of non-fatal events from two sources:
        1. MEM watch with DCGM_FR_XID_ERROR (recommendedAction=NONE → non-fatal)
        2. THERMAL watch with DCGM_FR_TEMP_VIOLATION (recommendedAction=NONE → non-fatal)
        Both should be published on state change and suppressed when unchanged.
        On recovery, healthy events are sent for all watches to clear state in
        the platform connector.
        """
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        exit = Event()
        dcgm_errors_info_dict = {
            "DCGM_FR_XID_ERROR": "NONE",
            "DCGM_FR_TEMP_VIOLATION": "NONE",
        }

        temp_file_path = metadata_file()

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            temp_file_path,
            platformconnector_pb2.STORE_ONLY,
        )

        try:
            # --- Phase 1: Both watches go unhealthy ---
            # MEM fails with DCGM_FR_XID_ERROR (recommendedAction=NONE → non-fatal)
            # THERMAL fails with DCGM_FR_TEMP_VIOLATION (recommendedAction=NONE → non-fatal)
            dcgm_health_events = watcher._get_health_status_dict()
            dcgm_health_events["DCGM_HEALTH_WATCH_MEM"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.FAIL,
                entity_failures={
                    4: dcgm.types.ErrorDetails(
                        code="DCGM_FR_XID_ERROR",
                        message="ErrorCode:DCGM_FR_XID_ERROR GPU:4 PCI:0000:c4:00.0 "
                        "Detected XID 31 for GPU 4 .Recommended Action=NONE;",
                    )
                },
            )
            dcgm_health_events["DCGM_HEALTH_WATCH_THERMAL"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.FAIL,
                entity_failures={
                    2: dcgm.types.ErrorDetails(
                        code="DCGM_FR_TEMP_VIOLATION",
                        message="GPU 2 temperature exceeds threshold",
                    )
                },
            )

            gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            health_events = healthEventProcessor.health_events

            # Verify non-fatal (MEM watch, GPU 4, recommendedAction=NONE)
            mem_failure = None
            for event in health_events:
                if (
                    event.checkName == "GpuMemWatch"
                    and not event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "4"
                ):
                    mem_failure = event
                    break

            assert mem_failure is not None, "GpuMemWatch failure event for GPU 4 not found"
            assert not mem_failure.isFatal, "Event with recommendedAction=NONE should be non-fatal"
            assert mem_failure.errorCode[0] == "DCGM_FR_XID_ERROR"
            assert mem_failure.recommendedAction == platformconnector_pb2.NONE

            mem_cache_key = platform_connector_test._build_cache_key("GpuMemWatch", "GPU", "4")
            assert "DCGM_FR_XID_ERROR" in platform_connector_test.entity_cache[mem_cache_key].active_errors
            assert not platform_connector_test.entity_cache[mem_cache_key].is_healthy

            # Verify non-fatal (THERMAL watch, GPU 2, recommendedAction=NONE)
            thermal_failure = None
            for event in health_events:
                if (
                    event.checkName == "GpuThermalWatch"
                    and not event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "2"
                ):
                    thermal_failure = event
                    break

            assert thermal_failure is not None, "GpuThermalWatch failure event for GPU 2 not found"
            assert not thermal_failure.isFatal, "Event with recommendedAction=NONE should be non-fatal"
            assert thermal_failure.errorCode[0] == "DCGM_FR_TEMP_VIOLATION"
            assert thermal_failure.recommendedAction == platformconnector_pb2.NONE

            thermal_cache_key = platform_connector_test._build_cache_key("GpuThermalWatch", "GPU", "2")
            assert "DCGM_FR_TEMP_VIOLATION" in platform_connector_test.entity_cache[thermal_cache_key].active_errors
            assert not platform_connector_test.entity_cache[thermal_cache_key].is_healthy

            # --- Phase 2: Same errors persist — should NOT be re-published (no state change) ---
            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            assert (
                healthEventProcessor.health_events is None
            ), "Non-fatal unhealthy events should not be re-published when cache state unchanged"

            # --- Phase 3: Both watches recover ---
            # Both MEM and THERMAL have recommendedAction=NONE → non-fatal
            # Healthy events are sent for all recoveries to clear the state in platform connector
            dcgm_health_events["DCGM_HEALTH_WATCH_MEM"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.PASS,
                entity_failures={},
            )
            dcgm_health_events["DCGM_HEALTH_WATCH_THERMAL"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.PASS,
                entity_failures={},
            )
            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            recovery_events = healthEventProcessor.health_events

            mem_recovery = None
            thermal_recovery = None
            for event in recovery_events:
                if (
                    event.checkName == "GpuMemWatch"
                    and event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "4"
                ):
                    mem_recovery = event
                if (
                    event.checkName == "GpuThermalWatch"
                    and event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "2"
                ):
                    thermal_recovery = event

            assert mem_recovery is not None, "Healthy event must be sent for MEM recovery"
            assert thermal_recovery is not None, "Healthy event must be sent for THERMAL recovery"

            assert platform_connector_test.entity_cache[mem_cache_key].is_healthy
            assert platform_connector_test.entity_cache[thermal_cache_key].is_healthy

            # --- Phase 4: Steady state — no events sent (all cached as healthy) ---
            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            assert (
                healthEventProcessor.health_events is None
            ), "No events should be sent when all watches are healthy and cached"
        finally:
            server.stop(0)
            if os.path.exists(temp_file_path):
                os.unlink(temp_file_path)

    def test_fatal_error_lifecycle_when_recommended_action_requires_remediation(self):
        """Test that an error with recommendedAction != NONE is treated as fatal.

        DCGM_FR_POWER_UNREADABLE maps to RESTART_VM (not NONE), so isFatal=True
        and the event creates a node condition. On recovery, a healthy event is
        sent to clear the condition.
        """
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        exit = Event()
        dcgm_errors_info_dict = {
            "DCGM_FR_POWER_UNREADABLE": "RESTART_VM",
        }

        temp_file_path = metadata_file()

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            temp_file_path,
            platformconnector_pb2.STORE_ONLY,
        )

        try:
            dcgm_health_events = watcher._get_health_status_dict()
            dcgm_health_events["DCGM_HEALTH_WATCH_POWER"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.FAIL,
                entity_failures={
                    3: dcgm.types.ErrorDetails(
                        code="DCGM_FR_POWER_UNREADABLE",
                        message="GPU 3 power reading is unavailable",
                    )
                },
            )

            gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            health_events = healthEventProcessor.health_events

            power_failure = None
            for event in health_events:
                if (
                    event.checkName == "GpuPowerWatch"
                    and not event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "3"
                ):
                    power_failure = event
                    break

            assert power_failure is not None, "GpuPowerWatch failure event for GPU 3 not found"
            assert power_failure.isFatal, "Event with recommendedAction=RESTART_VM should be fatal"
            assert power_failure.errorCode[0] == "DCGM_FR_POWER_UNREADABLE"
            assert power_failure.recommendedAction == platformconnector_pb2.RESTART_VM

            cache_key = platform_connector_test._build_cache_key("GpuPowerWatch", "GPU", "3")
            assert "DCGM_FR_POWER_UNREADABLE" in platform_connector_test.entity_cache[cache_key].active_errors
            assert not platform_connector_test.entity_cache[cache_key].is_healthy

            # Recovery: since it was fatal (recommendedAction!=NONE), healthy event IS sent via gRPC
            dcgm_health_events["DCGM_HEALTH_WATCH_POWER"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.PASS,
                entity_failures={},
            )
            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            recovery_events = healthEventProcessor.health_events

            power_recovery = None
            for event in recovery_events:
                if (
                    event.checkName == "GpuPowerWatch"
                    and event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "3"
                ):
                    power_recovery = event
                    break

            assert power_recovery is not None, "Healthy event must be sent for fatal watch to clear node condition"
            assert platform_connector_test.entity_cache[cache_key].is_healthy
        finally:
            server.stop(0)
            if os.path.exists(temp_file_path):
                os.unlink(temp_file_path)

    def test_multiple_error_codes_accumulate_and_single_pass_clears_all(self):
        """Test that different error codes on the same (watch, GPU) accumulate independently
        in active_errors, and a single PASS wipes them all.

        Scenario (GpuPowerWatch, GPU 3):
          Phase 1: DCGM_FR_CLOCK_THROTTLE_POWER (action=NONE -> non-fatal) -> published
          Phase 2: DCGM_FR_POWER_UNREADABLE (action=RESTART_VM -> fatal) -> published (new error code)
                   Cache now has both error codes in active_errors
          Phase 3: DCGM_FR_POWER_UNREADABLE again -> NOT re-published (already cached)
          Phase 4: PASS -> single healthy event clears both errors
          Phase 5: PASS again -> no event (already healthy)
        """
        healthEventProcessor = PlatformConnectorServicer()
        server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
        platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
        server.add_insecure_port(f"unix://{socket_path}")
        server.start()

        watcher = dcgm.DCGMWatcher(
            addr="localhost:5555",
            poll_interval_seconds=10,
            callbacks=[],
            dcgm_k8s_service_enabled=False,
        )

        exit = Event()
        dcgm_errors_info_dict = {
            "DCGM_FR_CLOCK_THROTTLE_POWER": "NONE",
            "DCGM_FR_POWER_UNREADABLE": "RESTART_VM",
        }

        temp_file_path = metadata_file()

        platform_connector_test = platform_connector.PlatformConnectorEventProcessor(
            socket_path,
            node_name,
            exit,
            dcgm_errors_info_dict,
            "statefile",
            temp_file_path,
            platformconnector_pb2.STORE_ONLY,
        )

        cache_key = platform_connector_test._build_cache_key("GpuPowerWatch", "GPU", "3")

        try:
            # --- Phase 1: Non-fatal error ---
            dcgm_health_events = watcher._get_health_status_dict()
            dcgm_health_events["DCGM_HEALTH_WATCH_POWER"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.FAIL,
                entity_failures={
                    3: dcgm.types.ErrorDetails(
                        code="DCGM_FR_CLOCK_THROTTLE_POWER",
                        message="GPU 3 clock throttled due to power",
                    )
                },
            )

            gpu_ids = [0, 1, 2, 3, 4, 5, 6, 7]
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            health_events = healthEventProcessor.health_events

            throttle_event = None
            for event in health_events:
                if (
                    event.checkName == "GpuPowerWatch"
                    and not event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "3"
                ):
                    throttle_event = event
                    break

            assert throttle_event is not None, "Non-fatal power throttle event should be published"
            assert not throttle_event.isFatal, "DCGM_FR_CLOCK_THROTTLE_POWER (action=NONE) should be non-fatal"
            assert throttle_event.errorCode[0] == "DCGM_FR_CLOCK_THROTTLE_POWER"

            assert cache_key in platform_connector_test.entity_cache
            assert platform_connector_test.entity_cache[cache_key].active_errors == {"DCGM_FR_CLOCK_THROTTLE_POWER"}
            assert not platform_connector_test.entity_cache[cache_key].is_healthy

            # --- Phase 2: Different fatal error on same (watch, GPU) ---
            dcgm_health_events["DCGM_HEALTH_WATCH_POWER"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.FAIL,
                entity_failures={
                    3: dcgm.types.ErrorDetails(
                        code="DCGM_FR_POWER_UNREADABLE",
                        message="GPU 3 power reading is unavailable",
                    )
                },
            )

            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            health_events = healthEventProcessor.health_events

            power_event = None
            for event in health_events:
                if (
                    event.checkName == "GpuPowerWatch"
                    and not event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "3"
                ):
                    power_event = event
                    break

            assert power_event is not None, "Fatal power unreadable event should be published (new error code)"
            assert power_event.isFatal, "DCGM_FR_POWER_UNREADABLE (action=RESTART_VM) should be fatal"
            assert power_event.errorCode[0] == "DCGM_FR_POWER_UNREADABLE"

            assert platform_connector_test.entity_cache[cache_key].active_errors == {
                "DCGM_FR_CLOCK_THROTTLE_POWER",
                "DCGM_FR_POWER_UNREADABLE",
            }, "Both error codes should be accumulated in active_errors"

            # --- Phase 3: Same fatal error again -> should NOT re-publish ---
            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)

            if healthEventProcessor.health_events:
                power_duplicate = None
                for event in healthEventProcessor.health_events:
                    if (
                        event.checkName == "GpuPowerWatch"
                        and not event.isHealthy
                        and event.entitiesImpacted[0].entityValue == "3"
                    ):
                        power_duplicate = event
                        break
                assert power_duplicate is None, "Same error code should not be re-published"

            # --- Phase 4: PASS -> single healthy event clears both errors ---
            dcgm_health_events["DCGM_HEALTH_WATCH_POWER"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.PASS,
                entity_failures={},
            )

            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)
            recovery_events = healthEventProcessor.health_events

            power_recovery = None
            for event in recovery_events:
                if (
                    event.checkName == "GpuPowerWatch"
                    and event.isHealthy
                    and event.entitiesImpacted[0].entityValue == "3"
                ):
                    power_recovery = event
                    break

            assert power_recovery is not None, "Healthy event must be sent to clear both errors"
            assert platform_connector_test.entity_cache[cache_key].is_healthy
            assert platform_connector_test.entity_cache[cache_key].active_errors == set()

            # --- Phase 5: PASS again -> no event (already healthy) ---
            healthEventProcessor.health_events = None
            platform_connector_test.health_event_occurred(dcgm_health_events, gpu_ids)

            if healthEventProcessor.health_events:
                power_dup_recovery = None
                for event in healthEventProcessor.health_events:
                    if (
                        event.checkName == "GpuPowerWatch"
                        and event.isHealthy
                        and event.entitiesImpacted[0].entityValue == "3"
                    ):
                        power_dup_recovery = event
                        break
                assert power_dup_recovery is None, "Healthy event should not be re-sent when already healthy"
        finally:
            server.stop(0)
            if os.path.exists(temp_file_path):
                os.unlink(temp_file_path)

    def test_event_retry_and_cache_cleanup_when_platform_connector_down(self) -> None:
        """Test when platform connector goes down and comes back up."""
        import tempfile
        import os

        original_max_retries = platform_connector.MAX_RETRIES
        original_initial_delay = platform_connector.INITIAL_DELAY
        platform_connector.MAX_RETRIES = 3
        platform_connector.INITIAL_DELAY = 1

        with tempfile.NamedTemporaryFile(mode="w", delete=False, suffix="_test_state") as f:
            f.write("test_boot_id")
            state_file_path = f.name

        try:
            watcher = dcgm.DCGMWatcher(
                addr="localhost:5555",
                poll_interval_seconds=10,
                callbacks=[],
                dcgm_k8s_service_enabled=False,
            )
            gpu_serials = {0: "1650924060039"}
            exit = Event()

            dcgm_errors_info_dict = {"DCGM_FR_CORRUPT_INFOROM": "COMPONENT_RESET"}

            platform_connector_processor = platform_connector.PlatformConnectorEventProcessor(
                socket_path=socket_path,
                node_name=node_name,
                exit=exit,
                dcgm_errors_info_dict=dcgm_errors_info_dict,
                state_file_path=state_file_path,
                metadata_path="/tmp/test_metadata.json",
                processing_strategy=platformconnector_pb2.STORE_ONLY,
            )

            # Verify cache is empty initially
            assert len(platform_connector_processor.entity_cache) == 0

            # Platform-connector is DOWN - send will fail, cache should NOT be updated
            platform_connector_processor.dcgm_connectivity_failed()
            dcgm_failure_cache_key = platform_connector_processor._build_cache_key(
                "GpuDcgmConnectivityFailure", "DCGM", "ALL"
            )
            assert (
                dcgm_failure_cache_key not in platform_connector_processor.entity_cache
            ), "Cache should NOT be updated when send fails"

            # Test 2: DCGM Connectivity Restored
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            platform_connector_processor.clear_dcgm_connectivity_failure(timestamp)
            assert (
                dcgm_failure_cache_key not in platform_connector_processor.entity_cache
            ), "Cache should NOT be updated when send fails"

            # GPU Health Event - send will fail, cache should NOT be updated
            dcgm_health_events = watcher._get_health_status_dict()
            dcgm_health_events["DCGM_HEALTH_WATCH_INFOROM"] = dcgmtypes.HealthDetails(
                status=dcgmtypes.HealthStatus.FAIL,
                entity_failures={
                    0: dcgm.types.ErrorDetails(
                        code="DCGM_FR_CORRUPT_INFOROM",
                        message="A corrupt InfoROM has been detected in GPU 0.",
                    )
                },
            )
            gpu_ids = [0]
            platform_connector_processor.health_event_occurred(dcgm_health_events, gpu_ids)
            gpu_health_cache_key = platform_connector_processor._build_cache_key("GpuInforomWatch", "GPU", "0")
            assert (
                gpu_health_cache_key not in platform_connector_processor.entity_cache
            ), "Cache should NOT be updated when send fails"

            # Platform connector comes UP - Start server
            healthEventProcessor = PlatformConnectorServicer()
            server = grpc.server(futures.ThreadPoolExecutor(max_workers=10))
            platformconnector_pb2_grpc.add_PlatformConnectorServicer_to_server(healthEventProcessor, server)
            server.add_insecure_port(f"unix://{socket_path}")
            server.start()

            # DCGM Connectivity Failure - send succeeds, cache should be updated
            platform_connector_processor.dcgm_connectivity_failed()
            time.sleep(1)
            assert (
                dcgm_failure_cache_key in platform_connector_processor.entity_cache
            ), "Cache should be updated after successful send"
            assert not platform_connector_processor.entity_cache[dcgm_failure_cache_key].is_healthy
            assert (
                "DCGM_CONNECTIVITY_ERROR"
                in platform_connector_processor.entity_cache[dcgm_failure_cache_key].active_errors
            )

            # Verify DCGM failure event was sent
            health_events = healthEventProcessor.health_events
            dcgm_failure_event = None
            for event in health_events:
                if event.checkName == "GpuDcgmConnectivityFailure" and not event.isHealthy:
                    dcgm_failure_event = event
                    break
            assert dcgm_failure_event is not None, "DCGM failure event should be sent"
            assert dcgm_failure_event.isFatal == True
            assert dcgm_failure_event.errorCode == ["DCGM_CONNECTIVITY_ERROR"]
            assert dcgm_failure_event.entitiesImpacted == []

            # DCGM Connectivity Restored - send succeeds, cache should be updated
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            platform_connector_processor.clear_dcgm_connectivity_failure(timestamp)
            time.sleep(1)
            assert platform_connector_processor.entity_cache[
                dcgm_failure_cache_key
            ].is_healthy, "Cache should be updated after successful send"

            # Verify DCGM restored event was sent
            health_events = healthEventProcessor.health_events
            dcgm_restored_event = None
            for event in health_events:
                if event.checkName == "GpuDcgmConnectivityFailure" and event.isHealthy:
                    dcgm_restored_event = event
                    break
            assert dcgm_restored_event is not None, "DCGM restored event should be sent"
            assert dcgm_restored_event.isFatal == False
            assert dcgm_restored_event.isHealthy == True

            # GPU Health Event - send succeeds, cache should be updated
            platform_connector_processor.health_event_occurred(dcgm_health_events, gpu_ids)
            time.sleep(1)
            assert (
                gpu_health_cache_key in platform_connector_processor.entity_cache
            ), "Cache should be updated after successful send"
            assert not platform_connector_processor.entity_cache[gpu_health_cache_key].is_healthy
            assert (
                "DCGM_FR_CORRUPT_INFOROM"
                in platform_connector_processor.entity_cache[gpu_health_cache_key].active_errors
            )

            # Verify GPU health event was sent
            health_events = healthEventProcessor.health_events
            gpu_health_event = None
            for event in health_events:
                if event.checkName == "GpuInforomWatch" and not event.isHealthy:
                    gpu_health_event = event
                    break
            assert gpu_health_event is not None, "GPU health event should be sent"
            assert gpu_health_event.errorCode[0] == "DCGM_FR_CORRUPT_INFOROM"
            assert gpu_health_event.entitiesImpacted[0].entityValue == "0"

            server.stop(0)
        finally:
            platform_connector.MAX_RETRIES = original_max_retries
            platform_connector.INITIAL_DELAY = original_initial_delay
            if os.path.exists(state_file_path):
                os.unlink(state_file_path)
