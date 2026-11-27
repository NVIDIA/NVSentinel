# Fault Quarantine Configuration

## Overview

The fault-quarantine module isolates nodes with detected hardware or software failures. It cordons the nodes, taints the nodes, or does both. This document describes all the Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the chart deploys the fault-quarantine module in the cluster.

```yaml
global:
  faultQuarantine:
    enabled: true
```

> Note: This module needs the datastore. Thus, also enable the datastore.

### Resources

Defines CPU and memory resource requests and limits for the fault-quarantine pod.

```yaml
fault-quarantine:
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "1"
      memory: "1Gi"
```

### Logging

Sets the verbosity level for fault-quarantine logs.

```yaml
fault-quarantine:
  logLevel: info  # Options: debug, info, warn, error
```

### Kubernetes API Rate Limits

Fault-quarantine inherits the Kubernetes client limits from `global.qps` and `global.burst` (defaults: `5` and `10`). If fault-quarantine needs different limits, set component values. Otherwise, keep the inherited limits. This example sets component values:

```yaml
fault-quarantine:
  qps: 40
  burst: 80
```

Positive `qps` values enable client-side throttling. The value `0` uses the client-go default. A negative value disables client-side throttling. `burst` must be non-negative. A value of `0` uses the client-go default.

### Change Stream Resume Token

> **Important**: In step 4, start only one replica. If two replicas start at the same time, both can run the reset. The second reset can then discard the resume token that the first replica saved.

To make fault-quarantine skip accumulated health events and start from the current stream head, do these steps:

1. If a GitOps controller, such as Argo CD, manages NVSentinel, stop its reconciliation, as in [Stop reconciliation](../runbooks/mongodb-bitnami-to-percona-migration.md#step-2-stop-reconciliation-gitops-managed-installations-only). A running controller can restore the replicas during this procedure.
2. Scale fault-quarantine to zero. Wait until no pod of fault-quarantine exists.
3. In the shared resume-control ConfigMap, set the key of fault-quarantine to `CREATE`.
4. Scale fault-quarantine to one replica.
5. After fault-quarantine sets the key back to `RESUME`, restore the replicas of fault-quarantine.
6. If you stopped the reconciliation in step 1, start it again.

During the startup in step 4, fault-quarantine resets only its own resume token, and sets the key back to `RESUME`. With MongoDB, it deletes the token. With PostgreSQL, it sets the token to the current head.

These commands do steps 2 to 5. Run them one at a time. If a `kubectl wait` command fails, find the cause before you continue.

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment fault-quarantine -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/fault-quarantine --replicas=0
kubectl -n nvsentinel rollout status deployment/fault-quarantine --timeout=180s
kubectl -n nvsentinel wait --for=delete pod -l app.kubernetes.io/name=fault-quarantine --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"fault-quarantine":"CREATE"}}'
kubectl -n nvsentinel scale deployment/fault-quarantine --replicas=1
kubectl -n nvsentinel rollout status deployment/fault-quarantine --timeout=180s
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.fault-quarantine}'=RESUME --timeout=180s && \
  kubectl -n nvsentinel scale deployment/fault-quarantine --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/fault-quarantine --timeout=180s
```

On a normal `RESUME` startup, fault-quarantine queries the datastore for processable health events that do not have a quarantine status. It replays these health events before it consumes live health events. Fault-quarantine suppresses a health event only when later health events together cover every check, entity, and error-code scope that the health event can change. Thus, only a later check-wide health event can suppress a check-wide health event. This conservative rule prevents old failures or recoveries from temporarily overwriting a newer node state.

For each replayed health event, recovery uses one current snapshot of the node from the API server. It does not use the informer cache. The live watcher opens first, so health events inserted during recovery stay queued for normal processing.

A live health event can also end with no quarantine status. This occurs when fault-quarantine cannot update the node, for example because the API server returns an error, or cannot store the status. Fault-quarantine then stops, and Kubernetes restarts the pod. The startup replays the health event. While the failure continues, each startup stops on the same health event. The `fault_quarantine_processing_errors_total` metric counts each stop with the `error_type` label `live_event_unfinished`.

Skipped, invalid, and superseded documents receive a terminal fault-quarantine recovery marker in their status. The detailed result stays available through the recovery metric. Successful quarantine updates already carry a terminal quarantine status. Later scans exclude both forms. Normal recovery does not advance a wall-clock checkpoint, because event creation time and datastore insertion order can differ.

If an installation has no persisted cutoff, fault-quarantine initializes and persists the lower bound at the current startup boundary. It does not scan the full history of the cluster.

A consumed `CREATE` request skips cold start and records its creation time. Later restarts consider only unresolved health events created after that cutoff. Thus, fault-quarantine does not revive health events that an operator deliberately abandoned.

These metrics show the cold-start activity: `fault_quarantine_cold_start_events_total{result=...}` and `fault_quarantine_cold_start_duration_seconds`.

### GPU Node Label

Label selector, as `key=value`, that identifies GPU nodes. The circuit breaker counts only the matching nodes when it measures the cordoned percentage. Fault-quarantine watches all nodes, so that it can recover a quarantined node whose GPU label is temporarily absent.

```yaml
fault-quarantine:
  gpuNodeLabel: "nvidia.com/gpu.present=true"
```

The default matches the label that the NVIDIA GPU Operator applies. If your cluster labels GPU nodes differently, override it.

A selector that matches no node gives the circuit breaker a node count of zero. If the window has no cordons, the breaker stays `CLOSED` and fault-quarantine continues to run. While the GPU node count is zero, a node cordoned in the window that is still in the cluster trips the breaker. Thus, if the breaker behaves unexpectedly on a cluster with non-standard labels, examine this value first.

### Label Prefix

Defines the prefix for all node labels that fault-quarantine creates to track the cordon and uncordon lifecycle.

```yaml
fault-quarantine:
  labelPrefix: "k8saas.nvidia.com/"
```

### Generated Labels

Labels use the configured `{labelPrefix}` (default `k8saas.nvidia.com/`):

- `{labelPrefix}cordon-by` — Service that cordoned the node
- `{labelPrefix}cordon-reason` — Reason for cordoning
- `{labelPrefix}cordon-timestamp` — Cordon timestamp (format: 2006-01-02T15-04-05Z)
- `{labelPrefix}uncordon-by` — Service that uncordoned the node
- `{labelPrefix}uncordon-reason` — Reason for uncordoning
- `{labelPrefix}uncordon-timestamp` — Uncordon timestamp (format: 2006-01-02T15-04-05Z)

## Circuit Breaker

Prevents fault-quarantine from quarantining too many nodes at the same time. This protects against cluster-wide cascading failures.

With PostgreSQL, a breaker that is already tripped blocks the startup of the watcher. It keeps the pod NotReady until someone resets the breaker. A rollout or Helm upgrade can wait for this reset. A breaker that trips after the watcher starts does not itself change datastore readiness.

In dry-run mode, fault-quarantine does not create the breaker. Dry run cordons nothing. Thus, dry run does not count toward the breaker, and the breaker does not halt dry run. When you disable dry run, no trip remains from the dry-run period.

### Configuration

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true
    percentage: 50
    maxNodes: 0
    maxCordonedNodes: 0
    maxCordonedPercentage: 0
    duration: "5m"
```

### Parameters

#### enabled

Enables or disables circuit breaker protection. If you disable the breaker, fault-quarantine can quarantine an unlimited number of nodes.

#### percentage

Maximum percentage of GPU nodes that fault-quarantine can quarantine within the time window. When the cordon count in the window reaches this limit, the circuit breaker trips and blocks all new quarantine actions. The value rounds up, so `1` on a 288-node cluster is 3 nodes, not 2. To bound by `maxNodes` alone, set it to `0`.

#### maxNodes

Maximum absolute number of nodes that fault-quarantine can quarantine within the time window. This bound does not depend on the cluster size. Defaults to `0`, which disables this bound.

If you set both bounds, **the lower one binds**. This is the purpose of the setting. A percentage silently follows fleet growth. Thus, a limit that you choose for a 100 node cluster becomes three times as permissive at 300 nodes. An absolute bound does not move. Fleet-wide operators can therefore express "no more than X% *and* never more than N nodes" in one configuration that stays correct as clusters grow.

#### maxCordonedNodes

Standing cap on the number of nodes that NVSentinel holds quarantined **at the same time**. It counts only the cordons of NVSentinel. Defaults to `0`, which disables it.

This is the only bound that limits a total. `percentage` and `maxNodes` both count cordon events inside `duration`. Thus, they limit the *rate* of cordoning, and not the final number of cordoned nodes. A rate that stays slightly below the threshold still reaches the whole fleet after enough windows. On a 300 node cluster, `maxNodes: 5` with a `5m` window permits 4 cordons in every window, for an unlimited time. After about six hours, this cordons the entire fleet, and the breaker never trips.

A longer `duration` is not a substitute. The breaker divides the window into one-second buckets, so `24h` allocates 86,400 buckets. The sliding-window advance is O(steps x buckets). A long window also trips on all the cordons of a day, and not on the real number of nodes that are out of service.

This bound does not count nodes that something other than NVSentinel cordoned. Thus, a GPU Operator upgrade or an operator's own `kubectl cordon` does not consume this budget.

The two kinds of bound work together, and do not compete. To bound a burst, keep `percentage` or `maxNodes`. To bound the standing total, add `maxCordonedNodes`. The breaker evaluates this bound only when the window bounds did not already trip. Thus, this bound can add a reason to trip, but it cannot remove one.

At least one of `percentage`, `maxNodes`, `maxCordonedNodes` and `maxCordonedPercentage` must be positive, and none of them can be negative. If all four values are zero, or if one value is negative, the breaker refuses to start. It does not ignore a negative value. Thus, the breaker cannot silently become a no-op while `enabled: true`.

For the same reason, the breaker clamps a **window** threshold above the fleet size to the fleet size. It does not leave the threshold unreachable. When this occurs, the `bound` label (described below) reports `fleetSize`. This clamp applies only to `percentage` and `maxNodes`.

The breaker does **not** clamp the standing bounds to the fleet size. The breaker compares these bounds against the count of nodes that NVSentinel holds quarantined. This count comes from an index over every watched node, but the fleet total counts only GPU-labeled nodes. Thus, the quarantined count can legitimately exceed the GPU total, for example when the GPU label of a quarantined node is temporarily absent. In that case, a clamp can cause a false trip of the breaker.

For this reason, `maxCordonedNodes` never reports `fleetSize`. `maxCordonedPercentage` reports it only for a value above `100`. For such a value, the breaker caps the scaled result before an unsafe float-to-int conversion.

To disable the breaker, use `enabled: false`. Do not use an unreachable threshold.

#### maxCordonedPercentage

The percentage form of `maxCordonedNodes`: the share of the GPU fleet that NVSentinel can hold quarantined at the same time. Defaults to `0`, which disables it. The value rounds up, so `25` on a 12-node cluster is 3 nodes.

It pairs with `maxCordonedNodes` exactly as `percentage` pairs with `maxNodes`. If you set both, **the lower one binds**. Thus, a larger fleet cannot silently raise the standing limit.

If your limit is a share of the fleet, not a count of machines, prefer this form. An absolute count silently changes meaning when you resize a cluster. For example, `maxCordonedNodes: 3` is a quarter of a 12-node cluster, and 19% when the cluster grows to 16 nodes. Nothing fails or warns at the resize. Prefer `maxCordonedNodes` in these cases:

- Your limit is a fixed number of machines.
- One value must apply to clusters of different sizes.

The denominator is the GPU node population, the same one that `percentage` uses. The numerator counts only the cordons of NVSentinel. Thus, the bound limits the standing footprint of NVSentinel, and not the total capacity that is out of service. An operator cordon or a GPU Operator upgrade does not consume or relax this bound. To alert on the total capacity out of service, watch all unschedulable nodes separately.

#### duration

Time window for tracking cordon events. The circuit breaker counts unique node cordons within this sliding window.

### Observability

`fault_quarantine_breaker_threshold_nodes{bound}` reports the effective threshold in nodes. The `bound` label names what produced the threshold:

- `percentage` or `maxNodes`
- `maxCordonedNodes` or `maxCordonedPercentage`, when the standing bound is what tripped
- `fleetSize`, when a configured bound exceeded the cluster size and the breaker clamped it
- `noGPUNodes`, when the GPU node count is zero. The metric then reports `1`, because one node cordoned in the window that is still in the cluster trips the breaker

`fault_quarantine_breaker_utilization` reports the recent cordon count as a fraction of GPU nodes.

### Configuration Examples

Aggressive:

```yaml
circuitBreaker:
  enabled: true
  percentage: 20
  duration: "10m"
```

Conservative:

```yaml
circuitBreaker:
  enabled: true
  percentage: 75
  duration: "3m"
```

Percentage with an absolute ceiling, for a fleet whose clusters vary in size:

```yaml
circuitBreaker:
  enabled: true
  percentage: 10
  maxNodes: 20
  duration: "5m"
```

Absolute burst bound only, for "never cordon more than 2 nodes in any 5 minutes, regardless of cluster size":

```yaml
circuitBreaker:
  enabled: true
  percentage: 0
  maxNodes: 2
  duration: "5m"
```

Burst bound plus a standing bound. It allows no more than 5 cordons in any 5 minutes. It also allows no more than 20 nodes that NVSentinel cordoned at the same time:

```yaml
circuitBreaker:
  enabled: true
  percentage: 0
  maxNodes: 5
  maxCordonedNodes: 20
  duration: "5m"
```

The same standing bound as a share of the fleet, so that it keeps its meaning when you resize the cluster:

```yaml
circuitBreaker:
  enabled: true
  percentage: 0
  maxNodes: 5
  maxCordonedPercentage: 25
  duration: "5m"
```

Disabled:

```yaml
circuitBreaker:
  enabled: false
```

## Rule Sets

Rule sets define the conditions for quarantining nodes with CEL expressions. Each rule set specifies match conditions (when to trigger) and actions (what to do).

### Rule Set Structure

```yaml
fault-quarantine:
  ruleSets:
    - enabled: true
      version: "1"
      name: "ruleset-name"
      priority: 100
      
      match:
        all:
          - kind: "HealthEvent"
            expression: "event.agent == 'gpu-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
          - kind: "Node"
            expression: |
              !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
        
        any:
          - kind: "HealthEvent"
            expression: "event.agent == 'syslog-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
      
      cordon:
        shouldCordon: true
      
      taint:
        key: "nvidia.com/gpu-error"
        value: "fatal"
        effect: "NoSchedule"

      label:
        key: "nvidia.com/gpu-fault"
        value: "active"
```

### Parameters

#### enabled

When `false`, fault-quarantine skips the rule set entirely. Use this to disable a built-in rule set without removing its definition.

#### version

Rule set format version for future compatibility.

#### name

Unique identifier used in logs, metrics, and as part of the cordon-reason label.

#### priority

Optional integer that resolves conflicts when multiple rule sets apply the same taint key-value pair or label key. Higher values take precedence. Fault-quarantine keeps the label priority across all matching health events in the same quarantine session. A later health event with a lower priority cannot downgrade the current label. For labels with equal priority, the later rule set in configuration order wins.

#### match

Defines the conditions that must be true for the rule set to trigger. Supports `all` (AND) and `any` (OR) logic.

#### kind

Specifies the object type to evaluate in the CEL expression. Valid values: `HealthEvent` (evaluates against health event data) or `Node` (evaluates against the Kubernetes node object).

#### expression

CEL (Common Expression Language) expression that evaluates to true or false. For `HealthEvent` kind, access fields through the `event` variable. For `Node` kind, access `node.metadata` and `node.spec`. Fault-quarantine does not cache `node.status`, and the expression cannot read it.

#### cordon

Specifies whether to mark the node as unschedulable when the rule matches. A later health event that matches this rule set on a node that is already quarantined also cordons the node. If someone else already cordoned the node, fault-quarantine records the cordon as pre-existing. Thus, it does not uncordon the node when the session ends.

#### taint

Optional Kubernetes taint to apply. Based on the effect, taints can prevent pod scheduling or evict existing pods. A later health event that matches this rule set on a node that is already quarantined also applies its taint. Fault-quarantine treats a taint that is already on the node as follows:

- If the session already applied a taint with the same key and effect, fault-quarantine keeps that taint unchanged.
- If the node already has the taint from another source, fault-quarantine records it as pre-existing and does not remove it.
- If the node has a taint with the same key and effect but a different value, fault-quarantine does not apply the rule taint. Thus, it does not overwrite the other value.

After every health event in the session recovers, fault-quarantine removes the taints and the cordon that it applied. It does not remove them earlier, when only some of the health events recover.

#### label

Optional Kubernetes label to apply for the lifetime of the quarantine session. If the key already exists, fault-quarantine overwrites its value. Fault-quarantine records the label, priority, and configuration order in the `quarantineHealthEventAppliedLabels` node annotation. Thus, later health events use the same conflict policy. Fault-quarantine removes the label after every health event tracked in `quarantineHealthEvent` recovers.

Manual uncordon, manual untaint, and stale-state cleanup remove the tracking annotation, but keep the label itself. This is the same as the behavior for applied taints. In dry-run mode, fault-quarantine records the intended label in annotations for observability. It does not add, overwrite, or remove Kubernetes Node labels.

### Example Rule Sets

#### Example 1: Fatal GPU Errors from GPU Health Monitor AND node not labeled with k8saas.nvidia.com/ManagedByNVSentinel=false

```yaml
ruleSets:
  - version: "1"
    name: "GPU fatal error ruleset"
    match:
      all:
        - kind: "HealthEvent"
          expression: "event.agent == 'gpu-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
        - kind: "Node"
          expression: |
            !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && 
              node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
    cordon:
      shouldCordon: true
```

#### Example 2: Syslog Fatal Errors Excluding XID 45 AND node not labeled with k8saas.nvidia.com/ManagedByNVSentinel=false

```yaml
ruleSets:
  - version: "1"
    name: "Syslog fatal error ruleset"
    match:
      all:
        - kind: "HealthEvent"
          expression: |
            event.agent == 'syslog-health-monitor' && 
            event.componentClass == 'GPU' && 
            event.isFatal == true && 
            (event.errorCode == null || !event.errorCode.exists(e, e == '45'))
        - kind: "Node"
          expression: |
            !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && 
              node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
    cordon:
      shouldCordon: true
```

#### Example 3: Health Events Analyzer Fatal Errors on Managed Nodes

```yaml
ruleSets:
  - version: "1"
    name: "Health events analyzer fatal error ruleset"
    match:
      all:
        - kind: "HealthEvent"
          expression: "event.agent == 'health-events-analyzer' && event.isFatal == true"
        - kind: "Node"
          expression: |
            !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && 
              node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false") &&
            !('nvsentinel.dgxc.nvidia.com/managed' in node.metadata.labels && 
              node.metadata.labels['nvsentinel.dgxc.nvidia.com/managed'] == "false")
    cordon:
      shouldCordon: true
```

### Adding and Modifying Rule Sets

`fault-quarantine.ruleSets` is a **list**. When Helm merges multiple values files (`-f`), Helm replaces lists entirely. It does not merge them by `name`. A file that sets only one rule set replaces the whole list. Helm drops the built-in rule sets from `charts/fault-quarantine/values.yaml` of the NVSentinel chart, unless you include them again. The chart has no keyed-merge for rule sets.

To change the rule sets, do these steps:

1. Copy the default `ruleSets` from values.yaml into your values file.
2. Edit the full list:
   - **Disable** a rule set: set `enabled: false` on that entry in your complete list.
   - **Add** a rule set: append an entry (see [examples above](#example-rule-sets)).
   - **Modify** a rule set: change `match`, `cordon`, or `taint` on that entry in your complete list.
3. Upgrade the Helm release:

```bash
helm upgrade nvsentinel ./distros/kubernetes/nvsentinel \
  -n nvsentinel \
  -f my-values.yaml \
  --wait
```

The chart renders the list into the `fault-quarantine` ConfigMap (`config.toml`). A configuration change triggers a pod restart.

To test without cordoning nodes, use `global.dryRun: true` (see [Dry Run Mode](./README.md#dry-run-mode)). To confirm the rollout, use `kubectl -n nvsentinel rollout status deployment/fault-quarantine`. Then examine the logs with `kubectl -n nvsentinel logs deployment/fault-quarantine`.

In dry run, fault-quarantine records its decisions, but it does not cordon, taint or label nodes:

- Fault-quarantine skips a health event that matches no rule set, as it does with dry run off. It writes no annotation, no datastore status and no dry-run count.
- Fault-quarantine writes the quarantine annotations as usual, and also `quarantineHealthEventDryRun: "True"`. The annotations stay across restarts, and when something else cordons or uncordons the node.
- `fault_quarantine_dry_run_actions_total{action="quarantine|unquarantine"}` counts these decisions. The quarantine metrics (`fault_quarantine_cordons_applied_total` and the others) count only applied actions.
- The logs show `Would cordon node (dry run)` and `Would uncordon node (dry run)`.
- The datastore status is still `Quarantined`. Thus, to find the dry-run decisions, compare their time with the time when you enabled dry run.

When you disable dry run, fault-quarantine discards the dry-run quarantines on startup. It cancels their quarantining health events, and then removes their annotations. Thus, node-drainer does not act on them. Fault-quarantine does not remove a cordon that someone else applied. For the next fault on the node, fault-quarantine quarantines the node for real.

Fault-quarantine cancels these health events in the same way as when you uncordon a node manually. If the write fails, fault-quarantine does these actions:

- It increments `fault_quarantine_processing_errors_total{error_type="mongodb_cancel_quarantine_error"}`.
- It keeps the annotations on the node.
- It tries the write again in the background, and again after a restart.

Until the write succeeds, node-drainer can still process the health events of the node. If a real fault occurs on the node during this time, fault-quarantine first discards the record and then quarantines the node. After you disable dry run, verify that `fault_quarantine_nodes_pending_cancellation` returns to zero. For more information, see [Cancelling Break-Fix Workflows](../cancelling-breakfix.md).
