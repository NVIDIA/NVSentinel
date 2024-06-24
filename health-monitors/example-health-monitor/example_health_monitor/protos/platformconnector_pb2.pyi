from google.protobuf import timestamp_pb2 as _timestamp_pb2
from google.protobuf import empty_pb2 as _empty_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from typing import (
    ClassVar as _ClassVar,
    Iterable as _Iterable,
    Mapping as _Mapping,
    Optional as _Optional,
    Union as _Union,
)

DESCRIPTOR: _descriptor.FileDescriptor

class RecommenedAction(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    NODE_REBOOT: _ClassVar[RecommenedAction]
    COMPONENT_RESET: _ClassVar[RecommenedAction]
    COMPONENT_REPLACEMENT: _ClassVar[RecommenedAction]
    APPLICATION_RESTART: _ClassVar[RecommenedAction]
    UNKNOWN: _ClassVar[RecommenedAction]

NODE_REBOOT: RecommenedAction
COMPONENT_RESET: RecommenedAction
COMPONENT_REPLACEMENT: RecommenedAction
APPLICATION_RESTART: RecommenedAction
UNKNOWN: RecommenedAction

class HealthEvents(_message.Message):
    __slots__ = ("version", "events")
    VERSION_FIELD_NUMBER: _ClassVar[int]
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    version: int
    events: _containers.RepeatedCompositeFieldContainer[HealthEvent]
    def __init__(
        self,
        version: _Optional[int] = ...,
        events: _Optional[_Iterable[_Union[HealthEvent, _Mapping]]] = ...,
    ) -> None: ...

class HealthEvent(_message.Message):
    __slots__ = (
        "version",
        "agent",
        "componentClass",
        "checkName",
        "isFatal",
        "isHealthy",
        "actionRequired",
        "message",
        "recommendedAction",
        "errorCode",
        "entitiesImpacted",
        "metadata",
        "generatedTimestamp",
    )

    class MetadataEntry(_message.Message):
        __slots__ = ("key", "value")
        KEY_FIELD_NUMBER: _ClassVar[int]
        VALUE_FIELD_NUMBER: _ClassVar[int]
        key: str
        value: str
        def __init__(
            self, key: _Optional[str] = ..., value: _Optional[str] = ...
        ) -> None: ...

    VERSION_FIELD_NUMBER: _ClassVar[int]
    AGENT_FIELD_NUMBER: _ClassVar[int]
    COMPONENTCLASS_FIELD_NUMBER: _ClassVar[int]
    CHECKNAME_FIELD_NUMBER: _ClassVar[int]
    ISFATAL_FIELD_NUMBER: _ClassVar[int]
    ISHEALTHY_FIELD_NUMBER: _ClassVar[int]
    ACTIONREQUIRED_FIELD_NUMBER: _ClassVar[int]
    MESSAGE_FIELD_NUMBER: _ClassVar[int]
    RECOMMENDEDACTION_FIELD_NUMBER: _ClassVar[int]
    ERRORCODE_FIELD_NUMBER: _ClassVar[int]
    ENTITIESIMPACTED_FIELD_NUMBER: _ClassVar[int]
    METADATA_FIELD_NUMBER: _ClassVar[int]
    GENERATEDTIMESTAMP_FIELD_NUMBER: _ClassVar[int]
    version: int
    agent: str
    componentClass: str
    checkName: str
    isFatal: bool
    isHealthy: bool
    actionRequired: bool
    message: str
    recommendedAction: RecommenedAction
    errorCode: str
    entitiesImpacted: _containers.RepeatedScalarFieldContainer[str]
    metadata: _containers.ScalarMap[str, str]
    generatedTimestamp: _timestamp_pb2.Timestamp
    def __init__(
        self,
        version: _Optional[int] = ...,
        agent: _Optional[str] = ...,
        componentClass: _Optional[str] = ...,
        checkName: _Optional[str] = ...,
        isFatal: bool = ...,
        isHealthy: bool = ...,
        actionRequired: bool = ...,
        message: _Optional[str] = ...,
        recommendedAction: _Optional[_Union[RecommenedAction, str]] = ...,
        errorCode: _Optional[str] = ...,
        entitiesImpacted: _Optional[_Iterable[str]] = ...,
        metadata: _Optional[_Mapping[str, str]] = ...,
        generatedTimestamp: _Optional[_Union[_timestamp_pb2.Timestamp, _Mapping]] = ...,
    ) -> None: ...
