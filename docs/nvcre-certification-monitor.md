# NVCRE Certification Monitor

## Overview

The NVCRE Certification Monitor connects NVIDIA Cluster Readiness Engine (NVCRE) certification results to NVSentinel's remediation pipeline. NVCRE runs GPU cluster burn-in tests such as NCCL collectives, training workloads and DCGM diagnostics, and records which nodes failed each test category in `Certification` custom resources. NVCRE itself never taints, cordons or marks a node, so after a failed certification the failed nodes stay schedulable.

This monitor reads every `Certification` in the cluster, turns each failed node into a health event, and publishes it to Platform Connectors. From there the standard pipeline takes over: Platform Connectors set a Node Condition and Fault Quarantine applies a `NoSchedule` taint. When a later certification run passes the node, or the failing `Certification` is deleted, the monitor publishes a healthy event and the taint is released.

### Why Do You Need This?

- **Failed nodes are otherwise left in service.** Without this monitor an operator has to read certification output and quarantine nodes by hand.
- **One pipeline for all faults.** Certification failures show up as Node Conditions and taints exactly like GPU or syslog faults, so existing dashboards and runbooks apply.
- **Recovery is automatic.** A rerun that passes a node clears the hold without anyone editing cluster state.
- **Certification workloads can still land.** The ruleset taints but does not cordon, so a rerun of the certification can be scheduled onto the failed node while ordinary workloads are kept away.

## How It Works

The monitor is a periodic reconciler with no authoritative in-memory state. Every sweep (default 15 minutes, with the first sweep at startup) recomputes everything from the cluster:

1. **Build the desired failure set.** List all `Certification` CRs in all namespaces and keep those in a terminal state. For each `Failed` category, read the ConfigMap referenced by `failedNodesRef`, decompress the `failed-nodes.json.gz` payload, and run each `{name, reason, message}` row through the configured CEL policy. Matching rows become tuples of `(node, variant, reason)`.
2. **Apply rerun recovery.** For each `Succeeded` category, read `succeededNodesRef` and record which `(node, variant)` pairs passed and when. A pass newer than a failure for the same `(node, variant)` drops that failure from the desired set.
3. **Build the observed set.** Read the `nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details` annotation from every node. It holds the tuples the monitor is currently enforcing on that node.
4. **Reconcile the difference.** A tuple that is desired but not observed is a new failure: publish an unhealthy event, add it to the node annotation, and stamp the owning `Certification`. A tuple that is observed but no longer desired has recovered: publish a healthy event and remove it from the annotation.
5. **Downstream.** Platform Connectors add a message to the node's `NVCRECertFailed` condition. The bundled Fault Quarantine ruleset applies the taint `nvsentinel.dgxc.nvidia.com/nvcre-cert-failed=true:NoSchedule` and removes it on the healthy event.

Two annotations on the `Certification` CR itself make the decision restart-safe. `cert-processed` records that the monitor already acted on the CR's current terminal state, and `error-recovered` lists tuples an operator has released so they are never republished while that CR still reports them. Because both live on the CR, a restarted pod reaches the same decision on its first sweep.

Each failure is identified by `(node, variant, reason)`, where `variant` is the test category name such as `nccl-all-gather`. The `errorCode` on the health event is `<variant>/<reason>`, so two different failing categories on one node produce two independently clearable Node Condition messages and two independent taint holds.

## What the Monitor Writes to the Cluster

| Object | Key | Meaning |
|--------|-----|---------|
| Node annotation | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details` | JSON array of `<variant>/<reason>` tuples currently held on this node |
| Node label | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failure=true` | Present while the annotation is non-empty, for `kubectl get nodes -l` |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/cert-processed` | RFC3339 time of the terminal condition the monitor has acted on |
| Certification annotation | `nvsentinel.dgxc.nvidia.com/error-recovered` | JSON array of `<node>#<variant>/<reason>` tuples released by an operator |
| Node Condition (via Platform Connectors) | `NVCRECertFailed` | One message per active tuple, tagged with its `errorCode` |
| Node taint (via Fault Quarantine) | `nvsentinel.dgxc.nvidia.com/nvcre-cert-failed=true:NoSchedule` | Applied while at least one tuple is held; no cordon |

## Recovery Paths

- **A rerun passes the node.** The newer `Succeeded` category proves `(node, variant)` passed, the failure is dropped from the desired set, and the monitor publishes a healthy event. Nothing needs to be deleted or edited.
- **The failing `Certification` is deleted.** Its tuples leave the desired set and the monitor publishes healthy events for them on the next sweep.
- **An operator removes a tuple from the node annotation.** The monitor treats this as a deliberate release: it publishes a healthy event and records the tuple in the CR's `error-recovered` annotation so it does not come back.
- **An operator removes the taint by hand.** Fault Quarantine handles the manual untaint. The annotation and Node Condition stay as a record until one of the paths above clears them.

## Configuration

Enable the module and, if needed, adjust the policy that decides which failure reasons are published:

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

The default policy deliberately excludes `HardwareFailureDetected`. That reason means NVCRE observed a node that another system had already cordoned, so the node is already quarantined by the monitor that found the underlying fault. See [Configuration](./configuration/nvcre-certification-monitor.md) for every option, the CEL variables available to policies, RBAC, and troubleshooting.

## Prerequisites

- The NVCRE operator and its `Certification` CRD (`nvcre.nvidia.com/v1alpha1`) must be installed. The monitor starts and stays healthy without the CRD, but every sweep logs an error and does nothing until the CRD appears.
- The monitor reports on nodes other than the one it runs on, so it is a cluster-scoped publisher. Its Platform Connector identity is derived automatically when the module is enabled; see [Platform Connectors](./platform-connectors.md#where-to-run-the-cluster-scoped-monitors) for placement guidance.

## Key Features

### Stateless and Restart-Safe
Every sweep is recomputed from `Certification` CRs and node annotations. A restart loses nothing.

### Rerun Recovery
A newer certification that passes a node releases the older failure for that test category automatically.

### Operator-Visible State
The node annotation and label show exactly which certification failures hold a node, and editing the annotation is a supported way to release a hold.

### Taint Without Cordon
The shipped Fault Quarantine ruleset keeps failed nodes schedulable for certification reruns while keeping other workloads off.

### Policy-Based Filtering
A CEL expression selects which failure reasons and test categories become health events.

## Further Reading

- [ADR-052: NVCRE Certification Monitor](./designs/052-nvcre-certification-monitor.md) for the full design, decision tables and edge cases.
- [Fault Quarantine](./fault-quarantine.md) for how rulesets and taint holds work.
