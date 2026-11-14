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


FROM public.ecr.aws/docker/library/golang:1.27.4-trixie@sha256:b54b2639de8c1bfe538ec5ffc45f29b1932235ac5f6d63c059853646677112d9 AS builder

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

# Retry go mod download, because proxy.golang.org can close the connection.
RUN --mount=type=cache,target=/go/pkg/mod \
    for attempt in 1 2 3; do \
      go mod download && break; \
      [ "$attempt" -lt 3 ] || exit 1; \
      sleep $((attempt * 10)); \
    done

# Copy the source code after go mod download
COPY tilt/simple-health-client/ .
COPY data-models/ ../../data-models/
COPY commons/ ../../commons/

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -o simple-health-client main.go

FROM nvcr.io/nvidia/distroless/go:v4.2.1@sha256:f68eec2eff2ecd70354573605ed5ab3bc6c727ae26978260e2486c34cc5adeec

WORKDIR /app

COPY --from=builder /go/src/tilt/simple-health-client/simple-health-client .

ENTRYPOINT ["/app/simple-health-client"]
