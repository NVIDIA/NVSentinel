import logging as log
from gpu_health_monitor.dcgm_watcher import types as dcgmtypes
from threading import Event
from .protos import platformconnector_pb2, platformconnector_pb2_grpc
from google.protobuf.timestamp_pb2 import Timestamp
import grpc
from . import metrics


class PlatformConnectorEventProcessor(dcgmtypes.CallbackInterface):
    def __init__(
        self,
        socket_path: str,
        exit: Event,
    ) -> None:
        self._exit = exit
        self._socket_path = socket_path
        self._version = 1
        self._agent = "gpu-health-monitor"
        self._component_class = "gpu"

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
                        )
                    )
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
                        )
                    )

            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=health_events, version=1))

    def clear_all_xid_errors(self):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            log.info("received callback for for clearing of xid errors")
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            checkName = "XidError"
            message = "NoXidErrorDetected"
            entitiesImpacted = []
            errorCode = ""
            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=checkName,
                generatedTimestamp=timestamp,
                isFatal=False,
                isHealthy=True,
                errorCode=errorCode,
                entitiesImpacted=entitiesImpacted,
                message=message,
                recommendedAction=platformconnector_pb2.UNKNOWN,
            )
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))

    def xid_event_occurred(self, gpu_id: str, error_num: int):
        with metrics.xid_events_publish_time_to_grpc_channel.labels("xid_events_publish_time_to_grpc_channel").time():
            timestamp = Timestamp()
            timestamp.GetCurrentTime()
            checkName = "XidError"
            message = "XID error occured"
            entitiesImpacted = [f"{gpu_id}"]
            errorCode = str(error_num)
            health_event = platformconnector_pb2.HealthEvent(
                version=self._version,
                agent=self._agent,
                componentClass=self._component_class,
                checkName=checkName,
                generatedTimestamp=timestamp,
                isFatal=True,
                isHealthy=False,
                errorCode=errorCode,
                entitiesImpacted=entitiesImpacted,
                message=message,
                recommendedAction=platformconnector_pb2.UNKNOWN,
            )
            with grpc.insecure_channel(f"unix://{self._socket_path}") as chan:
                stub = platformconnector_pb2_grpc.PlatformConnectorStub(chan)
                stub.HealthEventOccuredV1(platformconnector_pb2.HealthEvents(events=[health_event], version=1))

    def field_change_event_occurred(self, fields_changes: dict[str, list[dcgmtypes.FieldDetails]]):
        log.debug(f"received callback for field change event {fields_changes}")
