import datetime

from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import empty_pb2 as _empty_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class RecommenedAction(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    NONE: _ClassVar[RecommenedAction]
    COMPONENT_RESET: _ClassVar[RecommenedAction]
    CONTACT_SUPPORT: _ClassVar[RecommenedAction]
    RESTART_VM: _ClassVar[RecommenedAction]
    RESTART_BM: _ClassVar[RecommenedAction]
    REPLACE_VM: _ClassVar[RecommenedAction]
    UNKNOWN: _ClassVar[RecommenedAction]

NONE: RecommenedAction
COMPONENT_RESET: RecommenedAction
CONTACT_SUPPORT: RecommenedAction
RESTART_VM: RecommenedAction
RESTART_BM: RecommenedAction
REPLACE_VM: RecommenedAction
UNKNOWN: RecommenedAction

class HealthEvents(_message.Message):
    __slots__ = ("version", "events")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    version: int
    events: _containers.RepeatedCompositeFieldContainer[HealthEvent]
    def __init__(
        self, version: _Optional[int] = ..., events: _Optional[_Iterable[_Union[HealthEvent, _Mapping]]] = ...
    ) -> None: ...

class Entity(_message.Message):
    __slots__ = ("entityType", "entityValue")
    ENTITYTYPE_FIELD_NUMBER: _ClassVar[int]
    ENTITYVALUE_FIELD_NUMBER: _ClassVar[int]
    entityType: str
    entityValue: str
    def __init__(self, entityType: _Optional[str] = ..., entityValue: _Optional[str] = ...) -> None: ...

class HealthEvent(_message.Message):
    __slots__ = (
        "version",
        "agent",
        "componentClass",
        "checkName",
        "isFatal",
        "isHealthy",
        "message",
        "recommendedAction",
        "errorCode",
        "entitiesImpacted",
        "metadata",
        "generatedTimestamp",
        "nodeName",
        "quarantineOverrides",
        "drainOverrides",
    )

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
    quarantineOverrides: BehaviourOverrides
    drainOverrides: BehaviourOverrides
    def __init__(
        self,
        version: _Optional[int] = ...,
        agent: _Optional[str] = ...,
        componentClass: _Optional[str] = ...,
        checkName: _Optional[str] = ...,
        isFatal: bool = ...,
        isHealthy: bool = ...,
        message: _Optional[str] = ...,
        recommendedAction: _Optional[_Union[RecommenedAction, str]] = ...,
        errorCode: _Optional[_Iterable[str]] = ...,
        entitiesImpacted: _Optional[_Iterable[_Union[Entity, _Mapping]]] = ...,
        metadata: _Optional[_Mapping[str, str]] = ...,
        generatedTimestamp: _Optional[_Union[datetime.datetime, _timestamp_pb2.Timestamp, _Mapping]] = ...,
        nodeName: _Optional[str] = ...,
        quarantineOverrides: _Optional[_Union[BehaviourOverrides, _Mapping]] = ...,
        drainOverrides: _Optional[_Union[BehaviourOverrides, _Mapping]] = ...,
    ) -> None: ...

class BehaviourOverrides(_message.Message):
    __slots__ = ("force", "skip")
    FORCE_FIELD_NUMBER: _ClassVar[int]
    SKIP_FIELD_NUMBER: _ClassVar[int]
    force: bool
    skip: bool
    def __init__(self, force: bool = ..., skip: bool = ...) -> None: ...
