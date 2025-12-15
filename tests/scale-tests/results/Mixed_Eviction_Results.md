# Mixed Eviction Modes Scale Test Results

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.4.0  
**Test Date:** December 15, 2025  
**Status:** 🚧 **Blocked by bug [#593](https://github.com/NVIDIA/NVSentinel/issues/593)**

---

## Test Overview

**Objective:** Validate that Node Drainer correctly handles different eviction policies (Immediate, AllowCompletion, DeleteAfterTimeout) simultaneously at scale

**Description:** Production clusters may have namespaces with different eviction requirements:
- **Immediate**: Critical infrastructure needing fast failover
- **AllowCompletion**: Long-running batch jobs that should complete gracefully  
- **DeleteAfterTimeout**: Jobs that should be given time but eventually force-deleted

This test validates that Node Drainer Manager (NDM) correctly handles these mixed policies on the same nodes without cross-contamination.

### Test Configuration

Three namespaces configured with different eviction modes:

| Namespace | Eviction Mode | Expected Behavior |
|-----------|---------------|-------------------|
| `test-immediate` | Immediate | Pods evicted immediately via Eviction API |
| `test-allow-completion` | AllowCompletion | Pods wait for natural completion (no active eviction) |
| `test-delete-timeout` | DeleteAfterTimeout | Pods force-deleted after `deleteAfterTimeoutMinutes` |

**Node Drainer Configuration:**
```toml
evictionTimeoutInSeconds = "60"
systemNamespaces = "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator|skyhook)$"
deleteAfterTimeoutMinutes = 2
notReadyTimeoutMinutes = 5

[[userNamespaces]]
name = "test-immediate"
mode = "Immediate"

[[userNamespaces]]
name = "test-allow-completion"
mode = "AllowCompletion"

[[userNamespaces]]
name = "test-delete-timeout"
mode = "DeleteAfterTimeout"

[[userNamespaces]]
name = "*"
mode = "AllowCompletion"
```

### Test Matrix

| Test | Scale | Nodes | Pods/Namespace | Total Pods | Purpose |
|------|-------|-------|----------------|------------|---------|
| **M1** | 10% | 150 | 1,500 | 4,500 | Mixed eviction modes baseline |
| **M2** | 25% | 375 | 1,500 | 4,500 | Mixed eviction modes stress test |

### Metrics to Monitor

| Metric | Description |
|--------|-------------|
| `node_drainer_force_delete_pods_after_timeout` | Pods force-deleted (should only appear for DeleteAfterTimeout) |
| `node_drainer_waiting_for_timeout` | Pods waiting for timeout |
| `node_drainer_events_processed_total` | Events processed by outcome |
| `node_drainer_event_handling_duration_seconds` | Time to process each drain event |

---

## Preliminary Results

### Mini-Test (5 nodes)

Before running full scale tests, we ran a mini-test with 5 nodes to validate eviction mode behavior:

| Namespace | Eviction Mode | Pods on Cordoned Nodes | Status |
|-----------|---------------|------------------------|--------|
| `test-immediate` | Immediate | 0 | ✅ Evicted immediately |
| `test-allow-completion` | AllowCompletion | 5 | ✅ Waiting (correct behavior) |
| `test-delete-timeout` | DeleteAfterTimeout | 5 | ❌ **Never force-deleted** |

**Finding:** After 10+ minutes (well past the 2-minute `deleteAfterTimeoutMinutes`), pods in `test-delete-timeout` namespace were never force-deleted.

---

## Bug Discovered: [#593](https://github.com/NVIDIA/NVSentinel/issues/593)

### Summary

When a node has pods from both AllowCompletion AND DeleteAfterTimeout namespaces, the DeleteAfterTimeout behavior is never triggered.

### Root Cause

In `node-drainer/pkg/evaluator/evaluator.go`, the `getAction()` function processes eviction modes sequentially with early returns:

```go
func (e *NodeDrainEvaluator) getAction(ctx context.Context, ns namespaces, nodeName string) *DrainActionResult {
    // Step 1: Immediate - works fine
    if len(ns.immediateEvictionNamespaces) > 0 {
        // ... evicts immediately
    }

    // Step 2: AllowCompletion - THE BUG IS HERE
    if len(ns.allowCompletionNamespaces) > 0 {
        action := e.handleAllowCompletionNamespaces(ns, nodeName)
        if action != nil {
            return action  // ← EARLY RETURN blocks Step 3!
        }
    }

    // Step 3: DeleteAfterTimeout - NEVER REACHED
    if len(ns.deleteAfterTimeoutNamespaces) > 0 {
        action := e.handleDeleteAfterTimeoutNamespaces(ns, nodeName)
        // This code never runs when AllowCompletion pods exist
    }
}
```

### Evidence from Logs

Node drainer logs show only `CheckCompletion` action, never `EvictWithTimeout`:

```json
{"level":"INFO","msg":"Evaluated action for node","node":"ip-100-64-128-15.ec2.internal","action":"CheckCompletion"}
{"level":"INFO","msg":"Pods still running on node, requeueing for later check","node":"ip-100-64-128-15.ec2.internal","remainingPods":["test-allow-completion/inference-sim-597cb57655-j9s5r","test-delete-timeout/inference-sim-6564f68667-z97ps"]}
```

### Impact

- Mixed eviction modes with AllowCompletion + DeleteAfterTimeout on the same node will NEVER trigger force-delete
- Bug exists in v0.4.0, v0.5.0, and main branch
- Immediate mode works correctly regardless of other modes

### Workaround

None available. To use DeleteAfterTimeout mode, ensure no AllowCompletion namespaces have pods on the same nodes.

---

## Full Scale Test Results

🚧 **Pending bug fix for [#593](https://github.com/NVIDIA/NVSentinel/issues/593)**

### Test M1: 10% Scale (150 nodes)

| Namespace | Eviction Mode | Expected | Actual | Status |
|-----------|---------------|----------|--------|--------|
| test-immediate | Immediate | Evicted immediately | TBD | TBD |
| test-allow-completion | AllowCompletion | Wait indefinitely | TBD | TBD |
| test-delete-timeout | DeleteAfterTimeout | Force-delete after 2 min | TBD | TBD |

### Test M2: 25% Scale (375 nodes)

| Namespace | Eviction Mode | Expected | Actual | Status |
|-----------|---------------|----------|--------|--------|
| test-immediate | Immediate | Evicted immediately | TBD | TBD |
| test-allow-completion | AllowCompletion | Wait indefinitely | TBD | TBD |
| test-delete-timeout | DeleteAfterTimeout | Force-delete after 2 min | TBD | TBD |

---

## Test Environment

- **Cluster:** rs3 (1500 nodes)
- **NVSentinel Version:** v0.4.0
- **MongoDB:** 3-replica, 6Gi memory per replica
- **Test Workloads:** inference-sim (1500 pods per namespace, 30s terminationGracePeriodSeconds)
- **Test Date:** December 15, 2025

### Manifests Used

- `manifests/mixed-eviction-immediate.yaml` - Immediate mode namespace and workload
- `manifests/mixed-eviction-allow-completion.yaml` - AllowCompletion mode namespace and workload
- `manifests/mixed-eviction-delete-timeout.yaml` - DeleteAfterTimeout mode namespace and workload
- `configs/node-drainer-mixed-eviction.toml` - Node drainer configuration for mixed modes

---

## Conclusion

**Testing blocked by bug [#593](https://github.com/NVIDIA/NVSentinel/issues/593).**

Preliminary testing confirmed:
- ✅ Immediate mode works correctly at scale
- ✅ AllowCompletion mode works correctly (passive waiting)
- ❌ DeleteAfterTimeout mode is blocked when AllowCompletion namespaces exist

Full scale testing will resume after the bug fix is merged.

---

**Last Updated:** December 15, 2025

