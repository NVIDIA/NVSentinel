# FQM Latency & Queue Depth Performance

**Objective:** Measure FQM (Fault Quarantine Module) performance for cordoning nodes in response to fatal health events.

## Test Overview

This document covers two performance measurements:
1. **FQM Latency Measurement** - End-to-end latency from fatal event to node cordon
2. **Queue Depth Measurement** - Queue buildup when 10% of cluster experiences simultaneous failures

---

## Test Environment

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.4.0  
**MongoDB:** 3 replicas, 6Gi memory per replica

### Pipeline Measured

```
┌─────────────┐    ┌───────────────────┐    ┌─────────┐    ┌─────────────┐    ┌───────────┐
│  SIGUSR1    │───▶│ Event Generator   │───▶│ Platform│───▶│  MongoDB    │───▶│    FQM    │───▶ Node Cordoned
│  (T0)       │    │ (gRPC/UDS)        │    │Connector│    │  (insert)   │    │ (change   │
└─────────────┘    └───────────────────┘    └─────────┘    └─────────────┘    │  stream)  │
                                                                              └───────────┘
```

**Latency = T(cordon) - T(SIGUSR1)**

---

## FQM Latency Measurement

### Test Configuration

| Parameter | Value |
|-----------|-------|
| Nodes tested | 150 (10% of cluster) |
| Mode | Concurrent |
| Stagger | 0-30 seconds random |
| Event type | Fatal GPU XID error |
| Trigger | SIGUSR1 signal to event generator pods |

### Results

| Metric | Latency |
|--------|---------|
| **Min** | 1.79s |
| **P50 (Median)** | 16.75s |
| **P90** | **28.15s** |
| **P95** | 29.75s |
| **P99** | 31.35s |
| **Max** | 31.75s |
| **Mean** | 16.26s |

**Success Rate:** 150/150 nodes cordoned (100%)

### Key Findings

✅ **P90 latency: 28.15 seconds** under concurrent load

- All 150 nodes successfully cordoned within timeout
- Latency range reflects variable queueing delays due to concurrent processing
- FQM MongoDB change stream performed reliably
- Circuit breaker remained CLOSED (150/1500 nodes = 10% < 50% threshold)

### Test Timestamps (for Prometheus queries)

```
Time Range: 2025-12-04T11:29:00Z to 2025-12-04T11:31:00Z
Epoch:      1733314187 to 1733314252
```

---

## Queue Depth Measurement

### Test Configuration

| Parameter | Value |
|-----------|-------|
| Nodes tested | 150 (10% of cluster) |
| Mode | Combined (latency + queue depth) |
| Stagger | 0-10 seconds random |
| Polling interval | 30 seconds |
| Event type | Fatal GPU XID error |

### Results

#### Queue Depth Statistics

| Metric | Value |
|--------|-------|
| **Peak Queue Depth** | 138 nodes |
| **Average Queue Depth** | 63.8 nodes |
| **Time to Clear** | 149.1 seconds |

#### Queue Progression Over Time

| Time | Cordoned | Remaining Queue | Processing Rate |
|------|----------|-----------------|-----------------|
| T+29s | 12/150 | 138 | ~0.4 nodes/sec |
| T+59s | 51/150 | 99 | ~1.3 nodes/sec |
| T+89s | 91/150 | 59 | ~1.3 nodes/sec |
| T+119s | 127/150 | 23 | ~1.2 nodes/sec |
| T+149s | 150/150 | 0 | Complete |

**Success Rate:** 150/150 nodes cordoned (100%)

### Key Findings

✅ **Peak queue: 138 nodes** with complete clearance in ~149 seconds

- FQM processes approximately **1 node per second** under load
- Queue depth peaks early (first 30s) then steadily decreases
- All nodes successfully cordoned - no timeouts or failures
- System handles 10% simultaneous cluster failure gracefully

### Test Timestamps (for Prometheus queries)

```
Time Range: 2025-12-04T20:04:00Z to 2025-12-04T20:07:00Z
Epoch:      1764878647 to 1764878799
```

---

## Prometheus Queries

```promql
# FQM Event Handling Duration
histogram_quantile(0.90, sum(rate(fault_quarantine_event_handling_duration_seconds_bucket[5m])) by (le))

# FQM Event Backlog Count
fault_quarantine_event_backlog_count

# Platform Connector Write Queue Latency
histogram_quantile(0.90, sum(rate(platform_connector_workqueue_latency_seconds_bucket{queue="databaseStore"}[5m])) by (le))
```

---

## Test Setup & Execution

### Prerequisites

1. NVSentinel v0.4.0 deployed with FQM, Node Drainer, Fault Remediation enabled
2. MongoDB 3-replica with 6Gi memory per replica
3. Clean cluster state (no cordoned nodes)

### Deploy Event Generator (SIGUSR1 Mode)

```bash
# Deploy ConfigMap for SIGUSR1 mode (no EVENT_RATE)
kubectl apply -f manifests/event-generator-config-fqm-latency.yaml

# Deploy event generator DaemonSet
kubectl apply -f manifests/event-generator-daemonset.yaml

# Verify pods are in SIGUSR1 mode
kubectl logs -n nvsentinel -l app=event-generator --tail=5
# Expected: "✅ Ready to receive SIGUSR1 signals for fatal event generation"
```

### Run Tests

```bash
cd cmd/fqm-scale-test
go build -o fqm-scale-test .

# FQM Latency Measurement
./fqm-scale-test -mode=latency -nodes=150 -concurrent=true -stagger=30 -timeout=240

# Queue Depth Measurement
./fqm-scale-test -mode=combined -nodes=150 -concurrent=true -stagger=10 -timeout=300
```

### Cluster Reset Between Tests

```bash
# Restart FQM to clear in-memory state
kubectl rollout restart deployment/fault-quarantine -n nvsentinel

# Bulk uncordon all nodes
kubectl get nodes -l k8saas.nvidia.com/cordon-by=NVSentinel -o name | xargs -r kubectl uncordon

# Remove NVSentinel labels
kubectl label nodes --all \
    k8saas.nvidia.com/cordon-by- \
    k8saas.nvidia.com/cordon-reason- \
    k8saas.nvidia.com/cordon-timestamp- \
    dgxc.nvidia.com/nvsentinel-state-

# Wait for FQM ready
kubectl rollout status deployment/fault-quarantine -n nvsentinel --timeout=60s

# IMPORTANT: Also wipe MongoDB state
# scripts/mongodb-shell.sh
# db.ResumeTokens.deleteMany({})
# db.HealthEvents.deleteMany({})
```

---

## Summary

| Test | Key Metric | Result |
|------|------------|--------|
| **FQM Latency** | P90 Latency | 28.15s |
| **Queue Depth** | Peak Queue | 138 nodes |
| **Queue Depth** | Time to Clear | 149.1s |
| **Both** | Success Rate | 100% |

**Conclusion:** NVSentinel FQM handles 10% simultaneous cluster failures (150 nodes) with P90 latency of 28.15 seconds and complete queue clearance in under 3 minutes. The system processes approximately 1 node per second under concurrent load with 100% success rate.

---

## Raw Data

| File | Description |
|------|-------------|
| `fqm-latency-150nodes.csv` | Per-node latency data from FQM Latency test |
| `queue-depth-latency-150nodes.csv` | Per-node latency data from Queue Depth test |
| `queue-depth-snapshots.csv` | Queue depth snapshots over time |

---

*Cluster:* 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
*NVSentinel Version:* v0.4.0  
*Test Date:* December 4, 2025

