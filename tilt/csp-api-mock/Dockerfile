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

WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod \
    go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o csp-api-mock .

FROM public.ecr.aws/docker/library/debian:bookworm-slim AS runtime

COPY --from=builder /app/csp-api-mock /app/csp-api-mock

EXPOSE 8080 50051

ENTRYPOINT ["/app/csp-api-mock"]
