# Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

ARG TAG=v3.1.2
ARG IMG=nvcr.io/nvidia/distroless/go

FROM golang:1.23 AS builder

# Set necessary environment variables for Go modules
ENV GOPRIVATE=*.nvidia.com
ENV CGO_ENABLED=0

# Arguments for GitLab authentication
ARG GITLAB_HOST
ARG GITLAB_LOGIN

RUN if [ -z "$GITLAB_HOST" ] || [ -z "$GITLAB_LOGIN" ]; then \
        echo "Error: GITLAB_HOST and GITLAB_LOGIN must be provided as build arguments."; \
        exit 1; \
    fi

WORKDIR /go/src/csp-health-monitor

# Copy go module files first for better layer caching
COPY go.mod go.sum ./

# Download dependencies
# The .netrc needs to be created in the home directory of the user running the build (root in this stage)
RUN --mount=type=secret,id=gitlab_token \
    GITLAB_TOKEN=$(cat /run/secrets/gitlab_token) && \
    echo "machine ${GITLAB_HOST} login ${GITLAB_LOGIN} password ${GITLAB_TOKEN}" > ~/.netrc && \
    chmod 600 ~/.netrc && \
    git config --global url."https://${GITLAB_LOGIN}:${GITLAB_TOKEN}@${GITLAB_HOST}/".insteadOf "https://${GITLAB_HOST}/" && \
    go env -w GOPRIVATE=${GITLAB_HOST} && \
    go mod download && \
    rm ~/.netrc

# Copy the rest of the source code
COPY . .

# Build the main monitor application binary
RUN go build -trimpath -o csp-health-monitor ./cmd/csp-health-monitor/main.go

# Build the quarantine trigger engine sidecar binary
RUN go build -trimpath -o maintenance-notifier ./cmd/maintenance-notifier/main.go

# --- Final Stage ---
FROM ${IMG}:${TAG}

WORKDIR /app

# Copy binaries from builder stage
COPY --from=builder /go/src/csp-health-monitor/csp-health-monitor ./
COPY --from=builder /go/src/csp-health-monitor/maintenance-notifier ./

ENTRYPOINT ["/app/csp-health-monitor"]