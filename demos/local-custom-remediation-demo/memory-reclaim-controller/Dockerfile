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

FROM public.ecr.aws/docker/library/golang:1.27.3-trixie@sha256:fbb7ae40b62ff8afe31662df570d39b4f5052ac32b90dae8e60d7657a20a74f4 AS builder

WORKDIR /go/src/app

COPY demos/local-custom-remediation-demo/memory-reclaim-controller/go.mod demos/local-custom-remediation-demo/memory-reclaim-controller/go.sum ./

RUN go mod download

COPY demos/local-custom-remediation-demo/memory-reclaim-controller/ .

RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /memory-reclaim-controller .

FROM gcr.io/distroless/static-debian13:latest@sha256:448810357cc8e67b7208988abdba0f5ec3d3aebee0a5b13eb624979792cffc0f

COPY --from=builder /memory-reclaim-controller /memory-reclaim-controller

ENTRYPOINT ["/memory-reclaim-controller"]
