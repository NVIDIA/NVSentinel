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

# PyTorch version determines CUDA runtime version - must match cluster's GPU driver
# See: https://docs.nvidia.com/cuda/cuda-toolkit-release-notes/index.html#id4
#   25.01-py3 = CUDA 12.8 (driver 570+)
ARG PYTORCH_VERSION=25.01-py3
ARG NCCL_TESTS_VERSION=v2.13.11

# =============================================================================
# Build stage: Compile Go binary
# =============================================================================
FROM public.ecr.aws/docker/library/golang:1.25-bookworm AS go-builder

ARG VERSION="dev"
ARG GIT_COMMIT="none"
ARG BUILD_DATE="unknown"

ENV CGO_ENABLED=0

WORKDIR /go/src

# Copy go.mod and go.sum files for all dependencies first (layer caching)
COPY commons/go.mod commons/go.sum ./commons/
COPY data-models/go.mod data-models/go.sum ./data-models/
COPY preflight-checks/nccl-loopback/go.mod preflight-checks/nccl-loopback/go.sum ./preflight-checks/nccl-loopback/

WORKDIR /go/src/preflight-checks/nccl-loopback

RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download

# Copy source code after go mod download
COPY preflight-checks/nccl-loopback/ .
COPY commons/ ../../commons/
COPY data-models/ ../../data-models/

RUN --mount=type=cache,target=/go/pkg/mod \
    go build -trimpath \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.date=${BUILD_DATE}" \
    -o nccl-loopback-check \
    .

# =============================================================================
# Runtime stage: PyTorch base with nccl-tests compiled
#
# Using PyTorch NGC image provides:
# - NCCL library (used by PyTorch distributed)
# - CUDA toolkit for compiling nccl-tests
# - Future compatibility for multi-node tests using PyTorch's TCP backend
#
# We compile nccl-tests from source to get deterministic binary paths and
# ensure version compatibility with the bundled NCCL.
# =============================================================================
FROM nvcr.io/nvidia/pytorch:${PYTORCH_VERSION} AS runtime

ARG NCCL_TESTS_VERSION

# Build nccl-tests from source
# Uses NCCL bundled with PyTorch image for version compatibility
# NVCC_GENCODE targets modern data center GPUs:
#   - compute_80: A100 (Ampere)
#   - compute_89: L40S, L4 (Ada Lovelace)
#   - compute_90: H100, H200 (Hopper)
#   - compute_100: GB200, B100, B200 (Blackwell) - requires CUDA 12.8+
RUN git clone -b ${NCCL_TESTS_VERSION} https://github.com/NVIDIA/nccl-tests.git /opt/nccl-tests \
    && cd /opt/nccl-tests \
    && make -j$(nproc) \
        CUDA_HOME=/usr/local/cuda \
        NVCC_GENCODE="-gencode=arch=compute_80,code=sm_80 \
                      -gencode=arch=compute_89,code=sm_89 \
                      -gencode=arch=compute_90,code=sm_90 \
                      -gencode=arch=compute_100,code=sm_100"

WORKDIR /app

COPY --from=go-builder /go/src/preflight-checks/nccl-loopback/nccl-loopback-check .

# Default environment variables
ENV NCCL_TEST_BINARY_PATH=/opt/nccl-tests/build/all_reduce_perf \
    BW_THRESHOLD_GBPS=150 \
    TEST_SIZE_MB=256

# Note: Running as root because NCCL tests require GPU device access
# which typically needs elevated privileges

ENTRYPOINT ["/app/nccl-loopback-check"]
