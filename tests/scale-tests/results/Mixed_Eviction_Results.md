# Mixed Eviction Modes Scale Test Results

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.8.0  
**Test Date:** February 2, 2026

> **Note:** This test suite was added after the initial scale testing effort. Mixed eviction mode functionality requires NVSentinel v0.8.0 or later.

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
| **M3** | 50% | 750 | 1,500 | 4,500 | Mixed eviction modes at half-cluster scale |

### Workload Details

**Workload:** inference-sim (simulated inference serving workload)
- **Pod density:** 1,500 pods per namespace (intended ~1 per node)
- **Resources:** 10m CPU, 32Mi memory per pod
- **Termination grace period:** 30s
- **Total pods:** 4,500 (1,500 in each of 3 namespaces)

*Note: This workload simulates inference serving patterns with many lightweight pods and fast shutdown.*

### Note on Pod Distribution

Early test runs showed extremely uneven pod distribution (30-70+ pods on some nodes, 1,000+ nodes with zero pods). This was caused by **skyhook taints** on most nodes preventing pod scheduling. After removing the taints (`kubectl taint nodes --all skyhook.nvidia.com:NoSchedule-`), pod distribution improved dramatically.

The test manifests include `topologySpreadConstraints` with `maxSkew: 3`, which ensures reasonable distribution across nodes when taints are not blocking scheduling. Final test runs achieved near-perfect distribution: M1 showed 150/150 match, M2 showed 366/375 (97.6%), and M3 showed 750/750 (100%).

### Metrics Monitored

| Metric | Description |
|--------|-------------|
| `node_drainer_force_delete_pods_after_timeout` | Pods force-deleted (should only appear for DeleteAfterTimeout) |
| `node_drainer_waiting_for_timeout` | Pods waiting for timeout |
| `node_drainer_events_processed_total` | Total events processed |
| `node_drainer_events_received_total` | Total events received |
| `node_drainer_processing_errors_total` | Processing errors |
| `node_drainer_queue_depth` | Current queue depth |

---

## Results Summary

### Test M1: 10% Scale (150 nodes)

**Test Parameters:**
- **Nodes cordoned:** 150
- **Time to cordon (FQM):** 62.6s  
- **Cordon rate:** 2.50 nodes/sec
- **Peak queue depth:** 129 nodes

**Eviction Results:**

| Namespace | Eviction Mode | Initial on Cordoned | After 30s | After 60s | After 90s | Final | Status |
|-----------|---------------|---------------------|-----------|-----------|-----------|-------|--------|
| test-immediate | Immediate | 7 | 0 | 0 | 0 | 0 | Fully drained |
| test-allow-completion | AllowCompletion | 150 | 150 | 150 | 150 | 150 | NOT evicting (correct!) |
| test-delete-timeout | DeleteAfterTimeout | 151 | 83 | 9 | 0 | 0 | Force-deleted after 2 min |

**Drain Progress Timeline:**

| Time | test-immediate | test-allow-completion | test-delete-timeout |
|------|----------------|----------------------|---------------------|
| T+0 (cordon done) | 7 | 150 | 151 |
| T+30s | 0 | 150 | 83 |
| T+60s | 0 | 150 | 9 |
| T+90s | 0 | 150 | 0 |

**Key Observations:**
- **Immediate mode:** Almost fully drained *during* cordon phase (only 7 pods remaining at T+0); fully drained by T+30s
- **AllowCompletion mode:** Exactly 150 pods remained on cordoned nodes throughout (correct behavior - NOT actively evicting)
- **DeleteAfterTimeout mode:** 151 pods on cordoned nodes, fully drained by T+90s after 2-minute timeout
- **Drain speed:** Immediate mode is extremely fast — cordon and drain are separate sequential API calls, but the time gap is so small that eviction appears to happen concurrently with cordoning

---

### Test M2: 25% Scale (375 nodes)

**Test Parameters:**
- **Nodes cordoned:** 375
- **Time to cordon (FQM):** 150.6s
- **Cordon rate:** 2.59 nodes/sec
- **Peak queue depth:** 349 nodes

**Eviction Results (with proper pod distribution):**

| Namespace | Eviction Mode | Initial on Cordoned | After 1 min | After 2 min | After 3 min | Final | Status |
|-----------|---------------|---------------------|-------------|-------------|-------------|-------|--------|
| test-immediate | Immediate | ~375 | 145 | 45 | 0 | 0 | Fully drained |
| test-allow-completion | AllowCompletion | 366 | 366 | 366 | 366 | 366 | NOT evicting (correct!) |
| test-delete-timeout | DeleteAfterTimeout | ~375 | 298 | 164 | 70 | 0 | Force-deleted after 2 min |

**Drain Progress Timeline:**

| Time | test-immediate | test-allow-completion | test-delete-timeout |
|------|----------------|----------------------|---------------------|
| T+0 (initial) | 165 | 366 | 317 |
| T+30s | 145 | 366 | 298 |
| T+1 min | 96 | 366 | 252 |
| T+1.5 min | 45 | 366 | 206 |
| T+2 min | 0 | 366 | 164 |
| T+2.5 min | 0 | 366 | 112 |
| T+3 min | 0 | 366 | 70 |
| Final | 0 | 366 | 0 |

**Key Observations:**
- **Immediate mode:** All pods evicted - fully drained by ~2 minutes
- **AllowCompletion mode:** 366 pods remained on cordoned nodes throughout (correct behavior - NOT actively evicting)
- **DeleteAfterTimeout mode:** Force-deleted after 2-minute timeout; fully drained by ~4 minutes

**Note on Pod Distribution:** The 366 pods on cordoned nodes (vs 375 cordoned nodes) represents typical Kubernetes scheduler behavior. Even with 99.4% of nodes having exactly 1 pod, some variance is expected: a few cordoned nodes may have had 0 pods while others had 2. This 97.6% match (366/375) is representative of real-world deployments and validates that the eviction modes work correctly regardless of exact pod-to-node ratios.

**Behavior Validation:**
- Cordon rate consistent: 2.59 nodes/sec (matches M1 rate)
- All three eviction modes worked correctly without interference
- DeleteAfterTimeout correctly waited 2 minutes before force-deleting
- AllowCompletion correctly does NOT evict pods (steady at 366 throughout)

---

### Test M3: 50% Scale (750 nodes)

**Test Parameters:**
- **Nodes cordoned:** 750
- **Time to cordon (FQM):** 300.8s (~5 min)
- **Cordon rate:** 2.59 nodes/sec
- **Peak queue depth:** 711 nodes

**Eviction Results (with perfect pod distribution):**

| Namespace | Eviction Mode | Initial on Cordoned | After 2.5 min | After 6 min | After 8 min | Final (~10 min) | Status |
|-----------|---------------|---------------------|---------------|-------------|-------------|-----------------|--------|
| test-immediate | Immediate | ~750 | 224 | 0 | 0 | 0 | Fully drained |
| test-allow-completion | AllowCompletion | 750 | 750 | 750 | 750 | 750 | NOT evicting (correct!) |
| test-delete-timeout | DeleteAfterTimeout | ~750 | 473 | 293 | 118 | 0 | Force-deleted after 2 min |

**Drain Progress Timeline:**

| Time (from cordon complete) | test-immediate | test-allow-completion | test-delete-timeout |
|-----------------------------|----------------|----------------------|---------------------|
| T+2.5 min | 224 | 750 | 473 |
| T+6 min | 0 | 750 | 293 |
| T+8 min | 0 | 750 | 118 |
| T+10 min (final) | 0 | 750 | 0 |

**Key Observations:**
- **Immediate mode:** All pods evicted - fully drained by ~6 minutes after cordon complete
- **AllowCompletion mode:** Exactly 750 pods remained throughout (perfect 100% match with cordoned nodes!)
- **DeleteAfterTimeout mode:** Force-deleted after 2-minute timeout; fully drained by ~10 minutes
- **Pod distribution:** Perfect distribution (1 pod per node) resulted in exact 750/750 match for AllowCompletion

**Note on Pod Distribution:** With perfect pod distribution (max 1 pod per node, only 3 nodes with 0 pods), test-allow-completion showed an exact 750 pod count matching the 750 cordoned nodes. This validates that our eviction mode logic is working correctly and that pod distribution directly impacts observed counts.

---

## Comparison: M1 vs M2 vs M3

| Metric | M1 (150 nodes) | M2 (375 nodes) | M3 (750 nodes) | Scaling Behavior |
|--------|----------------|----------------|----------------|------------------|
| Nodes cordoned | 150 | 375 | 750 | Linear |
| Time to cordon | 62.6s | 150.6s | 300.8s | Linear (~2.5x per 2x nodes) |
| Cordon rate | 2.50 nodes/sec | 2.59 nodes/sec | 2.59 nodes/sec | Constant (~2.5/sec) |
| Peak queue | 129 | 349 | 711 | Linear |
| Immediate drain time | <30s | ~2 min | ~6 min | Concurrent with cordon |
| DeleteAfterTimeout drain | ~90s | ~4 min | ~10 min | ~2 min timeout + drain |
| AllowCompletion on cordoned | 150 | 366 | 750 | Matches cordoned nodes |
| AllowCompletion evicted | 0 | 0 | 0 | None (correct) |

**Key Findings (M1-M2-M3):**
1. **Linear scaling:** Cordon time scales linearly with node count (62s → 150s → 300s)
2. **Consistent throughput:** 2.5-2.6 nodes/sec cordon rate maintained across all scales
3. **No cross-contamination:** All three eviction modes worked independently without interference
4. **Immediate mode is fast:** Cordon and drain are sequential API calls, but eviction follows so quickly that most pods are drained before the cordon phase completes
5. **AllowCompletion verified:** Pods correctly NOT evicted in all three tests
6. **Perfect distribution validation:** M3 showed exact 750/750 match with perfect pod distribution

**Note on DeleteAfterTimeout Drain Time:**

The comparison table shows DeleteAfterTimeout drain times of ~90s, ~4 min, and ~10 min — longer than the configured 2-minute timeout. This is expected because:

1. **Timeout triggers initiation, not completion:** The 2-minute timeout determines *when* force-deletion begins, not when it finishes
2. **API rate limiting:** The Kubernetes client rate limiter (5 deletions/sec) throttles how fast pods can be deleted. With 750 pods to force-delete (M3), the deletion API calls alone take ~150 seconds
3. **Staggered timeouts:** Events are created over the cordon phase (~5 min for M3), so different nodes hit their 2-minute timeout at different times

The gradual decrease in pod counts (e.g., M2: 317 → 298 → 252 → 164 → 70 → 0) reflects this staggered timeout + rate-limited deletion pattern. The key validation is that all DeleteAfterTimeout pods are eventually force-deleted, which succeeds in all tests.

---

## Node Drainer Behavior Analysis

### Sequential Processing with Priority

Node Drainer processes eviction modes with the following priority:

1. **Step 1 - Immediate:** Evict all Immediate namespace pods first
2. **Step 2 - DeleteAfterTimeout:** Handle timeout logic for pods that should be force-deleted
3. **Step 3 - AllowCompletion:** Wait for remaining pods to complete naturally

### Timeline for Mixed Mode Drain

```
T0: Health event received → Node cordoned by FQM
T0+1s: NDM receives event
T0+2s: Immediate pods evicted (test-immediate)
T0+2s: Timeout timer starts for DeleteAfterTimeout pods (test-delete-timeout)
T0+2min: DeleteAfterTimeout pods force-deleted
T0+∞: AllowCompletion pods wait indefinitely (test-allow-completion)
```

### Observed Log Patterns

**Initial drain (first loop):**
```json
{"msg":"Evaluated action for node","action":"EvictImmediate","node":"ip-100-64-128-105"}
{"msg":"Pod eviction initiated","namespace":"test-immediate"}
```

**During timeout window (subsequent loops):**
```json
{"msg":"Evaluated action for node","action":"WaitingBeforeForceDelete","node":"ip-100-64-128-105"}
{"msg":"Waiting for following pods","namespace":"test-delete-timeout"}
```

**After 2-minute timeout:**
```json
{"msg":"Force deleting pods after timeout","namespace":"test-delete-timeout"}
{"msg":"Pod force deleted","pod":"inference-sim-xxx"}
```

**Ongoing check (AllowCompletion):**
```json
{"msg":"Evaluated action for node","action":"CheckCompletion","node":"ip-100-64-128-105"}
{"msg":"Pods still running on node","remainingPods":["test-allow-completion/inference-sim-xxx"]}
```

---

## Conclusions

### Test Objectives Met

1. **Mixed eviction modes work correctly at scale**
   - Immediate, AllowCompletion, and DeleteAfterTimeout all functioned as expected
   - No cross-contamination between modes
   - Validated at 10%, 25%, and 50% cluster scale on v0.8.0

2. **Performance scales linearly**
   - Consistent 2.5-2.6 nodes/sec cordon rate across all scales (150, 375, 750 nodes)
   - Queue handling remained efficient with peak depths scaling linearly (129 → 349 → 711 nodes)
   - Cordon time scales linearly: 62s → 150s → 300s

3. **Force-delete behavior correct**
   - DeleteAfterTimeout pods force-deleted after configured 2-minute timeout
   - All three tests showed complete drain of DeleteAfterTimeout pods
   - Drain time scales with node count but remains predictable

4. **AllowCompletion behavior verified at scale**
   - M3 showed perfect 750/750 match (100% of cordoned nodes retained pods)
   - Pods correctly NOT evicted in all three tests

### Key Metrics

| Metric | Target | M1 Result | M2 Result | M3 Result | Status |
|--------|--------|-----------|-----------|-----------|--------|
| Immediate eviction | All pods evicted | 0 remaining | 0 remaining | 0 remaining | Pass |
| AllowCompletion wait | Pods remain on node | 150 remaining | 366 remaining | 750 remaining | Pass |
| DeleteAfterTimeout | Force-delete after 2 min | 0 remaining | 0 remaining | 0 remaining | Pass |
| Cordon rate | ~2.5 nodes/sec | 2.50 nodes/sec | 2.59 nodes/sec | 2.59 nodes/sec | Pass |
| Pod distribution match | Close to cordoned | 150/150 (100%) | 366/375 (97.6%) | 750/750 (100%) | Pass |

### Areas for Further Investigation

1. **Full cluster scale (100%):** Consider testing at 1500 nodes if needed
   - Current tests validated up to 50% of cluster (750 nodes)
   - Cordon rate remained constant; expect similar behavior at full scale

2. **Pod distribution impact:** Initial M2 test showed uneven distribution (1014 nodes with 0 pods)
   - After cluster reset, distribution improved dramatically
   - M3 achieved perfect 750/750 match with fresh deployment
   - **Recommendation:** Always deploy test workloads on a freshly reset cluster for accurate results

### Recommendations

1. **Production readiness:** Mixed eviction modes are production-ready in v0.8.0+
2. **Configuration guidance:** Document the priority order of eviction modes for operators
3. **Monitoring:** Recommend tracking `node_drainer_force_delete_pods_after_timeout` metric in production to validate timeout behavior
4. **Cluster preparation:** For testing, ensure fresh pod deployments on reset clusters to get accurate pod distribution

---

## Test Environment

- **Cluster:** rs3 (1500 nodes)
- **NVSentinel Version:** v0.8.0
- **MongoDB:** 3-replica, 6Gi memory per replica
- **Test Workloads:** inference-sim (1500 pods per namespace, 30s terminationGracePeriodSeconds)
- **Test Dates:** February 2-10, 2026

### Manifests Used

- `manifests/mixed-eviction-immediate.yaml` - Immediate mode namespace and workload
- `manifests/mixed-eviction-allow-completion.yaml` - AllowCompletion mode namespace and workload
- `manifests/mixed-eviction-delete-timeout.yaml` - DeleteAfterTimeout mode namespace and workload
- `configs/node-drainer-mixed-eviction.toml` - Node drainer configuration for mixed modes

### Tools Used

- `fqm-scale-test` - Triggers fatal events and monitors cordon progress
- Prometheus - Metrics collection and querying
- kubectl - Pod and node status verification

---

**Last Updated:** February 10, 2026
