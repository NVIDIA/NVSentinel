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

# Collector image: executes nvidia-bug-report(.sh) inside the driver daemonset pod,
# runs GPU Operator must-gather, and optionally collects GCP SOS reports on GCP instances;
# optionally uploads artifacts to a file server.
# Base: Ubuntu for minimal, stable tooling (bash, curl, tar, gzip, sudo)

FROM public.ecr.aws/docker/library/ubuntu:22.04

ARG DEBIAN_FRONTEND=noninteractive
ARG KUBECTL_VERSION=v1.28.3

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

RUN curl -fsSL -o /usr/local/bin/kubectl https://storage.googleapis.com/kubernetes-release/release/${KUBECTL_VERSION}/bin/linux/amd64/kubectl \
  && chmod +x /usr/local/bin/kubectl

RUN useradd -u 10001 -m nvsentinel

WORKDIR /opt/nvsentinel-log-collector

COPY entrypoint.sh /opt/nvsentinel-log-collector/entrypoint.sh
RUN chmod +x /opt/nvsentinel-log-collector/entrypoint.sh

ENV PATH="/opt/nvsentinel-log-collector:${PATH}"

ENTRYPOINT ["/opt/nvsentinel-log-collector/entrypoint.sh"]
