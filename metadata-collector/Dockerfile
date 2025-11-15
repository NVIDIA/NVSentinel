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

FROM public.ecr.aws/docker/library/golang:1.25-bookworm AS builder

WORKDIR /workspace

COPY commons/ commons/
COPY data-models/ data-models/

COPY metadata-collector/go.mod metadata-collector/go.sum metadata-collector/
WORKDIR /workspace/metadata-collector
RUN go mod download

COPY metadata-collector/ .

RUN CGO_ENABLED=1 GOOS=linux go build -a -ldflags="-s -w" -o metadata-collector main.go

FROM nvcr.io/nvidia/cuda:12.3.0-base-ubuntu22.04

RUN groupadd -r nvsentinel && useradd -r -g nvsentinel nvsentinel

WORKDIR /app

COPY --from=builder --chown=nvsentinel:nvsentinel /workspace/metadata-collector/metadata-collector .

USER nvsentinel

ENTRYPOINT ["/app/metadata-collector"]


