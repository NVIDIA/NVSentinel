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

WORKDIR /go/src/nvsentinel

COPY health-monitors/kubernetes-object-monitor/go.mod health-monitors/kubernetes-object-monitor/go.sum health-monitors/kubernetes-object-monitor/
COPY data-models/go.mod data-models/go.sum ./data-models/
COPY commons/go.mod commons/go.sum ./commons/

RUN --mount=type=cache,target=/go/pkg/mod \
    cd health-monitors/kubernetes-object-monitor && go mod download

COPY health-monitors/kubernetes-object-monitor/ health-monitors/kubernetes-object-monitor/
COPY data-models/ data-models/
COPY commons/ commons/

RUN cd health-monitors/kubernetes-object-monitor && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o kubernetes-object-monitor main.go

FROM public.ecr.aws/docker/library/debian:bookworm-slim AS runtime

COPY --from=builder /go/src/nvsentinel/health-monitors/kubernetes-object-monitor/kubernetes-object-monitor /app/kubernetes-object-monitor

ENTRYPOINT ["/app/kubernetes-object-monitor"]

