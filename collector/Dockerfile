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

# Collector image: executes nvidia-bug-report(.sh) inside the driver daemonset pod
# and runs GPU Operator must-gather; optionally uploads artifacts to a file server.
# Base: Ubuntu for minimal, stable tooling (bash, curl, tar, gzip)

FROM ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG KUBECTL_VERSION=v1.28.3

# Minimal runtime deps for entrypoint.sh
# (bash is present in base; install curl/tar/gzip and CA certs for HTTPS)
RUN apt-get update \
  && apt-get install -y --no-install-recommends \
    ca-certificates \
    tar \
    gzip \
    curl \
  && rm -rf /var/lib/apt/lists/*

# Install kubectl (used for exec/cp and must-gather)
RUN curl -fsSL -o /usr/local/bin/kubectl https://storage.googleapis.com/kubernetes-release/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl \
  && chmod +x /usr/local/bin/kubectl \
  && kubectl version --client=true --output=yaml || true

# Create non-root user (optional; container runs privileged in Job context)
RUN useradd -u 10001 -m collector

WORKDIR /opt/collector

COPY entrypoint.sh /opt/collector/entrypoint.sh
RUN chmod +x /opt/collector/entrypoint.sh

ENV PATH="/opt/collector:${PATH}"

# Artifacts directory inside the container
# - /artifacts/<NODE_NAME>/<TIMESTAMP>
#
# Required/Optional env:
# - NODE_NAME (required; via Downward API)
# - GPU_OPERATOR_NAMESPACE (optional; default: gpu-operator)
# - DRIVER_CONTAINER_NAME (optional; default: nvidia-driver-ctr)
# - UPLOAD_URL_BASE (optional; if set, artifacts are PUT to this base URL)
# - COLLECTION_TIMEOUT_SECONDS (optional; default: 900)

ENTRYPOINT ["/opt/collector/entrypoint.sh"]
