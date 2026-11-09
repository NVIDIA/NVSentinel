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

FROM public.ecr.aws/docker/library/golang:1.27.3-trixie@sha256:af1bfc2614ebb1dbd31af514b78740fd678850e76498762b425ecee4c5c6ea80 AS builder

ARG VERSION="dev"
ARG GIT_COMMIT="none"
ARG BUILD_DATE=$(date -u +%FT%TZ)

WORKDIR /workspace

COPY commons/ commons/
COPY data-models/ data-models/

COPY metadata-collector/go.mod metadata-collector/go.sum metadata-collector/
WORKDIR /workspace/metadata-collector
# Retry go mod download, because proxy.golang.org can close the connection.
RUN for attempt in 1 2 3; do \
      go mod download && break; \
      [ "$attempt" -lt 3 ] || exit 1; \
      sleep $((attempt * 10)); \
    done

COPY metadata-collector/ .

# Builds the package, not a file list: `go build main.go` silently omits every other file in
# package main, so adding one breaks the image while `go build ./...` and the tests still pass.
RUN CGO_ENABLED=1 GOOS=linux go build -a -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.date=${BUILD_DATE}" -o metadata-collector .

FROM nvcr.io/nvidia/cuda:13.5.0-base-ubuntu22.04@sha256:4df174aaa911425cc8ab5beb6f897dfeafa92f03884c25176db134743f6e3736

RUN groupadd -r nvsentinel && useradd -r -g nvsentinel nvsentinel

WORKDIR /app

COPY --from=builder --chown=nvsentinel:nvsentinel /workspace/metadata-collector/metadata-collector .

USER nvsentinel

ENTRYPOINT ["/app/metadata-collector"]


