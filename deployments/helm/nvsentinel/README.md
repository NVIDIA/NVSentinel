# Device API Server Helm Chart

Node-local GPU device state cache server for Kubernetes.

## Introduction

The Device API Server is a DaemonSet that runs on each GPU node, providing a local gRPC cache for GPU device states. It acts as an intermediary between:

- **Providers** (health monitors) that update GPU device states
- **Consumers** (device plugins, DRA drivers) that read device states for scheduling decisions

Key features:

- Read-blocking semantics during provider updates
- Multiple provider and consumer support
- Optional NVML fallback provider for GPU enumeration and XID monitoring
- Prometheus metrics and alerting
- Unix socket for node-local communication

## Prerequisites

- Kubernetes 1.25+
- Helm 3.0+
- (Optional) NVIDIA GPU Operator for NVML provider support
- (Optional) Prometheus Operator for ServiceMonitor/PrometheusRule

## Installation

### Quick Start

```bash
# Add the Helm repository (when published)
helm repo add device-api https://nvidia.github.io/device-api
helm repo update

# Install with default configuration
helm install device-api-server device-api/device-api-server \
  --namespace device-api --create-namespace
```

### Install from Local Chart

```bash
helm install device-api-server ./charts/device-api-server \
  --namespace device-api --create-namespace
```

### Install with NVML Provider

To enable built-in GPU enumeration and health monitoring via NVML:

```bash
helm install device-api-server ./charts/device-api-server \
  --namespace device-api --create-namespace \
  --set nvml.enabled=true
```

> **Note**: NVML provider requires the `nvidia` RuntimeClass. Install the NVIDIA GPU Operator or create it manually.

### Install with Prometheus Monitoring

```bash
helm install device-api-server ./charts/device-api-server \
  --namespace device-api --create-namespace \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.prometheusRule.enabled=true
```

## Configuration

See [values.yaml](values.yaml) for the full list of configurable parameters.

### Key Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Image repository | `ghcr.io/nvidia/device-api-server` |
| `image.tag` | Image tag | Chart appVersion |
| `server.grpcAddress` | gRPC server address | `:50051` |
| `server.unixSocket` | Unix socket path | `/var/run/device-api/device.sock` |
| `server.healthPort` | Health endpoint port | `8081` |
| `server.metricsPort` | Metrics endpoint port | `9090` |
| `nvml.enabled` | Enable NVML fallback provider | `false` |
| `nvml.driverRoot` | NVIDIA driver library root | `/run/nvidia/driver` |
| `nvml.healthCheckEnabled` | Enable XID event monitoring | `true` |
| `runtimeClassName` | Pod RuntimeClass | `""` (auto `nvidia` if nvml.enabled) |
| `nodeSelector` | Node selector | `nvidia.com/gpu.present: "true"` |
| `metrics.serviceMonitor.enabled` | Create ServiceMonitor | `false` |
| `metrics.prometheusRule.enabled` | Create PrometheusRule | `false` |

### Resource Configuration

```yaml
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi
```

### NVML Provider Configuration

```yaml
nvml:
  enabled: true
  driverRoot: /run/nvidia/driver
  additionalIgnoredXids: ""  # Comma-separated list of XIDs to ignore
  healthCheckEnabled: true
```

Default ignored XIDs (application errors): 13, 31, 43, 45, 68, 109

### Node Scheduling

By default, the DaemonSet schedules only on nodes with `nvidia.com/gpu.present=true` label:

```yaml
nodeSelector:
  nvidia.com/gpu.present: "true"

tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule
```

Override for custom environments:

```bash
helm install device-api-server ./charts/device-api-server \
  --set 'nodeSelector.node-type=gpu' \
  --set 'nodeSelector.nvidia\.com/gpu\.present=null'
```

## Metrics

The server exposes Prometheus metrics at `/metrics` on the configured `metricsPort`.

### Available Metrics

| Metric | Type | Description |
|--------|------|-------------|
| `device_api_server_info` | Gauge | Server information |
| `device_api_server_cache_gpus_total` | Gauge | Total GPUs in cache |
| `device_api_server_cache_gpus_healthy` | Gauge | Healthy GPUs |
| `device_api_server_cache_gpus_unhealthy` | Gauge | Unhealthy GPUs |
| `device_api_server_cache_updates_total` | Counter | Cache update operations |
| `device_api_server_watch_streams_active` | Gauge | Active watch streams |
| `device_api_server_watch_events_total` | Counter | Watch events sent |
| `device_api_server_nvml_provider_enabled` | Gauge | NVML provider status |
| `device_api_server_nvml_gpu_count` | Gauge | GPUs discovered by NVML |

### Alerting Rules

When `metrics.prometheusRule.enabled=true`, the following alerts are configured:

| Alert | Severity | Description |
|-------|----------|-------------|
| `DeviceAPIServerDown` | Critical | Server unreachable for 5m |
| `DeviceAPIServerHighLatency` | Warning | P99 latency > 500ms |
| `DeviceAPIServerHighErrorRate` | Warning | Error rate > 10% |
| `DeviceAPIServerUnhealthyGPUs` | Warning | Unhealthy GPUs detected |
| `DeviceAPIServerNoGPUs` | Warning | No GPUs registered for 10m |
| `DeviceAPIServerNVMLProviderDown` | Warning | NVML provider not running |

## Client Connection

Clients on the same node can connect via:

### Unix Socket (Recommended)

```go
conn, err := grpc.Dial(
    "unix:///var/run/device-api/device.sock",
    grpc.WithInsecure(),
)
```

### TCP

```go
conn, err := grpc.Dial(
    "localhost:50051",
    grpc.WithInsecure(),
)
```

### grpcurl Examples

```bash
# List available services
grpcurl -plaintext localhost:50051 list

# List GPUs
grpcurl -plaintext localhost:50051 nvidia.device.v1alpha1.GpuService/ListGpus

# Watch GPU changes
grpcurl -plaintext localhost:50051 nvidia.device.v1alpha1.GpuService/WatchGpus
```

## Upgrading

```bash
helm upgrade device-api-server ./charts/device-api-server \
  --namespace device-api \
  --reuse-values \
  --set image.tag=v0.2.0
```

## Uninstallation

```bash
helm uninstall device-api-server --namespace device-api
```

## Troubleshooting

### Pod Not Scheduling

Check node labels:

```bash
kubectl get nodes --show-labels | grep gpu
```

Ensure nodes have `nvidia.com/gpu.present=true` or override `nodeSelector`.

### NVML Provider Fails to Start

1. Verify RuntimeClass exists:

   ```bash
   kubectl get runtimeclass nvidia
   ```

2. Check NVIDIA driver is installed on nodes:

   ```bash
   kubectl debug node/<node-name> -it --image=nvidia/cuda:12.0-base -- nvidia-smi
   ```

3. Check pod logs for NVML errors:

   ```bash
   kubectl logs -n device-api -l app.kubernetes.io/name=device-api-server
   ```

### Permission Denied on Unix Socket

If using custom security contexts, ensure the socket directory is writable:

```yaml
securityContext:
  runAsUser: 0  # May be needed for hostPath access
  runAsNonRoot: false
```

## License

Copyright (c) 2025, NVIDIA CORPORATION. All rights reserved.

Licensed under the Apache License, Version 2.0.
