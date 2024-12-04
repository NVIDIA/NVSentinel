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

ARG TAG=v3.1.2
ARG IMG=nvcr.io/nvidian/distroless/go

FROM golang:1.22 AS builder

ENV CGO_ENABLED=0

RUN mkdir -p /etc/config
WORKDIR /go/src/platform-connectors

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN go build -o platform-connectors main.go

FROM $IMG:$TAG

COPY --from=builder /go/src/platform-connectors/platform-connectors /app/platform-connectors

ENTRYPOINT ["/app/platform-connectors"]
