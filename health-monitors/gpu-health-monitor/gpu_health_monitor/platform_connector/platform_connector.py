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
from gpu_health_monitor.nvml_parser.nvml_parser import NvmlXidParser


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
        xid_errors_recommend_action_mapping: dict[str, platformconnector_pb2.RecommenedAction],
        xid_errors_batch_processing_interval: int,
        xid_errors_batch_processing_enabled: bool,
        nvml_xid_parser: NvmlXidParser,
        state_file_path: str,
    ) -> None:
        self._exit = exit
        self._socket_path = socket_path
        self._node_name = node_name
        self._version = 1
        self._agent = "gpu-health-monitor"
        self._component_class = "GPU"
        self.xid_errors_info_dict = xid_errors_info_dict
        self.xid_errors_recommend_action_mapping = xid_errors_recommend_action_mapping
        self.xid_errors_batch_processing_interval = xid_errors_batch_processing_interval
        self.xid_errors_sliding_window_index = 0
        self.nvml_xid_parser = nvml_xid_parser
        self.xid_errors_batch_processing_enabled = xid_errors_batch_processing_enabled
        self.nvml_xid_parser.register_xid_processing_done_callback(self.xid_error_batch_processing)
        self.state_file_path = state_file_path
        self.node_bootid_path = "/proc/sys/kernel/random/boot_id"
        self.old_bootid = self.read_old_system_bootid_from_state_file()
        self.current_bootid = self.fetch_current_bootid_and_clear_xid_errors()
        self.entity_cache: dict[str, CachedEntityState] = {}

    def read_old_system_bootid_from_state_file(self) -> str:
        bootid = ""
        try:
            with open(self.state_file_path, "r") as f:
                bootid = f.read().strip()
        except IOError:
            log.fatal(f"failed to read the data from file {self.state_file_path}")
        return bootid

    def fetch_current_bootid_and_clear_xid_errors(self) -> str:
        bootid = ""
        try:
            with open(self.node_bootid_path, "r") as f:
                bootid = f.read().strip()
        except IOError:
            log.fatal(f"failed to read the data from file {self.node_bootid_path}")

        log.info(f"current bootid is {bootid} and old_bootid is {self.old_bootid}")
        if self.old_bootid != bootid:
            log.info(f"clearing the xid errors as current_bootId {bootid} is not matching with {self.old_bootid}")
            self.old_bootid = bootid
            with open(self.state_file_path, "w") as output_file:
                output_file.write(bootid)
            self.clear_all_xid_errors()

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

    def health_event_occurred(self, health_details: dict[str, dcgmtypes.HealthDetails], gpu_ids: list):
        with metrics.dcgm_health_events_publish_time_to_grpc_channel.labels(
            "dcgm_health_events_to_grpc_channel"
        ).time():
            log.debug("received callback for health event")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()

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
                        isFatal = False if details.status == dcgmtypes.HealthStatus.PASS else True
                        isHealthy = True if details.status == dcgmtypes.HealthStatus.PASS else False
                        if (
                            key not in self.entity_cache
                            or self.entity_cache[key].isFatal != isFatal
                            or self.entity_cache[key].isHealthy != isHealthy
                        ):
                            self.entity_cache[key] = CachedEntityState(isFatal=isFatal, isHealthy=isHealthy)
                            log.info(f"Updated cache for key {key} with value {self.entity_cache[key]}")
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
                                    recommendedAction=platformconnector_pb2.REPORT_ISSUE,
                                    nodeName=self._node_name,
                                )
                            )
                    else:
                        entity = platformconnector_pb2.Entity(entityType=self._component_class, entityValue=str(gpu_id))
                        entities_impacted = []
                        entities_impacted.append(entity)
                        key = self._build_cache_key(check_name, entity.entityType, entity.entityValue)
                        if (
                            key not in self.entity_cache
                            or self.entity_cache[key].isFatal != False
                            or self.entity_cache[key].isHealthy != True
                        ):
                            self.entity_cache[key] = CachedEntityState(isFatal=False, isHealthy=True)
                            log.info(f"Updated cache for key {key} with value {self.entity_cache[key]}")
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
                                )
                            )
            log.debug(f"dcgm health event is {health_events}")
            if len(health_events):
                with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                    stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                    try:
                        stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=health_events, version=1))
                        metrics.health_events_insertion_to_uds_succeed.inc()
                    except grpc.RpcError:
                        metrics.health_events_insertion_to_uds_failed.inc()

    def clear_all_xid_errors(self):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            log.info("received callback for for clearing of xid errors")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            check_name = "GpuXidError"
            message = "NoXidErrorDetected"
            entities_impacted = []
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
            )
            log.debug(f"xid health event is {health_event}")
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))
                    metrics.health_events_insertion_to_uds_succeed.inc()
                except grpc.RpcError:
                    metrics.health_events_insertion_to_uds_failed.inc()

    def get_recommended_action_from_xid_error_map(self, error_code):
        recommended_action = self.xid_errors_info_dict[error_code].recommended_action
        return self.xid_errors_recommend_action_mapping[recommended_action]

    def xid_event_occurred(self, gpu_id: str, error_num: int):
        # The below if flag xid_errors_batch_processing_enabled is disabled for now as the NVML XID parser library is
        # not available yet. Once that is available, then this flag will be enabled.
        if self.xid_errors_batch_processing_enabled:
            self.nvml_xid_parser.process_xid_errors_on_gpu(error_num, gpu_id)
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
            )
            log.debug(f"xid health event is {health_event}")
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))
                    metrics.health_events_insertion_to_uds_succeed.inc()
                except grpc.RpcError:
                    metrics.health_events_insertion_to_uds_failed.inc()

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
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                try:
                    stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))
                    metrics.health_events_insertion_to_uds_succeed.inc()
                except grpc.RpcError:
                    metrics.health_events_insertion_to_uds_failed.inc()
