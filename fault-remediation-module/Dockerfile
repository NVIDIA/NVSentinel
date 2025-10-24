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
ENV CGO_ENABLED=0

WORKDIR /go/src

# Copy go.mod and go.sum files for all dependencies first
COPY data-models/go.mod data-models/go.sum ./data-models/
COPY platform-connectors/go.mod platform-connectors/go.sum ./platform-connectors/
COPY statemanager/go.mod statemanager/go.sum ./statemanager/
COPY store-client-sdk/go.mod store-client-sdk/go.sum ./store-client-sdk/
COPY fault-remediation-module/go.mod fault-remediation-module/go.sum ./fault-remediation-module/

WORKDIR /go/src/fault-remediation-module

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the source code after go mod download
COPY fault-remediation-module/ .
COPY platform-connectors/ ../platform-connectors/
COPY statemanager/ ../statemanager/
COPY store-client-sdk/ ../store-client-sdk/
COPY data-models/ ../data-models/

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -o faultremediation main.go

FROM ${IMG}:${TAG}

WORKDIR /app

COPY --from=builder /go/src/fault-remediation-module/faultremediation .

ENTRYPOINT ["/app/faultremediation"]
