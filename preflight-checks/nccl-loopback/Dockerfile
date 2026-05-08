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
#   26.03-py3 = CUDA 13.2 (Ubuntu 24.04, Python 3.12)
ARG PYTORCH_VERSION=26.03-py3
ARG NCCL_TESTS_VERSION=v2.13.11

# CUDA runtime base for the final stage. Pinned to a CUDA 13.x runtime image
# so the slim image carries libcudart/libcuda-stub matching the toolkit
# nccl-tests was compiled against. Driver compatibility is provided by the
# host via nvidia-container-runtime.
ARG CUDA_RUNTIME_BASE=nvcr.io/nvidia/cuda:13.0.2-runtime-ubuntu24.04

# =============================================================================
# Build stage: Compile Go binary
# =============================================================================
FROM public.ecr.aws/docker/library/golang:1.26.2-trixie AS go-builder

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
# nccl-tests build stage: PyTorch base provides CUDA toolkit + bundled NCCL
#
# Using PyTorch NGC image here gives us nvcc and a known-good NCCL build.
# We only need this stage to compile the nccl-tests binaries; the final
# runtime image does not inherit from it (see slim runtime stage below).
# =============================================================================
FROM nvcr.io/nvidia/pytorch:${PYTORCH_VERSION} AS nccl-builder

ARG NCCL_TESTS_VERSION

# Build nccl-tests from source.
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

# Stage the runtime libraries the compiled binaries need so the final stage
# can COPY them with a single instruction (and so we don't accidentally drag
# along PyTorch, JupyterLab, NVFuser, etc.).
RUN mkdir -p /staging/nccl-libs \
    && cp -L /usr/lib/x86_64-linux-gnu/libnccl.so* /staging/nccl-libs/ 2>/dev/null \
    || cp -L /usr/local/nccl/lib/libnccl.so* /staging/nccl-libs/

# =============================================================================
# Runtime stage: slim CUDA-only image
#
# Built on a plain CUDA runtime image instead of nvcr.io/nvidia/pytorch so
# we ship only what `all_reduce_perf` actually links against (CUDA runtime,
# NCCL, system C/C++ runtimes). PyTorch, JupyterLab, NVFuser, TorchAO,
# TorchTitan, cuDNN-devel, and the rest of the NGC stack — none of which the
# preflight Go binary or the nccl-tests binaries call into — are dropped.
#
# Image size impact: ~9.95 GB → ~1.5-1.7 GB compressed. On clusters with
# constrained egress (e.g. multi-tenant b300 hosts) this turns a multi-minute
# cold pull on freshly placed nodes into a sub-30s pull.
# =============================================================================
FROM ${CUDA_RUNTIME_BASE} AS runtime

# Copy NCCL shared libraries from the nccl-builder stage. Use the canonical
# /usr/lib/x86_64-linux-gnu location so the dynamic loader finds them without
# any LD_LIBRARY_PATH gymnastics.
COPY --from=nccl-builder /staging/nccl-libs/ /usr/lib/x86_64-linux-gnu/

# Copy compiled nccl-tests binaries (a few MB total).
COPY --from=nccl-builder /opt/nccl-tests/build /opt/nccl-tests/build

WORKDIR /app

COPY --from=go-builder /go/src/preflight-checks/nccl-loopback/nccl-loopback-check .

# Default environment variables
ENV NCCL_TEST_BINARY_PATH=/opt/nccl-tests/build/all_reduce_perf \
    BW_THRESHOLD_GBPS=150 \
    TEST_SIZE_MB=256

# Note: Running as root because NCCL tests require GPU device access
# which typically needs elevated privileges

ENTRYPOINT ["/app/nccl-loopback-check"]
