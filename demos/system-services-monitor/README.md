# System Service Monitor — Standalone Demo

A standalone DaemonSet companion to NVSentinel that catches GPU infrastructure failures invisible to DCGM-based monitoring.

**Related issue:** [#883 - NVSentinel not detecting fabric health on H100s](https://github.com/NVIDIA/NVSentinel/issues/883)

## Problem

NVIDIA Fabric Manager can fail and stay broken for weeks undetected. NVSentinel's existing monitors (DCGM-based, syslog-based) miss it because individual GPUs appear healthy to DCGM even when Fabric Manager is down. This tool fills the gap with service-level health checks.

**Requirements:** Kubernetes cluster with GPU nodes, Prometheus Operator

## What It Monitors

| # | Check | What It Catches | Method |
|---|-------|-----------------|--------|
| 1 | **Fabric Manager Service** | FM not running, flapping, error state | `nsenter` + `systemctl` |
| 2 | **Critical GPU Services** | persistenced dead | `nsenter` + `systemctl` |

> **Per-GPU fabric state** (FM_NOT_STARTED, FM_REGISTRATION_STUCK, FM_FABRIC_ERROR via
> `nvidia-smi -q`) is implemented in the integrated monitor
> [`health-monitors/system-services-monitor/`](../../health-monitors/system-services-monitor/)
> (#1382) and is not part of this standalone demo.

> **CUDA validation is not part of this monitor.** Polling CUDA context/memory tests from a long-running daemon contends for GPU memory with active workloads (see the [#891 review](https://github.com/NVIDIA/NVSentinel/pull/891)). The supported form is a preflight init-container that runs once before workloads schedule — see [`preflight-checks/cuda-validation/`](../../preflight-checks/cuda-validation/) (#1384).

## Quick Start

```bash
# Build (tag must match k8s/daemonset.yaml)
docker build -t system-services-monitor:0.1.0 .

# Deploy (assumes nvsentinel namespace exists)
kubectl apply -f k8s/rbac.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/daemonset.yaml
kubectl apply -f k8s/servicemonitor.yaml

# Verify
kubectl get ds -n nvsentinel system-services-monitor

# Port-forward to a specific node's pod
NODE=<node-name>
POD=$(kubectl get pod -n nvsentinel -o wide --field-selector spec.nodeName=${NODE} \
  -l app.kubernetes.io/name=system-services-monitor -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n nvsentinel pod/${POD} 9101:9101
curl -s localhost:9101/metrics | grep fabric_manager_up
```

## Metrics

Exposed on port 9101. Key metrics:

| Metric | Description |
|--------|-------------|
| `fabric_manager_up` | Fabric Manager running (1/0) |
| `fabric_manager_restarts_total` | FM restarts observed via systemd NRestarts deltas |
| `gpu_node_health_up` | Overall node health (1/0) |
| `nvidia_service_up` | Per-service status |

`gpu_node_health_up` reports unhealthy (past the boot grace period) when a
service is down **or** when the monitor could not inspect host services at
all — a broken probe must not report a healthy node on no evidence. A unit
that is absent on the host (`LoadState=not-found`) is skipped, not treated
as down.

## Alert Rules

The ServiceMonitor includes PrometheusRule with alerts:
- `FabricManagerDown` (critical, 5m)
- `FabricManagerFlapping` (warning, 5m)
- `GPUServiceDown` (warning, 3m)

## Validated On

- 2x P4d.24xlarge (8x A100-SXM4-40GB each) -- Amazon Linux 2023, EKS 1.32

## Configuration

All settings via ConfigMap environment variables. See `k8s/configmap.yaml`.

| Variable | Default | Notes |
|----------|---------|-------|
| `CHECK_INTERVAL` | `30` | Seconds between cycles; must be > 0 (rejected at startup otherwise) |
| `METRICS_PORT` | `9101` | Avoids NVSentinel's 2112 |
| `BOOT_GRACE_PERIOD` | `300` | Seconds before failures mark the node unhealthy |
| `FLAP_WINDOW` / `FLAP_THRESHOLD` | `600` / `3` | Restarts within window to flag flapping |
| `ENABLE_FABRIC_CHECK` | `true` | Gates the Fabric Manager check only |
| `ENABLE_GPU_SERVICES_CHECK` | `true` | Gates the generic service checks independently |
| `GPU_SERVICES` | `nvidia-persistenced` | Comma-separated; Fabric Manager has its own check and is not in this list |

## Relationship to NVSentinel

This is a **standalone companion tool** that exposes Prometheus metrics and alerts. It does not integrate with NVSentinel's gRPC event pipeline or remediation workflow. See the native `health-monitors/system-services-monitor/` for an integrated version that emits HealthEvents to platform-connector.
