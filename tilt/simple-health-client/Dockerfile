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


FROM public.ecr.aws/docker/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS builder

# TODO: remove when pushing to GitHub
ENV CGO_ENABLED=0

WORKDIR /go/src

# Copy go.mod and go.sum files for all dependencies first
COPY data-models/go.mod data-models/go.sum ./data-models/
# commons is required for the shared platform-connector token client. It in
# turn replaces data-models and store-client with local paths, so both of
# those go.mod files must exist for the module graph to resolve.
COPY commons/go.mod commons/go.sum ./commons/
COPY store-client/go.mod store-client/go.sum ./store-client/
COPY tilt/simple-health-client/go.mod tilt/simple-health-client/go.sum ./tilt/simple-health-client/

WORKDIR /go/src/tilt/simple-health-client

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy the source code after go mod download
COPY tilt/simple-health-client/ .
COPY data-models/ ../../data-models/
COPY commons/ ../../commons/

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -o simple-health-client main.go

FROM nvcr.io/nvidia/distroless/go:v4.1.7@sha256:9ef7b9ac43cbf4c833fd5d98689dc09b06a29e5ec3451796b037aa27928fe532

WORKDIR /app

COPY --from=builder /go/src/tilt/simple-health-client/simple-health-client .

ENTRYPOINT ["/app/simple-health-client"]
