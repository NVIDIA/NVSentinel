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

ARG VERSION="0.1.0"
# GIT_COMMIT and BUILD_DATE are passed by CI (make/docker.mk) for OCI annotations
ARG GIT_COMMIT="none"
ARG BUILD_DATE=$(date -u +%FT%TZ)

# =============================================================================
# Build stage: Compile Go binary
# =============================================================================
FROM public.ecr.aws/docker/library/golang:1.25-bookworm AS builder

ARG VERSION
ARG GIT_COMMIT
ARG BUILD_DATE

WORKDIR /workspace

# Copy go.mod files for dependency caching
COPY data-models/go.mod data-models/go.sum ./data-models/
COPY preflight-checks/nccl-loopback/go.mod preflight-checks/nccl-loopback/go.sum ./preflight-checks/nccl-loopback/

# Download dependencies (disable workspace mode - we only have the modules we need)
ENV GOWORK=off
RUN cd preflight-checks/nccl-loopback && go mod download

# Copy source code
COPY data-models/ ./data-models/
COPY preflight-checks/nccl-loopback/ ./preflight-checks/nccl-loopback/

# Build the binary
RUN cd preflight-checks/nccl-loopback && \
    CGO_ENABLED=0 GOOS=linux go build \
    -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.date=${BUILD_DATE}" \
    -o /nccl-loopback-check \
    .

# =============================================================================
# Runtime stage: Use AWS public nccl-tests image as base
# =============================================================================
FROM public.ecr.aws/hpc-cloud/nccl-tests:latest

# Copy our Go binary
COPY --from=builder /nccl-loopback-check /usr/local/bin/nccl-loopback-check

# Default environment variables
ENV NCCL_TEST_BINARY=/opt/nccl-tests/build/all_reduce_perf \
    BW_THRESHOLD_GBPS=150 \
    TEST_SIZE_MB=256

ENTRYPOINT ["/usr/local/bin/nccl-loopback-check"]
