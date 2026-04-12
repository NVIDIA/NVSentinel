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

"""Prometheus metric definitions for gRPC event publishing."""

from prometheus_client import Counter, Histogram

health_events_publish_time_to_grpc_channel = Histogram(
    "fabric_monitor_health_events_publish_time_to_grpc_channel",
    "Amount of time spent publishing health events on the gRPC channel",
    labelnames=["operation_name"],
)

events_sent_success = Counter(
    "fabric_monitor_events_sent_success",
    "Total number of successful health event sends to platform-connector UDS",
)

events_sent_error = Counter(
    "fabric_monitor_events_sent_error",
    "Total number of failed health event sends to platform-connector UDS",
)
