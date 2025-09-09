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

ARG TAG=v3.1.9
ARG IMG=nvcr.io/nvidia/distroless/go

FROM nvcr.io/nv-ngc-devops/golang:1.23-1.0 AS builder

ARG GITLAB_HOST
ARG GITLAB_LOGIN

RUN if [ -z "$GITLAB_HOST" ] || [ -z "$GITLAB_LOGIN" ]; then \
        echo "Error: GITLAB_HOST and GITLAB_LOGIN must be provided as build arguments."; \
        exit 1; \
    fi

ENV GOPRIVATE=*.nvidia.com
ENV CGO_ENABLED=0
ARG TARGETOS
ARG TARGETARCH

WORKDIR /go/src/node-drainer-module

COPY go.mod go.sum ./

# download dependencies with authentication
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=secret,id=gitlab_token \
    GITLAB_TOKEN=$(cat /run/secrets/gitlab_token) && \
    echo "machine ${GITLAB_HOST} login ${GITLAB_LOGIN} password ${GITLAB_TOKEN}" > ~/.netrc && \
    chmod 600 ~/.netrc && \
    go mod download && \
    rm ~/.netrc

    
COPY . .
    
RUN --mount=type=cache,target=/go/pkg/mod \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -o nodedrainer cmd/nodedrainer.go

FROM ${IMG}:${TAG}

WORKDIR /app

COPY --from=builder /go/src/node-drainer-module/nodedrainer .

ENTRYPOINT ["/app/nodedrainer"]
