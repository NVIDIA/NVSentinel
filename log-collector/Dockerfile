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

# Collector image: executes nvidia-bug-report(.sh) inside the driver daemonset pod,
# runs GPU Operator must-gather, and optionally collects GCP SOS reports on GCP instances;
# optionally uploads artifacts to a file server.
# Base: Ubuntu for minimal, stable tooling (bash, curl, tar, gzip, sudo)

FROM public.ecr.aws/docker/library/ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG KUBECTL_VERSION=v1.34.1

RUN --mount=type=cache,target=/var/cache/apt,sharing=locked \
    --mount=type=cache,target=/var/lib/apt,sharing=locked \
    apt-get update \
  && apt-get install -y --no-install-recommends \
    ca-certificates \
    tar \
    gzip \
    curl \
    sudo \
  && rm -rf /var/lib/apt/lists/*

RUN ARCH=$(case $(uname -m) in x86_64) echo amd64;; aarch64|arm64) echo arm64;; *) echo $(uname -m);; esac) && \
    curl -fsSL -o /usr/local/bin/kubectl https://dl.k8s.io/release/${KUBECTL_VERSION}/bin/linux/${ARCH}/kubectl \
  && chmod +x /usr/local/bin/kubectl

RUN useradd -u 10001 -m nvsentinel

WORKDIR /opt/log-collector

COPY log-collector/entrypoint.sh /opt/log-collector/entrypoint.sh
COPY log-collector/mock-nvidia-bug-report.sh /opt/log-collector/mock-nvidia-bug-report.sh
COPY log-collector/mock-must-gather.sh /opt/log-collector/mock-must-gather.sh
RUN chmod +x /opt/log-collector/entrypoint.sh /opt/log-collector/mock-nvidia-bug-report.sh /opt/log-collector/mock-must-gather.sh


ENV PATH="/opt/log-collector:${PATH}"

ENTRYPOINT ["/opt/log-collector/entrypoint.sh"]
