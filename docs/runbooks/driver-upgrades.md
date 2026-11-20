# Runbook: GPU Driver and GPU Operator Upgrades

## Overview

This runbook gives the procedure to safely upgrade GPU drivers and GPU Operator components. The procedure prevents interference from NVSentinel during the upgrade process.

## Background

During GPU Operator or driver upgrades, DCGM on affected nodes becomes temporarily disabled or unhealthy. NVSentinel uses DCGM as a health indicator for the GPU driver. When DCGM connectivity fails, NVSentinel:

1. Marks the node as unhealthy
2. Applies the `GpuDcgmConnectivityFailure` node condition
3. Cordons the node to prevent new workload scheduling

**Potential Issues:**

- If you upgrade multiple nodes in parallel, the circuit breaker can trip. Then NVSentinel does not process new health events, and the nodes stay in a cordoned state.
- When the circuit breaker resets after maintenance, NVSentinel can process queued health events. This can cause the circuit breaker to trip again. It can also cause NVSentinel to cordon and uncordon nodes repeatedly.
- These behaviors can cause cluster availability issues.

**Solution:** Temporarily disable NVSentinel management on nodes during their GPU driver or GPU Operator upgrades.

The opt-out label of this procedure takes effect only through a match condition of kind `Node` in the fault-quarantine rule sets. The default rule sets include this condition. If you use custom rule sets, make sure that each one includes this condition before you start. Otherwise, NVSentinel can still quarantine the nodes during the upgrade. See [Rule Sets](../configuration/fault-quarantine.md#rule-sets).

## Procedure

### 1. Disable NVSentinel Management on Target Nodes

Apply the `k8saas.nvidia.com/ManagedByNVSentinel=false` label to all the nodes to upgrade.

```bash
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false
```

**Note:** If you upgrade only a subset of nodes, replace `--all` with specific node names.

### 2. Perform the GPU Driver or GPU Operator Upgrade

Upgrade the GPU driver or GPU Operator with your organization's standard upgrade procedure.

### 3. Validate GPU Component Health

Verify that all pods in the `gpu-operator` namespace run and are healthy:

```bash
kubectl get po -n gpu-operator
```

Before you continue, make sure that all pods show `Running` status and are ready.

### 4. Re-enable NVSentinel Management

Remove the `k8saas.nvidia.com/ManagedByNVSentinel` label from the upgraded nodes:

```bash
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-
```

**Note:** If you upgraded only a subset of nodes, replace `--all` with specific node names.

#### Verification

After you re-enable NVSentinel management, monitor the nodes for these conditions:

- Nodes remain in `Ready` state
- No unexpected cordoning occurs
- NVSentinel metrics show normal operation
