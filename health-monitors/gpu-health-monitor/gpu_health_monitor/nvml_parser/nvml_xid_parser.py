# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

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
