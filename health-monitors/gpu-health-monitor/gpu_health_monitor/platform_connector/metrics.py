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

from prometheus_client import Histogram, Counter, Gauge

dcgm_health_events_publish_time_to_grpc_channel = Histogram(
    "dcgm_health_events_publish_time_to_grpc_channel",
    "Amount of time spent in publishing dcgm health events on the grpc channel",
    labelnames=["operation_name"],
)
xid_events_publish_time_to_grpc_channel = Histogram(
    "xid_events_publish_time_to_grpc_channel",
    "Amount of time spent in publishing xid events on the grpc channel",
    labelnames=["operation_name"],
)
overall_reconcile_loop_time = Histogram(
    "xid_errors_batch_processing_reconcile_time",
    "Amount of time spent running a single reconcile loop for batch processing of xid errors",
)
health_events_insertion_to_uds_succeed = Counter(
    "health_events_insertion_to_uds_succeed", "Total number of successful insertion of health events to UDS"
)

health_events_insertion_to_uds_error = Gauge(
    "health_events_insertion_to_uds_error", "Error in insertions of health events to UDS"
)

gpu_health_monitor_xid_errors = Gauge(
    "gpu_health_monitor_xid_errors",
    "XID observed on the GPU by node_name,serial_number",
    labelnames=["node_name", "serial_number"],  # Labels
)

dcgm_health_active_non_fatal_health_events = Gauge(
    "dcgm_health_active_non_fatal_health_events",
    "Total number of active non-fatal health events at any given time",
    labelnames=["event_type", "gpu_id"],
)

dcgm_health_active_fatal_health_events = Gauge(
    "dcgm_health_active_fatal_health_events",
    "Total number of active fatal health events at any given time",
    labelnames=["event_type", "gpu_id"],
)
