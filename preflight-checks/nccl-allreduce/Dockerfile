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

# Multi-node NCCL All-Reduce Preflight Check
#
# This container runs NCCL all-reduce benchmarks across gang members
# to verify cross-node GPU communication before workloads start.
#
# Build from repository root:
#   docker build -t preflight-nccl-allreduce -f preflight-checks/nccl-allreduce/Dockerfile .

# PyTorch version determines CUDA runtime version - must match cluster's GPU driver
# See: https://docs.nvidia.com/cuda/cuda-toolkit-release-notes/index.html#id4
#   25.01-py3 = CUDA 12.8 (driver 570+)
ARG PYTORCH_VERSION=25.01-py3
ARG VERSION="0.1.0"

# =============================================================================
# Build stage: Install Python dependencies
# =============================================================================
FROM public.ecr.aws/docker/library/python:3.10-bookworm AS build

ARG VERSION

RUN --mount=type=cache,target=/root/.cache/pip \
    pip install poetry==1.8.2

ENV POETRY_NO_INTERACTION=1 \
    POETRY_VIRTUALENVS_IN_PROJECT=1 \
    POETRY_VIRTUALENVS_CREATE=1 \
    POETRY_CACHE_DIR=/tmp/poetry_cache

WORKDIR /app

COPY preflight-checks/nccl-allreduce/ .

# Update version if it's a valid semver (skip for branch names)
RUN if echo "${VERSION}" | grep -qE '^v?[0-9]+\.[0-9]+'; then \
        poetry version $(echo "${VERSION}" | sed 's/^v//'); \
    fi
RUN --mount=type=cache,target=/tmp/poetry_cache \
    poetry build --format wheel

# Export requirements for installation without torch (provided by base image)
RUN poetry export --format requirements.txt --output constraints.txt --without-hashes

# =============================================================================
# Runtime stage: PyTorch base image with NCCL support
#
# Using PyTorch NGC image provides:
# - NCCL library optimized for data center GPUs
# - CUDA toolkit and drivers
# - PyTorch distributed with NCCL backend
# =============================================================================
FROM nvcr.io/nvidia/pytorch:${PYTORCH_VERSION} AS runtime

ARG VERSION

# Remove the NGC image's bundled EFA/OFI-NCCL stack and stale RDMA libs.
# On AWS, the host's /opt/amazon is mounted at runtime and provides the
# EFA libs (libfabric, ofi-nccl plugin) that match the host's kernel
# driver. The base image also ships an older libefa.so in the system lib
# path which causes ABI mismatches (e.g. EFA_1.4 not found) with the
# host's libfabric. Removing both ensures the host stack is used end-to-end.
# On non-AWS platforms these files don't exist, so the rm is a no-op.
RUN rm -rf /opt/amazon /usr/lib/x86_64-linux-gnu/libefa.so*

COPY --from=build /app/dist/*.whl /tmp/
COPY --from=build /app/constraints.txt /tmp/

RUN --mount=type=cache,target=/root/.cache/pip \
    pip install /tmp/nvsentinel_nccl_allreduce*.whl \
        --constraint /tmp/constraints.txt && \
    rm -rf /tmp/*.whl /tmp/constraints.txt

# Copy entrypoint script
COPY preflight-checks/nccl-allreduce/scripts/entrypoint.py /app/entrypoint.py
RUN chmod +x /app/entrypoint.py

WORKDIR /app

# Default environment variables
ENV GANG_CONFIG_DIR=/etc/preflight \
    GANG_TIMEOUT_SECONDS=600 \
    BW_THRESHOLD_GBPS=100 \
    MESSAGE_SIZES="4G,8G" \
    BENCHMARK_ITERS=20 \
    WARMUP_ITERS=5 \
    LOG_LEVEL=info \
    PYTHONUNBUFFERED=1

# Note: Running as root because NCCL tests require GPU device access
# which typically needs elevated privileges

ENTRYPOINT ["python3", "/app/entrypoint.py"]
