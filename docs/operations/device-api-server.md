# Device API Server - Operations Guide

This guide covers deployment, configuration, monitoring, and troubleshooting of the Device API Server.

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────┐
│                        GPU Node                              │
│  ┌─────────────────────────────────────────────────────────┐│
│  │                Device API Server (DaemonSet)            ││
│  │  ┌─────────────────────────────────────────────────┐   ││
│  │  │               GpuService (unified)              │   ││
│  │  │  Read:  GetGpu, ListGpus, WatchGpus             │   ││
│  │  │  Write: CreateGpu, UpdateGpuStatus, DeleteGpu   │   ││
│  │  └────────────────────┬────────────────────────────┘   ││
│  │                       │                                 ││
│  │                       ▼                                 ││
│  │  ┌─────────────────────────────────────────────────────┐││
│  │  │                  GPU Cache (RWMutex)                │││
│  │  │  - Read-blocking during writes                      │││
│  │  │  - Watch event broadcasting                         │││
│  │  └─────────────────────────────────────────────────────┘││
│  │                       ▲                                 ││
│  │                       │ (gRPC client)                   ││
│  │  ┌────────────────────┴────────────────────────────┐   ││
│  │  │            NVML Provider (optional)             │   ││
│  │  │  - GPU enumeration via CreateGpu                │   ││
│  │  │  - XID monitoring via UpdateGpuStatus           │   ││
│  │  └─────────────────────────────────────────────────┘   ││
│  └─────────────────────────────────────────────────────────┘│
│                                                              │
│  Clients:                                                    │
│  - Device plugins (consumer)                                 │
│  - DRA drivers (consumer)                                    │
│  - External health monitors (provider)                       │
└─────────────────────────────────────────────────────────────┘
```

## Deployment

### Prerequisites

- Kubernetes 1.25+
- Helm 3.0+
- GPU nodes with label `nvidia.com/gpu.present=true`
- (Optional) NVIDIA GPU Operator for NVML provider
- (Optional) Prometheus Operator for monitoring

### Installation

**Basic Installation**:

```bash
helm install device-api-server ./charts/device-api-server \
  --namespace device-api --create-namespace
```

**With NVML Provider**:

```bash
helm install device-api-server ./charts/device-api-server \
  --namespace device-api --create-namespace \
  --set nvml.enabled=true
```

**With Prometheus Monitoring**:

```bash
helm install device-api-server ./charts/device-api-server \
  --namespace device-api --create-namespace \
  --set metrics.serviceMonitor.enabled=true \
  --set metrics.prometheusRule.enabled=true
```

### Verify Installation

```bash
# Check DaemonSet status
kubectl get daemonset -n device-api

# Check pods are running on GPU nodes
kubectl get pods -n device-api -o wide

# Check logs
kubectl logs -n device-api -l app.kubernetes.io/name=device-api-server
```

---

## Configuration

### Command-Line Flags

| Flag | Default | Description |
|------|---------|-------------|
| `--grpc-address` | `127.0.0.1:50051` | TCP address for gRPC server (localhost only by default) |
| `--unix-socket` | `/var/run/device-api/device.sock` | Unix socket path |
| `--health-port` | `8081` | HTTP port for health endpoints |
| `--metrics-port` | `9090` | HTTP port for Prometheus metrics |
| `--shutdown-timeout` | `30` | Graceful shutdown timeout (seconds) |
| `--shutdown-delay` | `5` | Pre-shutdown delay (seconds) |
| `--log-format` | `json` | Log format: `text` or `json` |
| `-v` | `0` | Log verbosity level |
| `--enable-nvml-provider` | `false` | Enable NVML fallback provider |
| `--nvml-driver-root` | `/run/nvidia/driver` | NVIDIA driver library path |
| `--nvml-health-check` | `true` | Enable XID event monitoring |
| `--nvml-ignored-xids` | `` | Additional XIDs to ignore (comma-separated) |

### Helm Values

See [charts/device-api-server/values.yaml](../../charts/device-api-server/values.yaml) for the complete reference.

Key configuration sections:

```yaml
# Server configuration
server:
  grpcAddress: "127.0.0.1:50051"  # localhost only by default for security
  unixSocket: /var/run/device-api/device.sock
  healthPort: 8081
  metricsPort: 9090

# NVML provider (built-in GPU discovery)
nvml:
  enabled: false
  driverRoot: /run/nvidia/driver
  healthCheckEnabled: true
  additionalIgnoredXids: ""

# Node scheduling
nodeSelector:
  nvidia.com/gpu.present: "true"

# Resources
resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi
```

---

## NVML Provider

The NVML provider enables built-in GPU enumeration and health monitoring without external providers.

### How It Works

1. **RuntimeClass**: Uses the `nvidia` RuntimeClass to inject NVIDIA driver libraries
2. **No GPU Consumption**: Sets `NVIDIA_DRIVER_CAPABILITIES=utility` to access NVML without consuming GPU resources
3. **XID Monitoring**: Listens for XID events and marks GPUs unhealthy for critical errors

### Configuration

```yaml
nvml:
  enabled: true
  driverRoot: /run/nvidia/driver  # Container path to driver libs
  healthCheckEnabled: true         # Enable XID event monitoring
  additionalIgnoredXids: "13,31"   # Additional XIDs to ignore
```

### Default Ignored XIDs

These XIDs are ignored by default (application errors, not hardware issues):

| XID | Description |
|-----|-------------|
| 13 | Graphics Engine Exception |
| 31 | GPU memory page fault |
| 43 | GPU stopped processing |
| 45 | Preemptive cleanup |
| 68 | NVDEC0 Exception |
| 109 | Context switch timeout |

### Critical XIDs (Mark GPU Unhealthy)

| XID | Description |
|-----|-------------|
| 48 | Double-bit ECC error |
| 63 | ECC page retirement |
| 64 | ECC page retirement exceeded |
| 74 | NVLink error |
| 79 | GPU fallen off bus |
| 92 | High single-bit ECC error rate |
| 94 | GPU memory page retirement required |
| 95 | GPU memory page retirement failed |

---

## Monitoring

### Health Endpoints

| Endpoint | Port | Description |
|----------|------|-------------|
| `/healthz` | 8081 | Liveness probe - server is running |
| `/readyz` | 8081 | Readiness probe - server is accepting traffic |
| `/metrics` | 9090 | Prometheus metrics |

### Prometheus Metrics

**Server Metrics**:

| Metric | Type | Description |
|--------|------|-------------|
| `device_api_server_info` | Gauge | Server information (version, go_version) |

**Cache Metrics**:

| Metric | Type | Description |
|--------|------|-------------|
| `device_api_server_cache_gpus_total` | Gauge | Total GPUs in cache |
| `device_api_server_cache_gpus_healthy` | Gauge | Healthy GPUs |
| `device_api_server_cache_gpus_unhealthy` | Gauge | Unhealthy GPUs |
| `device_api_server_cache_gpus_unknown` | Gauge | GPUs with unknown status |
| `device_api_server_cache_updates_total` | Counter | Cache update operations |
| `device_api_server_cache_resource_version` | Gauge | Current cache version |

**Watch Metrics**:

| Metric | Type | Description |
|--------|------|-------------|
| `device_api_server_watch_streams_active` | Gauge | Active watch streams |
| `device_api_server_watch_events_total` | Counter | Watch events sent |

**NVML Metrics**:

| Metric | Type | Description |
|--------|------|-------------|
| `device_api_server_nvml_provider_enabled` | Gauge | NVML provider status |
| `device_api_server_nvml_gpu_count` | Gauge | GPUs discovered by NVML |
| `device_api_server_nvml_health_monitor_running` | Gauge | Health monitor status |

### Alerting Rules

When `metrics.prometheusRule.enabled=true`, the following alerts are created:

| Alert | Severity | Condition |
|-------|----------|-----------|
| `DeviceAPIServerDown` | Critical | Server unreachable for 5m |
| `DeviceAPIServerHighLatency` | Warning | P99 latency > 500ms |
| `DeviceAPIServerHighErrorRate` | Warning | Error rate > 10% |
| `DeviceAPIServerUnhealthyGPUs` | Warning | Unhealthy GPUs > 0 |
| `DeviceAPIServerNoGPUs` | Warning | No GPUs for 10m |
| `DeviceAPIServerNVMLProviderDown` | Warning | NVML provider not running |
| `DeviceAPIServerNVMLHealthMonitorDown` | Warning | Health monitor not running |
| `DeviceAPIServerHighMemory` | Warning | Memory > 512MB |

### Grafana Dashboard

Example PromQL queries for dashboards:

```promql
# GPU health overview
device_api_server_cache_gpus_healthy / device_api_server_cache_gpus_total * 100

# Watch stream activity
rate(device_api_server_watch_events_total[5m])

# Cache update rate
rate(device_api_server_cache_updates_total[5m])
```

---

## Troubleshooting

### Pod Not Scheduling

**Symptom**: DaemonSet shows 0/N pods ready

**Check**:

```bash
# Verify node labels
kubectl get nodes --show-labels | grep gpu

# Check DaemonSet events
kubectl describe daemonset -n device-api device-api-server
```

**Solution**: Ensure nodes have `nvidia.com/gpu.present=true` label or override `nodeSelector`.

### NVML Provider Fails to Start

**Symptom**: Logs show `NVML initialization failed`

**Check**:

```bash
# Verify RuntimeClass exists
kubectl get runtimeclass nvidia

# Check NVIDIA driver on node
kubectl debug node/<node> -it --image=nvidia/cuda:12.0-base -- nvidia-smi
```

**Solutions**:
1. Install NVIDIA GPU Operator
2. Manually create `nvidia` RuntimeClass
3. Verify driver installation on nodes

### Permission Denied on Unix Socket

**Symptom**: Clients cannot connect to Unix socket

**Check**:

```bash
# Check socket permissions on node
ls -la /var/run/device-api/
```

**Solution**: Verify `securityContext` allows socket creation, or adjust `runAsUser`.

### GPUs Not Appearing

**Symptom**: `ListGpus` returns empty

**Check**:

```bash
# Check if NVML provider is enabled
kubectl logs -n device-api <pod> | grep nvml

# Check for GPU enumeration errors
kubectl logs -n device-api <pod> | grep -i error
```

**Solutions**:
1. Enable NVML provider: `--set nvml.enabled=true`
2. Deploy an external health provider
3. Check NVML can access GPUs

### High Memory Usage

**Symptom**: Pod OOMKilled or memory alerts firing

**Check**:

```bash
# Check current memory usage
kubectl top pods -n device-api

# Check watch stream count
curl -s http://<pod-ip>:9090/metrics | grep watch_streams
```

**Solutions**:
1. Increase memory limits
2. Investigate clients creating excessive watch streams
3. Check for memory leaks in logs

### Watch Stream Disconnections

**Symptom**: Consumers report frequent reconnections

**Check**:

```bash
# Check network policy
kubectl get networkpolicy -n device-api

# Check for errors in logs
kubectl logs -n device-api <pod> | grep -i "stream\|watch"
```

**Solutions**:
1. Ensure network policies allow intra-node traffic
2. Check client timeout settings
3. Verify server is not overloaded

---

## Graceful Shutdown

The server implements graceful shutdown:

1. **PreStop Hook**: Sleeps for `shutdownDelay` seconds
2. **Signal Handling**: Catches SIGTERM/SIGINT
3. **Drain Period**: Stops accepting new connections
4. **In-Flight Completion**: Waits for active requests (up to `shutdownTimeout`)
5. **Resource Cleanup**: Closes connections, stops NVML

**Timeline**:

```
SIGTERM → [shutdownDelay] → Stop listeners → [shutdownTimeout] → Force close
```

Configure in Helm:

```yaml
server:
  shutdownTimeout: 30  # Max wait for in-flight requests
  shutdownDelay: 5     # Pre-shutdown delay for endpoint propagation
```

---

## Security Considerations

### Pod Security

Default security context (non-root, restricted):

```yaml
securityContext:
  runAsNonRoot: true
  runAsUser: 65534
  runAsGroup: 65534
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false
  capabilities:
    drop:
      - ALL
```

### Network Security

> **Warning**: The gRPC API is unauthenticated. Take care when exposing beyond localhost.

- gRPC over TCP is **plaintext and unauthenticated** by default and is intended **only for node-local access**.
- The TCP listener binds to `127.0.0.1:50051` (localhost) by default. This prevents network exposure.
- Prefer using a Unix domain socket for all local clients and providers; avoid exposing the TCP gRPC listener to the broader cluster network.
- In multi-tenant or partially untrusted clusters, strongly recommend **disabling the TCP listener** (set `--grpc-address=""`) or using a restricted Unix socket combined with Kubernetes `NetworkPolicy` to limit access to the Device API Server pod.
- If TCP gRPC must be exposed beyond the node (e.g., `--grpc-address=:50051`), terminate it behind TLS/mTLS or an equivalent authenticated tunnel (for example, a sidecar proxy or ingress with client auth) and enforce a least-privilege `NetworkPolicy`.

### Service Account

- `automountServiceAccountToken: false` by default
- No Kubernetes API access required

---

## See Also

- [API Reference](../api/device-api-server.md)
- [Design Document](../design/device-api-server.md)
- [NVML Fallback Provider](../design/nvml-fallback-provider.md)
- [Helm Chart README](../../charts/device-api-server/README.md)
