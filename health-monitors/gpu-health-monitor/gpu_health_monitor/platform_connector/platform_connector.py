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

import dataclasses
from collections.abc import Callable
from functools import wraps
import logging as log
import os
from typing import Any
from gpu_health_monitor.dcgm_watcher import types as dcgmtypes
from gpu_health_monitor.metadata import MetadataReader
from threading import Event, RLock

from gpu_health_monitor.protos import (
    health_event_pb2 as platformconnector_pb2,
    health_event_pb2_grpc as platformconnector_pb2_grpc,
)
from google.protobuf.timestamp_pb2 import Timestamp
import grpc
from . import metrics
from time import monotonic, sleep
import re

MAX_RETRIES = 10
INITIAL_DELAY = 5
GRPC_CALL_TIMEOUT_SECONDS = 5.0
# Critical events are emitted while the DCGM loop is about to enter cleanup or
# is already hung. Keep delivery bounded well inside the liveness restart budget.
CRITICAL_EVENT_DELIVERY_TIMEOUT_SECONDS = 15.0
# Only transport-level failures are worth another attempt. Every other status
# is a deterministic verdict from platform-connector (PERMISSION_DENIED,
# UNAUTHENTICATED, INVALID_ARGUMENT, ...) that will come back identical on the
# next attempt, so retrying it just spends the whole backoff budget - roughly
# six minutes at MAX_RETRIES=10 - before reporting the same failure.
RETRYABLE_STATUS_CODES = frozenset(
    {
        grpc.StatusCode.UNAVAILABLE,
        grpc.StatusCode.DEADLINE_EXCEEDED,
    }
)


def _rpc_status_code(error: grpc.RpcError) -> grpc.StatusCode | None:
    """The status code carried by a gRPC failure, or None when it carries none.

    Failures raised by a live channel are ``grpc.Call`` instances and always
    carry a code. A bare ``grpc.RpcError`` does not; it expresses no verdict
    either way, so callers keep treating it as retryable.
    """
    code_getter = getattr(error, "code", None)
    if not callable(code_getter):
        return None
    code = code_getter()
    return code if isinstance(code, grpc.StatusCode) else None


def _serialized_event_state(method: Callable[..., Any]) -> Callable[..., Any]:
    """Serialize cache/counter transitions across callback and watchdog threads."""

    @wraps(method)
    def wrapper(self: Any, *args: Any, **kwargs: Any) -> Any:
        with self._event_lock:
            return method(self, *args, **kwargs)

    return wrapper


@dataclasses.dataclass
class EntityCacheEntry:
    active_errors: set[str] = dataclasses.field(default_factory=set)

    @property
    def is_healthy(self) -> bool:
        return not self.active_errors


class PlatformConnectorEventProcessor(dcgmtypes.CallbackInterface):
    def __init__(
        self,
        socket_path: str,
        node_name: str,
        exit: Event,
        dcgm_errors_info_dict: dict[str, str],
        state_file_path: str,
        metadata_path: str,
        processing_strategy: platformconnector_pb2.ProcessingStrategy,
        store_only_checks: frozenset[str] = frozenset(),
        connectivity_failure_escalation_threshold: int = 0,
        token_path: str | None = None,
    ) -> None:
        self._exit = exit
        self._socket_path = socket_path
        # Projected ServiceAccount token presented as a bearer credential on
        # every publish, used exactly as given. The CLI layer already resolves
        # PLATFORM_CONNECTOR_TOKEN_PATH; resolving it again here would let
        # ambient process state override an explicitly empty argument.
        self._token_path = token_path
        self._node_name = node_name
        self._version = 1
        self._agent = "gpu-health-monitor"
        self._component_class = "GPU"
        self.dcgm_errors_info_dict = dcgm_errors_info_dict
        self.state_file_path = state_file_path
        self._dcgm_unresponsive_state_path = f"{state_file_path}.dcgm-unresponsive"
        self.node_bootid_path = "/proc/sys/kernel/random/boot_id"
        self.old_bootid = self.read_old_system_bootid_from_state_file()
        self.entity_cache: dict[str, EntityCacheEntry] = {}
        self._event_lock = RLock()
        self._metadata_reader = MetadataReader(metadata_path)
        self._processing_strategy = processing_strategy
        self._store_only_checks = store_only_checks
        self._connectivity_failure_escalation_threshold = connectivity_failure_escalation_threshold
        self._consecutive_connectivity_failures = 0
        self._connectivity_escalated = False
        # Strategy used for the active local-managed probe-hang event. Restored
        # from the marker so a clear after a liveness restart still matches the
        # unhealthy event even if Helm config changed in between.
        self._dcgm_unresponsive_strategy: platformconnector_pb2.ProcessingStrategy | None = None
        self._restore_dcgm_unresponsive_state()

    def read_old_system_bootid_from_state_file(self) -> str:
        bootid = ""
        try:
            with open(self.state_file_path, "r") as f:
                bootid = f.read().strip()
        except IOError:
            log.fatal(f"failed to read the data from file {self.state_file_path}")
        return bootid

    def _get_dcgm_watch(self, watch_name: str) -> str:
        watch_names = watch_name.split("_")[3:]
        watch_name = ""
        for name in watch_names:
            watch_name += f"{name[0]}{name[1:].lower()}"
        return watch_name

    def _convert_dcgm_watch_name_to_check_name(self, watch_name: str) -> str:
        ## DCGM_HEALTH_WATCH_PCIE ==> GpuPcieWatch; DCGM_HEALTH_WATCH_SM ==> GpuSmWatch
        return f"Gpu{self._get_dcgm_watch(watch_name)}Watch"

    def _build_cache_key(self, check_name: str, entity_type: str, entity_value: str) -> str:
        return f"{check_name}|{entity_type}|{entity_value}"

    def _persist_dcgm_unresponsive_state(self, processing_strategy: platformconnector_pb2.ProcessingStrategy) -> None:
        """Remember a delivered local-managed probe hang across liveness restarts.

        Format is two lines: error code, then the ProcessingStrategy name used
        for the unhealthy event. The strategy must be restored for the clear
        path so fault-quarantine still matches the pair after a config change.
        The marker is written to a sibling temporary file and renamed so a
        restart mid-write cannot leave the strategy line missing.
        """
        tmp_path = f"{self._dcgm_unresponsive_state_path}.tmp"
        try:
            strategy_name = platformconnector_pb2.ProcessingStrategy.Name(processing_strategy)
            with open(tmp_path, "w") as state_file:
                state_file.write(f"DCGM_PROBE_HANG\n{strategy_name}\n")
            os.replace(tmp_path, self._dcgm_unresponsive_state_path)
            self._dcgm_unresponsive_strategy = processing_strategy
        except OSError as e:
            log.error("Failed to persist unresponsive-DCGM state at %s: %s", self._dcgm_unresponsive_state_path, e)

    def _restore_dcgm_unresponsive_state(self) -> None:
        """Rebuild cache and gauge from a marker left by a previous process."""
        if not os.path.exists(self._dcgm_unresponsive_state_path):
            return

        strategy = self._effective_strategy("GpuDcgmUnresponsive")
        try:
            with open(self._dcgm_unresponsive_state_path, "r") as state_file:
                lines = [line.strip() for line in state_file.read().splitlines() if line.strip()]
            if len(lines) >= 2:
                try:
                    strategy = platformconnector_pb2.ProcessingStrategy.Value(lines[1])
                except ValueError:
                    log.warning(
                        "Unknown processing strategy %r in %s; falling back to current config",
                        lines[1],
                        self._dcgm_unresponsive_state_path,
                    )
        except OSError as e:
            log.error("Failed to read unresponsive-DCGM state at %s: %s", self._dcgm_unresponsive_state_path, e)

        key = self._build_cache_key("GpuDcgmUnresponsive", "DCGM", "ALL")
        self.entity_cache[key] = EntityCacheEntry(active_errors={"DCGM_PROBE_HANG"})
        self._dcgm_unresponsive_strategy = strategy
        metrics.dcgm_health_active_events.labels(event_type="GpuDcgmUnresponsive", gpu_id="").set(1)

    def _clear_dcgm_unresponsive_state(self) -> None:
        try:
            os.remove(self._dcgm_unresponsive_state_path)
        except FileNotFoundError:
            pass
        except OSError as e:
            log.error("Failed to remove unresponsive-DCGM state at %s: %s", self._dcgm_unresponsive_state_path, e)
        self._dcgm_unresponsive_strategy = None

    @_serialized_event_state
    def clear_dcgm_connectivity_failure(self, timestamp: Timestamp) -> None:
        """Clear DCGM connectivity failure events if connectivity has been restored."""
        health_events = []
        check_name = "GpuDcgmConnectivityFailure"

        self._consecutive_connectivity_failures = 0
        self._connectivity_escalated = False

        key = self._build_cache_key(check_name, "DCGM", "ALL")
        entry = self.entity_cache.get(key)
        if entry is None or not entry.is_healthy:
            event_metadata = {}
            chassis_serial = self._metadata_reader.get_chassis_serial()
            if chassis_serial:
                event_metadata["chassis_serial"] = chassis_serial

            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=check_name,
                generatedTimestamp=timestamp,
                isFatal=False,
                isHealthy=True,
                errorCode=[],
                entitiesImpacted=[],
                message="DCGM connectivity reported no errors",
                recommendedAction=platformconnector_pb2.NONE,
                nodeName=self._node_name,
                metadata=event_metadata,
                processingStrategy=self._processing_strategy,
            )
            health_events.append(health_event)

        if len(health_events):
            try:
                if self.send_health_event_with_retries(
                    health_events,
                    delivery_timeout_seconds=CRITICAL_EVENT_DELIVERY_TIMEOUT_SECONDS,
                ):
                    self.entity_cache[key] = EntityCacheEntry()
                    log.info(f"Updated cache for key {key} with value {self.entity_cache[key]} after successful send")
                    metrics.dcgm_health_active_events.labels(event_type=check_name, gpu_id="").set(0)
            except Exception as e:
                log.error(f"Exception while sending DCGM connectivity restored events: {e}")
                raise

    def _effective_strategy(self, check_name: str) -> platformconnector_pb2.ProcessingStrategy:
        """Force observe-only checks to STORE_ONLY, others use the global strategy.

        Applied to both the unhealthy and the clearing event of a check so that
        fault-quarantine sees a consistent strategy for the pair.
        """
        if check_name in self._store_only_checks:
            return platformconnector_pb2.STORE_ONLY
        return self._processing_strategy

    @_serialized_event_state
    def clear_dcgm_unresponsive(self, timestamp: Timestamp) -> None:
        """Clear a GpuDcgmUnresponsive event once a probe returns again."""
        health_events = []
        check_name = "GpuDcgmUnresponsive"

        key = self._build_cache_key(check_name, "DCGM", "ALL")
        entry = self.entity_cache.get(key)
        # A persisted marker restores an active entry during __init__, allowing
        # recovery to clear an event emitted before a liveness restart.
        if entry is not None and not entry.is_healthy:
            event_metadata = {}
            chassis_serial = self._metadata_reader.get_chassis_serial()
            if chassis_serial:
                event_metadata["chassis_serial"] = chassis_serial

            # Prefer the strategy captured with the unhealthy event so a Helm
            # change between liveness restarts cannot break the clear pair.
            clear_strategy = (
                self._dcgm_unresponsive_strategy
                if self._dcgm_unresponsive_strategy is not None
                else self._effective_strategy(check_name)
            )
            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=check_name,
                generatedTimestamp=timestamp,
                isFatal=False,
                isHealthy=True,
                errorCode=[],
                entitiesImpacted=[],
                message="DCGM answered a probe again",
                recommendedAction=platformconnector_pb2.NONE,
                nodeName=self._node_name,
                metadata=event_metadata,
                processingStrategy=clear_strategy,
            )
            health_events.append(health_event)

        if len(health_events):
            try:
                if self.send_health_event_with_retries(
                    health_events,
                    delivery_timeout_seconds=CRITICAL_EVENT_DELIVERY_TIMEOUT_SECONDS,
                ):
                    self.entity_cache[key] = EntityCacheEntry()
                    self._clear_dcgm_unresponsive_state()
                    log.info(f"Updated cache for key {key} with value {self.entity_cache[key]} after successful send")
                    metrics.dcgm_health_active_events.labels(event_type=check_name, gpu_id="").set(0)
            except Exception as e:
                log.error(f"Exception while sending DCGM responsive events: {e}")
                raise

    def health_event_occurred(self, health_details: dict[str, dcgmtypes.HealthDetails], gpu_ids: list) -> None:
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_health_events_to_grpc_channel"
        ).time():
            log.debug("received callback for health event")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()

            # First, check if we need to clear any previous connectivity failure events
            self.clear_dcgm_connectivity_failure(timestamp)
            # A completed health check proves DCGM answered, so retire any
            # unresponsive event a previous watchdog firing left active.
            self.clear_dcgm_unresponsive(timestamp)

            health_events = []
            # Collect pending cache and metric updates to apply only after successful send
            pending_cache_updates: dict[str, EntityCacheEntry] = {}
            pending_metric_updates: list[tuple[str, int, int]] = []  # (event_type, gpu_id, value)

            for watch_name, details in health_details.items():
                check_name = self._convert_dcgm_watch_name_to_check_name(watch_name)
                # Observe-only checks are forced to STORE_ONLY; all others use the
                # process-wide strategy. Applied to both the unhealthy and the
                # clearing event so fault-quarantine sees a consistent strategy.
                effective_strategy = self._effective_strategy(check_name)
                message = (
                    f"GPU {self._get_dcgm_watch(watch_name)} watch reported no errors"
                    if details.status == dcgmtypes.HealthStatus.PASS
                    else ""
                )

                error_code = ""
                log.debug(f"length of entity_failures are {len(details.entity_failures)}")
                for gpu_id in gpu_ids:
                    if details.entity_failures.get(gpu_id):
                        failure_details = details.entity_failures.get(gpu_id)
                        message = failure_details.message
                        error_code = [f"{failure_details.code}"]
                        entities_impacted = []
                        entity = platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
                        entities_impacted.append(entity)

                        pci_address = self._metadata_reader.get_pci_address(gpu_id)
                        if pci_address:
                            entities_impacted.append(
                                platformconnector_pb2.Entity(entityType="PCI", entityValue=pci_address)
                            )

                        gpu_uuid = self._metadata_reader.get_gpu_uuid(gpu_id)
                        if gpu_uuid:
                            entities_impacted.append(
                                platformconnector_pb2.Entity(entityType="GPU_UUID", entityValue=gpu_uuid)
                            )

                        entities_impacted_supports_component_reset = pci_address and gpu_uuid

                        recommended_action = self.get_recommended_action_from_dcgm_error_map(failure_details.code)
                        isHealthy = False
                        isFatal = recommended_action != platformconnector_pb2.NONE
                        key = self._build_cache_key(check_name, entity.entityType, entity.entityValue)

                        entry = self.entity_cache.get(key)
                        if entry is None or failure_details.code not in entry.active_errors:
                            existing_errors = set(entry.active_errors) if entry else set()
                            existing_errors.add(failure_details.code)
                            pending_cache_updates[key] = EntityCacheEntry(active_errors=existing_errors)

                            # The COMPONENT_RESET recommended action requires that the GPU_UUID is present on the
                            # unhealthy HealthEvent. Sending an event with COMPONENT_RESET that is missing the GPU_UUID
                            # impacted entity will result in a failed partial drain in node-drainer (as well as a
                            # failed remediation in fault-remediation). As a result, we are checking that the GPU_UUID
                            # can be read from the MetadataReader and are falling back to the RESTART_VM action if it
                            # is not present on the event.

                            # Note that entity-specific HealthEvents require an exact match for the set of impacted
                            # entities between the initial unhealthy event and the eventual healthy event which clears
                            # it in fault-quarantine. To ensure that there's a consistent view of impacted
                            # entities between healthy and unhealthy events, we will only send unhealthy HealthEvents
                            # for COMPONENT_RESET which include the GPU index, PCI, and GPU_UUID (and the corresponding
                            # HealthyEvent will include all of these as long as there's no failure extracting the PCI
                            # or GPU_UUID from the MetadataReader).
                            if (
                                recommended_action == platformconnector_pb2.COMPONENT_RESET
                                and not entities_impacted_supports_component_reset
                            ):
                                log.info(f"Overriding action from COMPONENT_RESET to RESTART_VM for {self._node_name}")
                                recommended_action = platformconnector_pb2.RESTART_VM

                            event_metadata = {}
                            chassis_serial = self._metadata_reader.get_chassis_serial()
                            if chassis_serial:
                                event_metadata["chassis_serial"] = chassis_serial

                            health_events.append(
                                platformconnector_pb2.HealthEvent(
                                    version=self._version,
                                    agent=self._agent,
                                    componentClass=self._component_class,
                                    checkName=check_name,
                                    generatedTimestamp=timestamp,
                                    isFatal=isFatal,
                                    isHealthy=isHealthy,
                                    errorCode=error_code,
                                    entitiesImpacted=entities_impacted,
                                    message=message,
                                    recommendedAction=recommended_action,
                                    nodeName=self._node_name,
                                    metadata=event_metadata,
                                    processingStrategy=effective_strategy,
                                )
                            )
                            pending_metric_updates.append((check_name, gpu_id, 1))
                    else:

                        entity = platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
                        entities_impacted = []
                        entities_impacted.append(entity)

                        pci_address = self._metadata_reader.get_pci_address(gpu_id)
                        if pci_address:
                            entities_impacted.append(
                                platformconnector_pb2.Entity(entityType="PCI", entityValue=pci_address)
                            )

                        gpu_uuid = self._metadata_reader.get_gpu_uuid(gpu_id)
                        if gpu_uuid:
                            entities_impacted.append(
                                platformconnector_pb2.Entity(entityType="GPU_UUID", entityValue=gpu_uuid)
                            )

                        key = self._build_cache_key(check_name, entity.entityType, entity.entityValue)
                        entry = self.entity_cache.get(key)

                        if entry is None or not entry.is_healthy:
                            had_errors = entry is not None and not entry.is_healthy
                            pending_cache_updates[key] = EntityCacheEntry()

                            event_metadata = {}
                            chassis_serial = self._metadata_reader.get_chassis_serial()
                            if chassis_serial:
                                event_metadata["chassis_serial"] = chassis_serial

                            health_events.append(
                                platformconnector_pb2.HealthEvent(
                                    version=self._version,
                                    agent=self._agent,
                                    componentClass=self._component_class,
                                    checkName=check_name,
                                    generatedTimestamp=timestamp,
                                    isFatal=False,
                                    isHealthy=True,
                                    errorCode=[],
                                    entitiesImpacted=entities_impacted,
                                    message=f"GPU {self._get_dcgm_watch(watch_name)} watch reported no errors",
                                    recommendedAction=platformconnector_pb2.NONE,
                                    nodeName=self._node_name,
                                    metadata=event_metadata,
                                    processingStrategy=effective_strategy,
                                )
                            )
                            if had_errors:
                                pending_metric_updates.append((check_name, gpu_id, 0))
            log.debug(f"dcgm health event is {health_events}")
            if len(health_events):
                try:
                    if self.send_health_event_with_retries(health_events):
                        # Only update cache and metrics after successful send
                        for key, state in pending_cache_updates.items():
                            self.entity_cache[key] = state
                            log.info(
                                f"Updated cache for key {key} with value {self.entity_cache[key]} after successful send"
                            )
                        for event_type, gpu_id, value in pending_metric_updates:
                            metrics.dcgm_health_active_events.labels(event_type=event_type, gpu_id=gpu_id).set(value)
                except Exception as e:
                    log.error(f"Exception while sending health events: {e}")

    def get_recommended_action_from_dcgm_error_map(self, error_code):
        if error_code in self.dcgm_errors_info_dict:
            recommended_action = self.dcgm_errors_info_dict[error_code]
            if recommended_action in platformconnector_pb2.RecommendedAction.keys():
                return platformconnector_pb2.RecommendedAction.Value(recommended_action)

        return platformconnector_pb2.RecommendedAction.CONTACT_SUPPORT

    def _is_platform_connector_socket_present(self) -> bool:
        # platform-connector removes the socket file on shutdown and on
        # startup before binding, so file-presence is a faithful proxy
        # for "PC is up" on this node.
        return os.path.exists(self._socket_path)

    def _token_metadata(self) -> list[tuple[str, str]] | None:
        """Bearer-token call metadata from the projected token file, or None.

        The kubelet rewrites the projected token file periodically, so the file
        is re-read on every call rather than cached. When a token path is
        configured but unreadable, the error propagates: a publisher configured
        to present a token must not fall back to publishing without one.
        """
        if not self._token_path:
            return None
        try:
            with open(self._token_path) as token_file:
                token = token_file.read()
        except OSError as e:
            log.error("Failed to read platform-connector token from %s: %s", self._token_path, e)
            raise
        # An empty file is a broken mount, not a credential. Publishing
        # "Bearer " gets a generic authentication error back from the server and
        # sends whoever debugs it looking at RBAC and audiences; failing here
        # names the actual problem.
        if not token:
            log.error("Platform-connector token file %s is empty", self._token_path)
            raise ValueError(f"platform-connector token file {self._token_path} is empty")
        return [("authorization", "Bearer " + token)]

    def send_health_event_with_retries(
        self,
        health_events: list[platformconnector_pb2.HealthEvent],
        delivery_timeout_seconds: float | None = None,
    ) -> bool:
        """Send health events to the platform connector with retries.

        If the platform-connector Unix socket is absent at send time the send
        is skipped immediately (no gRPC call, no buffering, no cache mutation)
        and `False` is returned. The caller's cache must be left untouched so
        the next poll re-emits with a fresh `generatedTimestamp`.

        Every gRPC call is bounded by ``GRPC_CALL_TIMEOUT_SECONDS`` so a stalled
        connector cannot block a worker indefinitely. When
        ``delivery_timeout_seconds`` is set (critical pre-cleanup paths), the
        overall retry budget is also capped; ordinary health events keep the
        existing MAX_RETRIES backoff without an overall deadline.

        When a token path is configured, every attempt re-reads the projected
        token file and attaches it as bearer metadata; a failed token read
        raises out of this method instead of publishing without the token.

        Only ``RETRYABLE_STATUS_CODES`` are retried. Any other gRPC status is a
        deterministic rejection, so the loop stops on the first one and reports
        the failure immediately.

        Returns:
            True on success. False if the socket was missing, a non-retryable
            status came back, or all retries were exhausted. Callers must update
            their cache only on True.
        """
        if not self._is_platform_connector_socket_present():
            metrics.health_events_insertion_skipped_pc_unavailable.inc()
            log.warning(
                "Platform-connector socket %s is missing; skipping send.",
                self._socket_path,
            )
            return False

        deadline = monotonic() + delivery_timeout_seconds if delivery_timeout_seconds is not None else None
        delay = INITIAL_DELAY
        attempts = 0
        timed_out = False
        non_retryable_code: grpc.StatusCode | None = None
        for attempt in range(MAX_RETRIES):
            remaining = deadline - monotonic() if deadline is not None else None
            if remaining is not None and remaining <= 0:
                timed_out = True
                break

            attempts = attempt + 1

            # Re-check between retries so a connector that disappears
            # mid-flight short-circuits instead of burning the budget.
            if attempt > 0 and not self._is_platform_connector_socket_present():
                metrics.health_events_insertion_skipped_pc_unavailable.inc()
                log.warning(
                    "Platform-connector socket %s disappeared mid-retry; aborting send.",
                    self._socket_path,
                )
                return False

            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    request = platformconnector_pb2.HealthEvents(events=health_events, version=1)
                    rpc_timeout = GRPC_CALL_TIMEOUT_SECONDS
                    if remaining is not None:
                        rpc_timeout = min(GRPC_CALL_TIMEOUT_SECONDS, remaining)
                    stub.HealthEventOccurredV1(request, timeout=rpc_timeout, metadata=self._token_metadata())
                    metrics.health_events_insertion_to_uds_succeed.inc()
                    return True
                except grpc.RpcError as e:
                    log.error(f"Failed to send health event {health_events} to UDS: {e}")
                    code = _rpc_status_code(e)
                    if code is not None and code not in RETRYABLE_STATUS_CODES:
                        # The same request will earn the same status next time,
                        # so stop here instead of spending the backoff budget.
                        non_retryable_code = code
                        log.error(
                            "Platform-connector returned non-retryable status %s; "
                            "abandoning the remaining %d attempt(s).",
                            code.name,
                            MAX_RETRIES - attempts,
                        )
                        break
                    if attempt == MAX_RETRIES - 1:
                        break
                    sleep_seconds = delay
                    if deadline is not None:
                        remaining = deadline - monotonic()
                        if remaining <= 0:
                            timed_out = True
                            break
                        sleep_seconds = min(sleep_seconds, remaining)
                    sleep(sleep_seconds)
                    delay *= 1.5
        metrics.health_events_insertion_to_uds_error.inc()
        if non_retryable_code is not None:
            reason = f"non-retryable status {non_retryable_code.name} on attempt {attempts}"
        elif timed_out:
            reason = (
                f"delivery budget exhausted after {attempts} attempt(s) "
                f"(delivery_timeout_seconds={delivery_timeout_seconds})"
            )
        else:
            reason = f"retry limit exhausted after {attempts} attempt(s)"
        log.warning(
            "Failed to send health event: %s. Events will be retried on next health check cycle.",
            reason,
        )
        return False

    @_serialized_event_state
    def dcgm_connectivity_failed(self) -> bool:
        """Handle a DCGM connectivity failure.

        Returns whether the event is already active or was delivered within the
        bounded critical-event budget. DCGMWatcher uses this synchronously before
        cleanup because cleanup itself can hang on an unresponsive DCGM probe.
        """
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_connectivity_failure_to_grpc_channel"
        ).time():
            log.error("DCGM connectivity failure detected, sending GpuDcgmConnectivityFailure health event")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            health_events = []
            check_name = "GpuDcgmConnectivityFailure"
            key = self._build_cache_key(check_name, "DCGM", "ALL")
            entry = self.entity_cache.get(key)

            self._consecutive_connectivity_failures += 1
            # One failed connection is worth a page. DCGM that stays unreachable
            # cycle after cycle is a stuck driver, and the only fix for that is a
            # reboot, so escalate the action once the operator's threshold is hit.
            escalate = (
                self._connectivity_failure_escalation_threshold > 0
                and self._consecutive_connectivity_failures >= self._connectivity_failure_escalation_threshold
            )
            newly_escalated = escalate and not self._connectivity_escalated

            if entry is None or entry.is_healthy or newly_escalated:
                message = "Failed to connect to DCGM for health check"
                recommended_action = platformconnector_pb2.CONTACT_SUPPORT
                if escalate:
                    message = (
                        "Failed to connect to DCGM for health check on "
                        f"{self._consecutive_connectivity_failures} consecutive cycles"
                    )
                    recommended_action = platformconnector_pb2.RESTART_BM

                event_metadata = {}
                chassis_serial = self._metadata_reader.get_chassis_serial()
                if chassis_serial:
                    event_metadata["chassis_serial"] = chassis_serial

                health_event = platformconnector_pb2.HealthEvent(
                    version=self._version,
                    agent=self._agent,
                    componentClass=self._component_class,
                    checkName=check_name,
                    generatedTimestamp=timestamp,
                    isFatal=True,
                    isHealthy=False,
                    errorCode=["DCGM_CONNECTIVITY_ERROR"],
                    entitiesImpacted=[],
                    message=message,
                    recommendedAction=recommended_action,
                    nodeName=self._node_name,
                    metadata=event_metadata,
                    processingStrategy=self._processing_strategy,
                )
                health_events.append(health_event)

            if not health_events:
                return True

            try:
                if self.send_health_event_with_retries(
                    health_events,
                    delivery_timeout_seconds=CRITICAL_EVENT_DELIVERY_TIMEOUT_SECONDS,
                ):
                    self.entity_cache[key] = EntityCacheEntry(active_errors={"DCGM_CONNECTIVITY_ERROR"})
                    if escalate:
                        self._connectivity_escalated = True
                    log.info(f"Updated cache for key {key} with value {self.entity_cache[key]} after successful send")
                    metrics.dcgm_health_active_events.labels(event_type=check_name, gpu_id="").set(1)
                    return True
                return False
            except Exception as e:
                log.error(f"Exception while sending DCGM connectivity failure events: {e}")
                raise

    @_serialized_event_state
    def dcgm_probe_unresponsive(
        self,
        operation: str,
        elapsed_seconds: float,
        dcgm_mode: str,
    ) -> bool:
        """Report a DCGM probe that stopped returning.

        An embedded (local-managed) hostengine hangs on-node, so the fault is
        local to this node even though it may still be DCGM userspace deadlock
        rather than a proven kernel-driver wedge. Classify that as
        GpuDcgmUnresponsive and recommend a reboot as the practical recovery.
        In remote modes the same symptom can be a service, DNS, or network
        outage; report it as a connectivity failure and leave the action at
        CONTACT_SUPPORT.

        Returns False when the event still needs publishing so the watchdog can
        retry: a hung poll loop has no later cycle to fall back on.
        """
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_probe_unresponsive_to_grpc_channel"
        ).time():
            local_managed = dcgm_mode == "local-managed"
            check_name = "GpuDcgmUnresponsive" if local_managed else "GpuDcgmConnectivityFailure"
            error_code = "DCGM_PROBE_HANG"
            recommended_action = (
                platformconnector_pb2.RESTART_BM if local_managed else platformconnector_pb2.CONTACT_SUPPORT
            )
            processing_strategy = self._effective_strategy(check_name) if local_managed else self._processing_strategy

            log.error(
                f"DCGM probe {operation} unresponsive for {elapsed_seconds:.1f}s, "
                f"sending {check_name} health event (mode={dcgm_mode})"
            )
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            health_events = []
            # Remote probe hangs intentionally share GpuDcgmConnectivityFailure with
            # ordinary connectivity failures: both mean "DCGM is unreachable from
            # this node" and should collapse to one active condition. Embedded hangs
            # use a distinct check name so the node-local reboot signal is preserved.
            key = self._build_cache_key(check_name, "DCGM", "ALL")
            entry = self.entity_cache.get(key)
            if entry is not None and not entry.is_healthy:
                # Already recorded for this episode.
                return True

            event_metadata = {"probe_operation": operation, "dcgm_mode": dcgm_mode}
            chassis_serial = self._metadata_reader.get_chassis_serial()
            if chassis_serial:
                event_metadata["chassis_serial"] = chassis_serial

            if local_managed:
                message = (
                    f"DCGM probe {operation} did not return after {elapsed_seconds:.1f}s "
                    "in local-managed mode; DCGM on this node is unresponsive and a reboot "
                    "is the practical recovery"
                )
            else:
                message = (
                    f"DCGM probe {operation} did not return after {elapsed_seconds:.1f}s "
                    f"in {dcgm_mode} mode; investigate the DCGM endpoint, network, and local driver"
                )

            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=check_name,
                generatedTimestamp=timestamp,
                isFatal=True,
                isHealthy=False,
                errorCode=[error_code],
                entitiesImpacted=[],
                message=message,
                recommendedAction=recommended_action,
                nodeName=self._node_name,
                metadata=event_metadata,
                processingStrategy=processing_strategy,
            )
            health_events.append(health_event)

            try:
                if self.send_health_event_with_retries(
                    health_events,
                    delivery_timeout_seconds=CRITICAL_EVENT_DELIVERY_TIMEOUT_SECONDS,
                ):
                    self.entity_cache[key] = EntityCacheEntry(active_errors={error_code})
                    if local_managed:
                        self._persist_dcgm_unresponsive_state(processing_strategy)
                    log.info(f"Updated cache for key {key} with value {self.entity_cache[key]} after successful send")
                    metrics.dcgm_health_active_events.labels(event_type=check_name, gpu_id="").set(1)
                    return True
                return False
            except Exception as e:
                log.error(f"Exception while sending DCGM unresponsive events: {e}")
                raise
