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

"""Platform-connector event processor.

Implements the CallbackInterface to convert CheckResults into protobuf HealthEvents
and send them to the platform-connector via gRPC over a Unix domain socket.
"""

import dataclasses
import logging as log
from time import sleep
from typing import List

import grpc
from google.protobuf.timestamp_pb2 import Timestamp

from system_services_monitor.checkers.types import CallbackInterface, CheckResult
from system_services_monitor.protos import (
    health_event_pb2 as platformconnector_pb2,
    health_event_pb2_grpc as platformconnector_pb2_grpc,
)
from . import metrics

MAX_RETRIES = 5
INITIAL_DELAY = 2
MAX_DELAY = 15


@dataclasses.dataclass
class CachedEntityState:
    is_fatal: bool
    is_healthy: bool


class PlatformConnectorEventProcessor(CallbackInterface):
    """Converts check results to HealthEvents and sends them via gRPC to platform-connector."""

    def __init__(
        self,
        socket_path: str,
        node_name: str,
        processing_strategy: platformconnector_pb2.ProcessingStrategy,
    ) -> None:
        self._socket_path = socket_path
        self._node_name = node_name
        self._version = 1
        self._agent = "system-services-monitor"
        self._component_class = "INFRASTRUCTURE"
        self._processing_strategy = processing_strategy
        self.entity_cache: dict[str, CachedEntityState] = {}

    def _build_cache_key(self, check_name: str, entities_impacted: List[dict]) -> str:
        """Build a cache key from check name and impacted entities."""
        entity_str = "|".join(
            f"{e['entityType']}:{e['entityValue']}"
            for e in sorted(entities_impacted, key=lambda e: (e["entityType"], e["entityValue"]))
        )
        return f"{check_name}|{entity_str}"

    def _get_recommended_action(self, result: CheckResult) -> int:
        """Map check result to a RecommendedAction enum value.

        Fatal infrastructure failures (Fabric Manager down, fabric error) recommend RESTART_BM.
        Non-fatal issues (GPU service down) recommend CONTACT_SUPPORT.
        Healthy results use NONE.
        """
        if result.is_healthy:
            return platformconnector_pb2.NONE
        if result.is_fatal:
            return platformconnector_pb2.RESTART_BM
        return platformconnector_pb2.CONTACT_SUPPORT

    def health_check_completed(self, results: List[CheckResult]) -> None:
        """Process check results and send state-change HealthEvents to platform-connector."""
        with metrics.health_events_publish_time_to_grpc_channel.labels(
            "health_check_completed_to_grpc_channel"
        ).time():
            log.debug("received callback for health check completed")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()

            health_events = []
            pending_cache_updates: dict[str, CachedEntityState] = {}

            for result in results:
                cache_key = self._build_cache_key(result.check_name, result.entities_impacted)
                cached = self.entity_cache.get(cache_key)

                # Only send if state changed (or first observation)
                if cached is None or cached.is_fatal != result.is_fatal or cached.is_healthy != result.is_healthy:
                    entities = [
                        platformconnector_pb2.Entity(entityType=e["entityType"], entityValue=e["entityValue"])
                        for e in result.entities_impacted
                    ]

                    recommended_action = self._get_recommended_action(result)

                    health_event = platformconnector_pb2.HealthEvent(
                        version=self._version,
                        agent=self._agent,
                        componentClass=self._component_class,
                        checkName=result.check_name,
                        isFatal=result.is_fatal,
                        isHealthy=result.is_healthy,
                        message=result.message,
                        recommendedAction=recommended_action,
                        errorCode=result.error_codes,
                        entitiesImpacted=entities,
                        metadata=result.metadata or {},
                        generatedTimestamp=timestamp,
                        nodeName=self._node_name,
                        processingStrategy=self._processing_strategy,
                    )
                    health_events.append(health_event)
                    pending_cache_updates[cache_key] = CachedEntityState(
                        is_fatal=result.is_fatal, is_healthy=result.is_healthy
                    )

            log.debug(f"health events to send: {len(health_events)}")
            if len(health_events):
                try:
                    if self.send_health_event_with_retries(health_events):
                        # Only update cache after successful send
                        for key, state in pending_cache_updates.items():
                            self.entity_cache[key] = state
                            log.info(f"Updated cache for key {key} with value {state} after successful send")
                except Exception as e:
                    log.error(f"Exception while sending health events: {e}")

    def send_health_event_with_retries(self, health_events: list[platformconnector_pb2.HealthEvent]) -> bool:
        """Send health events to the platform connector with retries.

        Returns:
            True if the send was successful, False if all retries were exhausted.
            Cache updates should only be performed by the caller when this returns True.
        """
        delay = INITIAL_DELAY
        for _ in range(MAX_RETRIES):
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    stub.HealthEventOccurredV1(platformconnector_pb2.HealthEvents(events=health_events, version=1))
                    metrics.events_sent_success.inc()
                    return True
                except grpc.RpcError as e:
                    log.error(f"Failed to send health event to UDS: {e}")
                    sleep(delay)
                    delay = min(delay * 1.5, MAX_DELAY)
                    continue
        metrics.events_sent_error.inc()
        log.warning(
            f"Failed to send health event after {MAX_RETRIES} retries. "
            "Events will be retried on next health check cycle."
        )
        return False
