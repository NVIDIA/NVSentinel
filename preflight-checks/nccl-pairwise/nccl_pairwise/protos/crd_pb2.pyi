from google.protobuf import descriptor_pb2 as _descriptor_pb2
from google.protobuf.internal import containers as _containers
from google.protobuf.internal import enum_type_wrapper as _enum_type_wrapper
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class ColumnType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CT_NONE: _ClassVar[ColumnType]
    CT_INTEGER: _ClassVar[ColumnType]
    CT_NUMBER: _ClassVar[ColumnType]
    CT_STRING: _ClassVar[ColumnType]
    CT_BOOLEAN: _ClassVar[ColumnType]
    CT_DATE: _ClassVar[ColumnType]

class ColumnFormat(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    CF_NONE: _ClassVar[ColumnFormat]
    CF_INT32: _ClassVar[ColumnFormat]
    CF_INT64: _ClassVar[ColumnFormat]
    CF_FLOAT: _ClassVar[ColumnFormat]
    CF_DOUBLE: _ClassVar[ColumnFormat]
    CF_BYTE: _ClassVar[ColumnFormat]
    CF_DATE: _ClassVar[ColumnFormat]
    CF_DATETIME: _ClassVar[ColumnFormat]
    CF_PASSWORD: _ClassVar[ColumnFormat]

class ScopeType(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    ST_NAMESPACED: _ClassVar[ScopeType]
    ST_CLUSTER: _ClassVar[ScopeType]

class SchemaSide(int, metaclass=_enum_type_wrapper.EnumTypeWrapper):
    __slots__ = ()
    SS_COMMON: _ClassVar[SchemaSide]
    SS_CLIENT: _ClassVar[SchemaSide]
CT_NONE: ColumnType
CT_INTEGER: ColumnType
CT_NUMBER: ColumnType
CT_STRING: ColumnType
CT_BOOLEAN: ColumnType
CT_DATE: ColumnType
CF_NONE: ColumnFormat
CF_INT32: ColumnFormat
CF_INT64: ColumnFormat
CF_FLOAT: ColumnFormat
CF_DOUBLE: ColumnFormat
CF_BYTE: ColumnFormat
CF_DATE: ColumnFormat
CF_DATETIME: ColumnFormat
CF_PASSWORD: ColumnFormat
ST_NAMESPACED: ScopeType
ST_CLUSTER: ScopeType
SS_COMMON: SchemaSide
SS_CLIENT: SchemaSide
K8S_CRD_FIELD_NUMBER: _ClassVar[int]
k8s_crd: _descriptor.FieldDescriptor
K8S_PATCH_FIELD_NUMBER: _ClassVar[int]
k8s_patch: _descriptor.FieldDescriptor
SCHEMA_FIELD_NUMBER: _ClassVar[int]
schema: _descriptor.FieldDescriptor

class PrinterColumn(_message.Message):
    __slots__ = ("name", "type", "format", "description", "json_path", "priority")
    NAME_FIELD_NUMBER: _ClassVar[int]
    TYPE_FIELD_NUMBER: _ClassVar[int]
    FORMAT_FIELD_NUMBER: _ClassVar[int]
    DESCRIPTION_FIELD_NUMBER: _ClassVar[int]
    JSON_PATH_FIELD_NUMBER: _ClassVar[int]
    PRIORITY_FIELD_NUMBER: _ClassVar[int]
    name: str
    type: ColumnType
    format: ColumnFormat
    description: str
    json_path: str
    priority: int
    def __init__(self, name: _Optional[str] = ..., type: _Optional[_Union[ColumnType, str]] = ..., format: _Optional[_Union[ColumnFormat, str]] = ..., description: _Optional[str] = ..., json_path: _Optional[str] = ..., priority: _Optional[int] = ...) -> None: ...

class K8sCRD(_message.Message):
    __slots__ = ("api_group", "kind", "singular", "plural", "short_names", "categories", "additional_columns", "scope", "field_patch_strategies")
    API_GROUP_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    SINGULAR_FIELD_NUMBER: _ClassVar[int]
    PLURAL_FIELD_NUMBER: _ClassVar[int]
    SHORT_NAMES_FIELD_NUMBER: _ClassVar[int]
    CATEGORIES_FIELD_NUMBER: _ClassVar[int]
    ADDITIONAL_COLUMNS_FIELD_NUMBER: _ClassVar[int]
    SCOPE_FIELD_NUMBER: _ClassVar[int]
    FIELD_PATCH_STRATEGIES_FIELD_NUMBER: _ClassVar[int]
    api_group: str
    kind: str
    singular: str
    plural: str
    short_names: _containers.RepeatedScalarFieldContainer[str]
    categories: _containers.RepeatedScalarFieldContainer[str]
    additional_columns: _containers.RepeatedCompositeFieldContainer[PrinterColumn]
    scope: ScopeType
    field_patch_strategies: _containers.RepeatedCompositeFieldContainer[K8sPatchSelector]
    def __init__(self, api_group: _Optional[str] = ..., kind: _Optional[str] = ..., singular: _Optional[str] = ..., plural: _Optional[str] = ..., short_names: _Optional[_Iterable[str]] = ..., categories: _Optional[_Iterable[str]] = ..., additional_columns: _Optional[_Iterable[_Union[PrinterColumn, _Mapping]]] = ..., scope: _Optional[_Union[ScopeType, str]] = ..., field_patch_strategies: _Optional[_Iterable[_Union[K8sPatchSelector, _Mapping]]] = ...) -> None: ...

class K8sPatch(_message.Message):
    __slots__ = ("merge_key", "merge_strategy")
    MERGE_KEY_FIELD_NUMBER: _ClassVar[int]
    MERGE_STRATEGY_FIELD_NUMBER: _ClassVar[int]
    merge_key: str
    merge_strategy: str
    def __init__(self, merge_key: _Optional[str] = ..., merge_strategy: _Optional[str] = ...) -> None: ...

class K8sPatchSelector(_message.Message):
    __slots__ = ("protobuf_type", "field_path", "k8s_patch")
    PROTOBUF_TYPE_FIELD_NUMBER: _ClassVar[int]
    FIELD_PATH_FIELD_NUMBER: _ClassVar[int]
    K8S_PATCH_FIELD_NUMBER: _ClassVar[int]
    protobuf_type: str
    field_path: str
    k8s_patch: K8sPatch
    def __init__(self, protobuf_type: _Optional[str] = ..., field_path: _Optional[str] = ..., k8s_patch: _Optional[_Union[K8sPatch, _Mapping]] = ...) -> None: ...
