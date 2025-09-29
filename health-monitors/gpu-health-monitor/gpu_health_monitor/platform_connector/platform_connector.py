# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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
from threading import Event
from .protos import platformconnector_pb2, platformconnector_pb2_grpc
from google.protobuf.timestamp_pb2 import Timestamp
import grpc
from . import metrics
from time import sleep
import re

MAX_RETRIES = 10
INITIAL_DELAY = 5


@dataclasses.dataclass
class XidErrorsMappingDetails:
    name: str
    recommended_action: str
    fatal: str


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
        xid_errors_info_dict: dict[str, XidErrorsMappingDetails],
        gpu_errors_recommend_action_mapping: dict[str, platformconnector_pb2.RecommenedAction],
        dcgm_errors_info_dict: dict[str, str],
        state_file_path: str,
        dcgm_health_conditions_categorization_mapping_config: dict[str, str],
    ) -> None:
        self._exit = exit
        self._socket_path = socket_path
        self._node_name = node_name
        self._version = 1
        self._agent = "gpu-health-monitor"
        self._component_class = "GPU"
        self.dcgm_errors_info_dict = dcgm_errors_info_dict
        self.xid_errors_info_dict = xid_errors_info_dict
        self.gpu_errors_recommend_action_mapping = gpu_errors_recommend_action_mapping
        self.state_file_path = state_file_path
        self.node_bootid_path = "/proc/sys/kernel/random/boot_id"
        self.old_bootid = self.read_old_system_bootid_from_state_file()
        self.entity_cache: dict[str, CachedEntityState] = {}
        self.dcgm_health_conditions_categorization_mapping_config = dcgm_health_conditions_categorization_mapping_config

    def read_old_system_bootid_from_state_file(self) -> str:
        bootid = ""
        try:
            with open(self.state_file_path, "r") as f:
                bootid = f.read().strip()
        except IOError:
            log.fatal(f"failed to read the data from file {self.state_file_path}")
        return bootid

    def clear_all_xid_errors(self, gpu_ids: list, gpu_serials: dict[int, str]) -> str:
        bootid = ""
        try:
            with open(self.node_bootid_path, "r") as f:
                bootid = f.read().strip()
        except IOError:
            log.fatal(f"failed to read the data from file {self.node_bootid_path}")

        log.info(f"Evaluating XID Clearance conditions. Current bootid is {bootid} and old_bootid is {self.old_bootid}")
        if self.old_bootid != bootid:
            log.info(f"clearing the xid errors as current_bootId {bootid} is not matching with {self.old_bootid}")
            self.old_bootid = bootid
            with open(self.state_file_path, "w") as output_file:
                output_file.write(bootid)
            self.clear_xid_errors(gpu_ids, gpu_serials)

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

    def clear_dcgm_connectivity_failure(self, timestamp: Timestamp):
        """Clear DCGM connectivity failure events if connectivity has been restored."""
        health_events = []
        check_name = "GpuDcgmConnectivityFailure"

        key = self._build_cache_key(check_name, self._component_class, "")
        if key not in self.entity_cache or not self.entity_cache[key].isHealthy:
            self.entity_cache[key] = CachedEntityState(isFatal=False, isHealthy=True)
            log.info(f"Updated cache for key {key} with connectivity failure")

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
                metadata={"SerialNumber": ""},
            )
            health_events.append(health_event)

            # Clear metric for connectivity failure
            metrics.dcgm_health_active_fatal_health_events.labels(event_type=check_name, gpu_id="").set(0)

        if len(health_events):
            try:
                self.send_health_event_with_retries(health_events)
            except Exception as e:
                log.error(f"Exception while sending DCGM connectivity restored events: {e}")

    def health_event_occurred(
        self, health_details: dict[str, dcgmtypes.HealthDetails], gpu_ids: list, serials: dict[int, str]
    ):
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_health_events_to_grpc_channel"
        ).time():
            log.debug("received callback for health event")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()

            # First, check if we need to clear any previous connectivity failure events
            self.clear_dcgm_connectivity_failure(timestamp)

            health_events = []
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
                            self.entity_cache[key] = CachedEntityState(isFatal=isFatal, isHealthy=isHealthy)
                            log.info(f"Updated cache for key {key} with value {self.entity_cache[key]}")
                            if failure_details.code == "DCGM_FR_XID_ERROR":
                                xid = self.get_xid_from_dcgm_message(message)
                                recommended_action = self.get_recommended_action_from_xid_error_map(xid)
                            else:
                                recommended_action = self.get_recommended_action_from_dcgm_error_map(
                                    failure_details.code
                                )

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
                                    metadata={"SerialNumber": serials[gpu_id]},
                                )
                            )
                            if self.dcgm_health_conditions_categorization_mapping_config[watch_name] == "NonFatal":
                                metrics.dcgm_health_active_non_fatal_health_events.labels(
                                    event_type=check_name, gpu_id=gpu_id
                                ).set(1)
                            else:
                                metrics.dcgm_health_active_fatal_health_events.labels(
                                    event_type=check_name, gpu_id=gpu_id
                                ).set(1)
                    else:

                        entity = platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
                        entities_impacted = []
                        entities_impacted.append(entity)
                        key = self._build_cache_key(check_name, entity.entityType, entity.entityValue)
                        if (
                            key not in self.entity_cache
                            or self.entity_cache[key].isFatal
                            or not self.entity_cache[key].isHealthy
                        ):

                            self.entity_cache[key] = CachedEntityState(isFatal=False, isHealthy=True)
                            log.info(f"Updated cache for key {key} with value {self.entity_cache[key]}")
                            # Don't send health events for non-fatal health conditions when they are healthy
                            # they will get published as node conditions which we don't want to do to have
                            # consistency in the health events publishing logic
                            if self.dcgm_health_conditions_categorization_mapping_config[watch_name] == "NonFatal":
                                log.debug(f"Skipping non-fatal health event for watch {watch_name}")
                            else:
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
                                        metadata={"SerialNumber": serials[gpu_id]},
                                    )
                                )
                            if self.dcgm_health_conditions_categorization_mapping_config[watch_name] == "NonFatal":
                                metrics.dcgm_health_active_non_fatal_health_events.labels(
                                    event_type=check_name, gpu_id=gpu_id
                                ).set(0)
                            else:
                                metrics.dcgm_health_active_fatal_health_events.labels(
                                    event_type=check_name, gpu_id=gpu_id
                                ).set(0)
            log.debug(f"dcgm health event is {health_events}")
            if len(health_events):
                try:
                    self.send_health_event_with_retries(health_events)
                except Exception as e:
                    log.error(f"Exception while sending health events: {e}. Events will be retried in next cycle.")
                    # Don't crash - continue monitoring

    def clear_xid_errors(self, gpu_ids: list, gpu_serials: dict[int, str]):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            for gpu_id in gpu_ids:

                serial = gpu_serials[gpu_id]
                log.info("received callback for for clearing of xid errors")
                timestamp = Timestamp()
                timestamp.GetCurrentTime()
                check_name = "GpuXidError"
                message = "NoXidErrorDetected"
                entities_impacted = [
                    platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
                ]
                error_code = []
                health_event = platformconnector_pb2.HealthEvent(
                    version=self._version,
                    agent=self._agent,
                    componentClass=self._component_class,
                    checkName=check_name,
                    generatedTimestamp=timestamp,
                    isFatal=False,
                    isHealthy=True,
                    errorCode=error_code,
                    entitiesImpacted=entities_impacted,
                    message=message,
                    recommendedAction=platformconnector_pb2.NONE,
                    nodeName=self._node_name,
                    metadata={"SerialNumber": serial},
                )

                metrics.gpu_health_monitor_xid_errors.labels(
                    node_name=self._node_name,
                    serial_number=serial,
                ).set(0)
                log.debug(f"xid health event is {health_event}")
                self.send_health_event_with_retries([health_event])

    def get_recommended_action_from_xid_error_map(self, error_code):
        recommended_action = self.xid_errors_info_dict[error_code].recommended_action
        return self.gpu_errors_recommend_action_mapping[recommended_action]

    def get_recommended_action_from_dcgm_error_map(self, error_code):
        if error_code in self.dcgm_errors_info_dict:
            recommended_action = self.dcgm_errors_info_dict[error_code]
            return self.gpu_errors_recommend_action_mapping[recommended_action]
        return platformconnector_pb2.RecommenedAction.REPORT_ISSUE

    def xid_event_occurred(self, gpu_id: str, error_num: int, serial: str):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            check_name = "GpuXidError"
            message = "XID error occured"
            entities_impacted = []
            entity = platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
            entities_impacted.append(entity)
            error_code = [f"{error_num}"]
            is_fatal = True
            recommended_action = platformconnector_pb2.UNKNOWN
            if str(error_num) in self.xid_errors_info_dict:
                if self.xid_errors_info_dict[str(error_num)].fatal == "NONFATAL":
                    is_fatal = False
                recommended_action = self.get_recommended_action_from_xid_error_map(str(error_num))

            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=check_name,
                generatedTimestamp=timestamp,
                isFatal=is_fatal,
                isHealthy=False,
                errorCode=error_code,
                entitiesImpacted=entities_impacted,
                message=message,
                recommendedAction=recommended_action,
                nodeName=self._node_name,
                metadata={"SerialNumber": serial},
            )
            metrics.gpu_health_monitor_xid_errors.labels(
                node_name=self._node_name,
                serial_number=serial,
            ).set(error_num)

            log.debug(f"xid health event is {health_event}")
            self.send_health_event_with_retries([health_event])

    def xid_error_batch_processing(
        self, xid_errors_list: list, gpu_id: str, recommendation_action: platformconnector_pb2.RecommenedAction
    ):
        log.debug(f"xid_error_list: {xid_errors_list}, gpu_id: {gpu_id} and recommeded_action: {recommendation_action}")
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            check_name = "XidBatchError"
            message = "XID batch errors occured"
            entities_impacted = []
            entity = platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
            entities_impacted.append(entity)
            error_code = [str(xid_error) for xid_error in xid_errors_list]
            is_fatal = True
            recommended_action = recommendation_action
            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=check_name,
                generatedTimestamp=timestamp,
                isFatal=is_fatal,
                isHealthy=False,
                errorCode=error_code,
                entitiesImpacted=entities_impacted,
                message=message,
                recommendedAction=recommended_action,
                nodeName=self._node_name,
            )
            log.info(f"xid health event is {health_event}")
            self.send_health_event_with_retries([health_event])

    def send_health_event_with_retries(self, health_events: list[platformconnector_pb2.HealthEvent]):
        delay = INITIAL_DELAY
        for _ in range(MAX_RETRIES):
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=health_events, version=1))
                    metrics.health_events_insertion_to_uds_succeed.inc()
                    metrics.health_events_insertion_to_uds_error.set(0.0)
                    return True
                except grpc.RpcError as e:
                    log.error(f"Failed to send health event {health_events} to UDS: {e}")
                    sleep(delay)
                    delay *= 1.5
                    continue
        metrics.health_events_insertion_to_uds_error.set(1.0)
        # Remove failed health events from entity cache
        for health_event in health_events:
            for entity in health_event.entitiesImpacted:
                cache_key = self._build_cache_key(health_event.checkName, entity.entityType, entity.entityValue)
                if cache_key in self.entity_cache:
                    del self.entity_cache[cache_key]
        return False

    def get_xid_from_dcgm_message(self, message: str) -> str:
        xid_pattern = r"XID (\d+)"
        match = re.search(xid_pattern, message)
        if match:
            return match.group(1)
        return None

    def dcgm_connectivity_failed(self):
        """Handle DCGM connectivity failure event."""
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_connectivity_failure_to_grpc_channel"
        ).time():
            log.error("DCGM connectivity failure detected, sending GpuDcgmConnectivityFailure health event")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()

            health_events = []
            check_name = "GpuDcgmConnectivityFailure"
            key = self._build_cache_key(check_name, self._component_class, "")
            if key not in self.entity_cache or self.entity_cache[key].isHealthy:
                self.entity_cache[key] = CachedEntityState(isFatal=True, isHealthy=False)
                log.info(f"Updated cache for key {key} with connectivity failure")

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
                    message="Failed to connect to DCGM for health check",
                    recommendedAction=platformconnector_pb2.REPORT_ISSUE,
                    nodeName=self._node_name,
                    metadata={"SerialNumber": ""},
                )
                health_events.append(health_event)
                metrics.dcgm_health_active_fatal_health_events.labels(event_type=check_name, gpu_id="").set(1)

            if len(health_events):
                try:
                    self.send_health_event_with_retries(health_events)
                except Exception as e:
                    log.error(f"Exception while sending DCGM connectivity failure events: {e}")
                    # Don't crash - continue monitoring
