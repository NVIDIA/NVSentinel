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
ARG BUILD_DATE="unknown"

WORKDIR /go/src/nvsentinel

# Copy go.mod for dependencies first
COPY health-monitors/slurm-drain-monitor/go.mod health-monitors/slurm-drain-monitor/go.sum health-monitors/slurm-drain-monitor/
COPY data-models/go.mod data-models/go.sum ./data-models/
COPY commons/go.mod commons/go.sum ./commons/

RUN cd health-monitors/slurm-drain-monitor && go mod download

# Copy source
COPY health-monitors/slurm-drain-monitor/ health-monitors/slurm-drain-monitor/
COPY data-models/ data-models/
COPY commons/ commons/

RUN cd health-monitors/slurm-drain-monitor && \
	CGO_ENABLED=0 go build -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${GIT_COMMIT} -X main.date=${BUILD_DATE}" \
	-o slurm-drain-monitor .

FROM public.ecr.aws/docker/library/debian:bookworm-slim AS runtime

RUN apt-get update && apt-get install -y --no-install-recommends \
	ca-certificates \
	&& rm -rf /var/lib/apt/lists/*

COPY --from=builder /go/src/nvsentinel/health-monitors/slurm-drain-monitor/slurm-drain-monitor /app/slurm-drain-monitor

ENTRYPOINT ["/app/slurm-drain-monitor"]
