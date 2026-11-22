# Cancelling Break-Fix Workflows

## Overview

NVSentinel provides two ways to stop automated break-fix workflows:

1. **Temporary cancellation**: Uncordon a quarantined node to stop the current workflow
2. **Permanent opt-out**: Label nodes to completely disable NVSentinel break-fix automation

## Temporary Cancellation: Uncordoning a Node

### When to Use This

In these situations, cancel the workflow, so that you can handle the node manually:

- False positive detection
- You must investigate the node before remediation
- You want to apply a different fix
- You need the node in service again urgently

### How to Cancel

Uncordon the node. Replace `{node-name}` with the actual node name:

```bash
kubectl uncordon {node-name}
```

NVSentinel detects the uncordon and immediately does these actions:

- Stops the automated workflow
- Removes its quarantine annotations and its cordon labels
- Marks health events as cancelled
- Returns the node to normal operation

Taints and labels that a quarantine rule applied stay on the node. To schedule workloads that do not tolerate such a taint, remove the taint manually. Apart from that, the node can accept new workloads immediately. If the health issue occurs again later, NVSentinel treats it as a new incident.

### Checking Cancellation Status

To see if someone uncordoned a node manually, run this command:

```bash
kubectl get node {node-name} -o jsonpath='{.metadata.annotations.quarantinedNodeUncordonedManually}'
```

If the command returns `"True"`, someone uncordoned the node manually.

NVSentinel can fail to mark the health events as cancelled, for example during a database failover. In that case, it keeps its quarantine annotations on the node. It tries the write again in the background, and again after a restart. Until the write succeeds, node-drainer and fault-remediation can still act on those health events. After the write succeeds, NVSentinel removes the annotations and sets `quarantinedNodeUncordonedManually`.

If a new fault occurs on the node during this time, NVSentinel first finishes the cancellation. Then it treats the new fault as a new incident. If the write still fails at that point, NVSentinel quarantines the node for the new fault anyway, and the earlier health events stay active.

The `fault_quarantine_nodes_pending_cancellation` metric shows the number of nodes that still wait for the cancellation. The metric does not count a node that NVSentinel quarantined again while the write failed.

## Permanent Opt-Out: Disabling Break-Fix on Nodes

### When to Use This

In these cases, disable NVSentinel break-fix automation permanently on nodes:

- You do planned maintenance (driver upgrades, OS updates)
- Testing or development nodes that you do not want NVSentinel to manage automatically
- Nodes with special workloads that need custom handling
- You want to use NVSentinel for monitoring only, not automation

### How to Disable Break-Fix

To opt out, label the node:

```bash
kubectl label node {node-name} k8saas.nvidia.com/ManagedByNVSentinel=false
```

**Effect**: The default fault-quarantine rulesets do not match health events from this node. Thus, NVSentinel does not quarantine, drain or remediate the node. NVSentinel still stores the health events and updates the node conditions. A custom ruleset respects the label only if it has the same `kind: "Node"` expression as the default rulesets.

### How to Re-Enable Break-Fix

To opt in again, remove the label:

```bash
kubectl label node {node-name} k8saas.nvidia.com/ManagedByNVSentinel-
```

NVSentinel then manages the node again for all new health events.
