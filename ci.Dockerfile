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
