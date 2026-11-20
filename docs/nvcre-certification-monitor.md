# NVCRE Certification Monitor

## Overview

The NVCRE Certification Monitor connects [NVIDIA Cluster Readiness Engine (NVCRE)](https://github.com/NVIDIA/cluster-readiness-engine) certification results to the NVSentinel remediation pipeline. NVCRE runs GPU cluster burn-in tests, such as NCCL collectives, training workloads and DCGM diagnostics. It records in `Certification` custom resources the nodes that failed each test category. NVCRE itself never taints, cordons or marks a node. Thus, after a failed certification, the failed nodes stay schedulable.

This monitor reads every `Certification` in the cluster. It turns each failed node into a health event and publishes the health event to Platform Connectors. Then the standard pipeline does the rest: Platform Connectors set a node condition, and Fault Quarantine applies a `NoSchedule` taint. If a later certification run passes the node, or the failing `Certification` is deleted, the monitor publishes a healthy health event. Fault Quarantine then removes the taint.

### Why Do You Need This?

- **Failed nodes are otherwise left in service.** Without this monitor, an operator must read the certification output and quarantine nodes manually.
- **One pipeline for all faults.** Certification failures appear as node conditions and taints, exactly like GPU or syslog faults. Thus, existing dashboards and runbooks apply.
- **Recovery is automatic.** A rerun that passes a node clears the hold. Nobody needs to edit the cluster state.
- **Certification workloads can still land.** The ruleset taints the node but does not cordon it. Thus, Kubernetes can schedule a rerun of the certification on the failed node. The taint keeps ordinary workloads away.

## How It Works

The monitor is a periodic reconciler with no authoritative in-memory state. Every sweep (default 15 minutes, with the first sweep at startup) recomputes everything from the cluster:

1. **Build the desired failure set.** The monitor lists all `Certification` CRs in all namespaces and keeps the CRs in a terminal state. For each `Failed` category, the monitor reads the ConfigMap that `failedNodesRef` references. It decompresses the `failed-nodes.json.gz` payload. It runs each `{name, reason, message}` row through the configured CEL policy. Matching rows become tuples of `(node, variant, reason)`.
2. **Apply rerun recovery.** The monitor processes the CRs in ascending order of completion time. For a `Succeeded` category, the monitor reads `succeededNodesRef`. For the nodes that passed, it removes every `(node, variant)` tuple that is in the desired set at that point. Thus, a pass clears only the failures from CRs that completed before it. A later failing CR adds the tuple again.
3. **Build the observed set.** The monitor reads the `nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details` annotation from every node. This annotation holds the tuples that the monitor enforces on that node.
4. **Reconcile the difference.** A tuple that is in the desired set but not in the observed set is a new failure. For it, the monitor publishes an unhealthy health event, adds the tuple to the node annotation, and writes the `cert-processed` annotation on the owning `Certification`. A tuple that is in the observed set but not in the desired set has recovered. For it, the monitor publishes a healthy health event and removes the tuple from the annotation.
5. **Downstream.** Platform Connectors add a message to the node's `NVCRECertFailed` condition. The bundled Fault Quarantine ruleset applies the taint `nvsentinel.dgxc.nvidia.com/nvcre-cert-failed=true:NoSchedule`. The ruleset removes the taint when the healthy health event arrives.

Two annotations on the `Certification` CR itself make the decision restart-safe:

- `cert-processed` records that the monitor already acted on the current terminal state of the CR.
- `error-recovered` lists the tuples that an operator released. The monitor never publishes these tuples again while that CR still reports them.

Both annotations are on the CR. Thus, a restarted pod reaches the same decision on its first sweep.

The tuple `(node, variant, reason)` identifies each failure. `variant` is the test category name, such as `nccl-all-gather`. The `errorCode` on the health event is `<variant>/<reason>`. Thus, two different failing categories on one node produce two node condition messages that clear independently, and two independent taint holds.

## What the Monitor Writes to the Cluster

| Object | Key | Meaning |
|--------|-----|---------|
| Node annotation | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details` | JSON array of the `<variant>/<reason>` tuples that hold this node |
| Node label | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failure=true` | Present while the annotation is non-empty, for `kubectl get nodes -l` |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/cert-processed` | RFC3339 time of the terminal condition that the monitor acted on |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/error-recovered` | JSON array of the `<node>#<variant>/<reason>` tuples that an operator released |
| Node condition (through Platform Connectors) | `NVCRECertFailed` | One message per active tuple, tagged with its `errorCode` |
| Node taint (through Fault Quarantine) | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failed=true:NoSchedule` | Applied while at least one tuple is held; no cordon |

## Recovery Paths

- **A rerun passes the node.** The newer `Succeeded` category proves that `(node, variant)` passed. The monitor drops the failure from the desired set and publishes a healthy health event. You do not need to delete or edit anything.
- **The failing `Certification` is deleted.** Its tuples leave the desired set. On the next sweep, the monitor publishes healthy health events for them.
- **An operator removes a tuple from the node annotation.** The monitor treats this as a deliberate release. It publishes a healthy health event and records the tuple in the `error-recovered` annotation of the CR. Thus, the tuple does not return.
- **An operator removes the taint by hand.** Fault Quarantine handles the manual untaint. The annotation and the node condition stay as a record until one of the paths above clears them.

## Configuration

Enable the module. If necessary, adjust the policy that decides which failure reasons the monitor publishes:

```yaml
global:
  nvcreCertificationMonitor:
    enabled: true

nvcre-certification-monitor:
  resyncInterval: 15m
  policies:
    - name: certification-failures
      match: "(failedNode.reason == 'ThresholdViolation') || (failedNode.reason == 'WorkloadFailed')"
```

The default policy deliberately excludes `HardwareFailureDetected`. This reason means that NVCRE observed a node that another system had already cordoned. Thus, the node is already in quarantine because of the monitor that found the underlying fault. For every option, the CEL variables available to policies, RBAC, and troubleshooting, see [Configuration](./configuration/nvcre-certification-monitor.md).

## Prerequisites

- The NVCRE operator and its `Certification` CRD (`nvcre.nvidia.com/v1alpha1`) must be installed. The monitor starts and stays healthy without the CRD. But until the CRD appears, every sweep logs an error and does nothing.
- The monitor reports on nodes other than the node that it runs on. Thus, it is a cluster-scoped publisher. When you enable the module, NVSentinel derives its Platform Connector identity automatically. For placement guidance, see [Platform Connectors](./platform-connectors.md#where-to-run-the-cluster-scoped-monitors).

## Key Features

### Stateless and Restart-Safe

The monitor recomputes every sweep from the `Certification` CRs and the node annotations. A restart loses nothing.

### Rerun Recovery

A newer certification that passes a node releases the older failure for that test category automatically.

### Operator-Visible State

The node annotation and label show exactly which certification failures hold a node. An edit of the annotation is a supported way to release a hold.

### Taint Without Cordon

The bundled Fault Quarantine ruleset keeps failed nodes schedulable for certification reruns, but keeps other workloads away.

### Policy-Based Filtering

A CEL expression selects which failure reasons and test categories become health events.

## Further Reading

- [Fault Quarantine](./fault-quarantine.md) for how rulesets and taint holds work.
