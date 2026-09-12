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

"""Tests for PlatformConnectorEventProcessor (CheckResult -> gRPC HealthEvent)."""

from unittest.mock import MagicMock, patch

import grpc
import pytest

from system_services_monitor.checkers.types import CheckResult
from system_services_monitor.platform_connector.event_processor import PlatformConnectorEventProcessor
from system_services_monitor.protos import health_event_pb2 as pb

NODE = "test-node"


@pytest.fixture
def processor() -> PlatformConnectorEventProcessor:
    return PlatformConnectorEventProcessor(
        socket_path="/tmp/does-not-exist.sock",
        node_name=NODE,
        processing_strategy=pb.ProcessingStrategy.Value("EXECUTE_REMEDIATION"),
    )


def _result(
    check_name: str = "FabricManagerServiceDown", is_healthy: bool = False, is_fatal: bool = True
) -> CheckResult:
    return CheckResult(
        check_name=check_name,
        is_healthy=is_healthy,
        is_fatal=is_fatal,
        error_codes=["FABRIC_MANAGER_NOT_RUNNING"] if not is_healthy else [],
        message="fm down" if not is_healthy else "fm ok",
        entities_impacted=[{"entityType": "NODE", "entityValue": NODE}],
        metadata={"sub_state": "dead"},
    )


class TestRecommendedAction:
    def test_healthy_maps_to_none(self, processor: PlatformConnectorEventProcessor) -> None:
        assert processor._get_recommended_action(_result(is_healthy=True, is_fatal=False)) == pb.NONE

    def test_fatal_maps_to_restart_bm(self, processor: PlatformConnectorEventProcessor) -> None:
        assert processor._get_recommended_action(_result(is_healthy=False, is_fatal=True)) == pb.RESTART_BM

    def test_nonfatal_maps_to_contact_support(self, processor: PlatformConnectorEventProcessor) -> None:
        assert processor._get_recommended_action(_result(is_healthy=False, is_fatal=False)) == pb.CONTACT_SUPPORT


class TestHealthCheckCompleted:
    def test_new_state_sends_event_and_updates_cache(self, processor: PlatformConnectorEventProcessor) -> None:
        """Happy path: a first-seen state change sends one event and caches it."""
        with patch.object(processor, "send_health_event_with_retries", return_value=True) as send:
            processor.health_check_completed([_result()])

        send.assert_called_once()
        (events_arg,), _ = send.call_args
        assert len(events_arg) == 1
        # The reservation is committed in the cache after a successful send.
        assert len(processor.entity_cache) == 1

    def test_unchanged_state_is_not_resent(self, processor: PlatformConnectorEventProcessor) -> None:
        """A repeated identical result is suppressed (only the first send fires)."""
        with patch.object(processor, "send_health_event_with_retries", return_value=True) as send:
            processor.health_check_completed([_result()])
            processor.health_check_completed([_result()])

        send.assert_called_once()

    def test_error_code_change_triggers_new_event(self, processor: PlatformConnectorEventProcessor) -> None:
        """A code-only transition (same fatal/healthy flags) still emits a new event."""
        base = _result()
        escalated = _result()
        escalated.error_codes = ["FABRIC_MANAGER_NOT_RUNNING", "FABRIC_MANAGER_FLAPPING"]

        with patch.object(processor, "send_health_event_with_retries", return_value=True) as send:
            processor.health_check_completed([base])
            processor.health_check_completed([escalated])

        assert send.call_count == 2

    def test_failed_rollback_preserves_newer_reservation(self, processor: PlatformConnectorEventProcessor) -> None:
        """An older failed send must not pop a newer state that landed mid-flight."""
        newer = _result(is_healthy=True, is_fatal=False)
        calls = {"n": 0}

        def send_side_effect(events):
            calls["n"] += 1
            if calls["n"] == 1:
                # While the older send is "in flight" (lock not held), a newer
                # callback for the same key completes successfully...
                processor.health_check_completed([newer])
                # ...then the older send fails and triggers rollback.
                return False
            return True

        with patch.object(processor, "send_health_event_with_retries", side_effect=send_side_effect):
            processor.health_check_completed([_result()])

        # The newer, successfully sent state survives the older rollback.
        assert len(processor.entity_cache) == 1
        assert next(iter(processor.entity_cache.values())).is_healthy is True

    def test_failed_send_rolls_back_reservation(self, processor: PlatformConnectorEventProcessor) -> None:
        """Failure path: when the send fails the cache reservation is rolled back."""
        with patch.object(processor, "send_health_event_with_retries", return_value=False):
            processor.health_check_completed([_result()])

        # Rollback means the next cycle re-attempts, so nothing stays cached.
        assert processor.entity_cache == {}

    def test_send_exception_rolls_back_reservation(self, processor: PlatformConnectorEventProcessor) -> None:
        """An exception during send is caught and the reservation is rolled back."""
        with patch.object(processor, "send_health_event_with_retries", side_effect=RuntimeError("kaboom")):
            processor.health_check_completed([_result()])

        assert processor.entity_cache == {}


class TestSendHealthEventWithRetries:
    def test_successful_send_returns_true(self, processor: PlatformConnectorEventProcessor) -> None:
        """Happy path: a stub that accepts the RPC returns True after one attempt."""
        mock_stub = MagicMock()
        with patch.object(processor, "_is_platform_connector_socket_present", return_value=True), patch(
            "grpc.insecure_channel"
        ), patch(
            "system_services_monitor.platform_connector.event_processor."
            "platformconnector_pb2_grpc.PlatformConnectorStub",
            return_value=mock_stub,
        ):
            ok = processor.send_health_event_with_retries([pb.HealthEvent()])

        assert ok is True
        mock_stub.HealthEventOccurredV1.assert_called_once()

    def test_rpc_error_exhausts_retries_and_returns_false(self, processor: PlatformConnectorEventProcessor) -> None:
        """Failure path: persistent RpcError exhausts retries and returns False."""
        mock_stub = MagicMock()
        mock_stub.HealthEventOccurredV1.side_effect = grpc.RpcError("unavailable")
        with patch.object(processor, "_is_platform_connector_socket_present", return_value=True), patch(
            "grpc.insecure_channel"
        ), patch(
            "system_services_monitor.platform_connector.event_processor."
            "platformconnector_pb2_grpc.PlatformConnectorStub",
            return_value=mock_stub,
        ), patch(
            "system_services_monitor.platform_connector.event_processor.sleep"
        ):
            ok = processor.send_health_event_with_retries([pb.HealthEvent()])

        assert ok is False
        assert mock_stub.HealthEventOccurredV1.call_count == 5  # MAX_RETRIES

    def test_missing_socket_skips_send(self, processor: PlatformConnectorEventProcessor) -> None:
        """When the platform-connector socket is absent the send short-circuits to False."""
        mock_stub = MagicMock()
        with patch.object(processor, "_is_platform_connector_socket_present", return_value=False), patch(
            "system_services_monitor.platform_connector.event_processor."
            "platformconnector_pb2_grpc.PlatformConnectorStub",
            return_value=mock_stub,
        ):
            ok = processor.send_health_event_with_retries([pb.HealthEvent()])

        assert ok is False
        mock_stub.HealthEventOccurredV1.assert_not_called()
