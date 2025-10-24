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

ARG CUDA_VERSION="12.1.1-base-ubuntu22.04"
ARG BUILD_TAGS="systemd"

FROM public.ecr.aws/docker/library/golang:1.24-bullseye AS builder

WORKDIR /go/src/nvsentinel

# Copy the go.mod files for dependencies first
COPY health-monitors/syslog-health-monitor/go.mod health-monitors/syslog-health-monitor/go.sum health-monitors/syslog-health-monitor/
COPY data-models/go.mod data-models/go.sum ./data-models/

# Download dependencies
RUN --mount=type=cache,target=/go/pkg/mod \
    cd health-monitors/syslog-health-monitor && go mod download

# Copy the source code
COPY health-monitors/syslog-health-monitor/ health-monitors/syslog-health-monitor/
COPY data-models/ data-models/

ENV CGO_ENABLED=1

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y \
    libsystemd-dev \
    pkg-config \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

RUN cd health-monitors/syslog-health-monitor && \
    CGO_ENABLED=1 go build -tags "${BUILD_TAGS}" -ldflags="-s -w" -o syslog-health-monitor main.go

FROM nvcr.io/nvidia/cuda:${CUDA_VERSION} AS runtime

RUN apt-get update && apt-get install -y \
    systemd-sysv \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvsentinel/health-monitors/syslog-health-monitor/syslog-health-monitor /app/syslog-health-monitor

RUN apt-get update && \
    apt-get install -y libsystemd0 liblz4-1 libzstd1

ENTRYPOINT ["/app/syslog-health-monitor"] 
