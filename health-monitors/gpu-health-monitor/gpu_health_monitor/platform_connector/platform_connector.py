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
import logging as log
from gpu_health_monitor.dcgm_watcher import types as dcgmtypes
from gpu_health_monitor.metadata import MetadataReader
from threading import Event

from gpu_health_monitor.protos import (
    health_event_pb2 as platformconnector_pb2,
    health_event_pb2_grpc as platformconnector_pb2_grpc,
)
from google.protobuf.timestamp_pb2 import Timestamp
import grpc
from . import metrics
from time import sleep
import re

MAX_RETRIES = 10
INITIAL_DELAY = 5


@dataclasses.dataclass
class CachedEntityState:
    isFatal: bool
    isHealthy: bool


class PlatformConnectorEventProcessor(dcgmtypes.CallbackInterface):
    def __init__(
        self,
        socket_path: str,
        node_name: str,
        exit: Event,
        dcgm_errors_info_dict: dict[str, str],
        state_file_path: str,
        dcgm_health_conditions_categorization_mapping_config: dict[str, str],
        metadata_path: str,
        processing_strategy: platformconnector_pb2.ProcessingStrategy,
    ) -> None:
        self._exit = exit
        self._socket_path = socket_path
        self._node_name = node_name
        self._version = 1
        self._agent = "gpu-health-monitor"
        self._component_class = "GPU"
        self.dcgm_errors_info_dict = dcgm_errors_info_dict
        self.state_file_path = state_file_path
        self.node_bootid_path = "/proc/sys/kernel/random/boot_id"
        self.old_bootid = self.read_old_system_bootid_from_state_file()
        self.entity_cache: dict[str, CachedEntityState] = {}
        self.dcgm_health_conditions_categorization_mapping_config = dcgm_health_conditions_categorization_mapping_config
        self._metadata_reader = MetadataReader(metadata_path)
        self._processing_strategy = processing_strategy

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

    def clear_dcgm_connectivity_failure(self, timestamp: Timestamp) -> None:
        """Clear DCGM connectivity failure events if connectivity has been restored."""
        health_events = []
        check_name = "GpuDcgmConnectivityFailure"

        key = self._build_cache_key(check_name, "DCGM", "ALL")
        if key not in self.entity_cache or not self.entity_cache[key].isHealthy:
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
                if self.send_health_event_with_retries(health_events):
                    # Only update cache after successful send
                    self.entity_cache[key] = CachedEntityState(isFatal=False, isHealthy=True)
                    log.info(f"Updated cache for key {key} after successful send")
                    metrics.dcgm_health_active_events.labels(event_type=check_name, gpu_id="", severity="fatal").set(0)
            except Exception as e:
                log.error(f"Exception while sending DCGM connectivity restored events: {e}")
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

            health_events = []
            # Collect pending cache and metric updates to apply only after successful send
            pending_cache_updates: dict[str, CachedEntityState] = {}
            pending_metric_updates: list[tuple[str, int, str, int]] = []  # (event_type, gpu_id, severity, value)

            for watch_name, details in health_details.items():
                check_name = self._convert_dcgm_watch_name_to_check_name(watch_name)
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

                        key = self._build_cache_key(check_name, entity.entityType, entity.entityValue)
                        isFatal = False
                        isHealthy = True
                        if details.status == dcgmtypes.HealthStatus.PASS:
                            isFatal = False
                            isHealthy = True
                        else:
                            isFatal = (
                                False
                                if self.dcgm_health_conditions_categorization_mapping_config[watch_name] == "NonFatal"
                                else True
                            )
                            isHealthy = False
                        if (
                            key not in self.entity_cache
                            or self.entity_cache[key].isFatal != isFatal
                            or self.entity_cache[key].isHealthy != isHealthy
                        ):
                            # Defer cache update until after successful send
                            pending_cache_updates[key] = CachedEntityState(isFatal=isFatal, isHealthy=isHealthy)
                            recommended_action = self.get_recommended_action_from_dcgm_error_map(failure_details.code)

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
                                    processingStrategy=self._processing_strategy,
                                )
                            )
                            severity = (
                                "non_fatal"
                                if self.dcgm_health_conditions_categorization_mapping_config[watch_name] == "NonFatal"
                                else "fatal"
                            )
                            # Defer metric update until after successful send
                            pending_metric_updates.append((check_name, gpu_id, severity, 1))
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
                        if (
                            key not in self.entity_cache
                            or self.entity_cache[key].isFatal
                            or not self.entity_cache[key].isHealthy
                        ):
                            # Don't send health events for non-fatal health conditions when they are healthy
                            # they will get published as node conditions which we don't want to do to have
                            # consistency in the health events publishing logic
                            if self.dcgm_health_conditions_categorization_mapping_config[watch_name] == "NonFatal":
                                log.debug(f"Skipping non-fatal health event for watch {watch_name}")
                                # For non-fatal events that are not sent, update cache immediately
                                # (no race condition since nothing is being sent)
                                self.entity_cache[key] = CachedEntityState(isFatal=False, isHealthy=True)
                                log.info(f"Updated cache for key {key} with value {self.entity_cache[key]}")
                                metrics.dcgm_health_active_events.labels(
                                    event_type=check_name, gpu_id=gpu_id, severity="non_fatal"
                                ).set(0)
                            else:
                                # Defer cache update until after successful send
                                pending_cache_updates[key] = CachedEntityState(isFatal=False, isHealthy=True)
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
                                        processingStrategy=self._processing_strategy,
                                    )
                                )
                                # Defer metric update until after successful send
                                pending_metric_updates.append((check_name, gpu_id, "fatal", 0))
            log.debug(f"dcgm health event is {health_events}")
            if len(health_events):
                try:
                    if self.send_health_event_with_retries(health_events):
                        # Only update cache and metrics after successful send
                        for key, state in pending_cache_updates.items():
                            self.entity_cache[key] = state
                            log.info(f"Updated cache for key {key} with value {state} after successful send")
                        for event_type, gpu_id, severity, value in pending_metric_updates:
                            metrics.dcgm_health_active_events.labels(
                                event_type=event_type, gpu_id=gpu_id, severity=severity
                            ).set(value)
                except Exception as e:
                    log.error(f"Exception while sending health events: {e}")

    def get_recommended_action_from_dcgm_error_map(self, error_code):
        if error_code in self.dcgm_errors_info_dict:
            recommended_action = self.dcgm_errors_info_dict[error_code]
            if recommended_action in platformconnector_pb2.RecommendedAction.keys():
                return platformconnector_pb2.RecommendedAction.Value(recommended_action)

        return platformconnector_pb2.RecommendedAction.CONTACT_SUPPORT

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
                    metrics.health_events_insertion_to_uds_succeed.inc()
                    return True
                except grpc.RpcError as e:
                    log.error(f"Failed to send health event {health_events} to UDS: {e}")
                    sleep(delay)
                    delay *= 1.5
                    continue
        metrics.health_events_insertion_to_uds_error.inc()
        log.warning(
            f"Failed to send health event after {MAX_RETRIES} retries. Events will be retried on next health check cycle."
        )
        return False

    def dcgm_connectivity_failed(self) -> None:
        """Handle DCGM connectivity failure event."""
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_connectivity_failure_to_grpc_channel"
        ).time():
            log.error("DCGM connectivity failure detected, sending GpuDcgmConnectivityFailure health event")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            message = "Failed to connect to DCGM for health check"
            health_events = []
            check_name = "GpuDcgmConnectivityFailure"
            key = self._build_cache_key(check_name, "DCGM", "ALL")
            if key not in self.entity_cache or self.entity_cache[key].isHealthy:
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
                    recommendedAction=platformconnector_pb2.CONTACT_SUPPORT,
                    nodeName=self._node_name,
                    metadata=event_metadata,
                    processingStrategy=self._processing_strategy,
                )
                health_events.append(health_event)

            if len(health_events):
                try:
                    if self.send_health_event_with_retries(health_events):
                        # Only update cache after successful send
                        self.entity_cache[key] = CachedEntityState(isFatal=True, isHealthy=False)
                        log.info(f"Updated cache for key {key} after successful send")
                        metrics.dcgm_health_active_events.labels(
                            event_type=check_name, gpu_id="", severity="fatal"
                        ).set(1)
                except Exception as e:
                    log.error(f"Exception while sending DCGM connectivity failure events: {e}")
                    raise
