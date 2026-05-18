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

from prometheus_client import Histogram, Counter, Gauge

dcgm_health_events_publish_time_to_grpc_channel = Histogram(
    "dcgm_health_events_publish_time_to_grpc_channel",
    "Amount of time spent in publishing dcgm health events on the grpc channel",
    labelnames=["operation_name"],
)

health_events_insertion_to_uds_succeed = Counter(
    "health_events_insertion_to_uds_succeed", "Total number of successful insertion of health events to UDS"
)

health_events_insertion_to_uds_error = Counter(
    "health_events_insertion_to_uds_error", "Total number of failed insertions of health events to UDS"
)

# Operator-visible signal that platform-connector was unavailable on this node,
# distinct from a true gRPC RPC error.
health_events_insertion_skipped_pc_unavailable = Counter(
    "health_events_insertion_skipped_pc_unavailable",
    "Total number of health-event sends skipped because the platform-connector Unix socket was missing",
)

dcgm_health_active_events = Gauge(
    "dcgm_health_active_events",
    "Active health events by watch type and GPU",
    labelnames=["event_type", "gpu_id"],
)
