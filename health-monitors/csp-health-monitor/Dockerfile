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

# syntax=docker/dockerfile:1.6

ARG TAG=v3.1.13
ARG IMG=nvcr.io/nvidia/distroless/go

FROM public.ecr.aws/docker/library/golang:1.24-bullseye AS builder

# TODO: remove when pushing to GitHub

WORKDIR /go/src/nvsentinel

# Copy the go.mod files for dependencies first
COPY data-models/go.mod data-models/go.sum data-models/
COPY platform-connectors/go.mod platform-connectors/go.sum platform-connectors/
COPY store-client-sdk/go.mod store-client-sdk/go.sum store-client-sdk/
COPY health-monitors/csp-health-monitor/go.mod health-monitors/csp-health-monitor/go.sum health-monitors/csp-health-monitor/

# Download dependencies
RUN --mount=type=cache,target=/go/pkg/mod \
    cd health-monitors/csp-health-monitor && go mod download

# Copy the source code
COPY data-models/ data-models/
COPY platform-connectors/ platform-connectors/
COPY store-client-sdk/ store-client-sdk/
COPY health-monitors/csp-health-monitor/ health-monitors/csp-health-monitor/

RUN --mount=type=cache,target=/go/pkg/mod \
    cd health-monitors/csp-health-monitor && \
    go build -trimpath -o csp-health-monitor ./cmd/csp-health-monitor/main.go

RUN --mount=type=cache,target=/go/pkg/mod \
    cd health-monitors/csp-health-monitor && \
    go build -trimpath -o maintenance-notifier ./cmd/maintenance-notifier/main.go

FROM ${IMG}:${TAG}

WORKDIR /app

COPY --from=builder /go/src/nvsentinel/health-monitors/csp-health-monitor/csp-health-monitor ./
COPY --from=builder /go/src/nvsentinel/health-monitors/csp-health-monitor/maintenance-notifier ./

ENTRYPOINT ["/app/csp-health-monitor"]
