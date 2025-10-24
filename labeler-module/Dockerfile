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

WORKDIR /go/src/labeler

COPY labeler-module/go.mod labeler-module/go.sum ./
COPY data-models/go.mod data-models/go.sum ./data-models/

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

COPY labeler-module/ .
COPY data-models/ ../data-models/

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath -o labeler main.go

FROM ${IMG}:${TAG}

WORKDIR /app

COPY --from=builder /go/src/labeler/labeler .

ENTRYPOINT ["/app/labeler"]
