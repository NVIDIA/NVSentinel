import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import empty_pb2 as _empty_pb2
from google.protobuf import wrappers_pb2 as _wrappers_pb2
from github.com.yandex.protoc_gen_crd.library.go.k8s.protoc_gen_crd.proto import crd_pb2 as _crd_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ProcessingStrategy(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    UNSPECIFIED: _ClassVar[ProcessingStrategy]
    EXECUTE_REMEDIATION: _ClassVar[ProcessingStrategy]
    STORE_ONLY: _ClassVar[ProcessingStrategy]

class RecommendedAction(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    NONE: _ClassVar[RecommendedAction]
    COMPONENT_RESET: _ClassVar[RecommendedAction]
    CONTACT_SUPPORT: _ClassVar[RecommendedAction]
    RUN_FIELDDIAG: _ClassVar[RecommendedAction]
    RESTART_VM: _ClassVar[RecommendedAction]
    RESTART_BM: _ClassVar[RecommendedAction]
    REPLACE_VM: _ClassVar[RecommendedAction]
    RUN_DCGMEUD: _ClassVar[RecommendedAction]
    CUSTOM: _ClassVar[RecommendedAction]
    UNKNOWN: _ClassVar[RecommendedAction]
UNSPECIFIED: ProcessingStrategy
EXECUTE_REMEDIATION: ProcessingStrategy
STORE_ONLY: ProcessingStrategy
NONE: RecommendedAction
COMPONENT_RESET: RecommendedAction
CONTACT_SUPPORT: RecommendedAction
RUN_FIELDDIAG: RecommendedAction
RESTART_VM: RecommendedAction
RESTART_BM: RecommendedAction
REPLACE_VM: RecommendedAction
RUN_DCGMEUD: RecommendedAction
CUSTOM: RecommendedAction
UNKNOWN: RecommendedAction

class OperationStatus(_message.Message):
    __slots__ = ("status", "message")
    STATUS_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    status: str
    message: str
    def __init__(self, status: _Optional[str] = ..., message: _Optional[str] = ...) -> None: ...

class HealthEventStatus(_message.Message):
    __slots__ = ("nodeQuarantined", "quarantineFinishTimestamp", "userPodsEvictionStatus", "drainFinishTimestamp", "faultRemediated", "lastRemediationTimestamp", "spanIds")
    class SpanIdsEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(self, key: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...
    NODEQUARANTINED_FIELD_NUMBER: _ClassVar[int]
    QUARANTINEFINISHTIMESTAMP_FIELD_NUMBER: _ClassVar[int]
    USERPODSEVICTIONSTATUS_FIELD_NUMBER: _ClassVar[int]
    DRAINFINISHTIMESTAMP_FIELD_NUMBER: _ClassVar[int]
    FAULTREMEDIATED_FIELD_NUMBER: _ClassVar[int]
    LASTREMEDIATIONTIMESTAMP_FIELD_NUMBER: _ClassVar[int]
    SPANIDS_FIELD_NUMBER: _ClassVar[int]
    nodeQuarantined: str
    quarantineFinishTimestamp: _timestamp_pb2.Timestamp
    userPodsEvictionStatus: OperationStatus
    drainFinishTimestamp: _timestamp_pb2.Timestamp
    faultRemediated: _wrappers_pb2.BoolValue
    lastRemediationTimestamp: _timestamp_pb2.Timestamp
    spanIds: _containers.ScalarMap[str, str]
    def __init__(self, nodeQuarantined: _Optional[str] = ..., quarantineFinishTimestamp: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., userPodsEvictionStatus: _Optional[_Union[OperationStatus, _Mapping]] = ..., drainFinishTimestamp: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., faultRemediated: _Optional[_Union[_wrappers_pb2.BoolValue, _Mapping]] = ..., lastRemediationTimestamp: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., spanIds: _Optional[_Mapping[str, str]] = ...) -> None: ...

class HealthEventWithStatus(_message.Message):
    __slots__ = ("createdAt", "healthEvent", "healthEventStatus")
    CREATEDAT_FIELD_NUMBER: _ClassVar[int]
    HEALTHEVENT_FIELD_NUMBER: _ClassVar[int]
    HEALTHEVENTSTATUS_FIELD_NUMBER: _ClassVar[int]
    createdAt: _timestamp_pb2.Timestamp
    healthEvent: HealthEvent
    healthEventStatus: HealthEventStatus
    def __init__(self, createdAt: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., healthEvent: _Optional[_Union[HealthEvent, _Mapping]] = ..., healthEventStatus: _Optional[_Union[HealthEventStatus, _Mapping]] = ...) -> None: ...

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
    __slots__ = ("version", "agent", "componentClass", "checkName", "isFatal", "isHealthy", "message", "recommendedAction", "errorCode", "entitiesImpacted", "metadata", "generatedTimestamp", "nodeName", "quarantineOverrides", "drainOverrides", "processingStrategy", "id", "customRecommendedAction")
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
    QUARANTINEOVERRIDES_FIELD_NUMBER: _ClassVar[int]
    DRAINOVERRIDES_FIELD_NUMBER: _ClassVar[int]
    PROCESSINGSTRATEGY_FIELD_NUMBER: _ClassVar[int]
    ID_FIELD_NUMBER: _ClassVar[int]
    CUSTOMRECOMMENDEDACTION_FIELD_NUMBER: _ClassVar[int]
    version: int
    agent: str
    componentClass: str
    checkName: str
    isFatal: bool
    isHealthy: bool
    message: str
    recommendedAction: RecommendedAction
    errorCode: _containers.RepeatedScalarFieldContainer[str]
    entitiesImpacted: _containers.RepeatedCompositeFieldContainer[Entity]
    metadata: _containers.ScalarMap[str, str]
    generatedTimestamp: _timestamp_pb2.Timestamp
    nodeName: str
    quarantineOverrides: BehaviourOverrides
    drainOverrides: BehaviourOverrides
    processingStrategy: ProcessingStrategy
    id: str
    customRecommendedAction: str
    def __init__(self, version: _Optional[int] = ..., agent: _Optional[str] = ..., componentClass: _Optional[str] = ..., checkName: _Optional[str] = ..., isFatal: bool = ..., isHealthy: bool = ..., message: _Optional[str] = ..., recommendedAction: _Optional[_Union[RecommendedAction, str]] = ..., errorCode: _Optional[_Iterable[str]] = ..., entitiesImpacted: _Optional[_Iterable[_Union[Entity, _Mapping]]] = ..., metadata: _Optional[_Mapping[str, str]] = ..., generatedTimestamp: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ..., nodeName: _Optional[str] = ..., quarantineOverrides: _Optional[_Union[BehaviourOverrides, _Mapping]] = ..., drainOverrides: _Optional[_Union[BehaviourOverrides, _Mapping]] = ..., processingStrategy: _Optional[_Union[ProcessingStrategy, str]] = ..., id: _Optional[str] = ..., customRecommendedAction: _Optional[str] = ...) -> None: ...

class BehaviourOverrides(_message.Message):
    __slots__ = ("force", "skip")
    FORCE_FIELD_NUMBER: _ClassVar[int]
    SKIP_FIELD_NUMBER: _ClassVar[int]
    force: bool
    skip: bool
    def __init__(self, force: bool = ..., skip: bool = ...) -> None: ...

class HealthEventResource(_message.Message):
    __slots__ = ("spec", "status")
    SPEC_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    spec: HealthEvent
    status: HealthEventStatus
    def __init__(self, spec: _Optional[_Union[HealthEvent, _Mapping]] = ..., status: _Optional[_Union[HealthEventStatus, _Mapping]] = ...) -> None: ...
