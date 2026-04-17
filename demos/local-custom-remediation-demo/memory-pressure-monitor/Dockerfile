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

FROM public.ecr.aws/docker/library/golang:1.25 AS builder

WORKDIR /go/src/nvsentinel

COPY demos/local-custom-remediation-demo/memory-pressure-monitor/go.mod demos/local-custom-remediation-demo/memory-pressure-monitor/go.sum demos/local-custom-remediation-demo/memory-pressure-monitor/
COPY data-models/go.mod data-models/go.sum ./data-models/

RUN cd demos/local-custom-remediation-demo/memory-pressure-monitor && go mod download

COPY demos/local-custom-remediation-demo/memory-pressure-monitor/ demos/local-custom-remediation-demo/memory-pressure-monitor/
COPY data-models/ data-models/

RUN cd demos/local-custom-remediation-demo/memory-pressure-monitor && \
    CGO_ENABLED=0 go build -ldflags="-s -w" -o memory-pressure-monitor main.go

FROM gcr.io/distroless/static-debian12

COPY --from=builder /go/src/nvsentinel/demos/local-custom-remediation-demo/memory-pressure-monitor/memory-pressure-monitor /memory-pressure-monitor

ENTRYPOINT ["/memory-pressure-monitor"]
