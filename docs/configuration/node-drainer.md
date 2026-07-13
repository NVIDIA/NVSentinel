# Node Drainer Configuration

## Overview

The Node Drainer module evacuates workloads from quarantined nodes, safely moving pods to healthy nodes. This document covers all Helm configuration options available for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the node-drainer module is deployed in the cluster.

```yaml
global:
  nodeDrainer:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the node-drainer pod.

```yaml
node-drainer:
  resources:
    limits:
      cpu: "2"
      memory: "2Gi"
    requests:
      cpu: "1"
      memory: "1Gi"
```

### Logging

Sets the verbosity level for node-drainer logs.

```yaml
node-drainer:
  logLevel: info  # Options: debug, info, warn, error
```

> Note: This module depends on the results from fault-quarantine. It also depends on the datastore being enabled. Therefore, ensure the datastore and the other modules are also enabled.

### Change Stream Resume Token

To make node-drainer skip accumulated events and start from the current stream head, scale it to zero, set its key in the shared resume-control ConfigMap to `CREATE`, then restore its replicas. Node-drainer deletes only its own resume token, records a cold-start cutoff timestamp, skips startup cold-start recovery for that run, and resets the key back to `RESUME` during startup. Future restarts still run cold-start recovery, but only for records newer than the recorded cutoff.

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment node-drainer -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/node-drainer --replicas=0
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"node-drainer":"CREATE"}}'
kubectl -n nvsentinel scale deployment/node-drainer --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
```

### Partial Drain

If enabled, the node-drainer will only drain pods which are leveraging the GPU_UUID impacted entity in COMPONENT_RESET HealthEvents. If disabled, the node-drainer will drain all eligible pods on the impacted node for the configured namespaces regardless of the remediation action. HealthEvents with the COMPONENT_RESET remediation action must include an impacted entity for the unhealthy GPU_UUID or else the drain will fail. 

IMPORTANT: If this setting is enabled, the COMPONENT_RESET action in fault-remediation must map to a custom resource which takes action only against the GPU_UUID. If partial drain was enabled in node-drainer but fault-remediation mapped COMPONENT_RESET to a reboot action, pods which weren't drained would be restarted as part of the reboot.
```yaml
node-drainer:
  partialDrainEnabled: true
```

### Eviction Timeout

Grace period in seconds applied to pod eviction requests in Immediate mode only.

```yaml
node-drainer:
  evictionTimeoutInSeconds: "60"
```

This timeout is passed as the `GracePeriodSeconds` in the Kubernetes eviction API call. Only used for `Immediate` eviction mode. Other modes respect the pod's configured `terminationGracePeriodSeconds`.

### System Namespaces

Regular expression pattern matching system namespaces that are skipped during node drain operations.

```yaml
node-drainer:
  systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator)$"
```

Pods in namespaces matching this regex are not evicted during drain operations.

### Delete After Timeout

Time in minutes from the health event creation after which pods will be force deleted if still running.

```yaml
node-drainer:
  deleteAfterTimeoutMinutes: 60
```

Used with `DeleteAfterTimeout` eviction mode. When the timeout expires, remaining pods are force deleted regardless of their state.

### Not Ready Timeout

Time in minutes after which a pod in NotReady state is skipped from eviction operations.

```yaml
node-drainer:
  notReadyTimeoutMinutes: 5
```

When a pod has been in NotReady state for longer than this timeout, it is excluded from the list of pods to evict. This prevents attempting to evict pods that are already unhealthy and unlikely to respond to eviction requests.

### GPU-Only Draining

If enabled, the node-drainer filters pod eviction to only target workloads that request GPU resources.

```yaml
node-drainer:
  drainGPUPods: false
```

The node-drainer detects GPU resource requests through device annotations added to pods by the metadata-collector. Pods with device annotations are identified as GPU workloads and eligible for eviction.

Device annotations are added to pods requesting GPU resources by metadata-collector with the format:
```yaml
annotations:
	  dgxc.nvidia.com/devices: '{"devices":{"nvidia.com/gpu":["GPU-123"]}}'
```

#### Behavior

- **When enabled (`true`)**: Only pods with GPU device annotations are evicted during drain operations
- **When disabled (`false`)**: All eligible pods in configured namespaces are evicted (default behavior)
- Pods without GPU requests are preserved, maintaining critical infrastructure services

## User Namespaces

Defines eviction behavior for user workloads based on namespace patterns.

### Configuration Structure

```yaml
node-drainer:
  userNamespaces:
    - name: "namespace-pattern"
      mode: "EvictionMode"
```

### Parameters

#### name
Namespace name or pattern to match. Use `*` to match all user namespaces not explicitly configured.

#### mode
Eviction strategy to apply for pods in matching namespaces. Valid values: `Immediate`, `AllowCompletion`, or `DeleteAfterTimeout`.

### Eviction Modes

#### Immediate

Evicts pods with minimal grace period without waiting for completion.

Best for: Stateless applications that can be quickly restarted.

```yaml
userNamespaces:
  - name: "web-frontend"
    mode: "Immediate"
```

#### AllowCompletion

Waits for pods to terminate gracefully, respecting their `terminationGracePeriodSeconds`.

Best for: Most workloads including stateful applications and services.

```yaml
userNamespaces:
  - name: "production"
    mode: "AllowCompletion"
```

#### DeleteAfterTimeout

Waits up to `deleteAfterTimeoutMinutes` (from health event creation), then force deletes remaining pods.

Best for: Long-running training jobs that need time to checkpoint and save state.

```yaml
userNamespaces:
  - name: "ml-training"
    mode: "DeleteAfterTimeout"
```

### Configuration Examples

#### Example 1: Default Configuration for All Namespaces

```yaml
userNamespaces:
  - name: "*"
    mode: "AllowCompletion"
```

#### Example 2: Different Modes for Different Workloads

```yaml
userNamespaces:
  - name: "training"
    mode: "DeleteAfterTimeout"
  - name: "inference-*"
    mode: "Immediate"
```

#### Example 3: Multi-Tier Application

```yaml
userNamespaces:
  - name: "frontend"
    mode: "Immediate"
  - name: "batch-jobs"
    mode: "DeleteAfterTimeout"
  - name: "database"
    mode: "AllowCompletion"
  - name: "*"
    mode: "AllowCompletion"
```

### Observe a Drain

On a quarantined node with workloads still scheduled:

```bash
NODE=<cordoned-node>

kubectl get pods -A --field-selector "spec.nodeName=$NODE" -w
kubectl get node "$NODE" -L dgxc.nvidia.com/nvsentinel-state
kubectl get events -n default \
  --field-selector "involvedObject.kind=Node,involvedObject.name=$NODE"
```

Node Drainer events are created in the **`default`** namespace (`Type=NodeDraining`,
`Source=nvsentinel-node-drainer`). Example output for a pod `training-job-0` in namespace
`batch-jobs` on node `gpu-node-42`:

#### `Immediate`

Pods are evicted via the Eviction API — no Node Drainer event is emitted.

```text
# kubectl get pods -A --field-selector "spec.nodeName=gpu-node-42"
NAMESPACE   NAME              READY   STATUS        RESTARTS   AGE
frontend    inference-api-0   1/1     Terminating   0          2h

# (pod disappears within evictionTimeoutInSeconds)

# kubectl get node gpu-node-42 -L dgxc.nvidia.com/nvsentinel-state
NAME         STATUS                     ROLES    AGE   VERSION   NVSENTINEL-STATE
gpu-node-42  Ready,SchedulingDisabled   <none>   30d   v1.29.0   draining
```

#### `AllowCompletion`

```text
# kubectl get pods -A --field-selector "spec.nodeName=gpu-node-42"
NAMESPACE   NAME              READY   STATUS    RESTARTS   AGE
database    postgres-0        1/1     Running   0          2h

# kubectl get events -n default --field-selector "involvedObject.name=gpu-node-42"
LAST SEEN   TYPE          REASON                  OBJECT           MESSAGE
2m          NodeDraining  AwaitingPodCompletion   Node/gpu-node-42 Waiting for following pods to finish: [database/postgres-0]
```

#### `DeleteAfterTimeout`

```text
# kubectl get pods -A --field-selector "spec.nodeName=gpu-node-42"
NAMESPACE   NAME              READY   STATUS    RESTARTS   AGE
batch-jobs  training-job-0    1/1     Running   0          2h

# kubectl get events -n default --field-selector "involvedObject.name=gpu-node-42"
LAST SEEN   TYPE          REASON                    OBJECT           MESSAGE
1m          NodeDraining  WaitingBeforeForceDelete  Node/gpu-node-42 Waiting for following pods to finish: [training-job-0] in namespace: [batch-jobs] or they will be force deleted on: 2026-07-01 18:30:00 +0000 UTC
```

After the deadline, the pod is force-deleted and `kubectl get pods` shows it gone.

### One-shot AI prompt (per-namespace drain modes)

Paste to an AI coding agent. Replace bracketed parts.

```text
Create a Helm values overlay for NVSentinel node-drainer per-namespace drain modes.

Modes (exact strings): Immediate, AllowCompletion, DeleteAfterTimeout.
- Immediate: Eviction API; evictionTimeoutInSeconds as a quoted string (e.g. "60")
- AllowCompletion: wait for pod exit (terminationGracePeriodSeconds)
- DeleteAfterTimeout: force-delete after deleteAfterTimeoutMinutes from health-event createdAt

YAML under global.nodeDrainer and node-drainer:
  global.nodeDrainer.enabled: true
  node-drainer.evictionTimeoutInSeconds: "60"
  node-drainer.deleteAfterTimeoutMinutes: [minutes, default 60]
  node-drainer.systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator|skyhook)$"
  node-drainer.userNamespaces (specific globs before "*"; omit customDrain):
    [ns1] → Immediate     (default: frontend)
    [ns2] → DeleteAfterTimeout (default: batch-jobs)
    [ns3] → AllowCompletion  (default: database)
    *     → AllowCompletion

Output only the values YAML. User applies it with their existing NVSentinel Helm release.
```
