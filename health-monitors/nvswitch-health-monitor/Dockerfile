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

# syntax=docker/dockerfile:1.6

ARG CUDA_VERSION="12.1.1-base-ubuntu22.04"
FROM golang:1.23-bullseye AS builder

ENV CGO_ENABLED=0

WORKDIR /go/src/nvswitch-health-monitor

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

RUN go build -o nvswitch-health-monitor cmd/nvswitch_health_monitor.go

RUN go build -o driver-watcher ./cmd/driver-watcher

RUN go build -o clear_fatal_errors clear-nvswitch-fatal-errors/clear_fatal_errors.go

FROM nvcr.io/nvidia/cuda:${CUDA_VERSION} AS runtime

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y --no-install-recommends \
    e2fsprogs && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

COPY --from=builder /go/src/nvswitch-health-monitor/driver-watcher /app/driver-watcher
COPY --from=builder /go/src/nvswitch-health-monitor/clear_fatal_errors /usr/local/bin/clear_fatal_errors

ENTRYPOINT ["/app/nvswitch-health-monitor"]
