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
| 3 | **Per-GPU Fabric State** | FM_NOT_STARTED, FM_REGISTRATION_STUCK, FM_FABRIC_ERROR | `nsenter` + `nvidia-smi` |
| 4 | **CUDA Validation** | Context failures, memory errors | PyTorch subprocess |

## Quick Start

```bash
# Build
docker build -t system-services-monitor:latest .

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
  -l app=system-services-monitor -o jsonpath='{.items[0].metadata.name}')
kubectl port-forward -n nvsentinel pod/${POD} 9101:9101
curl -s localhost:9101/metrics | grep fabric_manager_up
```

## Metrics

Exposed on port 9101. Key metrics:

| Metric | Description |
|--------|-------------|
| `fabric_manager_up` | Fabric Manager running (1/0) |
| `gpu_node_health_up` | Overall node health (1/0) |
| `nvidia_service_up` | Per-service status |
| `fabric_state_healthy` | Per-GPU fabric state (1/0) |

## Alert Rules

The ServiceMonitor includes PrometheusRule with alerts:
- `FabricManagerDown` (critical, 5m)
- `FabricManagerFlapping` (warning, 5m)
- `FabricStateUnhealthy` (critical, 5m) -- per-GPU fabric orchestration failure
- `GPUServiceDown` (critical, 3m)
- `CUDAValidationFailed` (critical, 5m)

## Validated On

- 2x P4d.24xlarge (8x A100-SXM4-40GB each) -- Amazon Linux 2023, EKS 1.32
- All check categories produce correct metrics

## Configuration

All settings via ConfigMap environment variables. See `k8s/configmap.yaml`.

## Relationship to NVSentinel

This is a **standalone companion tool** that exposes Prometheus metrics and alerts. It does not integrate with NVSentinel's gRPC event pipeline or remediation workflow. See the native `health-monitors/system-services-monitor/` for an integrated version that emits HealthEvents to platform-connector.
