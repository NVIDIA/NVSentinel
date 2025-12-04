# FQM Scale Test Plan

**Issue:** #384  
**Version:** NVSentinel v0.4.0  
**Cluster:** 1500 nodes (rs3)

---

## ⚠️ IMPORTANT NOTES

1. **Image Registry:** Manifests use `ghcr.io/nvidia/nvsentinel` as a placeholder. For testing, **locally edit** to use `nvcr.io/nv-ngc-devops/` but **DO NOT check in** the private registry URL.

2. **DaemonSet Updates:** NEVER do `kubectl rollout restart daemonset` on 1500 nodes. Instead:
   ```bash
   kubectl delete ds event-generator -n nvsentinel
   # wait for pods to terminate
   kubectl apply -f manifests/event-generator-daemonset.yaml
   ```

3. **ConfigMap for SIGUSR1 Mode:** Use `event-generator-config-fqm-latency.yaml` (no EVENT_RATE) for latency testing. The presence of EVENT_RATE triggers continuous mode instead of SIGUSR1 mode.

---

## Objectives

Answer these questions from the NVSentinel team:

1. **What is the P90 latency of cordoning a node from the time of adding node condition?**
2. **What is the average number of nodes waiting to get cordoned if we assume a 10% error rate?**

---

## Test Structure

### Phase 1: End-to-End Tests

Unified test program (`fqm-scale-test`) measures the full pipeline:

```
┌─────────────┐    ┌───────────────────┐    ┌─────────┐    ┌─────────────┐    ┌───────────┐
│  SIGUSR1    │───▶│ Event Generator   │───▶│ Platform│───▶│  MongoDB    │───▶│    FQM    │───▶ Node Cordoned
│  (T0)       │    │ (gRPC/UDS)        │    │Connector│    │  (insert)   │    │ (change   │
└─────────────┘    └───────────────────┘    └─────────┘    └─────────────┘    │  stream)  │
                                                                              └───────────┘
     [A]                  [B]                   [C]              [D]              [E]
```

**Latency = T(cordon) - T(SIGUSR1)**

Two measurements (can run multiple times with different stagger values):

| Measurement | Purpose | Parameters |
|-------------|---------|------------|
| **FQM Latency Measurement** | P90 latency from fatal event → node cordon | 150 nodes, stagger 0-30s |
| **Queue Depth Measurement** | Queue buildup when 10% of cluster fails | 150 nodes, stagger 0-10s, poll every 1s |

### Phase 2: Microbenchmarks

Isolate individual pipeline stages using Prometheus metrics and direct MongoDB benchmarks.

---

## Phase 1: End-to-End Tests

### FQM Latency Measurement

**Purpose:** Measure FQM latency with concurrent load

| Parameter | Value |
|-----------|-------|
| Nodes tested | 150 |
| Mode | Concurrent (all at once) |
| Stagger | 0-30 seconds random |
| Event type | Fatal GPU XID 79 |

**Expected Result (from v0.3.0):** P90 ≈ 29.2 seconds

### Queue Depth Measurement

**Purpose:** Measure FQM queue buildup when 10% of cluster fails simultaneously

| Parameter | Value |
|-----------|-------|
| Nodes tested | 150 (10% of cluster) |
| Mode | Concurrent |
| Stagger | 0-10 seconds random |
| Measurement | Poll every 1 second |

**Expected Results (from v0.3.0):**
- Peak queue depth: 104-112 nodes
- Average queue depth: 43-45 nodes  
- Processing rate: 0.9 nodes/sec
- Time to clear: ~170 seconds

---

## Phase 2: Microbenchmarks

### Prometheus Metrics

| Metric | Component | Purpose |
|--------|-----------|---------|
| `fault_quarantine_event_handling_duration_seconds` | FQM | Time FQM takes to process each event |
| `fault_quarantine_event_backlog_count` | FQM | Real-time queue depth |
| `platform_connector_workqueue_latency_seconds_databaseStore` | Platform Connector | Time in MongoDB write queue |

### MongoDB Insert+Update Benchmark

Direct MongoDB benchmark to isolate database latency:

```javascript
// Run in mongosh
const start = new Date();
for (let i = 0; i < 1000; i++) {
  const id = ObjectId();
  db.HealthEvents.insertOne({
    _id: id,
    healthevent: {agent: 'benchmark', isfatal: true, nodename: 'test-'+i},
    healtheventstatus: {nodequarantined: null}
  });
  db.HealthEvents.updateOne(
    {_id: id},
    {$set: {'healtheventstatus.nodequarantined': 'Quarantined'}}
  );
}
print(`Duration: ${new Date() - start}ms for 1000 insert+update pairs`);
```

---

## Test Execution

### Prerequisites

1. **NVSentinel v0.4.0** deployed with FQM, Node Drainer, Fault Remediation enabled
2. **MongoDB** 3-replica with 6Gi memory per replica
3. **Clean cluster state** (no cordoned nodes)

### Setup

```bash
# 1. Edit manifest locally (DO NOT CHECK IN)
# Change: ghcr.io/nvidia/nvsentinel → nvcr.io/nv-ngc-devops
vim manifests/event-generator-daemonset.yaml

# 2. Deploy ConfigMap for SIGUSR1 mode (no EVENT_RATE)
kubectl apply -f manifests/event-generator-config-fqm-latency.yaml

# 3. Deploy event generator DaemonSet
kubectl apply -f manifests/event-generator-daemonset.yaml

# 4. Verify pods are in SIGUSR1 mode (check logs say "On-Demand Mode")
kubectl logs -n nvsentinel -l app=event-generator --tail=5
```

### Run FQM Latency Measurement

```bash
cd cmd/fqm-scale-test
go build -o fqm-scale-test .

./fqm-scale-test -mode=latency -nodes=150 -concurrent=true -stagger=30 -timeout=240 -output=../../results/latency
```

### ⚠️ CLUSTER RESET (Between Tests)

```bash
# 1. Restart FQM FIRST to clear in-memory state
kubectl rollout restart deployment/fault-quarantine -n nvsentinel

# 2. Bulk uncordon all nodes
kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel -o name | xargs -r kubectl uncordon

# 3. Bulk remove NVSentinel labels
kubectl label nodes -l k8saas.nvidia.com/cordon-by=NVSentinel \
    k8saas.nvidia.com/cordon-by- \
    k8saas.nvidia.com/cordon-reason- \
    k8saas.nvidia.com/cordon-timestamp- \
    dgxc.nvidia.com/nvsentinel-state-

# 4. Wait for FQM to be ready
kubectl rollout status deployment/fault-quarantine -n nvsentinel --timeout=60s

# 5. Verify clean state
kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel --no-headers | wc -l
# Should be 0
```

**⚠️ ALSO: Wipe MongoDB state manually!**
```bash
scripts/mongodb-shell.sh
# Then in mongosh: db.HealthEvents.deleteMany({})
```

### Run Queue Depth Measurement

```bash
./fqm-scale-test -mode=combined -nodes=150 -stagger=10 -timeout=300 -output=../../results/queue-depth
```

---

## Deliverables

1. **Results document** with P50/P90/P95 latencies
2. **Comparison table** v0.3.0 vs v0.4.0
3. **Prometheus metric snapshots**
4. **Graphs:**
   - Latency distribution histogram
   - Queue depth over time
   - Processing rate over time

---

**Last Updated:** December 4, 2025
