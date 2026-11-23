# NVCRE Certification Monitor Configuration

## Overview

The NVCRE Certification Monitor reads NVIDIA Cluster Readiness Engine (NVCRE) `Certification` custom resources. It publishes one health event for each failed `(node, variant, reason)`. This document covers every Helm configuration option, the state that the monitor writes to the cluster, and how to observe and troubleshoot it.

## Prerequisites

### NVCRE

The NVIDIA Cluster Readiness Engine must be installed in the cluster. This includes its controller, the `nvcre.nvidia.com/v1alpha1` `Certification` CRD and the workload dependencies that it adds. Follow the [NVCRE installation guide](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/getting-started/install.md).

The monitor tolerates a later installation of NVCRE. Without the CRD, every sweep logs `Failed to list Certification CRs` with a `no matches for kind "Certification"` error, and then returns. The pod stays `Running` and passes its readiness probe. After you install NVCRE, the next sweep proceeds normally with no restart.

### Cross-node identity

The monitor reports on nodes other than the node that it runs on. Thus, Platform Connectors accept its health events only from an allowlisted, token-authenticated identity. When you enable the module, NVSentinel automatically adds its ServiceAccount to the derived allowlist. You do not need an entry in `global.platformConnectorAuth.crossNodeServiceAccounts`. Run the pod on a system or control-plane node pool, not on GPU nodes that serve tenants. For the reasons, see [Platform Connectors](../platform-connectors.md#where-to-run-the-cluster-scoped-monitors).

## Configuration Reference

### Module Enable/Disable

Controls whether Helm deploys the NVCRE Certification Monitor module. By default, the module is disabled.

```yaml
global:
  nvcreCertificationMonitor:
    enabled: true
```

### Resources

```yaml
nvcre-certification-monitor:
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 256Mi
```

The Node informer uses most of the memory. The monitor reduces every cached Node to its name, UID, labels and the single annotation that it reads. Thus, the cache stays small, even on large clusters.

### Logging

```yaml
nvcre-certification-monitor:
  logLevel: info  # Options: debug, info, warn, error
```

### Resync Interval

```yaml
nvcre-certification-monitor:
  resyncInterval: 15m
```

How often a full sweep runs. The first sweep runs immediately at startup, and then a sweep runs every interval. There is no event-driven trigger. Thus, when a `Certification` reaches a terminal state, the monitor notices it within one interval.

Shorter values give faster detection, but cause more API reads. Each sweep lists all `Certification` CRs from the informer cache. It also fetches the result ConfigMaps of terminal categories directly from the API server.

### Processing Strategy

```yaml
nvcre-certification-monitor:
  processingStrategy: EXECUTE_REMEDIATION  # or STORE_ONLY
```

`EXECUTE_REMEDIATION` is the normal mode: downstream modules set node conditions and apply taints. `STORE_ONLY` records and exports the health events, but keeps them out of the remediation pipeline (no node condition, no cordon). This mode is useful for the first rollout of the monitor.

The bookkeeping of the monitor does not change with the strategy. In both modes, the monitor still writes these items:

- its `nvcre-cert-failures-details` annotation and `nvcre-cert-failure` label on Nodes
- the `cert-processed` and `error-recovered` annotations on Certifications

This state prevents the monitor from publishing the same failure again at every sweep.

### Image and Scheduling

```yaml
nvcre-certification-monitor:
  image:
    repository: ghcr.io/nvidia/nvsentinel/nvcre-certification-monitor
    pullPolicy: IfNotPresent
    tag: ""            # defaults to the chart appVersion
  priorityClassName: "" # overridden by the matching global when set
  podAnnotations: {}
```

The Deployment always runs one replica, and the chart exposes no `replicaCount`. The sweep has no leader election. If a second pod runs, it races the first pod on the node and cert annotations, and it publishes duplicate health events.

## Policies

Policies decide which failed-node rows become health events. A row that matches no policy produces no health event, no annotation entry and no node condition.

### Structure

```yaml
nvcre-certification-monitor:
  policies:
    - name: certification-failures
      match: "(failedNode.reason == 'ThresholdViolation') || (failedNode.reason == 'WorkloadFailed')"
```

Each policy has a `name` for logs and a `match` CEL expression. The monitor evaluates the expression against every row of every `Failed` category of every terminal `Certification`. If any policy returns `true`, the row enters the desired set. Helm renders the list into the config file of the monitor. The monitor supports a multi-line `match`.

### CEL Variables

| Variable | Fields | Source |
|----------|--------|--------|
| `failedNode` | `name`, `reason`, `message` | One row of the category's `failed-nodes.json.gz` |
| `category` | `domain`, `variant` | The `status.categoryStatuses[]` entry that the row belongs to |

All fields are strings.

### Failure Reasons

| Reason | Meaning | In default policy |
|--------|---------|-------------------|
| `ThresholdViolation` | The test ran and a measured value missed its threshold | Yes |
| `WorkloadFailed` | The test workload failed or was deleted before it completed | Yes |
| `HardwareFailureDetected` | The NVCRE health check found the node already cordoned by another system | No |

The default policy excludes `HardwareFailureDetected` on purpose. The node is already in quarantine because of whichever monitor found the underlying fault. A second health event for the same node creates a lifecycle that the monitor cannot recover on its own. If a second hold that outlives the original quarantine is acceptable, you can include it. Otherwise, keep it excluded.

### Examples

Publish every failure reason:

```yaml
policies:
  - name: all-failures
    match: "true"
```

Publish only NCCL categories, any reason except hardware pass-through:

```yaml
policies:
  - name: nccl-only
    match: |
      category.domain == 'communication' &&
      failedNode.reason != 'HardwareFailureDetected'
```

Publish threshold violations for one specific test:

```yaml
policies:
  - name: all-gather-threshold
    match: "category.variant == 'nccl-all-gather' && failedNode.reason == 'ThresholdViolation'"
```

## Health Event Fields

| Field | Value |
|-------|-------|
| `agent` | `nvcre-certification-monitor` |
| `checkName` | `NVCRECertFailed` |
| `componentClass` | `Node` |
| `isFatal` | `true` |
| `nodeName` | the failed node |
| `errorCode` | `["<variant>/<reason>"]` |
| `recommendedAction` | `CONTACT_SUPPORT` |
| `entitiesImpacted` | `[{entityType: "v1/Node", entityValue: "<nodeName>"}]` |
| `message` | the row's `message`, or `certification failure has occurred on this node, investigate the cause` when it is empty |

The `errorCode` carries the variant, because Platform Connectors collapse all the certification failures of a node into the single `NVCRECertFailed` condition. The `errorCode` keeps two failing categories on one node as two messages that clear separately, and as two separate taint holds.

## Fault Quarantine Ruleset

The Fault Quarantine chart includes a ruleset for this monitor. The ruleset matches fatal health events from `agent == 'nvcre-certification-monitor'` with `checkName == 'NVCRECertFailed'`. It skips the nodes that opted out of NVSentinel management. It applies a taint without a cordon:

```yaml
taint:
  key: "nvsentinel.dgxc.nvidia.com/nvcre-cert-failed"
  value: "true"
  effect: "NoSchedule"
cordon:
  shouldCordon: false
```

Without a cordon, Kubernetes can still schedule a certification rerun on the failed node, if the rerun pod has a matching toleration. The taint keeps ordinary workloads off the node. To change the taint or add a cordon, edit the `NVCRE certification monitor failure ruleset` entry under `fault-quarantine.ruleSets` in your values file.

## Cluster State Written by the Monitor

| Object | Key | Value |
|--------|-----|-------|
| Node annotation | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details` | JSON array of `<variant>/<reason>` |
| Node label | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failure` | `"true"` while the annotation is non-empty; removed with the last tuple |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/cert-processed` | RFC3339 `lastTransitionTime` of the terminal condition that the monitor acted on |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/error-recovered` | JSON array of the `<node>#<variant>/<reason>` tuples that an operator released for the cert's current terminal state; dropped when `cert-processed` moves to a newer transition |

The node annotation is the only record of what the monitor enforces. With the two `Certification` annotations, the monitor can distinguish a new failure from a failure that an operator already released. It does not need to query the datastore for this.

## Observing

Nodes that a certification failure holds:

```bash
kubectl get nodes -l nvsentinel.dgxc.nvidia.com/nvcre-cert-failure=true
```

Which tuples hold a given node:

```bash
kubectl get node <node> -o jsonpath='{.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/nvcre-cert-failures-details}'
```

The node condition and the taint that downstream modules set:

```bash
kubectl get node <node> -o jsonpath='{.status.conditions[?(@.type=="NVCRECertFailed")]}'
kubectl get node <node> -o jsonpath='{.spec.taints}'
```

What the monitor recorded on a Certification:

```bash
kubectl get certification -n <namespace> <name> \
  -o jsonpath='{.metadata.annotations}'
```

### Metrics

The monitor exports Prometheus metrics on the `metrics` container port (`global.metricsPort`, default 2112) at `/metrics`.

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `nvcre_certification_monitor_sweep_errors_total` | counter | `error_type` = `list_certs`, `completion_time`, `configmap_not_found`, `configmap_get`, `configmap_decode`, `node_not_found`, `node_get` | Errors during a sweep. `list_certs`, `configmap_get` and a failed-nodes `configmap_decode` abort the sweep. The other errors skip the affected cert, category or tuple |
| `nvcre_certification_monitor_sweep_duration_seconds` | histogram | | Duration of one sweep |
| `nvcre_certification_monitor_health_events_published_total` | counter | `node`, `is_healthy` = `true`, `false` | Health events that Platform Connectors accepted |
| `nvcre_certification_monitor_health_event_publish_errors_total` | counter | `node`, `is_healthy` | Health events that failed to publish after retries |
| `nvcre_certification_monitor_active_failures` | gauge | | `(node, variant, reason)` failures that terminal Certifications assert |
| `nvcre_certification_monitor_malformed_node_annotations` | gauge | | Nodes that the last sweep skipped, because their annotation is unparsable |

A non-zero `malformed_node_annotations`, a rising `sweep_errors_total` or a rising `publish_errors_total` means that the monitor cannot record or deliver failures. In this case, the monitor needs attention.

## Recovery and Operator Actions

| Situation | What to do | What the monitor does on the next sweep |
|-----------|------------|------------------------------------------|
| Node fixed and re-certified | Run a new certification that includes the node | The newer `Succeeded` category drops the failure. The monitor publishes a healthy health event and removes the annotation tuple. Fault Quarantine removes the taint |
| Certification no longer relevant | Delete the `Certification` CR | Tuples that no other Failed cert still reports leave the desired set. The monitor publishes a healthy health event for each of them. A tuple that another cert still asserts stays held |
| Release one hold by hand | Remove the `<variant>/<reason>` entry from the node annotation | Publishes a healthy health event, and records the tuple in the CR's `error-recovered` so that it does not return |
| Release every hold on a node | Delete the node annotation | Same as above for each tuple |
| Taint removed by hand | No other action is necessary for scheduling | Fault Quarantine records the manual untaint. The annotation and the condition stay until one of the rows above clears them |

Recovery never requires the deletion of a Node object. If a node is deleted while it has a hold, the monitor publishes healthy health events for its tuples and records them in `error-recovered`. If a node that an unprocessed `Certification` names does not exist, the monitor skips the node with a warning. The monitor evaluates the node again on every sweep.

## RBAC Permissions

The chart creates a ClusterRole with exactly these rules:

```yaml
- apiGroups: [""]
  resources: ["nodes"]
  verbs: ["get", "list", "watch", "patch"]             # cert-failures annotation and label
- apiGroups: ["nvcre.nvidia.com"]
  resources: ["certifications"]
  verbs: ["get", "list", "watch", "patch"]             # cert-processed / error-recovered annotations
- apiGroups: [""]
  resources: ["configmaps"]
  verbs: ["get"]                                       # per-category result ConfigMaps, read uncached
```

The monitor fetches ConfigMaps directly from the API server, not through an informer. Thus, the ClusterRole grants only `get`.

## Troubleshooting

| Log line | Level | Meaning | Action |
|----------|-------|---------|--------|
| `Failed to list Certification CRs` with `no matches for kind "Certification"` | error, every sweep | NVCRE is not installed | Install NVCRE ([installation guide](https://github.com/NVIDIA/cluster-readiness-engine/blob/main/docs/getting-started/install.md)). No restart is necessary |
| `Result ConfigMap not found, treating category as having no entries` | warn | The `failedNodesRef` or `succeededNodesRef` of a category points to a ConfigMap that does not exist. NVCRE writes the ConfigMap before it publishes the reference. Thus, someone deleted the ConfigMap manually, or the deletion of its namespace is in progress | The category asserts nothing. Thus, its failures heal as if the Certification was deleted. If you run the certification again, it asserts again each node that still fails |
| `Failed to reconcile` with `failed to decode failed-nodes ConfigMap` | error | The failed-nodes ConfigMap of a category exists, but its `failed-nodes.json.gz` is not valid gzip or JSON. NVCRE writes the ConfigMap in a single update. Thus, this is corruption or tampering, not a transient state | The whole sweep aborts and tries again at the next interval. The monitor publishes and heals nothing until you repair or delete the ConfigMap. The monitor treats a deleted ConfigMap as "no entries" (see the row above) |
| `Skipping certification failure for a node that does not exist` | warn | A failed row names a node that is not in the cluster | The monitor publishes nothing. It evaluates the row again at each sweep, in case the node reappears |
| `Skipping node, cert-failures annotation is malformed` | error, every sweep | The cert-failures annotation of a node is present but is not a JSON string array, usually after a manual edit | The monitor does not change the node. It publishes nothing for the node, and the taint and condition of the node stay unchanged. The monitor does not mark a cert that asserts a tuple on the node as processed until the annotation is readable. The monitor processes the other nodes normally. Fix or delete the annotation manually. On the next sweep, the monitor reads the held tuples again and publishes the pending failures once |
| `Skipping tuple, node cert-failures annotation is malformed` | error | A desired tuple is on a node whose annotation is not a JSON string array, either at read time or at write time | The monitor does not touch the annotation, so that you can inspect it. Fix or delete it manually. The monitor does not mark the owning cert as processed until it writes the tuple |
| `Platform connector not ready` | warn, at startup | The Platform Connectors DaemonSet is not ready on this node. The socket file is missing, or the gRPC connection did not become ready within 10 seconds | The monitor tries again with backoff, up to 10 attempts. Check the Platform Connectors pod on the same node |
| `Platform-connector socket missing; skipping send.` | warn | The Platform Connectors socket disappeared after startup (the pod restarted or was evicted). The monitor skips the send and does not try again against a dead socket | The monitor records nothing for that tuple. Thus, when the socket is back, the next sweep publishes the tuple again with a fresh timestamp. Check the Platform Connectors pod on the same node |
| Health events accepted but no taint appears | | The Fault Quarantine ruleset is disabled, or the node opted out through `k8saas.nvidia.com/ManagedByNVSentinel=false` or `nvsentinel.dgxc.nvidia.com/managed=false` | Check `fault-quarantine.ruleSets` and the node labels |
| Health events rejected with `PermissionDenied` | | The module is enabled, but Platform Connectors did not load the derived identity. Or, the token comes from a different node | See the authentication section in [Platform Connectors](../platform-connectors.md) |
