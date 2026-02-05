# NVIDIA Device API: Go Client

The `client-go` module is the official Go SDK for interacting with the node-local NVIDIA Device API. It provides a Kubernetes-native developer experience for building node-level agents, telemetry sidecars, and operators **driven by local device state.**

By utilizing a node-local gRPC transport, this SDK allows agents to query device telemetry and status **without putting load on the central Kubernetes API server**. This architecture enables fine-grained **hardware monitoring** that scales independently of the cluster control plane.

> [!WARNING]
> **Experimental Preview Release**
> This is an experimental release of the NVIDIA Device API Go client. Use at your own risk in production environments. The software is provided "as is" without warranties of any kind. Features, APIs, and configurations may change without notice in future releases. For production deployments, thoroughly test in non-critical environments first.
>
> **Capabilities (Read-Only)**
> * ✅ **Supported**: `Get`, `List`, `Watch`
> * ❌ **Unsupported**: `Create`, `Update`, `UpdateStatus`, `Patch`, `Delete`

## Features

- **Kubernetes-Native API**: Provides generated versioned clientsets, informers, and listers that mirror standard K8s interfaces.
- **gRPC Transport**: Optimized for low-latency, local communication via Unix domain sockets (UDS).
- **controller-runtime Integration**: Supports **Informer Injection** to drive standard Reconcilers with local gRPC streams.
- **Observability**: Integrated Prometheus metrics, error logging, and support for structured logging.

## Installation

```bash
go get github.com/nvidia/nvsentinel/client-go
```

## Quick Start

The following snippet demonstrates how to initialize the client and retrieve a list of GPUs from the local node.

```go
package main

import (
    "context"
    "fmt"
    "log"

    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
    "k8s.io/apimachinery/pkg/api/meta"
    "github.com/nvidia/nvsentinel/client-go/clientset/versioned"
    "github.com/nvidia/nvsentinel/client-go/nvgrpc"
)
func main() {
    config := &nvgrpc.Config{Target: "unix:///var/run/nvidia-device-api/device-api.sock"}
    clientset := versioned.NewForConfigOrDie(config)

    gpus, err := clientset.DeviceV1alpha1().GPUs().List(context.Background(), metav1.ListOptions{})
    if err != nil {
        log.Fatalf("failed to list GPUs: %v", err)
    }

    for _, gpu := range gpus.Items {
        isReady := meta.IsStatusConditionTrue(gpu.Status.Conditions, "Ready")
        fmt.Printf("GPU: %s | Ready: %v\n", gpu.Name, isReady)
    }
}
```

## Integration Patterns

| Pattern | Focus | Description |
| :--- | :--- | :--- |
| **[Basic Client](./examples/client)** | **Point-in-Time** | Initializing the clientset, listing resources, and inspecting status. |
| **[Watch Monitor](./examples/watch)** | **Event-Driven** | Using gRPC interceptors and asynchronous `Watch` streams. |
| **[Controller](./examples/controller)** | **State Enforcement** | Driving a `controller-runtime` reconciler with node-local gRPC data. |
| **[Fake Client](./examples/fake-client)** | **Unit Testing** | Testing logic using the in-memory `ObjectTracker`. |

## Deployment

Clients are typically deployed as a **DaemonSet**. The following configuration is required to ensure connectivity to the host's NVIDIA Device API socket.

### Volume Mounts

```yaml
volumeMounts:
- name: device-api-socket
  mountPath: /var/run/nvidia-device-api
  readOnly: true
volumes:
- name: device-api-socket
  hostPath:
    path: /var/run/nvidia-device-api
    type: DirectoryOrCreate
```

### Configuration

| Variable | Description | Default |
| :--- | :--- | :--- |
| **`NVIDIA_DEVICE_API_TARGET`** | gRPC target URI (e.g., `unix:///...`) | `unix:///var/run/nvidia-device-api/device-api.sock` |

> [!NOTE]
> The container user must have filesystem permissions to read/write to the Unix socket file.

## Development

Refer to this module's [Development Guide](./DEVELOPMENT.md) for instructions on building the client and running the code generation pipeline.

---
