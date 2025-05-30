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

ARG DCGM_VERSION="4.2.0-1-ubuntu22.04"
FROM nvcr.io/nv-ngc-devops/python:3.10-bookworm1.0 AS build

RUN pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

WORKDIR /app

COPY . .
RUN poetry build --format wheel 
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes


FROM nvcr.io/nvidia/cloud-native/dcgm:${DCGM_VERSION} AS runtime

RUN apt-get update && \
    apt-get install -y python3-pip && \
    rm -rf /var/lib/apt/lists/*

ARG PYTHONPATH="/usr/share/datacenter-gpu-manager-4/bindings/python3"
ENV PYTHONPATH=${PYTHONPATH} \
    PYTHONUNBUFFERED=1

COPY --from=build /app/dist/*.whl ./
COPY --from=build /app/constraints.txt ./
RUN pip install ./gpu_health_monitor*.whl --constraint constraints.txt

# Copy the proto file and the Python script
COPY gpu_health_monitor/clear_xid_errors/clear_xid_error_health_event.py .
COPY gpu_health_monitor/clear_xid_errors/protos/ protos/
RUN pip install kubernetes==28.1.0
COPY gpu_health_monitor/node_watcher/src.py node_watcher.py
