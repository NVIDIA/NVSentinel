# Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http:#www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

FROM public.ecr.aws/docker/library/golang:1.27.1-trixie@sha256:433790e515d27dc6003e847e644cc0af956985cf315c1c58a3b73ee2dd305183 AS builder

ARG VERSION="dev"
ARG GIT_COMMIT="none"
ARG BUILD_DATE=$(date -u +%FT%TZ)

WORKDIR /workspace

COPY commons/ commons/
COPY data-models/ data-models/

COPY metadata-collector/go.mod metadata-collector/go.sum metadata-collector/
WORKDIR /workspace/metadata-collector
RUN go mod download

COPY metadata-collector/ .

# Builds the package, not a file list: `go build main.go` silently omits every other file in
# package main, so adding one breaks the image while `go build ./...` and the tests still pass.
RUN CGO_ENABLED=1 GOOS=linux go build -a -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.date=${BUILD_DATE}" -o metadata-collector .

FROM nvcr.io/nvidia/cuda:13.4.1-base-ubuntu22.04@sha256:6eb5abcde4c31204f5128ef6eaa027832345ebe23d55df7aa719db5602fafe87

RUN groupadd -r nvsentinel && useradd -r -g nvsentinel nvsentinel

WORKDIR /app

COPY --from=builder --chown=nvsentinel:nvsentinel /workspace/metadata-collector/metadata-collector .

USER nvsentinel

ENTRYPOINT ["/app/metadata-collector"]


