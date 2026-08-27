# Tutorial: Writing a Preflight Check

This tutorial walks you through building **`preflight-demo-gpu-visible`** — a preflight check that runs `nvidia-smi -L`, requires at least `MIN_GPU_COUNT` GPUs, reports via gRPC, and exits — from an empty Go module to a container image registered in the preflight webhook and verified on a GPU pod.

By the end you will have:

- A working init-container check that validates GPUs are visible before the workload starts.
- A container image for it, registered in the preflight Helm chart.
- Verification that a failing check blocks pod startup and emits a health event.

> **Who is this for?** Teams **already running NVSentinel preflight** who want to add a custom diagnostic (or ship a third-party image) without changing the webhook code.

> **Just want the AI to do it?** Jump to [Appendix: One-shot AI prompt](#appendix-one-shot-ai-prompt).

## Prerequisites

- `go` 1.27+ and `docker` — build the check image.
- `kubectl` with access to a cluster where:
  - NVSentinel is running (`platform-connectors` at minimum).
  - Preflight is enabled (`global.preflight.enabled: true`).
  - At least one namespace is labeled `nvsentinel.nvidia.com/preflight=enabled`.
  - You can schedule a GPU pod (device plugin or DRA).

See [Preflight overview](../preflight.md), [ADR-026](../designs/026-preflight-checks.md), and [Preflight configuration](../configuration/preflight.md) for how preflight works. [DEVELOPMENT.md](../../DEVELOPMENT.md) covers local dev cluster setup.

---

## 1. Scaffold the module

Create a standalone Go module (it can live in any repository; you only need the image URI at deploy time):

```bash
mkdir demo-preflight-check && cd demo-preflight-check
go mod init github.com/nvidia/demo-preflight-check
```

Add the NVSentinel protobuf module (not published as tagged releases — pin a commit):

```bash
export GOTOOLCHAIN=auto
go get github.com/nvidia/nvsentinel/data-models@a3fb47d4
```

---

## 2. Write the check

Create `main.go`. Structure: parse env → run diagnostic → send health event → exit.

```go
package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	agentName      = "preflight-demo-gpu-visible"
	componentClass = "Node"
	checkName      = "GPUVisibility"

	exitSuccess        = 0
	exitTestFailed     = 1
	exitConfigError    = 2
	exitSendEventError = 3
)

func main() {
	os.Exit(run())
}

func run() int {
	ctx := context.Background()

	minGPUs, err := parsePositiveInt("MIN_GPU_COUNT", 1)
	if err != nil {
		slog.Error("config error", "error", err)
		return exitConfigError
	}

	nodeName, err := requireEnv("NODE_NAME")
	if err != nil {
		return exitConfigError
	}

	socket, err := requireEnv("PLATFORM_CONNECTOR_SOCKET")
	if err != nil {
		return exitConfigError
	}

	strategy, err := parseProcessingStrategy()
	if err != nil {
		return exitConfigError
	}

	gpuLines, err := listGPUs(ctx)
	healthy := err == nil && len(gpuLines) >= minGPUs

	var message string
	var errorCode string
	if err != nil {
		message = fmt.Sprintf("nvidia-smi failed: %v", err)
		errorCode = "NVIDIA_SMI_ERROR"
	} else if len(gpuLines) < minGPUs {
		message = fmt.Sprintf("expected >= %d GPUs, found %d", minGPUs, len(gpuLines))
		errorCode = "NO_GPUS_VISIBLE"
	} else {
		message = fmt.Sprintf("found %d GPU(s): %s", len(gpuLines), strings.Join(gpuLines, "; "))
	}

	exitCode := exitSuccess
	if !healthy {
		exitCode = exitTestFailed
	}

	if sendErr := sendEvent(ctx, socket, nodeName, strategy, healthy, healthy == false, message, errorCode); sendErr != nil {
		slog.Error("failed to send health event", "error", sendErr)
		return exitSendEventError
	}

	if exitCode != exitSuccess && strategy == pb.ProcessingStrategy_STORE_ONLY {
		slog.Warn("check failed (STORE_ONLY — not blocking pod)")
		return exitSuccess
	}

	return exitCode
}

func listGPUs(ctx context.Context) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, "nvidia-smi", "-L")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}

	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	var gpus []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line != "" {
			gpus = append(gpus, line)
		}
	}
	return gpus, nil
}

func sendEvent(ctx context.Context, socket, nodeName string, strategy pb.ProcessingStrategy,
	isHealthy, isFatal bool, message, errorCode string) error {

	recommendedAction := pb.RecommendedAction_NONE
	if !isHealthy {
		recommendedAction = pb.RecommendedAction_CONTACT_SUPPORT
	}

	var codes []string
	if errorCode != "" {
		codes = []string{errorCode}
	}

	event := &pb.HealthEvent{
		Version:            1,
		Agent:              agentName,
		ComponentClass:     componentClass,
		CheckName:          checkName,
		IsHealthy:          isHealthy,
		IsFatal:            isFatal,
		Message:            message,
		RecommendedAction:  recommendedAction,
		ErrorCode:          codes,
		GeneratedTimestamp: timestamppb.Now(),
		NodeName:           nodeName,
		ProcessingStrategy: strategy,
	}

	events := &pb.HealthEvents{Version: 1, Events: []*pb.HealthEvent{event}}

	target := strings.TrimPrefix(socket, "unix://")
	conn, err := grpc.NewClient("unix://"+target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return err
	}
	defer conn.Close()

	client := pb.NewPlatformConnectorClient(conn)
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	_, err = client.HealthEventOccurredV1(ctx, events)
	return err
}

func parseProcessingStrategy() (pb.ProcessingStrategy, error) {
	raw := os.Getenv("PROCESSING_STRATEGY")
	if raw == "" {
		raw = "EXECUTE_REMEDIATION"
	}
	v, ok := pb.ProcessingStrategy_value[raw]
	if !ok {
		return 0, fmt.Errorf("invalid PROCESSING_STRATEGY: %s", raw)
	}
	return pb.ProcessingStrategy(v), nil
}

func parsePositiveInt(key string, defaultVal int) (int, error) {
	raw := os.Getenv(key)
	if raw == "" {
		return defaultVal, nil
	}
	i, err := strconv.Atoi(raw)
	if err != nil || i <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", key)
	}
	return i, nil
}

func requireEnv(key string) (string, error) {
	v := os.Getenv(key)
	if v == "" {
		return "", fmt.Errorf("%s is required", key)
	}
	return v, nil
}
```

Tidy and build (on a machine without `nvidia-smi`, `go build` still succeeds — the binary only shells out at runtime):

```bash
go mod tidy
go build ./...
```

For production code, copy the retry/backoff reporter from `preflight-checks/nccl-loopback/pkg/health/reporter.go` instead of the inline `sendEvent` above.

---

## 3. Containerize

```dockerfile
FROM public.ecr.aws/docker/library/golang:1.27.0-trixie AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o demo-preflight-check .

FROM nvcr.io/nvidia/cuda:12.8.1-base-ubuntu24.04
COPY --from=builder /src/demo-preflight-check /demo-preflight-check
ENTRYPOINT ["/demo-preflight-check"]
```

Build and push to a registry your cluster can pull from. Example using NGC (`nvcr.io`):

```bash
docker login nvcr.io
# Username: $oauthtoken
# Password: <your NGC API key>

docker build . -t nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible:dev
docker push nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible:dev
```

---

## 4. Register and deploy

Add your check to `distros/kubernetes/nvsentinel/charts/preflight/values.yaml`:
```yaml
initContainers:
  # ... existing checks ...
  - name: preflight-demo-gpu-visible
    defaultEnabled: false   # opt-in via annotation while testing
    image:
      repository: nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible
      tag: "dev"
    env:
      - name: MIN_GPU_COUNT
        value: "1"
    volumeMounts:
      - name: nvsentinel-socket
        mountPath: /var/run
```

Upgrade the release:

```bash
helm upgrade nvsentinel ./distros/kubernetes/nvsentinel \
  --namespace nvsentinel \
  --reuse-values \
  -f your-overrides.yaml

kubectl rollout status deploy/preflight-injector -n nvsentinel
```

Because `defaultEnabled: false`, pods must select this check via annotation (see [ADR-034](../designs/034-preflight-check-selection.md)):

```yaml
nvsentinel.nvidia.com/preflight-checks: "preflight-demo-gpu-visible"
```

---

## 5. Test and verify

### Manual container smoke test

On a GPU node where platform-connectors is running (or `docker run --gpus all` on that host):

```bash
docker run --rm --gpus all \
  -v /var/run/nvsentinel:/var/run \
  -e NODE_NAME=my-node \
  -e PLATFORM_CONNECTOR_SOCKET=unix:///var/run/nvsentinel.sock \
  -e PROCESSING_STRATEGY=STORE_ONLY \
  -e MIN_GPU_COUNT=1 \
  nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible:dev
echo "exit code: $?"
```

### Verify on a GPU pod

Label the namespace so the preflight webhook injects init containers:

```bash
kubectl label namespace training nvsentinel.nvidia.com/preflight=enabled --overwrite
```

```bash
kubectl apply -f - <<'YAML'
apiVersion: v1
kind: Pod
metadata:
  name: preflight-demo-test
  namespace: training
  annotations:
    nvsentinel.nvidia.com/preflight-checks: "preflight-demo-gpu-visible"
spec:
  restartPolicy: Never
  containers:
    - name: main
      image: nvcr.io/nvidia/cuda:12.8.1-base-ubuntu24.04
      command: ["sleep", "3600"]
      resources:
        limits:
          nvidia.com/gpu: "1"
YAML

kubectl get pod preflight-demo-test -n training -w
kubectl logs preflight-demo-test -n training -c preflight-demo-gpu-visible
```

To test the fail path, set `MIN_GPU_COUNT` unreasonably high in Helm, upgrade, and recreate the pod. The pod should stay `Init:Error`.

```bash
kubectl delete pod preflight-demo-test -n training
```

### E2E (in-repo checks only)

If you contributed the check under `preflight-checks/` in the NVSentinel repo (build context and Makefile differ — see [DEVELOPMENT.md](../../DEVELOPMENT.md#creating-a-new-preflight-check)):

```bash
make -C preflight-checks/{your-check} lint-test
cd tests && go test -tags=amd64_group -run TestPreflightEndToEnd -v ./...
```

---

## Appendix: One-shot AI prompt

Paste this to an AI coding agent. It is **self-contained**: the check is a standalone Go module and container image that can live in **any repository** (it does not need to be inside the NVSentinel repo).

```text
Create a new NVSentinel preflight check named "preflight-demo-gpu-visible" that validates GPU
visibility before a workload starts. It is a standalone Go program that can live in any repo;
follow this spec exactly.

Preflight checks are init containers injected by the NVSentinel preflight webhook into GPU pods.
They read config from env, run a diagnostic once, send a HealthEvent over gRPC to the
platform-connector Unix socket, then exit. Contract (do not duplicate long background prose in
output docs — just implement it):

- Exit codes: 0 pass, 1 check failed, 2 config error, 3 failed to send health event.
- Required webhook-injected env: NODE_NAME, PLATFORM_CONNECTOR_SOCKET (e.g.
  unix:///var/run/nvsentinel.sock), PROCESSING_STRATEGY (EXECUTE_REMEDIATION or STORE_ONLY).
- On failure: send HealthEvent via PlatformConnector.HealthEventOccurredV1 (proto package
  github.com/nvidia/nvsentinel/data-models/pkg/protos) before exiting non-zero.
- When PROCESSING_STRATEGY=STORE_ONLY: still send the unhealthy event, but exit 0 so the pod is
  not blocked.

Implementation:

- Scaffold: mkdir demo-preflight-check && cd demo-preflight-check; go mod init
  github.com/nvidia/demo-preflight-check; export GOTOOLCHAIN=auto (Go 1.27+); go get
  github.com/nvidia/nvsentinel/data-models@a3fb47d4
- main.go: parse MIN_GPU_COUNT (default 1), NODE_NAME, PLATFORM_CONNECTOR_SOCKET,
  PROCESSING_STRATEGY from env; run `nvidia-smi -L`; pass when visible GPU count >= MIN_GPU_COUNT;
  send HealthEvent on both pass and fail with agent="preflight-demo-gpu-visible",
  componentClass="Node", checkName="GPUVisibility", nodeName from NODE_NAME,
  processingStrategy from PROCESSING_STRATEGY; on failure set isHealthy=false, isFatal=true,
  recommendedAction=CONTACT_SUPPORT, errorCode NVIDIA_SMI_ERROR or NO_GPUS_VISIBLE; on success
  isHealthy=true, isFatal=false, recommendedAction=NONE.
- Multi-stage Dockerfile (build context = demo-preflight-check directory):
  builder public.ecr.aws/docker/library/golang:1.27.0-trixie, runtime
  nvcr.io/nvidia/cuda:12.8.1-base-ubuntu24.04 (needs nvidia-smi), ENTRYPOINT the built binary.
- Build/push example (NGC):
    docker login nvcr.io   # Username: $oauthtoken, Password: <NGC API key>
    docker build . -t nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible:dev
    docker push nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible:dev
- Helm registration snippet for distros/kubernetes/nvsentinel/charts/preflight/values.yaml under
  initContainers: name preflight-demo-gpu-visible, defaultEnabled: false, image.repository
  nvcr.io/nv-ngc-devops/preflight-demo-gpu-visible, image.tag dev, env MIN_GPU_COUNT=1,
  volumeMounts nvsentinel-socket -> /var/run. Upgrade:
    helm upgrade nvsentinel ./distros/kubernetes/nvsentinel \
      --namespace nvsentinel --reuse-values -f your-overrides.yaml
    kubectl rollout status deploy/preflight-injector -n nvsentinel
- Verification manifest: GPU Pod in a preflight-enabled namespace (e.g. training) with annotation
  nvsentinel.nvidia.com/preflight-checks: "preflight-demo-gpu-visible", nvidia.com/gpu: "1",
  then kubectl logs on init container preflight-demo-gpu-visible.

Ensure `go mod tidy` and `go build ./...` pass. Do not run docker push, helm upgrade, or kubectl
apply unless credentials/cluster are available — but do create the Dockerfile and YAML snippets.
```
