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

FROM ubuntu:noble

RUN apt-get update && \
    apt-get install -y python3 python3-pip curl git wget unzip && \
    pip install --break-system-packages poetry==1.8.2 && \
    curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash && \
    helm plugin install https://github.com/chartmuseum/helm-push && \
    wget -q https://go.dev/dl/go1.24.8.linux-amd64.tar.gz && tar -C /usr/local -xzf go1.24.8.linux-amd64.tar.gz

ENV PATH="${PATH}:/usr/local/go/bin:/root/go/bin"

RUN go install github.com/boumenot/gocover-cobertura@latest && \
    go install gotest.tools/gotestsum@latest && \
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v1.64.8

RUN wget -q https://github.com/protocolbuffers/protobuf/releases/download/v27.1/protoc-27.1-linux-x86_64.zip && \
    unzip protoc-27.1-linux-x86_64.zip -d protoc-27.1-linux-x86_64 && \
    cp protoc-27.1-linux-x86_64/bin/protoc /usr/local/bin/ && mkdir -p /usr/local/bin/include/google && cp -r protoc-27.1-linux-x86_64/include/google /usr/local/bin/include && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.36.6 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.3.0 && \
    python3 -m pip install --break-system-packages grpcio grpcio-tools black

RUN apt-get update && apt-get install -y wget && \
    wget -q https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/cuda-keyring_1.1-1_all.deb && \
    dpkg -i cuda-keyring_1.1-1_all.deb && rm cuda-keyring_1.1-1_all.deb && \
    apt-get update && apt-get install -y datacenter-gpu-manager=1:3.3.5 && \
    apt-get clean

RUN curl -sSL "https://github.com/koalaman/shellcheck/releases/download/v0.11.0/shellcheck-v0.11.0.linux.x86_64.tar.xz" | \
    tar -xJ --wildcards -C /usr/local/bin/ --strip-components=1 "*/shellcheck" && \
    chmod +x /usr/local/bin/shellcheck

RUN go install github.com/google/addlicense@latest && \
    go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest

ENV PYTHONPATH=/usr/local/dcgm/bindings/python3 \
    PYTHONUNBUFFERED=1
