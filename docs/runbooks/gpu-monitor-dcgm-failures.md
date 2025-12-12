# Runbook: GPU Health Monitor DCGM Connectivity Failures

## Overview

GPU health monitor requires connection to NVIDIA DCGM for all GPU health checks. Connectivity failures prevent GPU monitoring entirely on affected nodes.

**Key points:**
- DCGM can be exposed via Kubernetes service or localhost
- Failures generate `GpuDcgmConnectivityFailure` node condition
- Complete loss of GPU health monitoring on affected node

## Symptoms

- Node condition `GpuDcgmConnectivityFailure` present
- GPU monitor logs show DCGM connection errors

## Procedure

### 1. Check GPU Monitor Logs

```bash
kubectl logs -n nvsentinel <GPU_MONITOR_POD> --tail=50 | grep -i dcgm
```

Look for:
- `"Error getting DCGM handle"`
- `"DCGM connectivity failure detected"`
- `"Failed to connect to DCGM"`

### 2. Identify DCGM Configuration

Check which DCGM mode is in use by verifying if the GPU Operator DCGM service exists:

```bash
# Check if DCGM service exists
kubectl get svc -n gpu-operator nvidia-dcgm

# If service exists, check service details
kubectl get svc -n gpu-operator nvidia-dcgm -o yaml
```

If the service exists, the cluster is using **Kubernetes Service Mode**. If the service doesn't exist or is not exposed, the cluster is using **Localhost Mode**.

Verify the gpu-health-monitor pod configuration matches the expected mode:

```bash
kubectl get pod -n nvsentinel <GPU_MONITOR_POD> -o yaml | grep -A 2 "dcgm-addr"
```

Expected configurations:
- **Kubernetes Service Mode**: `--dcgm-addr nvidia-dcgm.gpu-operator.svc:5555` and `--dcgm-k8s-service-enabled true`
- **Localhost Mode**: `--dcgm-addr localhost:5555` and `--dcgm-k8s-service-enabled false` (requires `hostNetwork: true`)

These values come from Helm values `dcgm.dcgmK8sServiceEnabled` and `dcgm.service.endpoint`/`dcgm.service.port`.

### 3. Verify DCGM Pod Running

```bash
# Check DCGM pod on affected node
kubectl get pods -n gpu-operator -l app=nvidia-dcgm -o wide

# Check DCGM logs
kubectl logs -n gpu-operator <DCGM_POD> --tail=30
```

DCGM pod must be `Running` on the same node as the failing GPU monitor.

### 4. Test DCGM Connectivity

Test DCGM connectivity from within the gpu-health-monitor pod:

```bash
# Exec into the GPU monitor pod
kubectl exec -it -n nvsentinel <GPU_MONITOR_POD> -- /bin/bash

# For Kubernetes Service Mode, use the service endpoint
dcgmi discovery -l --host nvidia-dcgm.gpu-operator.svc:5555

# For Localhost Mode, use localhost
dcgmi discovery -l --host localhost:5555
```

If DCGM commands fail, check:
- DCGM service exists: `kubectl get svc -n gpu-operator | grep dcgm`
- DCGM pod is running on the same node
- Network policies allow traffic from nvsentinel to gpu-operator namespace
- For localhost mode: Verify `hostNetwork: true` in gpu-health-monitor DaemonSet

### 5. Verify Resolution

```bash
# Check condition cleared
kubectl describe node <NODE_NAME> | grep GpuDcgmConnectivityFailure
# Should show: Status: False (or condition absent)

# Watch GPU monitor logs for health checks
kubectl logs -n nvsentinel <GPU_MONITOR_POD> -f | grep "Publish DCGM"
```
