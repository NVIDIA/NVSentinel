import logging

from gpu_health_monitor.nvml_parser.nvml_parser import NvmlXidParser
from gpu_health_monitor.platform_connector.protos import platformconnector_pb2
from collections import defaultdict


# TODO jira NGCC-19041
# Replace this DummyNvmlXidParser with the real implementation of nvml xid parsing logic once it is
# available from NVML team
class DummyNvmlXidParser(NvmlXidParser):
    def __init__(self):
        self.callback_list = []
        self.xid_errors_list = defaultdict(list)

    def process_xid_errors_on_gpu(self, xid_error: int, gpu_id: str):
        if gpu_id not in self.xid_errors_list:
            self.xid_errors_list[gpu_id] = []
        self.xid_errors_list[gpu_id].append(xid_error)
        recommended_action = platformconnector_pb2.RecommenedAction.UNKNOWN
        for callback in self.callback_list:
            callback(self.xid_errors_list[gpu_id], gpu_id, recommended_action)

    def register_xid_processing_done_callback(self, callback):
        self.callback_list.append(callback)
