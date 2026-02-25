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

# Match DCGM_VERSION used in gpu-health-monitor and preflight dcgm-diag Dockerfiles
ARG DCGM_VERSION=4.4.2-1-ubuntu22.04
FROM nvcr.io/nvidia/cloud-native/dcgm:${DCGM_VERSION}

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update || true && \
    apt-get install -y --no-install-recommends \
        ca-certificates && \
    apt-get update && apt list -a datacenter-gpu-manager-4-dev && \
    apt-get install -y --no-install-recommends \
        datacenter-gpu-manager-4-dev=1:4.4.2-1 && \
    rm -rf /var/lib/apt/lists/*

COPY gpu-spec.yaml /gpu-spec.yaml

ENV NVML_INJECTION_MODE=True
ENV NVML_YAML_FILE=/gpu-spec.yaml
