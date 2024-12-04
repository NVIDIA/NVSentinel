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

FROM golang:1.22 AS builder

ENV CGO_ENABLED=0

WORKDIR /go/src/nvswitch-health-monitor

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o nvswitch-health-monitor cmd/nvswitch_health_monitor.go


FROM debian:bullseye-slim

RUN apt-get update && apt-get install -y pciutils

RUN apt-get install -y --no-install-recommends \
    e2fsprogs=1.46.2-2+deb11u1 && \
    apt-get clean && \
    rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvswitch-health-monitor/nvswitch-health-monitor /app/nvswitch-health-monitor

ENTRYPOINT ["/app/nvswitch-health-monitor"]