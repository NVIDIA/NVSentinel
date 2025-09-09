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

#Platform connector build
FROM nvcr.io/nv-ngc-devops/golang:1.23-1.0 AS builder

RUN mkdir -p /app
COPY platform-connectors /app/platform-connectors
WORKDIR /app/platform-connectors

ENV CGO_ENABLED=0
ARG TARGETOS
ARG TARGETARCH

RUN mkdir -p /etc/config

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
RUN go mod tidy

RUN GOOS=$TARGETOS GOARCH=$TARGETARCH go build -o platform-connector main.go

# gpu-health-monitor build dependencies requirements
FROM nvcr.io/nv-ngc-devops/python:3.10-bookworm1.0 AS build

RUN mkdir -p /app
COPY health-monitors/gpu-health-monitor /app/gpu-health-monitor
WORKDIR /app/gpu-health-monitor
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

RUN poetry build --format wheel
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes

# Final stage application for gpu_health_monitor and platform-connectors
FROM nvcr.io/nvidia/cloud-native/dcgm:3.3.7-1-ubuntu22.04

WORKDIR /app

# Update package list and install Python and pip
RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update && \
    apt-get install -y python3-pip && \
    rm -rf /var/lib/apt/lists/*

ENV PYTHONPATH=/usr/local/dcgm/bindings/python3 \
    PYTHONUNBUFFERED=1

COPY --from=build /app/gpu-health-monitor/dist/*.whl /app/
COPY --from=build /app/gpu-health-monitor/constraints.txt /app/

# Install gpu-health-monitor
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install /app/gpu_health_monitor*.whl --constraint /app/constraints.txt

# Copy the built Go application from the builder stage
COPY --from=builder /app/platform-connectors/platform-connector /app/
