# Building the Event Generator

## Prerequisites

- Go 1.25+
- Docker
- Access to a container registry

## Build Steps

```bash
# 1. Navigate to event-generator directory
cd tests/scale-tests/event-generator

# 2. Build binary (disable CGO for Alpine container compatibility)
go mod tidy
CGO_ENABLED=0 go build -o event-generator .

# 3. Build Docker image (replace YOUR_REGISTRY with your registry)
docker build -t YOUR_REGISTRY/event-generator:v1 .

# 4. Push to your registry
docker login  # if needed
docker push YOUR_REGISTRY/event-generator:v1

# 5. Update manifests to use your registry
cd ../manifests
sed -i 's|ghcr.io/nvidia/nvsentinel|YOUR_REGISTRY|g' event-generator-daemonset.yaml
```

Done! The same image works for all test scenarios (light/medium/heavy) - just change the ConfigMap.

See [README.md](README.md) for deployment instructions.
