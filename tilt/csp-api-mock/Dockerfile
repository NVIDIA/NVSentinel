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

WORKDIR /app
COPY go.mod go.sum ./
# Retry go mod download, because proxy.golang.org can close the connection.
RUN --mount=type=cache,target=/go/pkg/mod \
    for attempt in 1 2 3; do \
      go mod download && break; \
      [ "$attempt" -lt 3 ] || exit 1; \
      sleep $((attempt * 10)); \
    done
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o csp-api-mock .

FROM public.ecr.aws/docker/library/debian:trixie-slim@sha256:a72b4433de1cf7d6bae0c4cee74d0547f58601a643b3375f070c51600d63b6b2 AS runtime

COPY --from=builder /app/csp-api-mock /app/csp-api-mock

EXPOSE 8080 50051

ENTRYPOINT ["/app/csp-api-mock"]
