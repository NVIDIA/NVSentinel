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

FROM nvcr.io/nv-ngc-devops/golang:1.23-1.0 AS builder

ENV CGO_ENABLED=0
ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/nvswitch-health-monitor

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o nvswitch-health-monitor cmd/nvswitch_health_monitor.go

# build the clear_fatal_errors utility for testing
RUN go build -o clear_fatal_errors clear-nvswitch-fatal-errors/clear_fatal_errors.go


FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y pciutils curl

RUN update-pciids

RUN apt-get install -y --no-install-recommends \
    e2fsprogs=1.46.2-2+deb11u1 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

# binary is used for testing
COPY --from=builder /go/src/nvswitch-health-monitor/clear_fatal_errors /usr/local/bin/clear_fatal_errors

ENTRYPOINT ["/app/nvswitch-health-monitor"]