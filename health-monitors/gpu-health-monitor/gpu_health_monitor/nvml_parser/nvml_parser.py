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
