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

# CUDA Validation Preflight Check (init-container).
#
# One-shot probe that allocates a small tensor on every visible GPU and
# verifies CUDA context creation + kernel launch + synchronize succeed.
# Designed to run as a Pod init-container so the workload never coexists
# with this probe — eliminating the GPU-memory contention concern that
# applied when CUDA validation lived in a steady-state daemon checker.
#
# Build from repository root:
#   docker build -t preflight-cuda-validation \
#     -f preflight-checks/cuda-validation/Dockerfile .

# PyTorch version determines CUDA runtime version - must match cluster's GPU driver.
# See: https://docs.nvidia.com/cuda/cuda-toolkit-release-notes/index.html#id4
#   26.04-py3 = CUDA 13.2.1 (Ubuntu 24.04, Python 3.12)
ARG PYTORCH_VERSION=26.04-py3
ARG VERSION="0.1.0"
ARG GIT_COMMIT="none"
ARG BUILD_DATE=""

FROM nvcr.io/nvidia/pytorch:${PYTORCH_VERSION} AS runtime

ARG VERSION
ARG GIT_COMMIT
ARG BUILD_DATE

ENV PYTHONUNBUFFERED=1 \
    LOG_LEVEL=info \
    CUDA_VALIDATION_TENSOR_SIZE=1024

WORKDIR /app

COPY preflight-checks/cuda-validation/cuda_validation.py /app/cuda_validation.py
RUN chmod +x /app/cuda_validation.py

# Run as a dedicated non-root user (matches the dcgm-diag sibling). CUDA
# device access is permission-dependent, not root-only: the device plugin
# exposes /dev/nvidia* with world-accessible modes on standard GPU operator
# deployments. HOME points at /tmp so any torch/JIT cache writes succeed.
RUN groupadd -r nvsentinel && useradd -r -g nvsentinel nvsentinel
USER nvsentinel
ENV HOME=/tmp

ENTRYPOINT ["python3", "/app/cuda_validation.py"]
