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

ENV CGO_ENABLED=0

RUN mkdir -p /etc/config
WORKDIR /go/src/platform-connectors

COPY platform-connectors/go.mod platform-connectors/go.sum ./
COPY data-models/go.mod data-models/go.sum ../data-models/
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY platform-connectors/ .
COPY data-models/ ../data-models/

RUN go build -o platform-connectors main.go

FROM $IMG:$TAG

COPY --from=builder /go/src/platform-connectors/platform-connectors /app/platform-connectors

ENTRYPOINT ["/app/platform-connectors"]
