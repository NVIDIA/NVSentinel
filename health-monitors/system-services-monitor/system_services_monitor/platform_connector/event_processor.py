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
import os
import threading
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
# Per-attempt gRPC deadline. Shorter than the total retry budget so a hung
# platform-connector surfaces as DEADLINE_EXCEEDED and is retried instead of
# blocking a ThreadPoolExecutor thread indefinitely.
GRPC_SEND_TIMEOUT_SECS = 10


@dataclasses.dataclass
class CachedEntityState:
    """Last-sent health state for one entity; the transition cache's value type."""

    is_fatal: bool
    is_healthy: bool
    # Normalized (sorted, de-duplicated) condition codes. Part of the cached
    # identity so a code-only transition (e.g. FABRIC_MANAGER_NOT_RUNNING ->
    # FABRIC_MANAGER_NOT_RUNNING + FABRIC_MANAGER_FLAPPING) emits a new event
    # even when the fatal/healthy flags are unchanged.
    error_codes: tuple = ()


class PlatformConnectorEventProcessor(CallbackInterface):
    """Converts check results to HealthEvents and sends them via gRPC to platform-connector."""

    def __init__(
        self,
        socket_path: str,
        node_name: str,
        processing_strategy: platformconnector_pb2.ProcessingStrategy,
    ) -> None:
        """Bind the UDS socket path, node identity, and processing strategy."""
        self._socket_path = socket_path
        self._node_name = node_name
        self._version = 1
        self._agent = "system-services-monitor"
        self._component_class = "INFRASTRUCTURE"
        self._processing_strategy = processing_strategy
        self.entity_cache: dict[str, CachedEntityState] = {}
        # Guards entity_cache read/decide/write against overlapping callback
        # invocations under a ThreadPoolExecutor (TOCTOU race — duplicate
        # HealthEvent emissions while send_health_event_with_retries blocks).
        self._cache_lock = threading.Lock()

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
        with metrics.health_events_publish_time_to_grpc_channel.labels("health_check_completed_to_grpc_channel").time():
            log.debug("received callback for health check completed")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()

            health_events = []
            pending_cache_updates: dict[str, CachedEntityState] = {}

            # Snapshot the cache state and decide which events to send under a
            # single lock so overlapping callbacks (e.g. from a
            # ThreadPoolExecutor) don't both observe the same stale entry and
            # emit duplicate HealthEvents while one of them blocks in
            # send_health_event_with_retries (~26 s worst case).
            with self._cache_lock:
                for result in results:
                    cache_key = self._build_cache_key(result.check_name, result.entities_impacted)
                    cached = self.entity_cache.get(cache_key)
                    new_state = CachedEntityState(
                        is_fatal=result.is_fatal,
                        is_healthy=result.is_healthy,
                        error_codes=tuple(sorted(set(result.error_codes or []))),
                    )

                    # Only send if state changed (or first observation)
                    if cached is None or cached != new_state:
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
                        pending_cache_updates[cache_key] = new_state
                        # Reserve the decision immediately so a concurrent
                        # callback observing the same check result skips it
                        # instead of re-emitting a duplicate HealthEvent while
                        # the gRPC call below is in flight.
                        self.entity_cache[cache_key] = pending_cache_updates[cache_key]

            log.debug(f"health events to send: {len(health_events)}")
            if len(health_events):
                try:
                    if self.send_health_event_with_retries(health_events):
                        for key, state in pending_cache_updates.items():
                            log.info(f"Updated cache for key {key} with value {state} after successful send")
                    else:
                        # Send failed after retries -- roll back the reservations
                        # so the next cycle re-attempts these events. Pop only
                        # entries that are still OUR reservation (object identity
                        # is the reservation token): a newer overlapping callback
                        # may have replaced the entry with an already-sent state,
                        # and popping that would re-emit it as a duplicate.
                        with self._cache_lock:
                            for key, reserved in pending_cache_updates.items():
                                if self.entity_cache.get(key) is reserved:
                                    self.entity_cache.pop(key, None)
                except Exception as e:
                    log.error(f"Exception while sending health events: {e}")
                    with self._cache_lock:
                        for key, reserved in pending_cache_updates.items():
                            if self.entity_cache.get(key) is reserved:
                                self.entity_cache.pop(key, None)

    def _is_platform_connector_socket_present(self) -> bool:
        """True when the platform-connector UDS socket file exists."""
        # platform-connector removes the socket file on shutdown and on
        # startup before binding, so file-presence is a faithful proxy
        # for "PC is up" on this node.
        return os.path.exists(self._socket_path)

    def send_health_event_with_retries(self, health_events: list[platformconnector_pb2.HealthEvent]) -> bool:
        """Send health events to the platform connector with retries.

        If the platform-connector Unix socket is absent at send time the send
        is skipped immediately (no gRPC call, no buffering, no cache mutation)
        and ``False`` is returned so the caller re-emits on the next cycle.

        Returns:
            True if the send was successful, False if the socket was missing or
            all retries were exhausted.
            Cache updates should only be performed by the caller when this returns True.
        """
        if not self._is_platform_connector_socket_present():
            metrics.events_sent_skipped_pc_unavailable.inc()
            log.warning(
                "Platform-connector socket %s is missing; skipping send.",
                self._socket_path,
            )
            return False

        delay = INITIAL_DELAY
        for attempt in range(MAX_RETRIES):
            # Re-check between retries so a connector that disappears
            # mid-flight short-circuits instead of burning the budget.
            if attempt > 0 and not self._is_platform_connector_socket_present():
                metrics.events_sent_skipped_pc_unavailable.inc()
                log.warning(
                    "Platform-connector socket %s disappeared mid-retry; aborting send.",
                    self._socket_path,
                )
                return False

            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    stub.HealthEventOccurredV1(
                        platformconnector_pb2.HealthEvents(events=health_events, version=1),
                        timeout=GRPC_SEND_TIMEOUT_SECS,
                    )
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
