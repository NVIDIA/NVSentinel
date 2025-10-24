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
COPY node-drainer-module/go.mod node-drainer-module/go.sum ./node-drainer-module/

WORKDIR /go/src/node-drainer-module

# download dependencies with authentication
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the source code after go mod download
COPY node-drainer-module/ .
COPY data-models/ ../data-models/
COPY platform-connectors/ ../platform-connectors/
COPY statemanager/ ../statemanager/
COPY store-client-sdk/ ../store-client-sdk/

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -o nodedrainer main.go

FROM ${IMG}:${TAG}

WORKDIR /app

COPY --from=builder /go/src/node-drainer-module/nodedrainer .

ENTRYPOINT ["/app/nodedrainer"]
