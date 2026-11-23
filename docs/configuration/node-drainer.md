# Node Drainer Configuration

## Overview

The node-drainer module safely evacuates workloads from quarantined nodes. Kubernetes then reschedules the pods on healthy nodes. This document describes all the Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the chart deploys the node-drainer module in the cluster.

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
      cpu: "200m"
      memory: "300Mi"
    requests:
      cpu: "200m"
      memory: "300Mi"
```

### Logging

Sets the verbosity level for node-drainer logs.

```yaml
node-drainer:
  logLevel: info  # Options: debug, info, warn, error
```

### Kubernetes API Rate Limits

Node-drainer inherits the Kubernetes client limits from `global.qps` and `global.burst` (defaults: `5` and `10`). If node-drainer needs different limits, set component values. Otherwise, keep the inherited limits. This example sets component values:

```yaml
node-drainer:
  qps: 40
  burst: 80
```

Positive `qps` values enable client-side throttling. The value `0` uses the client-go default. A negative value disables client-side throttling. `burst` must be non-negative. A value of `0` uses the client-go default.

> Note: This module needs the results from fault-quarantine. It also needs the datastore. Thus, also enable the datastore and the other modules.

### Change Stream Resume Token

> **Important**: In step 4, start only one replica. If two replicas start at the same time, both can run the reset. The second reset can then discard the resume token that the first replica saved.

To make node-drainer skip accumulated health events and start from the current stream head, do these steps:

1. If a GitOps controller, such as Argo CD, manages NVSentinel, stop its reconciliation, as in [Stop reconciliation](../runbooks/mongodb-bitnami-to-percona-migration.md#step-2-stop-reconciliation-gitops-managed-installations-only). A running controller can restore the replicas during this procedure.
2. Scale node-drainer to zero. Wait until no pod of node-drainer exists.
3. In the shared resume-control ConfigMap, set the key of node-drainer to `CREATE`.
4. Scale node-drainer to one replica.
5. After node-drainer sets the key back to `RESUME`, restore the replicas of node-drainer.
6. If you stopped the reconciliation in step 1, start it again.

During the startup in step 4, node-drainer does these actions:

- It resets only its own resume token. With MongoDB, it deletes the token. With PostgreSQL, it sets the token to the current head.
- It records a cold-start cutoff timestamp.
- It skips startup cold-start recovery for that run.
- It sets the key back to `RESUME`.

Later restarts still run cold-start recovery, but only for records newer than the recorded cutoff.

These commands do steps 2 to 5. Run them one at a time. If a `kubectl wait` command fails, find the cause before you continue.

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment node-drainer -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/node-drainer --replicas=0
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
kubectl -n nvsentinel wait --for=delete pod -l app.kubernetes.io/name=node-drainer --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"node-drainer":"CREATE"}}'
kubectl -n nvsentinel scale deployment/node-drainer --replicas=1
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.node-drainer}'=RESUME --timeout=180s && \
  kubectl -n nvsentinel scale deployment/node-drainer --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
```

### Partial Drain

If enabled, the node-drainer drains only the pods that use the failed device when the action of a health event repairs one device. If disabled, the node-drainer drains all eligible pods on the impacted node for the configured namespaces, whatever the action.

```yaml
node-drainer:
  partialDrainEnabled: true
```

`global.partialRemediationEntityTypes` says which actions repair one device, and which devices they cover. The default covers NVIDIA GPUs: for `COMPONENT_RESET`, the node-drainer drains the pods that use the GPU named by the `GPU_UUID` entity of the event.

```yaml
global:
  partialRemediationEntityTypes:
    GPU_UUID:
      devices:
        - resourceNames: ["nvidia.com/gpu", "nvidia.com/pgpu"]
        - driver: gpu.nvidia.com
          deviceClasses: ["gpu.nvidia.com"]
          entityValueAttribute: uuid
      scopedActions: ["COMPONENT_RESET"]
```

- Each key is an entity type of health events, such as `GPU_UUID`.
- `devices` lists how pods get a device of that type. A device-plugin entry sets `resourceNames`, the exact resource names that pods request. A DRA entry sets `driver`, the name that the kubelet reports for an allocated device, `deviceClasses`, the DeviceClass names that pods use in their claims, and `entityValueAttribute`, the ResourceSlice attribute whose value health events report for the device. One entry cannot set both kinds of fields.
- `scopedActions` lists the actions, built-in or custom, that repair one device of the type. An action can be listed under only one entity type. Any other action drains the full node.

The chart fails the install if an entry breaks these rules, and the node-drainer checks them again when it starts.

To add a device type, add another key. Helm merges it with the default. This example drains only the pods that use the failed accelerator for the custom action `accel-reset`:

```yaml
global:
  partialRemediationEntityTypes:
    ACCEL_SERIAL:
      devices:
        - driver: accel.example.com
          deviceClasses: ["accel.example.com"]
          entityValueAttribute: serial
      scopedActions: ["accel-reset"]
```

To stop partial drains for GPUs but keep another type, set `GPU_UUID.scopedActions` to `[]`. Setting `GPU_UUID` to `null` does not remove the default.

A health event with a scoped action must include an entity of that type with a value. Otherwise the drain fails and retries.

A partial drain waits if a pod requests a device of the entity type and its `dgxc.nvidia.com/devices` annotation has no entry for that resource name or DRA driver. This happens when the metadata-collector is not working, when its pod device mapper is disabled (`metadata-collector.podMapper.enabled: false`), or when the mapper wrote the annotation before you added the name. Rather than drain the wrong pods, the node-drainer returns an error and retries the event until the annotation has the entry or the pod is removed. A pod requests a device through a container limit for one of the `resourceNames`, or through a DRA ResourceClaim for one of the `deviceClasses`. The node-drainer reads the ResourceClaims of a pod only when its annotation has no entry for one of the drivers of the entity type.

At this time, the pod device mapper records only the default GPU resource names and the `gpu.nvidia.com` driver. You can configure another entity type here, but its partial drains wait for the mapper to record its devices.

IMPORTANT: If partial drain is enabled, each action in `scopedActions` must map to a custom resource in fault-remediation that acts only on the device that the event names, with the same entity type in `impactedEntityScope`. Otherwise the chart fails the install or upgrade. If fault-remediation still gets an action that the partial drain does not cover, it creates no CR and retries the event. See [partial drains in fault-remediation](fault-remediation.md#partial-drains).

Post-remediation validation in fault-quarantine must know whether a health event was drained partially. `fault-quarantine.validation.partialDrainEnabled` defaults to `false`, the same as this setting. If you enable partial drain here, do one of these steps:

- Also enable it in fault-quarantine.
- Set `global.partialDrainEnabled`, which overrides both subchart values.

With either step, a partial drain never counts as a full drain.

For a health event that has a recorded drain decision, the already-drained check reads that record. `fault-quarantine.validation.partialDrainEnabled` then applies only to a health event that has no record:

```yaml
global:
  partialDrainEnabled: true
```

### Partial Drain Entity Metric

Registers `node_drainer_partial_drains_total{node, entity_type, entity_value}`.

```yaml
node-drainer:
  partialDrainEntityMetricEnabled: false
```

Off by default, because `entity_value` holds a GPU UUID, which adds one time series for each GPU in the fleet. The `drain_scope` label on `node_drainer_events_processed_total` already separates partial drains from full drains, without that cardinality. Enable this metric only in this case: you need per-GPU attribution, and your Prometheus can carry the extra series.

### AllowCompletion Drain Metric

`node_drainer_awaiting_completion_event_timestamp_seconds{node}` holds the creation time of the health event behind an `allowCompletion` drain that still waits for pods. That mode is the only mode with no deadline. The metric gives it a way to say "waiting too long". Alert when `time() -` this value exceeds the longest expected run of the drained namespaces.

```promql
time() - node_drainer_awaiting_completion_event_timestamp_seconds > 7200
```

The value is the creation time of the health event, not the moment when the drain started. It is the same instant that the `DeleteAfterTimeout` deadline measures from, and it can be earlier than the drain itself. If the drain for a health event starts hours after the event was created, the first poll already shows hours of waiting. Set thresholds with this behavior in mind.

Several health events can be in the queue for one node while it drains. The poll of each health event sets the time of that health event again. The series keeps the earliest time, so a newer health event cannot hide an older wait.

**Absence is not proof that no drain waits.** Node-drainer removes the series when the node stops draining and when it observes completion. It never publishes the series for a health event that has no creation time.

The cancellation of one of several health events queued for the same node removes the series, while another health event still waits. The next poll of that health event restores its time, so the gap does not restart its clock. This normally occurs within one requeue interval (exponential from `requeueBackoffBase`, capped at two minutes). Queue delays or repeated failures can extend the gap.

Treat a short gap as normal. For a definitive answer, use this metric together with the drain state of the node. A node drained in part, and not in whole, shares this series, because the node is the only key of the series.

### Eviction Timeout

Grace period in seconds applied to pod eviction requests in Immediate mode only.

```yaml
node-drainer:
  evictionTimeoutInSeconds: "60"
```

Node-drainer passes this timeout as the `GracePeriodSeconds` in the Kubernetes eviction API call. Node-drainer uses this timeout only for the `Immediate` eviction mode. The other modes do not evict pods. `AllowCompletion` waits until the pods exit by themselves. At its deadline, `DeleteAfterTimeout` force deletes the remaining pods with a grace period of `0`.

### Requeue Backoff Base

Base duration for exponential backoff on drain requeues in `Immediate` mode.

```yaml
node-drainer:
  requeueBackoffBase: 10s
```

If node-drainer cannot evict pods immediately, it requeues the health event. Retries use exponential backoff. The backoff starts at `requeueBackoffBase` and doubles up to a two-minute maximum.

Configure this setting based on the termination time of the workload pods:

- For zero-grace pods (`terminationGracePeriodSeconds: 0`), set a lower base such as `2s` to decrease drain latency.
- For workloads with standard grace periods (`terminationGracePeriodSeconds: 30`), pod termination dominates drain latency. A short retry interval increases redundant eviction calls.

### System Namespaces

Regular expression pattern that matches the system namespaces that node-drainer skips during drain operations.

```yaml
node-drainer:
  systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator|skyhook)$"
```

During drain operations, node-drainer does not evict pods in namespaces that match this regex.

### Delete After Timeout

Time in minutes, from the creation of the health event, after which node-drainer force deletes the pods that still run.

```yaml
node-drainer:
  deleteAfterTimeoutMinutes: 60
```

Node-drainer uses this timeout with the `DeleteAfterTimeout` eviction mode. When the timeout expires, node-drainer force deletes the remaining pods, regardless of their state.

### Not Ready Timeout

Time in minutes after which node-drainer skips a pod in NotReady state during eviction operations.

```yaml
node-drainer:
  notReadyTimeoutMinutes: 5
```

In a full drain, node-drainer skips a pod that is in NotReady state for longer than this timeout. It does not evict the pod, and it does not wait for the pod in any eviction mode. Thus, the drain can complete while the pod stays on the node. Node-drainer skips these pods because they are already unhealthy and unlikely to respond to eviction requests.

### GPU-Only Draining

If enabled, the node-drainer limits pod eviction to the workloads that request GPU resources.

```yaml
node-drainer:
  drainGPUPods: false
```

The node-drainer detects GPU resource requests through device annotations added to pods by the metadata-collector. Pods with device annotations are identified as GPU workloads and eligible for eviction. A pod without the annotation is also a GPU workload if it requests a device of any entity type in [`global.partialRemediationEntityTypes`](#partial-drain), through a container limit for one of its `resourceNames` or a DRA ResourceClaim for one of its `deviceClasses`. By default, these are `nvidia.com/gpu` and `nvidia.com/pgpu` container limits, and the `gpu.nvidia.com` DeviceClass. If the node-drainer cannot read a ResourceClaim, the drain stops and retries.

The metadata-collector adds device annotations to pods that request GPU resources. The annotations have this format:

```yaml
annotations:
	  dgxc.nvidia.com/devices: '{"devices":{"nvidia.com/gpu":["GPU-123"]}}'
```

#### Behavior

- **When enabled (`true`)**: During drain operations, node-drainer evicts only pods that have GPU device annotations, or that request GPUs through container limits or DRA claims. It keeps pods without GPU requests, to maintain critical infrastructure services
- **When disabled (`false`)**: Node-drainer evicts all eligible pods in the configured namespaces (default behavior)

## Pod Drain Policies

If workloads in the same namespace need different eviction modes, use `podDrainPolicies`. Policies match pod labels with standard [Kubernetes label selector syntax](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors), the kind of the owner of the pod, or both. The first matching policy wins.

`podDrainPolicies` and `userNamespaces` are mutually exclusive. If you set both lists to non-empty values, NVSentinel rejects the configuration. When you enable policies, set `userNamespaces: []` to clear the default namespace rule of the Helm chart. Pods that match no policy are outside the drain scope and do not block completion.

```yaml
node-drainer:
  userNamespaces: []
  podDrainPolicies:
    - name: finish-training
      namespace: training-*
      podSelector: "example.com/drain-mode=finish"
      mode: AllowCompletion
    - name: replaceable-workers
      podSelector: "example.com/drain-mode in (immediate,replaceable)"
      mode: Immediate
    - name: bounded-shutdown
      podSelector: "example.com/drain-mode=timeout"
      mode: DeleteAfterTimeout
    - name: stateful-workloads
      ownerKinds: ["StatefulSet"]
      mode: AllowCompletion
```

Each policy has these fields:

- a unique, non-empty `name`
- a `podSelector`, `ownerKinds`, or both
- a `mode` (`Immediate`, `AllowCompletion`, or `DeleteAfterTimeout`)

`namespace` is an optional namespace-name glob. If you omit it, the policy matches all user namespaces. Selectors support equality, set membership, and key existence. As in Kubernetes, `key!=value` and `key notin (value)` also match pods without that key. If absent labels must not match, add a key-existence requirement.

`ownerKinds` lists controller kinds. A pod matches when the kind of its controller owner reference is in the list. The comparison is exact and case-sensitive. Thus, use the kind as Kubernetes stores it, for example `StatefulSet`, `ReplicaSet` or `Job`.

Node-drainer reads only the direct controller of the pod, and does not follow owners further. Pods that a Deployment creates have a `ReplicaSet` controller. Pods that a CronJob creates have a `Job` controller. Pods without a controller never match a policy that sets `ownerKinds`. When a policy sets both `podSelector` and `ownerKinds`, a pod must match both.

For built-in workloads, you can also use the labels that controllers add. For example, `podSelector: "statefulset.kubernetes.io/pod-name"` selects StatefulSet pods, and `podSelector: "batch.kubernetes.io/job-name"` selects Job pods. Labels come from the pod template, so any workload can carry them. `ownerKinds` matches the controller reference, which the controller sets when it creates the pod.

Policy order takes precedence over mode. A later `Immediate` match cannot override an early `AllowCompletion` match. Policy mode has no namespace fallback. Write selectors that cover every workload that node-drainer must drain. To use namespace-based draining instead, leave `podDrainPolicies` empty and configure `userNamespaces`.

Policies narrow the eligible workloads. System namespace exclusions, DaemonSet exclusions, GPU-only filtering and the partial GPU drain scope still apply. [`drainOverrides.force`](../health-event-data-model.md#drainoverrides) changes the mode of the selected pods to `Immediate`, but does not widen that scope. Policies do not change how `drainOverrides.skip` works. You cannot combine a custom drain configuration with pod drain policies, unless `customDrain.nodeSelector` limits custom drain to part of the cluster. See [Scoping custom drain to part of the cluster](#scoping-custom-drain-to-part-of-the-cluster).

Node-drainer reads pod labels from the informer cache on each reconciliation, and before eviction or timeout deletion. If you change a relevant label, the policy changes on a later observation. API deletion preconditions prevent a stale observation from deleting a relabeled or replaced pod. To apply configuration changes, restart node-drainer. The pod cache of node-drainer keeps only the label keys that policies reference. The cache keeps the kind and controller flag of each owner reference.

`DeleteAfterTimeout` uses `deleteAfterTimeoutMinutes`, measured from the creation of the health event, also after a restart. Policies do not add a new timeout. By default, policies never choose a mode from the workload kind. Operators list the owner kinds in `ownerKinds`, or workload owners select a policy through labels.

## Scoping custom drain to part of the cluster

`customDrain.enabled: true` replaces the built-in eviction of node-drainer with a [drain plugin](../tutorials/writing-a-drain-plugin.md), for the whole cluster. If only some nodes must use the plugin, set `customDrain.nodeSelector`. An example is a Slurm or LSF partition next to nodes that run ordinary Kubernetes workloads. Nodes that match the selector use the plugin. Every other node keeps the built-in eviction path, so `userNamespaces` or `podDrainPolicies` must stay configured for these nodes. The selector uses standard [Kubernetes label selector syntax](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors).

```yaml
node-drainer:
  customDrain:
    enabled: true
    nodeSelector: "nvsentinel.example.com/scheduler=slurm"
    # ... CR template and status condition settings
  userNamespaces:
    - name: "*"
      mode: "AllowCompletion"
```

With an empty or omitted `nodeSelector`, every node uses custom drain. If you also configure `userNamespaces` or `podDrainPolicies`, node-drainer rejects the configuration at startup.

Node-drainer also rejects at startup a `nodeSelector` with neither `userNamespaces` nor `podDrainPolicies`. Without this check, node-drainer marks the unmatched nodes drained while their pods continue to run, because no namespace and no policy select their pods. Node-drainer also rejects an invalid selector at startup.

Node-drainer chooses the drain method one time for each health event, and records it on the health event. For a partial drain, the record also holds the entity that node-drainer drains, for example a GPU UUID. Later passes drain the recorded entity. Thus, a change to `partialDrainEnabled` during the drain does not change what node-drainer drains.

The already-drained check reads that recorded scope for each earlier health event. A record with no scope is a full drain. A health event with no record still uses `partialDrainEnabled`.

Node-drainer stores the record before it reads, evicts or deletes a pod, and before it creates a custom drain CR. Every drain action then follows the stored record. If two node-drainer passes choose at the same time, node-drainer keeps only the first record, and the other pass follows it. A label change during the drain does not change the method. Node-drainer chooses the method of a new health event from the labels at the time that health event starts.

Sometimes the method is `Custom`, and the CR of another health event already drained the node. In that case, node-drainer marks the health event drained and records only the scope. It records no method, because this health event did not drain the node.

Node-drainer can mark a health event `AlreadyDrained` because an earlier drain covered it. If that health event has no decision yet, node-drainer records the scope of that health event in the same update as the status. A full drain records a decision with no scope and no method. A health event that already has a decision keeps it.

If the cache cannot resolve a node, node-drainer retries the node, and does not assign a path to it. The node cache keeps only the label keys that the selector references.

Node-drainer keeps a recorded method when the current configuration cannot apply it. In these cases, node-drainer logs an error, increments `node_drainer_processing_errors_total`, and retries:

- The record says `Custom`, but custom drain is off. While custom drain is off, node-drainer does not delete the DrainRequest CR of the health event. Thus, the plugin can continue to drain the node. The name of the CR is `drain-<node name>-<event ID>`, and the CR is in the custom drain namespace. Do one of these steps:
  - Enable custom drain again.
  - End the quarantine. Then delete the DrainRequest CR of the health event.
- The record says `BuiltIn`, custom drain is on with no `nodeSelector`, and both `userNamespaces` and `podDrainPolicies` are empty. Node-drainer does not use the empty lists. With empty lists, node-drainer can mark the node drained while its pods continue to run. While custom drain is off, empty lists act as they do for a health event with no record. Do one of these steps:
  - Set the `nodeSelector` and one of those lists again.
  - End the quarantine.
- The record has no method, or a method other than `BuiltIn` or `Custom`. A stop (`stopped: true`) is one example of a record with no method. Node-drainer does not choose a method in its place.

## User Namespaces

Defines the eviction behavior for user workloads, based on namespace patterns. You cannot combine it with a non-empty `podDrainPolicies` list.

### Configuration Structure

```yaml
node-drainer:
  userNamespaces:
    - name: "namespace-pattern"
      mode: "EvictionMode"
```

### Parameters

#### name

Namespace name or pattern to match. `*` matches all user namespaces, including the namespaces that have their own entry. If more than one entry matches a namespace, the mode with the highest priority applies: `Immediate`, then `DeleteAfterTimeout`, then `AllowCompletion`. The order and the specificity of the entries do not change the result. If a health event sets `drainOverrides.force`, node-drainer uses `Immediate` for every namespace that an entry matches. The `mode` of the entries then has no effect.

#### mode

Eviction strategy to apply for pods in matching namespaces. Valid values: `Immediate`, `AllowCompletion`, or `DeleteAfterTimeout`.

### Eviction Modes

#### Immediate

Evicts pods with a grace period of `evictionTimeoutInSeconds` (60 seconds by default), and does not wait for completion.

Best for: Stateless applications that restart quickly.

```yaml
userNamespaces:
  - name: "web-frontend"
    mode: "Immediate"
```

#### AllowCompletion

Does not evict pods. Waits with no time limit until each pod exits by itself. A pod that does not exit by itself, such as a Deployment or StatefulSet pod, stays on the node. The drain then does not complete, and fault-remediation does not create a maintenance CR. To find such a drain, see [AllowCompletion Drain Metric](#allowcompletion-drain-metric).

> **Note**: In a full drain, node-drainer does not wait for a pod that stays NotReady for longer than `notReadyTimeoutMinutes`. The drain can then complete, and fault-remediation can create a maintenance CR while this pod stays on the node. See [Not Ready Timeout](#not-ready-timeout).

Best for: Jobs and batch workloads that exit by themselves.

```yaml
userNamespaces:
  - name: "production"
    mode: "AllowCompletion"
```

#### DeleteAfterTimeout

Waits up to `deleteAfterTimeoutMinutes` (from the creation of the health event), then force deletes the remaining pods.

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
  # Database - no eviction; the drain waits until the pods exit by themselves
  - name: "database"
    mode: "AllowCompletion"
  - name: "*"
    mode: "AllowCompletion"
```

### Observe a Drain

On a quarantined node that still has scheduled workloads, run these commands:

```bash
NODE={cordoned-node}

kubectl get pods -A --field-selector "spec.nodeName=$NODE" -w
kubectl get node "$NODE" -L dgxc.nvidia.com/nvsentinel-state
kubectl get events -n default \
  --field-selector "involvedObject.kind=Node,involvedObject.name=$NODE"
```

Node-drainer creates its Kubernetes events in the **`default`** namespace (`Type=Normal`, `Source=nvsentinel-node-drainer`). Example output for a pod `training-job-0` in namespace `batch-jobs` on node `gpu-node-42`:

#### `Immediate`

Node-drainer evicts pods through the Eviction API. It does not emit a Kubernetes event.

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
2m          Normal        AwaitingPodCompletion   Node/gpu-node-42 Waiting for following pods to finish: [database/postgres-0]
```

#### `DeleteAfterTimeout`

```text
# kubectl get pods -A --field-selector "spec.nodeName=gpu-node-42"
NAMESPACE   NAME              READY   STATUS    RESTARTS   AGE
batch-jobs  training-job-0    1/1     Running   0          2h

# kubectl get events -n default --field-selector "involvedObject.name=gpu-node-42"
LAST SEEN   TYPE          REASON                    OBJECT           MESSAGE
1m          Normal        WaitingBeforeForceDelete  Node/gpu-node-42 Waiting for following pods to finish: [training-job-0] in namespace: [batch-jobs] or they will be force deleted on: 2026-07-01 18:30:00 +0000 UTC
```

After the deadline, node-drainer force-deletes the pod. Then `kubectl get pods` does not show the pod.

### One-shot AI prompt (per-namespace drain modes)

Paste this prompt into an AI coding agent. Replace the bracketed parts.

```text
Create a Helm values overlay for NVSentinel node-drainer per-namespace drain modes.

Modes (exact strings): Immediate, AllowCompletion, DeleteAfterTimeout.
- Immediate: Eviction API; evictionTimeoutInSeconds as a quoted string (e.g. "60")
- AllowCompletion: no eviction; wait with no time limit until the pod exits by itself (Jobs, batch)
- DeleteAfterTimeout: force-delete after deleteAfterTimeoutMinutes from health-event createdAt

YAML under global.nodeDrainer and node-drainer:
  global.nodeDrainer.enabled: true
  node-drainer.evictionTimeoutInSeconds: "60"
  node-drainer.deleteAfterTimeoutMinutes: [minutes, default 60]
  node-drainer.systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator|skyhook)$"
  node-drainer.userNamespaces (entry order does not matter; if several entries match a namespace, Immediate wins, then DeleteAfterTimeout, then AllowCompletion; omit customDrain):
    [ns1] → Immediate     (default: frontend)
    [ns2] → DeleteAfterTimeout (default: batch-jobs)
    [ns3] → AllowCompletion  (default: database)
    *     → AllowCompletion

Output only the values YAML. User applies it with their existing NVSentinel Helm release.
```
