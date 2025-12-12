# Runbook: GPU Driver and GPU Operator Upgrades

## Overview

This runbook provides the procedure to safely upgrade GPU drivers and GPU Operator components while preventing NVSentinel from interfering with the upgrade process.

## Background

During GPU Operator or driver upgrades, DCGM on affected nodes becomes temporarily disabled or unhealthy. NVSentinel uses DCGM as a health indicator for the GPU driver. When DCGM connectivity fails, NVSentinel:

1. Marks the node as unhealthy
2. Applies the `GpuDcgmConnectivityFailure` node condition
3. Cordons the node to prevent new workload scheduling

**Potential Issues:**
- When multiple nodes are upgraded in parallel, the circuit breaker may trip, preventing NVSentinel from processing new events and leaving nodes in a cordoned state.
- When the circuit breaker resets after maintenance, NVSentinel may process queued events, potentially causing the circuit breaker to trip again or nodes to be cordoned and uncordoned repeatedly (tracked in [#450](https://github.com/NVIDIA/NVSentinel/issues/450)).
- These behaviors can cause cluster availability issues.

**Solution:** Temporarily disable NVSentinel management on nodes undergoing GPU driver or GPU Operator upgrades.

## Procedure

### 1. Disable NVSentinel Management on Target Nodes

Apply the `k8saas.nvidia.com/ManagedByNVSentinel=false` label to all nodes that will be upgraded.

```bash
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false
```

**Note:** Replace `--all` with specific node names if only upgrading a subset of nodes.

### 2. Perform the GPU Driver or GPU Operator Upgrade

Execute the GPU driver or GPU Operator upgrade using your organization's standard upgrade procedure.

### 3. Validate GPU Component Health

Verify that all pods in the `gpu-operator` namespace are running and healthy:

```bash
kubectl get po -n gpu-operator
```

Ensure all pods show `Running` status and are ready before proceeding.

### 4. Re-enable NVSentinel Management

Remove the `k8saas.nvidia.com/ManagedByNVSentinel` label from the upgraded nodes:

```bash
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-
```

**Note:** Replace `--all` with specific node names if only a subset of nodes was upgraded.

#### Verification

After re-enabling NVSentinel management, monitor the nodes to ensure:
- Nodes remain in `Ready` state
- No unexpected cordoning occurs
- NVSentinel metrics show normal operation
