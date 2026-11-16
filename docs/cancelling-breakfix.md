# Cancelling Break-Fix Workflows

## Overview

NVSentinel provides two ways to stop automated break-fix workflows:

1. **Temporary cancellation**: Uncordon a quarantined node to stop the current workflow
2. **Permanent opt-out**: Label nodes to completely disable NVSentinel break-fix automation

## Temporary Cancellation: Uncordoning a Node

### When to Use This

Cancel a workflow when you need to handle a specific situation manually:

- False positive detection
- Need to investigate the node before remediation
- Want to apply a different fix
- Need the node back in service urgently

### How to Cancel

Simply uncordon the node (replace `{node-name}` with the actual node name):

```bash
kubectl uncordon {node-name}
```

NVSentinel detects the uncordon and immediately:
- Stops the automated workflow
- Cleans up quarantine state (annotations, taints, labels)
- Marks health events as cancelled
- Returns the node to normal operation

The node can schedule new workloads right away. If the health issue happens again later, NVSentinel will treat it as a new incident.

### Checking Cancellation Status

See if a node was manually uncordoned:

```bash
kubectl get node {node-name} -o jsonpath='{.metadata.annotations.quarantinedNodeUncordonedManually}'
```

If this returns `"True"`, the node was manually uncordoned.

If NVSentinel cannot mark the health events as cancelled, for example during a database failover, it keeps its quarantine annotations on the node and retries in the background, and again after a restart. Until the write succeeds, node-drainer and fault-remediation can still act on those health events. Once it succeeds, NVSentinel removes the annotations and sets `quarantinedNodeUncordonedManually`. A new fault on the node in the meantime finishes the cancellation first and is then treated as a new incident. If the write still fails at that point, NVSentinel quarantines the node for the new fault anyway and the earlier health events stay active. The `fault_quarantine_nodes_pending_cancellation` metric shows how many nodes are still waiting.

## Permanent Opt-Out: Disabling Break-Fix on Nodes

### When to Use This

Disable NVSentinel break-fix automation permanently on nodes when:

- Performing planned maintenance (driver upgrades, OS updates)
- Testing or development nodes that shouldn't be automatically managed
- Nodes with special workloads that need custom handling
- Want to use NVSentinel for monitoring only, not automation

### How to Disable Break-Fix

Label the node to opt out:

```bash
kubectl label node {node-name} k8saas.nvidia.com/ManagedByNVSentinel=false
```

**Effect**: NVSentinel will completely ignore health events from this node. No quarantine, no drain, no remediation.

### How to Re-Enable Break-Fix

Remove the label to opt back in:

```bash
kubectl label node {node-name} k8saas.nvidia.com/ManagedByNVSentinel-
```

The node will be managed by NVSentinel again for any new health events.
