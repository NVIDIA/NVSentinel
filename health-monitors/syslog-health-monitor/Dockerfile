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

FROM golang:1.23-bullseye AS builder

WORKDIR /go/src/syslog-health-monitor

COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY . .

ENV CGO_ENABLED=1

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && apt-get install -y \
    libsystemd-dev \
    pkg-config \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

RUN CGO_ENABLED=1 go build -tags "systemd" -ldflags="-s -w" -o syslog-health-monitor ./cmd/main.go

FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y \
    systemd-sysv \
    --no-install-recommends \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/syslog-health-monitor/syslog-health-monitor /app/syslog-health-monitor

RUN echo "deb http://deb.debian.org/debian bookworm main" \
        > /etc/apt/sources.list.d/bookworm.list && \
    apt-get update && \
    apt-get install -y --no-install-recommends -t bookworm \
        libsystemd0 liblz4-1 libzstd1 && \
    rm /etc/apt/sources.list.d/bookworm.list

ENTRYPOINT ["/app/syslog-health-monitor"] 
