# FQM Latency & Queue Depth Performance

**Objective:** Measure FQM (Fault Quarantine Module) performance for cordoning nodes in response to fatal health events.

---

## Test Environment

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.4.0  
**MongoDB:** 3 replicas, 6Gi memory per replica  
**Test Date:** December 4, 2025

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

## FQM Latency Results

| Scale | Nodes | Min | P50 | P90 | P95 | P99 | Max | Mean | Success |
|-------|-------|-----|-----|-----|-----|-----|-----|------|---------|
| 10% | 150 | 1.79s | 16.75s | **28.15s** | 29.75s | 31.35s | 31.75s | 16.26s | 100% |
| 25% | 375 | 2.34s | 91.06s | **163.44s** | 171.86s | 181.06s | 184.05s | 90.63s | 100% |
| 50% | 750 | - | - | **418.6s** | - | - | - | 2.31/sec | 100% |

**Test Parameters:** Concurrent mode, 0-30s random stagger, Fatal GPU XID error, Circuit breaker DISABLED for 25%+

---

## Queue Depth Results (10% = 150 nodes)

| Metric | Value |
|--------|-------|
| **Peak Queue Depth** | 138 nodes |
| **Average Queue Depth** | 63.8 nodes |
| **Time to Clear** | 149.1 seconds |
| **Processing Rate** | ~1 node/sec |

### Queue Progression

| Time | Cordoned | Queue | Rate |
|------|----------|-------|------|
| T+29s | 12/150 | 138 | ~0.4/sec |
| T+59s | 51/150 | 99 | ~1.3/sec |
| T+89s | 91/150 | 59 | ~1.3/sec |
| T+119s | 127/150 | 23 | ~1.2/sec |
| T+149s | 150/150 | 0 | Complete |

**Test Parameters:** 0-10s random stagger, 30s polling interval

---

## Key Findings

- ✅ P90 latency scales with load: 28s (10%) → 163s (25%)
- ✅ FQM processes ~1 node/sec under load
- ✅ 100% success rate up to 25% cluster failure
- ✅ Peak queue of 138 nodes clears in ~149 seconds

---

## Test Timestamps (for Prometheus)

| Test | Time Range (UTC) |
|------|------------------|
| Latency 10% (150 nodes) | 2025-12-04T11:29:00Z to 2025-12-04T11:31:00Z |
| Latency 25% (375 nodes) | 2025-12-04T21:43:50Z to 2025-12-04T21:47:30Z |
| Queue Depth (150 nodes) | 2025-12-04T20:04:00Z to 2025-12-04T20:07:00Z |

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

## Raw Data

| File | Description |
|------|-------------|
| `fqm-latency-150nodes.csv` | Per-node latency data from 10% test |
| `fqm-latency-375nodes.csv` | Per-node latency data from 25% test |
| `queue-depth-snapshots.csv` | Queue depth snapshots over time |

