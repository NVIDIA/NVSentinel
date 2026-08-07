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
kubectl logs -n nvsentinel {GPU_MONITOR_POD} --tail=50 | grep -i dcgm
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
kubectl get pod -n nvsentinel {GPU_MONITOR_POD} -o yaml | grep -A 2 "dcgm-addr"
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
kubectl logs -n gpu-operator {DCGM_POD} --tail=30
```

DCGM pod must be `Running` on the same node as the failing GPU monitor.

### 4. Test DCGM Connectivity

Test DCGM connectivity from within the gpu-health-monitor pod:

```bash
# Exec into the GPU monitor pod
kubectl exec -it -n nvsentinel {GPU_MONITOR_POD} -- /bin/bash

# For Kubernetes Service Mode, use the service endpoint
dcgmi discovery -l --host nvidia-dcgm.gpu-operator.svc:5555

# For Localhost Mode, use localhost
dcgmi discovery -l --host localhost:5555
```

If `dcgmi` produces no output at all and cannot be interrupted with Ctrl-C, stop here and go to [Unresponsive DCGM](#unresponsive-dcgm) — the probe is hung rather than unreachable, and every further query will hang the same way.

If DCGM commands fail, check:
- DCGM service exists: `kubectl get svc -n gpu-operator | grep dcgm`
- DCGM pod is running on the same node
- Network policies allow traffic from nvsentinel to gpu-operator namespace
- For localhost mode: Verify `hostNetwork: true` in gpu-health-monitor DaemonSet

### 5. Verify Resolution

```bash
# Check condition cleared
kubectl describe node {NODE_NAME} | grep GpuDcgmConnectivityFailure
# Should show: Status: False (or condition absent)

# Watch GPU monitor logs for health checks
kubectl logs -n nvsentinel {GPU_MONITOR_POD} -f | grep "Publish DCGM"
```

## Unresponsive DCGM

A different failure mode from the above: instead of refusing the connection, a DCGM probe stops answering. Callers can park indefinitely, and the query never returns an error. In embedded mode this is node-local but not yet proof of a kernel-driver wedge — DCGM userspace deadlock can look the same. The node may keep reporting `Ready` with every GPU allocatable and no taint, so it continues accepting work that then fails to start.

**Symptoms:**
- Node condition `GpuDcgmUnresponsive`, error code `DCGM_PROBE_HANG` in embedded mode (when `probeStoreOnly` is false; otherwise the event is stored/metric-only)
- Remote modes use `GpuDcgmConnectivityFailure` / `DCGM_PROBE_HANG` with `CONTACT_SUPPORT`, because an endpoint or network hang does not prove that this node needs a reboot
- Metric `dcgm_probe_hangs` incremented for the hung `operation_name`
- GPU monitor logs: `"has not returned after Ns ... treating the DCGM probe as unresponsive"`
- Two common on-node shapes for the same underlying fault:
  - `dcgm-exporter` stays `Running` with `/health` green while scrapes fail (`up==0` for hours/days, no new log lines after "Listening on")
  - Or GPU containers fail to start with `context deadline exceeded` / `StartError` exit 128, while the node itself remains `Ready` with GPUs allocatable
- Do **not** treat a `NotReady`/`unreachable` node as this failure mode — that is ordinary kubelet death, already tainted, and the monitor is not running there to begin with

Confirm from the node rather than through DCGM, since anything that touches a wedged driver will hang:

```bash
# Processes stuck in uninterruptible sleep, usually including nvidia-smi
ps -eo stat,pid,comm | awk '$1 ~ /^D/'

# The driver module cannot be unloaded while those processes reference it
lsmod | grep nvidia
```

**Resolution:** reboot the node. Whether the hang is a wedged driver or DCGM userspace holding driver locks, the stuck processes typically cannot be cleared short of a reboot.

The check ships observe-only, so by default it records the fault and leaves the reboot to you. Where [`probeStoreOnly`](../configuration/gpu-health-monitor.md#probestoreonly) has been set to `false`, the monitor requests the reboot itself: the event's `RESTART_BM` action becomes a `RebootNode` CR once node-drainer has evicted the workloads.

If the condition never appeared, either the watchdog is disabled (`dcgm.probeDeadlineSeconds: 0`), or its deadline is late enough that the liveness probe restarted the container before the event was published. Check for a restart loop on the monitor pod with no accompanying node condition, and compare `probeDeadlineSeconds` against the liveness budget described in [probeDeadlineSeconds](../configuration/gpu-health-monitor.md#probedeadlineseconds).
