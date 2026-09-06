# NVCRE Certification Monitor Configuration

## Overview

The NVCRE Certification Monitor reads NVIDIA Cluster Readiness Engine (NVCRE) `Certification` custom resources and publishes one health event per failed `(node, variant, reason)`. This document covers every Helm configuration option, the state the monitor writes to the cluster, and how to observe and troubleshoot it. For the design and decision tables see [ADR-052](../designs/052-nvcre-certification-monitor.md).

## Prerequisites

### Certification CRD

The monitor reads `nvcre.nvidia.com/v1alpha1` `Certification` resources in all namespaces. The CRD does not have to exist when the monitor starts. Without it, every sweep logs `Failed to list Certification CRs` with a `no matches for kind "Certification"` error and returns; the pod stays `Running` and passes its readiness probe. Once the CRD is installed the next sweep proceeds normally with no restart.

### Cross-node identity

The monitor reports on nodes other than the one it runs on, so Platform Connectors only accept its events from an allowlisted, token-authenticated identity. Enabling the module adds its ServiceAccount to the derived allowlist automatically; no entry in `global.platformConnectorAuth.crossNodeServiceAccounts` is needed. Run the pod on a system or control-plane node pool rather than on GPU nodes serving tenants, for the reasons given in [Platform Connectors](../platform-connectors.md#where-to-run-the-cluster-scoped-monitors).

## Configuration Reference

### Module Enable/Disable

Controls whether the nvcre-certification-monitor module is deployed. It ships disabled.

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

Memory is dominated by the Node informer. The monitor strips every cached Node down to its name, UID, labels and the single annotation it reads, so the cache stays small even on large clusters.

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

How often a full sweep runs. The first sweep runs immediately at startup, then every interval. There is no event-driven trigger, so a `Certification` reaching a terminal state is noticed within one interval. Shorter values give faster detection at the cost of more API reads; each sweep lists all `Certification` CRs from the informer cache and fetches the result ConfigMaps of terminal categories directly from the API server.

### Processing Strategy

```yaml
nvcre-certification-monitor:
  processingStrategy: EXECUTE_REMEDIATION  # or STORE_ONLY
```

`EXECUTE_REMEDIATION` is the normal mode: downstream modules set Node Conditions and apply taints. `STORE_ONLY` records and exports the events but keeps them out of the remediation pipeline (no Node Condition, no cordon), which is useful when first rolling the monitor out. The monitor's own bookkeeping does not change with the strategy: in both modes it still writes its `nvcre-cert-failures-details` annotation and `nvcre-cert-failure` label on Nodes and the `cert-processed` and `error-recovered` annotations on Certifications, because that state is what keeps it from republishing the same failure every sweep.

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

The Deployment always runs one replica and the chart exposes no `replicaCount`. The sweep has no leader election, so a second pod would race the first on the node and cert annotations and publish duplicate health events.

## Policies

Policies decide which failed-node rows become health events. A row that matches no policy produces no event, no annotation entry and no Node Condition.

### Structure

```yaml
nvcre-certification-monitor:
  policies:
    - name: certification-failures
      match: "(failedNode.reason == 'ThresholdViolation') || (failedNode.reason == 'WorkloadFailed')"
```

Each policy has a `name` used in logs and a `match` CEL expression. The expression is evaluated against every row of every `Failed` category of every terminal `Certification`. If any policy returns `true`, the row enters the desired set. The list is rendered into the monitor's config file; a multi-line `match` is supported.

### CEL Variables

| Variable | Fields | Source |
|----------|--------|--------|
| `failedNode` | `name`, `reason`, `message` | One row of the category's `failed-nodes.json.gz` |
| `category` | `domain`, `variant` | The `status.categoryStatuses[]` entry the row belongs to |

All fields are strings.

### Failure Reasons

| Reason | Meaning | In default policy |
|--------|---------|-------------------|
| `ThresholdViolation` | The test ran and a measured value missed its threshold | Yes |
| `WorkloadFailed` | The test workload failed or was deleted before completing | Yes |
| `HardwareFailureDetected` | NVCRE's health check found the node already cordoned by another system | No |

`HardwareFailureDetected` is excluded on purpose. The node is already quarantined by whichever monitor found the underlying fault, and a second health event for the same node creates a lifecycle the monitor cannot recover on its own. Include it only if you accept that recovery for those entries is manual.

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
| `message` | the row's `message`, or `certification failure has occurred on this node, investigate the cause` when empty |

The `errorCode` carries the variant because Platform Connectors collapse all of a node's certification failures into the single `NVCRECertFailed` condition. The `errorCode` is what keeps two failing categories on one node as two separately clearable messages and two separate taint holds.

## Fault Quarantine Ruleset

The Fault Quarantine chart ships a ruleset for this monitor. It matches fatal events from `agent == 'nvcre-certification-monitor'` with `checkName == 'NVCRECertFailed'`, skips nodes opted out of NVSentinel management, and applies a taint without cordoning:

```yaml
taint:
  key: "nvsentinel.dgxc.nvidia.com/nvcre-cert-failed"
  value: "true"
  effect: "NoSchedule"
cordon:
  shouldCordon: false
```

No cordon means a certification rerun can still be scheduled onto the failed node with a matching toleration, while ordinary workloads are kept off. To change the taint or add a cordon, edit the `NVCRE certification monitor failure ruleset` entry under `fault-quarantine.ruleSets` in your values file.

## Cluster State Written by the Monitor

| Object | Key | Value |
|--------|-----|-------|
| Node annotation | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details` | JSON array of `<variant>/<reason>` |
| Node label | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failure` | `"true"` while the annotation is non-empty; removed with the last tuple |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/cert-processed` | RFC3339 `lastTransitionTime` of the terminal condition the monitor acted on |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/error-recovered` | JSON array of `<node>#<variant>/<reason>` released by an operator for the cert's current terminal state; dropped when `cert-processed` moves to a newer transition |

The node annotation is the monitor's only record of what it is enforcing. The two `Certification` annotations let it tell a brand-new failure apart from one an operator already released, without consulting the health-events store.

## Observing

Nodes currently held by a certification failure:

```bash
kubectl get nodes -l nvsentinel.dgxc.nvidia.com/nvcre-cert-failure=true
```

Which tuples hold a given node:

```bash
kubectl get node <node> -o jsonpath='{.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/nvcre-cert-failures-details}'
```

The Node Condition and taint set downstream:

```bash
kubectl get node <node> -o jsonpath='{.status.conditions[?(@.type=="NVCRECertFailed")]}'
kubectl get node <node> -o jsonpath='{.spec.taints}'
```

What the monitor has recorded on a Certification:

```bash
kubectl get certification -n <namespace> <name> \
  -o jsonpath='{.metadata.annotations}'
```

## Recovery and Operator Actions

| Situation | What to do | What the monitor does on the next sweep |
|-----------|------------|------------------------------------------|
| Node fixed and re-certified | Run a new certification that includes the node | The newer `Succeeded` category drops the failure; healthy event published, annotation tuple and taint removed |
| Certification no longer relevant | Delete the `Certification` CR | All its tuples leave the desired set; healthy events published for each |
| Release one hold by hand | Remove the `<variant>/<reason>` entry from the node annotation | Publishes healthy, records the tuple in the CR's `error-recovered` so it does not return |
| Release every hold on a node | Delete the node annotation | Same as above for each tuple |
| Taint removed by hand | Nothing further is required for scheduling | Fault Quarantine records the manual untaint; annotation and condition remain until cleared by one of the rows above |

Deleting a Node object is never required for recovery. If a node is deleted while held, the monitor publishes healthy for its tuples and records them in `error-recovered`. If a node named in a not-yet-processed `Certification` does not exist, the monitor skips it with a warning and re-evaluates on every sweep.

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

ConfigMaps are fetched directly from the API server rather than through an informer, which is why only `get` is granted.

## Troubleshooting

| Log line | Level | Meaning | Action |
|----------|-------|---------|--------|
| `Failed to list Certification CRs` with `no matches for kind "Certification"` | error, every sweep | The Certification CRD is not installed | Install NVCRE; no restart needed |
| `Result ConfigMap not found, treating category as having no entries` | warn | A category's `failedNodesRef` or `succeededNodesRef` points at a ConfigMap that no longer exists. NVCRE writes the ConfigMap before it publishes the reference, so it was deleted by hand or its namespace is being deleted | The category asserts nothing, so its failures heal as if the Certification had been deleted. Re-running certification re-asserts any node that still fails |
| `Failed to reconcile` with `failed to decode failed-nodes ConfigMap` | error | A category's failed-nodes ConfigMap exists but its `failed-nodes.json.gz` is not valid gzip or JSON. NVCRE writes the ConfigMap in a single update, so this is corruption or tampering rather than a transient state | The whole sweep aborts and retries on the next interval; nothing is published or healed until the ConfigMap is repaired or deleted (a deleted ConfigMap is treated as "no entries", see the row above) |
| `Skipping certification failure for a node that does not exist` | warn | A failed row names a node that is not in the cluster | Nothing published; re-evaluated each sweep in case the node reappears |
| `Skipping node, cert-failures annotation is malformed` | warn, every sweep | A node's cert-failures annotation is present but is not a JSON string array, usually after a hand edit | The node is read as holding nothing and the other nodes are processed normally. A tuple on it that the monitor already published is treated as an operator clear (healthy published, `error-recovered` written on the cert); a new failure on it is published on every sweep because the tuple cannot be recorded. Fix or delete the annotation by hand; a failure that persists is re-asserted by the next certification run |
| `Skipping tuple, node cert-failures annotation is malformed` | warn | A tuple could not be written to or removed from a node annotation that is not a JSON string array | The annotation is left untouched for inspection; fix or delete it by hand. The owning cert is not marked processed until the tuple is written |
| `Platform connector socket not found` | warn, at startup | The Platform Connector DaemonSet is not up on this node yet | Retries with backoff; check the platform-connectors pod on the same node |
| Events accepted but no taint appears | | Fault Quarantine ruleset disabled or node opted out via `k8saas.nvidia.com/ManagedByNVSentinel=false` or `nvsentinel.dgxc.nvidia.com/managed=false` | Check `fault-quarantine.ruleSets` and the node labels |
| Events rejected with `PermissionDenied` | | The module is enabled but Platform Connectors did not pick up the derived identity, or the token is being presented from a different node | See [Platform Connectors](../platform-connectors.md) authentication section |
