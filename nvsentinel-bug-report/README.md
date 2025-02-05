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

Run the bash scripts which are mentioned in step 1 and 2 and attach to the bug
1. Capture the NVSentinel debug logs by running bash script NVSentinel_debug.sh and zip the directory and attach to the bug
2. Capture the logs by running the must-gather script which is present here https://github.com/NVIDIA/gpu-operator/blob/main/hack/must-gather.sh and attached to the bug