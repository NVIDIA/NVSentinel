import abc, dataclasses

# TODO jira NGCC-19041
# The NvmlXidParserInterface will be implemented by the NVML team parser in order to send the recommendation action
# to platform connector for a bunch of valid xid errors. In accordance with that,  the interface needs to be modified.


class NvmlXidParser(abc.ABC):
    @abc.abstractmethod
    def process_xid_errors_on_gpu(self, xid_error: int, gpu_id: str):
        pass

    @abc.abstractmethod
    def register_xid_processing_done_callback(self, callback):
        pass
