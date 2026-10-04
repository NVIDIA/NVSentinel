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


FROM public.ecr.aws/docker/library/golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS builder

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

FROM nvcr.io/nvidia/distroless/go:v4.1.4@sha256:965c2a14a594d7d467bbe9a6517d958cee22ed035333097bdc216da68fd55ba5

WORKDIR /app

COPY --from=builder /go/src/tilt/simple-health-client/simple-health-client .

ENTRYPOINT ["/app/simple-health-client"]
