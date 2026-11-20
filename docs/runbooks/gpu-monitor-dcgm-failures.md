# Runbook: GPU Health Monitor DCGM Connectivity Failures

## Overview

The GPU Health Monitor requires a connection to NVIDIA DCGM for all GPU health checks. A connectivity failure stops all GPU monitoring on the affected node.

**Key points:**

- DCGM comes from one of three sources, which `global.dcgm.mode` selects: the GPU Operator service, an external node-local hostengine or an in-process embedded hostengine.
- Failures generate the `GpuDcgmConnectivityFailure` node condition.
- GPU health monitoring stops completely on the affected node.

## Symptoms

- The node condition `GpuDcgmConnectivityFailure` is present.
- The GPU Health Monitor logs show DCGM connection errors.

If there is no GPU Health Monitor pod on the node, this runbook does not apply. A missing pod is a scheduling problem, not a connectivity problem. Go to [No Monitor Pod on the Node](#no-monitor-pod-on-the-node).

## Procedure

### 1. Check GPU Monitor Logs

```bash
kubectl logs -n nvsentinel {GPU_HEALTH_MONITOR_POD} --tail=50 | grep -i dcgm
```

Find these messages in the output:

- `"Error getting DCGM handle"`
- `"DCGM connectivity failure detected"`
- `"Failed to connect to DCGM"`

### 2. Identify the DCGM Source Mode

Read the configured mode from the release. This is the authoritative answer:

```bash
helm get values nvsentinel -n nvsentinel --all | grep -A 14 "^  dcgm:"
```

The mode is `operator-service` (the default), `external-hostengine` or `embedded-mode`. To confirm the mode that the pod uses, read its arguments:

```bash
kubectl get pod -n nvsentinel {GPU_HEALTH_MONITOR_POD} -o yaml | grep -E "dcgm-addr|dcgm-mode" -A 1
```

`--dcgm-mode` shows the family of the pod. `--dcgm-addr` separates the two remote modes:

| Configured mode | `--dcgm-mode` | `--dcgm-addr` | `hostNetwork` |
|---|---|---|---|
| `operator-service` | `remote` | DCGM service DNS name, for example `nvidia-dcgm.gpu-operator.svc:5555` | absent |
| `external-hostengine` | `remote` | node-local, `localhost:5555` by default | `true` |
| `embedded-mode` | `local-managed` | loopback, `localhost:5555` by default | absent |

Then diagnose the DCGM source that the mode uses:

- **`operator-service`** — continue to step 3. The DCGM pod and the DCGM service are the dependency.
- **`external-hostengine`** — skip step 3. No DCGM pod exists. Instead, examine the externally managed `nv-hostengine` on the node. Confirm that the GPU Health Monitor pod has `hostNetwork: true`, so that `localhost` resolves in the host network namespace.
- **`embedded-mode`** — skip step 3. The hostengine runs in the GPU Health Monitor container. Thus, a connectivity failure shows a problem with the GPU and driver access of the container itself. Confirm that `runtimeClassName` names the NVIDIA RuntimeClass of the cluster. Also confirm that the container is privileged.

### 3. Verify DCGM Pod Running (operator-service only)

`{DCGM_NS}` is the namespace in the `--dcgm-addr` value from step 2.

```bash
# Check DCGM pod on affected node
kubectl get pods -n {DCGM_NS} -l app=nvidia-dcgm -o wide

# Check DCGM logs
kubectl logs -n {DCGM_NS} {DCGM_POD} --tail=30
```

The DCGM pod must be `Running` on the same node as the failing GPU Health Monitor.

### 4. Test DCGM Connectivity

Test the DCGM connectivity from inside the GPU Health Monitor pod:

```bash
# Exec into the GPU monitor pod
kubectl exec -it -n nvsentinel {GPU_HEALTH_MONITOR_POD} -- /bin/bash

# Use the address the pod was given in --dcgm-addr, read in step 2. The values
# below are the chart defaults; a custom global.dcgm.service.endpoint changes the
# operator-service address, including its namespace.
dcgmi discovery -l --host nvidia-dcgm.gpu-operator.svc:5555   # operator-service (default)
dcgmi discovery -l --host localhost:5555                      # external-hostengine or embedded-mode (default)
```

In `embedded-mode`, the GPU Health Monitor starts the hostengine in-process and exposes it on the pod-local loopback. Thus, `dcgmi` in this pod reaches that same engine. A failure here is node-local and not a broken network hop. But the failure alone does not show which part failed. There are two possible causes:

- The hostengine did not start.
- The hostengine started, but it cannot reach the GPUs, because the driver, the RuntimeClass or the container runtime does not give the pod device access.

The checks that follow separate the two causes.

If `dcgmi` produces no output and you cannot interrupt it with Ctrl-C, go directly to [Unresponsive DCGM](#unresponsive-dcgm). The probe is hung, not unreachable, and every further query hangs in the same way.

If DCGM commands fail, check the items for your mode:

- **`operator-service`** — the configured DCGM service exists, and network policies allow traffic from the nvsentinel namespace to the namespace of the service. Get the Service name and namespace from `--dcgm-addr`. Do not assume the default values:

  ```bash
  # --dcgm-addr is <service>.<namespace>.svc[.cluster.local]:<port>
  # e.g. nvidia-dcgm.gpu-operator.svc:5555 -> service nvidia-dcgm, namespace gpu-operator
  DCGM_SVC=nvidia-dcgm; DCGM_NS=gpu-operator      # replace from your --dcgm-addr

  kubectl get svc -n "$DCGM_NS" "$DCGM_SVC"
  kubectl get svc -n "$DCGM_NS" "$DCGM_SVC" -o jsonpath='{.spec.internalTrafficPolicy}{"\n"}'
  ```

- **`external-hostengine`** — `nv-hostengine` runs on the node and listens on the configured port. The GPU Health Monitor pod has `hostNetwork: true`. A host firewall does not block the port.
- **`embedded-mode`** — `runtimeClassName` names the NVIDIA RuntimeClass of the cluster. The container is privileged. `nvidia-smi -L` in the pod lists the GPUs. Without GPU and driver injection, the embedded engine cannot start.

If `dcgmi` works but the GPU Health Monitor still logs `Duplicate key passed to function` or `Max limit reached for the object`, go to [Stale DCGM Field Groups](#stale-dcgm-field-groups).

### 5. Verify Resolution

```bash
# Check condition cleared
kubectl describe node {NODE_NAME} | grep GpuDcgmConnectivityFailure
# Should show: Status: False (or condition absent)

# Watch GPU monitor logs for health checks
kubectl logs -n nvsentinel {GPU_HEALTH_MONITOR_POD} -f | grep "Publish DCGM"
```

## No Monitor Pod on the Node

A node with no GPU Health Monitor pod produces no connectivity failure, because no process is there to report one. The node reports no GPU health, and nothing in the stack flags this problem.

Compare the GPU Health Monitor pods with the GPU nodes:

```bash
# Nodes running a monitor pod
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=gpu-health-monitor -o wide

# Nodes carrying the version label the monitor DaemonSets select on
kubectl get nodes -L nvsentinel.dgxc.nvidia.com/dcgm.version
```

Every GPU node needs `nvsentinel.dgxc.nvidia.com/dcgm.version` set to `3.x` or `4.x`. The GPU Health Monitor DaemonSets select nodes by this label in all three source modes. Thus, an unlabeled node is never a scheduling target. The DaemonSet stays healthy and reports no error, because Kubernetes never places a pod there that can fail.

The mode that you run decides whether a missing label is a bug or an expected setup step:

- **`operator-service`** — labeler sets the label from the version in the GPU Operator DCGM pod image. A missing label means that labeler was not able to read a usable DCGM pod for that node. This is not the same as an absent pod. Do the checks in this order:

  ```bash
  # Is a DCGM pod scheduled on the node, and is it Ready?
  kubectl get pods -n gpu-operator -l app=nvidia-dcgm -o wide | grep {NODE_NAME}

  # Has the node completed DCGM bootstrap?
  kubectl get node {NODE_NAME} -o jsonpath='{.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/dcgm-bootstrap-completed}{"\n"}'

  kubectl logs -n nvsentinel deployment/labeler | grep -i dcgm
  ```

  `requireDCGMReadyForBootstrap` defaults to `true`. Thus, on a node that bootstraps for the first time, labeler does not set the label until the DCGM pod reports Ready. A pod that is stuck in `Running` but is not Ready causes exactly this symptom. After labeler writes the bootstrap annotation, it sets the label regardless of later readiness. See [labeler](../labeler.md).
- **`external-hostengine` and `embedded-mode`** — there is no DCGM pod. Thus, labeler cannot get the version, and it never creates the label. You must supply the label. See [DCGM Version Node Label](../configuration/gpu-health-monitor.md#dcgm-version-node-label).

Also make sure that the node does not have the opt-out label. Labeler removes its detection labels from a node that has the label `nvsentinel.dgxc.nvidia.com/managed=false`:

```bash
kubectl get node {NODE_NAME} -o jsonpath='{.metadata.labels}' | tr ',' '\n' | grep -E "managed|dcgm.version"
```

## Stale DCGM Field Groups

This failure applies to `operator-service` and `external-hostengine`. In these modes, the GPU Health Monitor connects to a hostengine that it does not own. That hostengine outlives the GPU Health Monitor pod.

The hostengine deletes the field groups of a client only when it sees the client connection close. If the connection drops silently, for example when a network policy or a network fault drops the TCP flow, the hostengine keeps the old groups. Each stale group stays until the hostengine restarts.

**Symptoms:**

- The node condition `GpuDcgmConnectivityFailure` stays `True`.
- Step 4 passes: `dcgmi` reaches the hostengine.
- The GPU Health Monitor logs show one of these errors on each retry:
  - `Error getting DCGM handle: Duplicate key passed to function`. A stale field group still holds the name that the GPU Health Monitor uses.
  - `Error getting DCGM handle: Max limit reached for the object`. The hostengine has no free slots. It allows 64 field groups and 64 entity groups in total, for all clients.
- The metric `dcgm_api_failures_total{error_name="dcgm_initialize_monitoring"}` increases. This metric counts every setup failure after the GPU Health Monitor connects, so other causes are possible. Use the log errors above to confirm the cause. A failed connection increments `error_name="ErrorInitDCGMHandle"` instead.

List the field groups and the entity groups on the hostengine. Run the commands in the GPU Health Monitor pod. `dcgmi` accepts only one address. If the `--dcgm-addr` value from step 2 has one address, use it for `{DCGM_ADDR}`. If it has more than one, use the address from the last `Successfully created DCGM handle to` log line:

```bash
kubectl logs -n nvsentinel {GPU_HEALTH_MONITOR_POD} | grep "Successfully created DCGM handle to" | tail -1
kubectl exec -n nvsentinel {GPU_HEALTH_MONITOR_POD} -- dcgmi fieldgroup -l --host {DCGM_ADDR}
kubectl exec -n nvsentinel {GPU_HEALTH_MONITOR_POD} -- dcgmi group -l --host {DCGM_ADDR}
```

Each output shows the total, for example `8 field groups found.` The GPU Health Monitor creates one entity group named `dcgm_health` and one field group named `nvsentinel_field_monitors`. More than one group with the same name as a GPU Health Monitor group confirms this failure. A total close to 64 in either list can show this failure, but it does not confirm it. The active groups of other clients also count.

Before you restart the hostengine, do these steps:

1. Read the group names in each list.
2. Identify the stale groups.

The `Duplicate key passed to function` error needs only one stale group. In this case, one field group with the field-group name of the GPU Health Monitor confirms this failure, whatever the totals are.

The GPU Health Monitor deletes stale groups by name. Each time it connects, it first deletes every entity group named `dcgm_health` and every field group named `nvsentinel_field_monitors`. Then it creates new groups with these names. It also deletes every field group named `gpu_temp_limit`.

The GPU Health Monitor selects these groups by name, not by owner. If another client uses one of these names, the GPU Health Monitor also deletes the group of that client. Groups with other names stay. The metric `dcgm_api_failures_total{error_name="stale_group_cleanup"}` increases when the GPU Health Monitor cannot list or delete a group. The GPU Health Monitor tries again at the next connection.

**Resolution:** restart the hostengine. The restart deletes all stale groups.

- **`operator-service`** — delete the DCGM pod on the node. The GPU Operator DaemonSet creates a new one.

  ```bash
  kubectl delete pod -n {DCGM_NS} {DCGM_POD}
  ```

- **`external-hostengine`** — restart `nv-hostengine` with the method that your platform uses to manage it.

The restart disconnects all DCGM clients on the node. The GPU Health Monitor connects again without a restart. Other clients, such as `dcgm-exporter`, can restart one time. Then do [step 5](#5-verify-resolution). If NVSentinel cordoned the node because of this condition, see [Cordoned Nodes](cordoned-nodes.md).

## Unresponsive DCGM

This failure mode is different from the failure modes above. A DCGM probe does not refuse the connection. Instead, it stops answering. The callers can wait indefinitely, and the query never returns an error. In embedded mode, this hang is node-local, but it does not prove a kernel-driver wedge. A DCGM userspace deadlock can look the same.

The node can continue to report `Ready`, with every GPU allocatable and no taint. Thus, it continues to accept work that then fails to start.

**Symptoms:**

- In embedded mode, the node condition `GpuDcgmUnresponsive` with error code `DCGM_PROBE_HANG` appears when `probeStoreOnly` is false. Otherwise, NVSentinel only stores the health event and exports it as a metric.
- Remote modes use `GpuDcgmConnectivityFailure` / `DCGM_PROBE_HANG` with `CONTACT_SUPPORT`, because an endpoint or network hang does not prove that this node needs a reboot.
- The metric `dcgm_probe_hangs` increments for the hung `operation_name`.
- GPU Health Monitor logs: `"has not returned after Ns ... treating the DCGM probe as unresponsive"`
- The same underlying fault has two common forms on the node:
  - `dcgm-exporter` stays `Running` with `/health` green while scrapes fail (`up==0` for hours or days, and no new log lines after "Listening on").
  - Or GPU containers fail to start with `context deadline exceeded` / `StartError` exit 128, while the node itself stays `Ready` with GPUs allocatable.
- Do **not** use `NotReady`/`unreachable` alone to exclude this failure mode. A node can become `NotReady`/`unreachable` after the GPU Health Monitor has already started and recorded a hang. Before you dismiss this failure mode, verify that the GPU Health Monitor pod ran at that time. Also examine `dcgm_probe_hangs` and `GpuDcgmUnresponsive` for evidence of a hang. Ordinary kubelet death with no GPU Health Monitor activity is a different failure class.

Confirm with a bounded `nvidia-smi` probe. Always use `timeout`, because an unbounded hang can leave another unkillable `D`-state process:

```bash
timeout 15 nvidia-smi -L >/dev/null 2>&1; echo $?
# 124 → broader GPU/driver hang
# 0   → nvidia-smi is fine; the stuck call is more likely DCGM-specific
```

**Resolution:** reboot the node. The hang can be a wedged driver, or DCGM userspace that holds driver locks. In both cases, usually only a reboot can clear the stuck processes.

By default, the check is observe-only. Thus, it records the fault, and you do the reboot. In embedded mode, if you set [`probeStoreOnly`](../configuration/gpu-health-monitor.md#probestoreonly) to `false`, the GPU Health Monitor requests the reboot itself. After node-drainer evicts the workloads, the `RESTART_BM` action of the health event becomes a `RebootNode` CR.

If the condition never appeared, there are two possible causes:

- The watchdog is disabled (`dcgm.probeDeadlineSeconds: 0`).
- The deadline of the watchdog is so late that the liveness probe restarted the container before the GPU Health Monitor published the health event.

To find the cause, do these steps:

1. Check for a restart loop on the GPU Health Monitor pod with no related node condition.
2. Compare `probeDeadlineSeconds` with the liveness budget that [probeDeadlineSeconds](../configuration/gpu-health-monitor.md#probedeadlineseconds) describes.
