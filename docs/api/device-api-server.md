# Device API Server - API Reference

This document provides the complete API reference for the Device API Server gRPC services.

## Overview

The Device API Server exposes a unified `GpuService` that provides both read and write operations following Kubernetes API conventions:

| Operation Type | Methods | Clients |
|----------------|---------|---------|
| Read | `GetGpu`, `ListGpus`, `WatchGpus` | Consumers (device plugins, DRA drivers) |
| Write | `CreateGpu`, `UpdateGpu`, `UpdateGpuStatus`, `DeleteGpu` | Providers (health monitors, NVML) |

**Package**: `nvidia.device.v1alpha1`

**Connection Endpoints**:
- Unix Socket: `unix:///var/run/device-api/device.sock` (recommended)
- TCP: `localhost:50051`

## GpuService

The `GpuService` provides a unified API for GPU resource management:

- **Read operations** (`GetGpu`, `ListGpus`, `WatchGpus`) for consumers
- **Write operations** (`CreateGpu`, `UpdateGpu`, `UpdateGpuStatus`, `DeleteGpu`) for providers

> **Important**: Write operations acquire exclusive locks, blocking all consumer reads until completion. This prevents consumers from reading stale "healthy" states during GPU health transitions.

### Read Operations

### GetGpu

Retrieves a single GPU resource by its unique name.

```protobuf
rpc GetGpu(GetGpuRequest) returns (GetGpuResponse);
```

**Request**:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | The unique resource name of the GPU |

**Response**:

| Field | Type | Description |
|-------|------|-------------|
| `gpu` | Gpu | The requested GPU resource |

**Errors**:
- `NOT_FOUND`: GPU with the specified name does not exist

**Example**:

```bash
grpcurl -plaintext localhost:50051 \
  -d '{"name": "gpu-abc123"}' \
  nvidia.device.v1alpha1.GpuService/GetGpu
```

### ListGpus

Retrieves a list of all GPU resources.

```protobuf
rpc ListGpus(ListGpusRequest) returns (ListGpusResponse);
```

**Request**: Empty (reserved for future filtering/pagination)

**Response**:

| Field | Type | Description |
|-------|------|-------------|
| `gpu_list` | GpuList | List of all GPU resources |

**Example**:

```bash
grpcurl -plaintext localhost:50051 \
  nvidia.device.v1alpha1.GpuService/ListGpus
```

**Response Example**:

```json
{
  "gpuList": {
    "items": [
      {
        "name": "gpu-abc123",
        "spec": {
          "uuid": "GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"
        },
        "status": {
          "conditions": [
            {
              "type": "Ready",
              "status": "True",
              "lastTransitionTime": "2026-01-21T10:00:00Z",
              "reason": "GPUHealthy",
              "message": "GPU is healthy and available"
            }
          ]
        },
        "resourceVersion": "42"
      }
    ]
  }
}
```

### WatchGpus

Streams lifecycle events for GPU resources. The stream remains open until the client disconnects or an error occurs.

```protobuf
rpc WatchGpus(WatchGpusRequest) returns (stream WatchGpusResponse);
```

**Request**: Empty (reserved for future filtering/resumption)

**Response Stream**:

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Event type: `ADDED`, `MODIFIED`, `DELETED`, `ERROR` |
| `object` | Gpu | The GPU resource (last known state for DELETED) |

**Event Types**:

| Type | Description |
|------|-------------|
| `ADDED` | GPU was registered or first observed |
| `MODIFIED` | GPU status was updated |
| `DELETED` | GPU was unregistered |
| `ERROR` | An error occurred in the watch stream |

**Example**:

```bash
grpcurl -plaintext localhost:50051 \
  nvidia.device.v1alpha1.GpuService/WatchGpus
```

**Behavior**:
- On connection, receives `ADDED` events for all existing GPUs
- Subsequent events reflect real-time changes
- Stream is per-client; multiple clients can watch simultaneously

### Write Operations

#### CreateGpu

Creates a new GPU resource. This is the standard way for providers to register GPUs.

```protobuf
rpc CreateGpu(CreateGpuRequest) returns (CreateGpuResponse);
```

**Request**:

| Field | Type | Description |
|-------|------|-------------|
| `gpu` | Gpu | The GPU to create (metadata.name and spec.uuid required) |

**Response**:

| Field | Type | Description |
|-------|------|-------------|
| `gpu` | Gpu | The created GPU with server-assigned fields |
| `created` | bool | True if new GPU was created, false if already existed |

**Errors**:
- `INVALID_ARGUMENT`: Required fields missing

**Behavior**:
- If GPU already exists, returns existing GPU (idempotent)
- Triggers `ADDED` event for active watch streams

**Example**:

```bash
grpcurl -plaintext localhost:50051 \
  -d '{
    "gpu": {
      "metadata": {"name": "gpu-abc123"},
      "spec": {"uuid": "GPU-a1b2c3d4-e5f6-a7b8-c9d0-e1f2a3b4c5d6"}
    }
  }' \
  nvidia.device.v1alpha1.GpuService/CreateGpu
```

#### UpdateGpu

Replaces an entire GPU resource (spec and status).

```protobuf
rpc UpdateGpu(UpdateGpuRequest) returns (Gpu);
```

**Request**:

| Field | Type | Description |
|-------|------|-------------|
| `gpu` | Gpu | The GPU to update (metadata.name required) |

**Response**: The updated GPU resource.

**Errors**:
- `NOT_FOUND`: GPU does not exist
- `ABORTED`: Resource version conflict (optimistic concurrency)

**Behavior**:
- Uses optimistic concurrency via `resource_version`
- Triggers `MODIFIED` event for active watch streams

#### UpdateGpuStatus

Updates only the status of an existing GPU (follows Kubernetes subresource pattern).

```protobuf
rpc UpdateGpuStatus(UpdateGpuStatusRequest) returns (Gpu);
```

**Request**:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | The GPU name to update |
| `status` | GpuStatus | New status (completely replaces existing) |
| `resource_version` | int64 | Optional: expected version for conflict detection |

**Response**: The updated GPU resource.

**Errors**:
- `NOT_FOUND`: GPU does not exist
- `ABORTED`: Resource version conflict (optimistic concurrency)

**Locking**: Acquires exclusive write lock, blocking all reads.

**Example** (mark GPU unhealthy due to XID error):

```bash
grpcurl -plaintext localhost:50051 \
  -d '{
    "name": "gpu-abc123",
    "status": {
      "conditions": [{
        "type": "Ready",
        "status": "False",
        "reason": "XidError",
        "message": "Critical XID error 79 detected"
      }]
    }
  }' \
  nvidia.device.v1alpha1.GpuService/UpdateGpuStatus
```

#### DeleteGpu

Removes a GPU from the server.

```protobuf
rpc DeleteGpu(DeleteGpuRequest) returns (google.protobuf.Empty);
```

**Request**:

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique identifier of GPU to remove |

**Response**: Empty on success.

**Errors**:
- `NOT_FOUND`: GPU does not exist

**Behavior**:
- GPU will no longer appear in ListGpus/GetGpu responses
- Triggers `DELETED` event for active watch streams

**Example**:

```bash
grpcurl -plaintext localhost:50051 \
  -d '{"name": "gpu-abc123"}' \
  nvidia.device.v1alpha1.GpuService/DeleteGpu
```

---

## Resource Types

### Gpu

The main GPU resource following the Kubernetes Resource Model pattern.

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Unique logical identifier |
| `spec` | GpuSpec | Identity and desired attributes |
| `status` | GpuStatus | Most recently observed state |
| `resource_version` | int64 | Monotonically increasing version |

### GpuSpec

Defines the identity of a GPU.

| Field | Type | Description |
|-------|------|-------------|
| `uuid` | string | Physical hardware UUID (e.g., `GPU-a1b2c3d4-...`) |

### GpuStatus

Contains the observed state of a GPU.

| Field | Type | Description |
|-------|------|-------------|
| `conditions` | Condition[] | Current state observations |
| `recommended_action` | string | Suggested resolution for negative states |

### Condition

Describes one aspect of the GPU's current state.

| Field | Type | Description |
|-------|------|-------------|
| `type` | string | Category (e.g., `Ready`, `MemoryHealthy`) |
| `status` | string | `True`, `False`, or `Unknown` |
| `last_transition_time` | Timestamp | When status last changed |
| `reason` | string | Machine-readable reason (UpperCamelCase) |
| `message` | string | Human-readable details |

**Standard Condition Types**:

| Type | Description |
|------|-------------|
| `Ready` | Overall GPU health and availability |
| `MemoryHealthy` | GPU memory is functioning correctly |
| `ThermalHealthy` | GPU temperature is within safe limits |

---

## Go Client Example

```go
package main

import (
    "context"
    "log"

    v1alpha1 "github.com/nvidia/nvsentinel/api/gen/go/device/v1alpha1"
    "google.golang.org/grpc"
    "google.golang.org/grpc/credentials/insecure"
)

func main() {
    // Connect via Unix socket (recommended)
    conn, err := grpc.NewClient(
        "unix:///var/run/device-api/device.sock",
        grpc.WithTransportCredentials(insecure.NewCredentials()),
    )
    if err != nil {
        log.Fatalf("failed to connect: %v", err)
    }
    defer conn.Close()

    client := v1alpha1.NewGpuServiceClient(conn)

    // Consumer: List GPUs
    resp, err := client.ListGpus(context.Background(), &v1alpha1.ListGpusRequest{})
    if err != nil {
        log.Fatalf("failed to list GPUs: %v", err)
    }

    for _, gpu := range resp.GpuList.Items {
        log.Printf("GPU: %s, Version: %d", gpu.Metadata.Name, gpu.Metadata.ResourceVersion)
        for _, cond := range gpu.Status.Conditions {
            log.Printf("  Condition: %s=%s (%s)", cond.Type, cond.Status, cond.Reason)
        }
    }

    // Provider: Update GPU status
    _, err = client.UpdateGpuStatus(context.Background(),
        &v1alpha1.UpdateGpuStatusRequest{
            Name: "gpu-abc123",
            Status: &v1alpha1.GpuStatus{
                Conditions: []*v1alpha1.Condition{{
                    Type:    "Ready",
                    Status:  "False",
                    Reason:  "XidError",
                    Message: "Critical XID 79 detected",
                }},
            },
        })
    if err != nil {
        log.Fatalf("failed to update status: %v", err)
    }
}
```

---

## Error Codes

| Code | Meaning |
|------|---------|
| `NOT_FOUND` | GPU with specified name does not exist |
| `INVALID_ARGUMENT` | Request contains invalid parameters |
| `ABORTED` | Resource version conflict (optimistic concurrency) |
| `INTERNAL` | Server-side error occurred |
| `UNAVAILABLE` | Server is temporarily unavailable |

---

## See Also

- [Operations Guide](../operations/device-api-server.md)
- [Design Document](../design/device-api-server.md)
- [NVML Fallback Provider](../design/nvml-fallback-provider.md)
