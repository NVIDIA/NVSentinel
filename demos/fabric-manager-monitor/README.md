# Fabric Manager & GPU Node Health Validator

A standalone DaemonSet companion to NVSentinel that catches GPU infrastructure failures invisible to telemetry-based monitoring.

**Related issue:** [#883 - NVSentinel not detecting fabric health on H100s](https://github.com/NVIDIA/NVSentinel/issues/883)

## Problem

NVIDIA Fabric Manager can fail and stay broken for weeks undetected. NVSentinel's existing monitors (DCGM-based, syslog-based) miss it because individual GPUs appear healthy to DCGM even when Fabric Manager is down. This tool fills the gap with service-level health checks.

**Requirements:** Kubernetes cluster with GPU nodes, Prometheus Operator

## What It Monitors

| # | Check | What It Catches | Method |
|---|-------|-----------------|--------|
| 1 | **Fabric Manager Service** | FM not running, flapping, error state | `nsenter` + `systemctl` |
| 2 | **Critical GPU Services** | persistenced, DCGM dead | `nsenter` + `systemctl` |
| 3 | **PCIe Link Health** | Link downtraining (Gen5->Gen3, x16->x8) | `nsenter` + `nvidia-smi` |
| 4 | **NVLink Fabric** | Bandwidth zero with FM down, CRC errors | DCGM metrics HTTP |
| 5 | **CUDA Validation** | Context failures, memory errors | PyTorch subprocess |
| 6 | **Clock & Throttle** | Silent throttling without XID | `nsenter` + `nvidia-smi` |

## Quick Start

```bash
# Build
docker build -t fabric-manager-monitor:latest .

# Deploy (assumes nvsentinel namespace exists)
kubectl apply -f k8s/rbac.yaml
kubectl apply -f k8s/configmap.yaml
kubectl apply -f k8s/daemonset.yaml
kubectl apply -f k8s/servicemonitor.yaml

# Verify
kubectl get ds -n nvsentinel fabric-manager-monitor
kubectl port-forward -n nvsentinel ds/fabric-manager-monitor 9101:9101
curl -s localhost:9101/metrics | grep fabric_manager_up
```

## Metrics

Exposed on port 9101. Key metrics:

| Metric | Description |
|--------|-------------|
| `fabric_manager_up` | Fabric Manager running (1/0) |
| `gpu_node_health_up` | Overall node health (1/0) |
| `nvidia_service_up` | Per-service status |
| `pcie_link_degraded` | PCIe link degraded per GPU |
| `nvlink_fabric_healthy` | NVLink health |
| `gpu_clock_throttled` | Clock throttled per GPU |
| `gpu_clock_ratio` | Current/max clock ratio |

## Alert Rules

The ServiceMonitor includes PrometheusRule with 7 alerts:
- `FabricManagerDown` (critical, 5m)
- `FabricManagerFlapping` (warning, 5m)
- `NVLinkFabricDegraded` (critical, 5m) — correlated: requires FM down AND NVLink degraded
- `GPUPCIeLinkDegraded` (warning, 5m)
- `GPUClockThrottled` (warning, 10m)
- `GPUServiceDown` (critical, 3m)
- `CUDAValidationFailed` (critical, 5m)

## Validated On

- 2x P4d.24xlarge (8x A100-SXM4-40GB each) — Amazon Linux 2023, EKS 1.32
- All 6 check categories produce correct metrics
- GPU Idle downclocking correctly filtered as benign

## Configuration

All settings via ConfigMap environment variables. See `k8s/configmap.yaml`.

## Relationship to NVSentinel

This is a **standalone companion tool** that exposes Prometheus metrics and alerts. It does not integrate with NVSentinel's gRPC event pipeline or remediation workflow. See the native `health-monitors/fabric-manager-monitor/` for an integrated version that emits HealthEvents to platform-connector.
