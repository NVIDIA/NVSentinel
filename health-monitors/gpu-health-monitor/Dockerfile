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

ARG DCGM_VERSION="4.7.0-ubuntu24.04@sha256:c7e042a939a5c0d4d588fff28f9371d9d101e66eece7b3f831cf5d44fa182fc8"
ARG VERSION="0.1.0"
# GIT_COMMIT and BUILD_DATE are passed by CI (make/docker.mk) for OCI annotations
ARG GIT_COMMIT="none"
ARG BUILD_DATE=$(date -u +%FT%TZ)

FROM public.ecr.aws/docker/library/python:3.15-trixie@sha256:e5eb16387fc01b6d5af685c908cbc7f82f18b383d3a358d6e37cfbac6e88ac37 AS build

ARG VERSION

RUN --mount=type=cache,target=/root/.cache/pip \
    pip install poetry==2.5.1 poetry-plugin-export==1.10.1

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

WORKDIR /app

COPY health-monitors/gpu-health-monitor/ .
# Set package version from build arg (strip 'v' prefix for PEP 440 compliance)
RUN poetry version $(echo "${VERSION}" | sed 's/^v//')
RUN --mount=type=cache,target=/tmp/poetry_cache \
    poetry build --format wheel
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes


# Digest-pinned through the ARG value (tag@sha256). The tag is a build-matrix parameter (see
# Makefile) used to produce image variants. Dependabot cannot update an ARG, so update the tag
# and the digest together by hand.
FROM nvcr.io/nvidia/cloud-native/dcgm:${DCGM_VERSION} AS runtime

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    echo 'Acquire::Retries "3";' > /etc/apt/apt.conf.d/80-retries && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        apt-transport-https \
        ca-certificates \
        python3-venv && \
    python3 -m venv /opt/venv && \
    rm -rf /var/lib/apt/lists/*

ARG PYTHONPATH="/usr/share/datacenter-gpu-manager-4/bindings/python3"

ENV PYTHONPATH=${PYTHONPATH} \
    PATH="/opt/venv/bin:${PATH}" \
    PYTHONUNBUFFERED=1

COPY --from=build /app/dist/*.whl ./
COPY --from=build /app/constraints.txt ./
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install ./gpu_health_monitor*.whl --constraint constraints.txt