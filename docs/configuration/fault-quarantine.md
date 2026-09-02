# Fault Quarantine Configuration

## Overview

The Fault Quarantine module isolates nodes with detected hardware or software failures by cordoning and/or tainting them. This document covers all Helm configuration options available for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the fault-quarantine module is deployed in the cluster.

```yaml
global:
  faultQuarantine:
    enabled: true
```

> Note: This module depends on the datastore being enabled. Therefore, ensure the datastore is also enabled.

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

Fault Quarantine inherits the Kubernetes client limits from `global.qps` and `global.burst` (defaults: `5` and `10`). Set component values only when Fault Quarantine needs different limits:

```yaml
fault-quarantine:
  qps: 40
  burst: 80
```

Positive `qps` values enable client-side throttling, `0` uses the client-go default, and a negative value disables client-side throttling. `burst` must be non-negative; `0` uses the client-go default.

### Change Stream Resume Token

To make fault-quarantine skip accumulated events and start from the current stream head, scale it to zero, set its key in the shared resume-control ConfigMap to `CREATE`, then restore its replicas. Fault-quarantine deletes only its own resume token and resets the key back to `RESUME` during startup.

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment fault-quarantine -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/fault-quarantine --replicas=0
kubectl -n nvsentinel rollout status deployment/fault-quarantine --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"fault-quarantine":"CREATE"}}'
kubectl -n nvsentinel scale deployment/fault-quarantine --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/fault-quarantine --timeout=180s
```

### Label Prefix

Defines the prefix for all node labels created by the module to track cordon/uncordon lifecycle.

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

Prevents too many nodes from being quarantined simultaneously, protecting against cluster-wide cascading failures.

### Configuration

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true
    percentage: 50
    maxNodes: 0
    duration: "5m"
```

### Parameters

#### enabled
Enables or disables circuit breaker protection. When disabled, unlimited nodes can be quarantined.

#### percentage
Maximum percentage of GPU nodes that can be quarantined within the time window. When reached, the circuit breaker trips and blocks all new quarantine actions. Rounds up, so `1` on a 288-node cluster is 3 nodes, not 2. Set to `0` to bound by `maxNodes` alone.

#### maxNodes
Maximum absolute number of nodes that can be quarantined within the time window, independent of cluster size. Defaults to `0`, which disables this bound and preserves the percentage-only behaviour.

When both bounds are set, **the lower one binds**. This is the point of the setting: a percentage silently tracks fleet growth, so a limit chosen for a 100 node cluster becomes three times as permissive at 300 nodes, while an absolute bound does not move. Fleet-wide operators can therefore express "no more than X% *and* never more than N nodes" in one config that stays correct as clusters grow.

At least one of `percentage` and `maxNodes` must be positive, and a negative value for either is rejected rather than ignored. The breaker refuses to start otherwise, so it cannot be silently reduced to a no-op while `enabled: true`. For the same reason, a threshold above the fleet size is clamped to the fleet size rather than being left unreachable, and the `bound` label described below reports `fleetSize` when that happens. To turn the breaker off, use `enabled: false` rather than an unreachable threshold.

#### duration
Time window for tracking cordon events. The circuit breaker counts unique node cordons within this sliding window.

### Observability

`fault_quarantine_breaker_threshold_nodes{bound}` reports the effective threshold in nodes, with `bound` naming what produced it: `percentage`, `maxNodes`, or `fleetSize` when a configured bound exceeded the cluster size and was clamped. `fault_quarantine_breaker_utilization` keeps its existing meaning, the recent cordon count as a fraction of GPU nodes, so existing dashboards and alerts are unaffected.

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

Absolute bound only, for "never cordon more than 2 nodes regardless of cluster size":
```yaml
circuitBreaker:
  enabled: true
  percentage: 0
  maxNodes: 2
  duration: "5m"
```

Disabled:
```yaml
circuitBreaker:
  enabled: false
```

## Rule Sets

Rule sets define conditions for quarantining nodes using CEL expressions. Each rule set specifies match conditions (when to trigger) and actions (what to do).

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
When `false`, the ruleset is skipped entirely. Use this to turn off a built-in ruleset without removing its definition.

#### version
Rule set format version for future compatibility.

#### name
Unique identifier used in logs, metrics, and as part of the cordon-reason label.

#### priority
Optional integer for resolving conflicts when multiple rule sets apply the same taint key-value pair or label key. Higher values take precedence. Label priority is preserved across all matching health events in the same quarantine session; a later lower-priority event cannot downgrade the current label. For labels with equal priority, the later rule set in configuration order wins.

#### match
Defines conditions that must be satisfied for the rule set to trigger. Supports `all` (AND) and `any` (OR) logic.

#### kind
Specifies the object type to evaluate in the CEL expression. Valid values: `HealthEvent` (evaluates against health event data) or `Node` (evaluates against Kubernetes node object).

#### expression
CEL (Common Expression Language) expression that evaluates to true or false. For `HealthEvent` kind, access fields via the `event` variable. For `Node` kind, access `node.metadata` and `node.spec`; `node.status` is not cached or available.

#### cordon
Specifies whether to mark the node as unschedulable when the rule matches.

#### taint
Optional Kubernetes taint to apply. Taints can prevent pod scheduling or evict existing pods based on the effect.

#### label
Optional Kubernetes label to apply for the lifetime of the quarantine session. If the key already exists, fault-quarantine overwrites its value. The label, priority, and configuration order are recorded in the `quarantineHealthEventAppliedLabels` node annotation so later events use the same conflict policy. The label is removed after every health event tracked in `quarantineHealthEvent` has recovered. Manual uncordon, manual untaint, and stale-state cleanup remove the tracking annotation but preserve the label itself, mirroring the existing applied-taint behavior. In dry-run mode, the intended label is recorded in annotations for observability, but Kubernetes Node labels are not added, overwritten, or removed.

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

### Adding and Modifying Rule Sets

`fault-quarantine.ruleSets` is a **list**. When Helm merges multiple values files (`-f`), lists are replaced entirely — not merged by `name`. A file that sets only one ruleset replaces the whole list; built-in rulesets from [values.yaml](../../distros/kubernetes/nvsentinel/charts/fault-quarantine/values.yaml) are dropped unless you include them again. The chart has no keyed-merge for rulesets.

Copy the default `ruleSets` from values.yaml into your values file, edit the full list, then upgrade:

- **Disable** a ruleset: set `enabled: false` on that entry in your complete list.
- **Add** a ruleset: append an entry (see [examples above](#example-rule-sets)).
- **Modify** a ruleset: change `match`, `cordon`, or `taint` on that entry in your complete list.

```bash
helm upgrade nvsentinel ./distros/kubernetes/nvsentinel \
  -n nvsentinel \
  -f my-values.yaml \
  --wait
```

The chart renders the list into the `fault-quarantine` ConfigMap (`config.toml`); a config change triggers a pod restart.

Use `global.dryRun: true` to test without cordoning nodes (see [Dry Run Mode](./README.md#dry-run-mode)). Confirm the rollout with `kubectl -n nvsentinel rollout status deployment/fault-quarantine` and check `kubectl -n nvsentinel logs deployment/fault-quarantine`.
