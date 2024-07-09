import abc, dataclasses, enum, dcgm_structs


class HealthStatus(enum.Enum):
    PASS = dcgm_structs.DCGM_HEALTH_RESULT_PASS
    WARN = dcgm_structs.DCGM_HEALTH_RESULT_WARN
    FAIL = dcgm_structs.DCGM_HEALTH_RESULT_FAIL
    UNKNOWN = -1


@dataclasses.dataclass
class ErrorDetails:
    code: str
    message: str


@dataclasses.dataclass
class HealthDetails:
    status: HealthStatus
    entity_failures: dict[int, ErrorDetails]


@dataclasses.dataclass(order=True)
class FieldDetails:
    field_id: str
    value: str


class CallbackInterface(abc.ABC):
    @abc.abstractmethod
    def health_event_occurred(self, health_details: dict[str, HealthDetails]):
        pass

    @abc.abstractmethod
    def xid_event_occurred(self, gpu_id: str, error_num: int):
        pass

    @abc.abstractmethod
    def clear_all_xid_errors(self):
        pass

    @abc.abstractmethod
    def field_change_event_occurred(self, fields_changes: dict[str, list[FieldDetails]]):
        pass
