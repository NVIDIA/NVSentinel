from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import empty_pb2 as _empty_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import ClassVar as _ClassVar, Iterable as _Iterable, Mapping as _Mapping, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RecommenedAction(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    NONE: _ClassVar[RecommenedAction]
    NODE_REBOOT: _ClassVar[RecommenedAction]
    COMPONENT_RESET: _ClassVar[RecommenedAction]
    COMPONENT_REPLACEMENT: _ClassVar[RecommenedAction]
    APPLICATION_RESTART: _ClassVar[RecommenedAction]
    REPORT_ISSUE: _ClassVar[RecommenedAction]
    RUN_FIELDDIAG: _ClassVar[RecommenedAction]
    WORKFLOW_XID_13_31: _ClassVar[RecommenedAction]
    WORKFLOW_ECC_DBE_SRAM: _ClassVar[RecommenedAction]
    WORKFLOW_NVLINK5_ERR: _ClassVar[RecommenedAction]
    WORKFLOW_XID_45: _ClassVar[RecommenedAction]
    WORKFLOW_XID_48: _ClassVar[RecommenedAction]
    CHECK_MECHANICALS: _ClassVar[RecommenedAction]
    WORKFLOW_NVLINK_ERR: _ClassVar[RecommenedAction]
    UPDATE_SWFW: _ClassVar[RecommenedAction]
    RESTART_VM: _ClassVar[RecommenedAction]
    RESET_FABRIC: _ClassVar[RecommenedAction]
    WORKFLOW_NVLINK_POTENTIALY_FATAL_ERR: _ClassVar[RecommenedAction]
    CHECK_FM_CONFIG: _ClassVar[RecommenedAction]
    CHECK_THERMALS: _ClassVar[RecommenedAction]
    RESET_GPU: _ClassVar[RecommenedAction]
    CHECK_LINK_MECHANICAL_CONNECTIONS: _ClassVar[RecommenedAction]
    INVESTIGATE_LINK_SI: _ClassVar[RecommenedAction]
    UNKNOWN: _ClassVar[RecommenedAction]
NONE: RecommenedAction
NODE_REBOOT: RecommenedAction
COMPONENT_RESET: RecommenedAction
COMPONENT_REPLACEMENT: RecommenedAction
APPLICATION_RESTART: RecommenedAction
REPORT_ISSUE: RecommenedAction
RUN_FIELDDIAG: RecommenedAction
WORKFLOW_XID_13_31: RecommenedAction
WORKFLOW_ECC_DBE_SRAM: RecommenedAction
WORKFLOW_NVLINK5_ERR: RecommenedAction
WORKFLOW_XID_45: RecommenedAction
WORKFLOW_XID_48: RecommenedAction
CHECK_MECHANICALS: RecommenedAction
WORKFLOW_NVLINK_ERR: RecommenedAction
UPDATE_SWFW: RecommenedAction
RESTART_VM: RecommenedAction
RESET_FABRIC: RecommenedAction
WORKFLOW_NVLINK_POTENTIALY_FATAL_ERR: RecommenedAction
CHECK_FM_CONFIG: RecommenedAction
CHECK_THERMALS: RecommenedAction
RESET_GPU: RecommenedAction
CHECK_LINK_MECHANICAL_CONNECTIONS: RecommenedAction
INVESTIGATE_LINK_SI: RecommenedAction
UNKNOWN: RecommenedAction

class HealthEvents(_message.Message):
    __slots__ = ("version", "events")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    version: int
    events: _containers.RepeatedCompositeFieldContainer[HealthEvent]
    def __init__(self, version: _Optional[int] = ..., events: _Optional[_Iterable[_Union[HealthEvent, _Mapping]]] = ...) -> None: ...

class Entity(_message.Message):
    __slots__ = ("entityType", "entityValue")
    ENTITYTYPE_FIELD_NUMBER: _ClassVar[int]
    ENTITYVALUE_FIELD_NUMBER: _ClassVar[int]
    entityType: str
    entityValue: str
    def __init__(self, entityType: _Optional[str] = ..., entityValue: _Optional[str] = ...) -> None: ...

class HealthEvent(_message.Message):
    __slots__ = ("version", "agent", "componentClass", "checkName", "isFatal", "isHealthy", "message", "recommendedAction", "errorCode", "entitiesImpacted", "metadata", "generatedTimestamp", "nodeName")
    class MetadataEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    VERSION_FIELD_NUMBER: _ClassVar[int]
    AGENT_FIELD_NUMBER: _ClassVar[int]
    COMPONENTCLASS_FIELD_NUMBER: _ClassVar[int]
    CHECKNAME_FIELD_NUMBER: _ClassVar[int]
    ISFATAL_FIELD_NUMBER: _ClassVar[int]
    ISHEALTHY_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    RECOMMENDEDACTION_FIELD_NUMBER: _ClassVar[int]
    ERRORCODE_FIELD_NUMBER: _ClassVar[int]
    ENTITIESIMPACTED_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    GENERATEDTIMESTAMP_FIELD_NUMBER: _ClassVar[int]
    NODENAME_FIELD_NUMBER: _ClassVar[int]
    version: int
    agent: str
    componentClass: str
    checkName: str
    isFatal: bool
    isHealthy: bool
    message: str
    recommendedAction: RecommenedAction
    errorCode: _containers.RepeatedScalarFieldContainer[str]
    entitiesImpacted: _containers.RepeatedCompositeFieldContainer[Entity]
    metadata: _containers.ScalarMap[str, str]
    generatedTimestamp: _timestamp_pb2.Timestamp
    nodeName: str
    def __init__(self, version: _Optional[int] = ..., agent: _Optional[str] = ..., componentClass: _Optional[str] = ..., checkName: _Optional[str] = ..., isFatal: bool = ..., isHealthy: bool = ..., message: _Optional[str] = ..., recommendedAction: _Optional[_Union[RecommenedAction, str]] = ..., errorCode: _Optional[_Iterable[str]] = ..., entitiesImpacted: _Optional[_Iterable[_Union[Entity, _Mapping]]] = ..., metadata: _Optional[_Mapping[str, str]] = ..., generatedTimestamp: _Optional[_Union[_timestamp_pb2.Timestamp, _Mapping]] = ..., nodeName: _Optional[str] = ...) -> None: ...
