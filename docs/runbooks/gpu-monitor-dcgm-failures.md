# Runbook: GPU Health Monitor DCGM Connectivity Failures

## Overview

GPU health monitor requires connection to NVIDIA DCGM for all GPU health checks. Connectivity failures prevent GPU monitoring entirely on affected nodes.

**Key points:**
- DCGM comes from one of three sources, set by `global.dcgm.mode`: the GPU Operator service, an external node-local hostengine, or an in-process embedded hostengine
- Failures generate `GpuDcgmConnectivityFailure` node condition
- Complete loss of GPU health monitoring on affected node

## Symptoms

- Node condition `GpuDcgmConnectivityFailure` present
- GPU monitor logs show DCGM connection errors

If there is no GPU Health Monitor pod on the node at all, this runbook does not apply. A missing pod is a scheduling problem, not a connectivity problem — go to [No Monitor Pod on the Node](#no-monitor-pod-on-the-node).

## Procedure

### 1. Check GPU Monitor Logs

```bash
kubectl logs -n nvsentinel {GPU_MONITOR_POD} --tail=50 | grep -i dcgm
```

Look for:
- `"Error getting DCGM handle"`
- `"DCGM connectivity failure detected"`
- `"Failed to connect to DCGM"`

### 2. Identify the DCGM Source Mode

Read the configured mode from the release. This is the authoritative answer:

```bash
helm get values nvsentinel -n nvsentinel --all | grep -A 14 "^  dcgm:"
```

The mode is `operator-service` (the default), `external-hostengine`, or `embedded-mode`. To confirm what the pod actually runs, read its arguments:

```bash
kubectl get pod -n nvsentinel {GPU_MONITOR_POD} -o yaml | grep -E "dcgm-addr|dcgm-mode" -A 1
```

`--dcgm-mode` tells you which family the pod is in, and `--dcgm-addr` separates the two remote modes:

| Configured mode | `--dcgm-mode` | `--dcgm-addr` | `hostNetwork` |
|---|---|---|---|
| `operator-service` | `remote` | DCGM service DNS name, e.g. `nvidia-dcgm.gpu-operator.svc:5555` | absent |
| `external-hostengine` | `remote` | node-local, `localhost:5555` by default | `true` |
| `embedded-mode` | `local-managed` | loopback, `localhost:5555` by default | absent |

Then diagnose against the source the mode actually uses:

- **`operator-service`** — continue to step 3. The DCGM pod and service are the dependency.
- **`external-hostengine`** — skip step 3. No DCGM pod exists. Check the externally managed `nv-hostengine` on the node instead, and confirm the monitor pod has `hostNetwork: true` so `localhost` resolves in the host network namespace.
- **`embedded-mode`** — skip step 3. The hostengine runs inside the monitor container, so a connectivity failure points at the container's own GPU and driver access. Confirm `runtimeClassName` names the cluster's NVIDIA RuntimeClass and that the container is privileged.

### 3. Verify DCGM Pod Running (operator-service only)

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

# Use the address the pod was given in --dcgm-addr, read in step 2. The values
# below are the chart defaults; a custom global.dcgm.service.endpoint changes the
# operator-service address, including its namespace.
dcgmi discovery -l --host nvidia-dcgm.gpu-operator.svc:5555   # operator-service (default)
dcgmi discovery -l --host localhost:5555                      # external-hostengine or embedded-mode (default)
```

In `embedded-mode` the monitor starts the hostengine in-process and exposes it on pod-local loopback, so `dcgmi` inside this pod reaches that same engine. A failure here is node-local rather than a broken network hop, but it does not by itself say which part failed: the hostengine may not have started, or it started and cannot reach the GPUs because the driver, RuntimeClass, or container runtime is not giving the pod device access. The checks below separate the two.

If `dcgmi` produces no output at all and cannot be interrupted with Ctrl-C, stop here and go to [Unresponsive DCGM](#unresponsive-dcgm) — the probe is hung rather than unreachable, and every further query will hang the same way.

If DCGM commands fail, check the items for your mode:

- **`operator-service`** — the configured DCGM service exists, and network policies allow traffic from the nvsentinel namespace to the one hosting it. Derive the Service name and namespace from `--dcgm-addr` rather than assuming the default:

  ```bash
  # --dcgm-addr is <service>.<namespace>.svc[.cluster.local]:<port>
  # e.g. nvidia-dcgm.gpu-operator.svc:5555 -> service nvidia-dcgm, namespace gpu-operator
  DCGM_SVC=nvidia-dcgm; DCGM_NS=gpu-operator      # replace from your --dcgm-addr

  kubectl get svc -n "$DCGM_NS" "$DCGM_SVC"
  kubectl get svc -n "$DCGM_NS" "$DCGM_SVC" -o jsonpath='{.spec.internalTrafficPolicy}{"\n"}'
  ```
- **`external-hostengine`** — `nv-hostengine` is running on the node and listening on the configured port; the monitor pod has `hostNetwork: true`; a host firewall does not block the port.
- **`embedded-mode`** — `runtimeClassName` names the cluster's NVIDIA RuntimeClass; the container is privileged; `nvidia-smi -L` inside the pod lists the GPUs. Without GPU and driver injection the embedded engine cannot start.

### 5. Verify Resolution

```bash
# Check condition cleared
kubectl describe node {NODE_NAME} | grep GpuDcgmConnectivityFailure
# Should show: Status: False (or condition absent)

# Watch GPU monitor logs for health checks
kubectl logs -n nvsentinel {GPU_MONITOR_POD} -f | grep "Publish DCGM"
```

## No Monitor Pod on the Node

A node with no GPU Health Monitor pod produces no connectivity failure, because no process is there to report one. The node reports no GPU health at all, and nothing in the stack flags it.

Compare the monitor pods against the GPU nodes:

```bash
# Nodes running a monitor pod
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=gpu-health-monitor -o wide

# Nodes carrying the version label the monitor DaemonSets select on
kubectl get nodes -L nvsentinel.dgxc.nvidia.com/dcgm.version
```

Every GPU node needs `nvsentinel.dgxc.nvidia.com/dcgm.version` set to `3.x` or `4.x`. The monitor DaemonSets select on it in all three source modes, so an unlabeled node is never a scheduling target. The DaemonSet stays healthy and reports no error, because Kubernetes never places a pod there to fail.

Which mode you run decides whether that is a bug or the expected setup step:

- **`operator-service`** — labeler derives the label from the GPU Operator DCGM pod image. A missing label means labeler could not read a usable DCGM pod for that node, which is not the same as the pod being absent. Check in this order:

  ```bash
  # Is a DCGM pod scheduled on the node, and is it Ready?
  kubectl get pods -n gpu-operator -l app=nvidia-dcgm -o wide | grep {NODE_NAME}

  # Has the node completed DCGM bootstrap?
  kubectl get node {NODE_NAME} -o jsonpath='{.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/dcgm-bootstrap-completed}{"\n"}'

  kubectl logs -n nvsentinel deployment/labeler | grep -i dcgm
  ```

  `requireDCGMReadyForBootstrap` defaults to `true`, so on a node bootstrapping for the first time labeler withholds the label until the DCGM pod reports Ready. A pod stuck `Running` but not Ready produces exactly this symptom. Once the bootstrap annotation is written, labeler sets the label regardless of later readiness. See [labeler](../labeler.md).
- **`external-hostengine` and `embedded-mode`** — there is no DCGM pod, so labeler cannot derive the version and never creates the label. You supply it. See [DCGM Version Node Label](../configuration/gpu-health-monitor.md#dcgm-version-node-label).

Also check that the node is not opted out. Labeler removes its detection labels from a node labeled `nvsentinel.dgxc.nvidia.com/managed=false`:

```bash
kubectl get node {NODE_NAME} -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep -E "managed|dcgm.version"
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
- Do **not** use `NotReady`/`unreachable` alone to exclude this failure mode. A node can become `NotReady`/`unreachable` after the monitor has already started and recorded a hang; verify the monitor pod was running and check `dcgm_probe_hangs` / `GpuDcgmUnresponsive` evidence before dismissing it. Ordinary kubelet death with no monitor activity is a different failure class.

Confirm with a bounded `nvidia-smi` probe (always use `timeout` — an unbounded hang can leave another unkillable `D`-state process):

```bash
timeout 15 nvidia-smi -L >/dev/null 2>&1; echo $?
# 124 → broader GPU/driver hang
# 0   → nvidia-smi is fine; the stuck call is more likely DCGM-specific
```

**Resolution:** reboot the node. Whether the hang is a wedged driver or DCGM userspace holding driver locks, the stuck processes typically cannot be cleared short of a reboot.

The check ships observe-only, so by default it records the fault and leaves the reboot to you. Where [`probeStoreOnly`](../configuration/gpu-health-monitor.md#probestoreonly) has been set to `false`, the monitor requests the reboot itself: the event's `RESTART_BM` action becomes a `RebootNode` CR once node-drainer has evicted the workloads.

If the condition never appeared, either the watchdog is disabled (`dcgm.probeDeadlineSeconds: 0`), or its deadline is late enough that the liveness probe restarted the container before the event was published. Check for a restart loop on the monitor pod with no accompanying node condition, and compare `probeDeadlineSeconds` against the liveness budget described in [probeDeadlineSeconds](../configuration/gpu-health-monitor.md#probedeadlineseconds).
