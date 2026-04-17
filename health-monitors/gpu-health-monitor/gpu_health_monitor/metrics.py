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

"""Expose runtime feature toggles as a Prometheus gauge metric
(nvsentinel_feature_flag_enabled) for observability."""

from prometheus_client import Gauge

nvsentinel_feature_flag_enabled = Gauge(
    "nvsentinel_feature_flag_enabled",
    "Reports whether a feature flag is enabled (1) or disabled (0).",
    labelnames=["service", "flag"],
)


def set_flag(flag: str, enabled: bool) -> None:
    nvsentinel_feature_flag_enabled.labels(service="gpu-health-monitor", flag=flag).set(1 if enabled else 0)
