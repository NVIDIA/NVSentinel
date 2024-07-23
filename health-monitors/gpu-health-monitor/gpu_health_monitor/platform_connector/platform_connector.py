import dataclasses
import logging as log
from gpu_health_monitor.dcgm_watcher import types as dcgmtypes
from threading import Event
from .protos import platformconnector_pb2, platformconnector_pb2_grpc
from google.protobuf.timestamp_pb2 import Timestamp
import grpc
from . import metrics


@dataclasses.dataclass
class XidErrorsMappingDetails:
    name: str
    recommended_action: str
    fatal: str


class PlatformConnectorEventProcessor(dcgmtypes.CallbackInterface):
    def __init__(
        self,
        socket_path: str,
        node_name: str,
        exit: Event,
        xid_errors_info_dict: dict[str, XidErrorsMappingDetails],
        xid_errors_recommend_action_mapping: dict[str, platformconnector_pb2.RecommenedAction],
    ) -> None:
        self._exit = exit
        self._socket_path = socket_path
        self._node_name = node_name
        self._version = 1
        self._agent = "gpu-health-monitor"
        self._component_class = "gpu"
        self.xid_errors_info_dict = xid_errors_info_dict
        self.xid_errors_recommend_action_mapping = xid_errors_recommend_action_mapping

    def _get_dcgm_watch(self, watch_name: str) -> str:
        watch_names = watch_name.split("_")[3:]
        watch_name = ""
        for name in watch_names:
            watch_name += f"{name[0]}{name[1:].lower()}"
        return watch_name

    def _convert_dcgm_watch_name_to_check_name(self, watch_name: str) -> str:
        ## DCGM_HEALTH_WATCH_PCIE ==> GpuPcieWatch; DCGM_HEALTH_WATCH_SM ==> GpuSmWatch
        return f"Gpu{self._get_dcgm_watch(watch_name)}Watch"

    def health_event_occurred(self, health_details: dict[str, dcgmtypes.HealthDetails]):
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
                entities_impacted = []

                error_code = ""
                for id, failure_details in details.entity_failures.items():
                    error_code = f"{failure_details.code}"
                    entities_impacted = [f"{id}"]
                    health_events.append(
                        platformconnector_pb2.HealthEvent(
                            version=self._version,
                            agent=self._agent,
                            componentClass=self._component_class,
                            checkName=check_name,
                            generatedTimestamp=timestamp,
                            isFatal=False if details.status == dcgmtypes.HealthStatus.PASS else True,
                            isHealthy=True if details.status == dcgmtypes.HealthStatus.PASS else False,
                            errorCode=error_code,
                            entitiesImpacted=entities_impacted,
                            message=message,
                            recommendedAction=platformconnector_pb2.UNKNOWN,
                            nodeName=self._node_name,
                        )
                    )
                    break
                else:
                    health_events.append(
                        platformconnector_pb2.HealthEvent(
                            version=self._version,
                            agent=self._agent,
                            componentClass=self._component_class,
                            checkName=check_name,
                            generatedTimestamp=timestamp,
                            isFatal=False if details.status == dcgmtypes.HealthStatus.PASS else True,
                            isHealthy=True if details.status == dcgmtypes.HealthStatus.PASS else False,
                            errorCode="",
                            entitiesImpacted=[],
                            message=message,
                            recommendedAction=platformconnector_pb2.UNKNOWN,
                            nodeName=self._node_name,
                        )
                    )
            log.debug(f"xid health event is {health_events}")
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=health_events, version=1))

    def clear_all_xid_errors(self):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            log.info("received callback for for clearing of xid errors")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            check_name = "XidError"
            message = "NoXidErrorDetected"
            entities_impacted = []
            error_code = ""
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
                stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))

    def get_recommended_action_from_xid_error_map(self, error_code):
        recommended_action = self.xid_errors_info_dict[error_code].recommended_action
        return self.xid_errors_recommend_action_mapping[recommended_action]

    def xid_event_occurred(self, gpu_id: str, error_num: int):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            check_name = "XidError"
            message = "XID error occured"
            entities_impacted = [f"{gpu_id}"]
            error_code = str(error_num)
            is_fatal = True
            recommended_action = platformconnector_pb2.UNKNOWN
            if error_code in self.xid_errors_info_dict:
                if self.xid_errors_info_dict[error_code].fatal == "NONFATAL":
                    is_fatal = False
                recommended_action = self.get_recommended_action_from_xid_error_map(error_code)

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
                stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))

    def field_change_event_occurred(self, fields_changes: dict[str, list[dcgmtypes.FieldDetails]]):
        log.debug(f"received callback for field change event {fields_changes}")
