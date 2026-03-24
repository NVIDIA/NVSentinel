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

FROM public.ecr.aws/docker/library/golang:1.25-trixie AS builder

ARG VERSION="dev"
ARG GIT_COMMIT="none"
ARG BUILD_DATE=$(date -u +%FT%TZ)

WORKDIR /go/src/nvsentinel

# Copy the go.mod files for dependencies first
COPY health-monitors/nic-health-monitor/go.mod health-monitors/nic-health-monitor/go.sum health-monitors/nic-health-monitor/
COPY commons/go.mod commons/go.sum ./commons/

# Download dependencies
RUN --mount=type=cache,target=/go/pkg/mod \
    cd health-monitors/nic-health-monitor && go mod download

# Copy the source code
COPY health-monitors/nic-health-monitor/ health-monitors/nic-health-monitor/
COPY commons/ commons/

# Build with caches for faster iterations
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    cd health-monitors/nic-health-monitor && \
    CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.date=${BUILD_DATE}" -o nic-health-monitor main.go

# Runtime stage - minimal image since no CGO dependencies needed
FROM public.ecr.aws/docker/library/debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
    ca-certificates \
    && rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvsentinel/health-monitors/nic-health-monitor/nic-health-monitor /app/nic-health-monitor

ENTRYPOINT ["/app/nic-health-monitor"]
