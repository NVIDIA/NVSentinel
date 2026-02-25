# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

from prometheus_client import Histogram, Gauge, Counter

dcgm_api_latency = Histogram(
    "dcgm_api_latency", "Amount of time spent calling dcgm APIs", labelnames=["operation_name"]
)
overall_reconcile_loop_time = Histogram("dcgm_reconcile_time", "Amount of time spent running a single reconcile loop")
num_health_watches = Gauge("number_of_health_watches", "Number of health watches available")
num_fields = Gauge("number_of_fields", "Number of available fields to monitor")
callback_failures = Counter(
    "callback_failures",
    "Number of times a callback function has thrown an exception",
    labelnames=["class_name", "func_name"],
)
callback_success = Counter(
    "callback_success",
    "Number of times a callback function has successfully completed",
    labelnames=["class_name", "func_name"],
)
dcgm_api_failures = Counter(
    "dcgm_api_failures",
    "Number of times an error has occurred",
    labelnames=["error_name"],
)
dcgm_health_check_unknown_system_skipped = Counter(
    "dcgm_health_check_unknown_system_skipped",
    "Number of DCGM health check incidents skipped due to unrecognized system value",
)
