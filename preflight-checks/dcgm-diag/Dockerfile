# Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

ARG DCGM_VERSION="4.4.2-1-ubuntu22.04"
ARG VERSION="0.1.0"
ARG GIT_COMMIT="none"
ARG BUILD_DATE=$(date -u +%FT%TZ)

FROM public.ecr.aws/docker/library/python:3.10-bookworm AS build

ARG VERSION

RUN --mount=type=cache,target=/root/.cache/pip \
    pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

WORKDIR /app

COPY preflight-checks/dcgm-diag/ .
RUN poetry version $(echo "${VERSION}" | sed 's/^v//')
RUN --mount=type=cache,target=/tmp/poetry_cache \
    poetry build --format wheel
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes


FROM nvcr.io/nvidia/cloud-native/dcgm:${DCGM_VERSION} AS runtime

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    echo 'Acquire::Retries "3";' > /etc/apt/apt.conf.d/80-retries && \
    apt-get update && \
    apt-get install -y --no-install-recommends \
        apt-transport-https \
        ca-certificates \
        gnupg \
        python3-pip && \
    rm -rf /var/lib/apt/lists/*

ARG PYTHONPATH="/usr/share/datacenter-gpu-manager-4/bindings/python3"

ENV PYTHONPATH=${PYTHONPATH} \
    PYTHONUNBUFFERED=1

COPY --from=build /app/dist/*.whl ./
COPY --from=build /app/constraints.txt ./
RUN --mount=type=cache,target=/root/.cache/pip \
    pip install ./nvsentinel_dcgm_diag*.whl --constraint constraints.txt

# Bundle error mapping file (static release data, not runtime config)
COPY distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv /etc/dcgm/dcgmerrorsmapping.csv

RUN groupadd -r nvsentinel && useradd -r -g nvsentinel nvsentinel
USER nvsentinel

ENTRYPOINT ["python3", "-m", "dcgm_diag"]
