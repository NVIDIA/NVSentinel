FROM ubuntu:noble

RUN apt-get update && \
    apt-get install -y python3 python3-pip curl git wget unzip && \
    pip install --break-system-packages poetry==1.8.2 && \
    curl https://raw.githubusercontent.com/helm/helm/main/scripts/get-helm-3 | bash && \
    helm plugin install https://github.com/chartmuseum/helm-push && \
    wget https://go.dev/dl/go1.22.4.linux-amd64.tar.gz && tar -C /usr/local -xzf go1.22.4.linux-amd64.tar.gz

ENV PATH="${PATH}:/usr/local/go/bin:/root/go/bin"

RUN go install github.com/boumenot/gocover-cobertura@latest && \
    go install gotest.tools/gotestsum@latest && \
    curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh | sh -s -- -b $(go env GOPATH)/bin v1.59.1

RUN wget https://github.com/protocolbuffers/protobuf/releases/download/v27.1/protoc-27.1-linux-x86_64.zip && \
    unzip protoc-27.1-linux-x86_64.zip -d protoc-27.1-linux-x86_64 && \
    cp protoc-27.1-linux-x86_64/bin/protoc /usr/local/bin/ && mkdir -p /usr/local/bin/include/google && cp -r protoc-27.1-linux-x86_64/include/google /usr/local/bin/include && \
    go install google.golang.org/protobuf/cmd/protoc-gen-go@v1.28.1 && go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@v1.2 && \
    python3 -m pip install --break-system-packages grpcio grpcio-tools

RUN apt-get update && apt-get install -y wget && \
    wget https://developer.download.nvidia.com/compute/cuda/repos/debian12/x86_64/cuda-keyring_1.1-1_all.deb && \
    dpkg -i cuda-keyring_1.1-1_all.deb && rm cuda-keyring_1.1-1_all.deb && \
    apt-get update && apt-get install -y datacenter-gpu-manager=1:3.3.5 && \
    apt-get clean

ENV PYTHONPATH=/usr/local/dcgm/bindings/python3 \
    PYTHONUNBUFFERED=1