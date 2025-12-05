# Concurrent Drain Operations Scale Test Results

**Issue:** #385  
**Version:** NVSentinel v0.5.0  
**Cluster:** 1500 nodes

---

## Test Overview

**Objective:** Validate Node Drainer Manager's ability to handle concurrent drain operations at scale

**Test Parameters:**
| Parameter | Value |
|-----------|-------|
| Nodes cordoned | 300 |
| Pods per node | 10-20 (user workload, not system namespace) |
| Total pods to evict | ~4,500 |
| Stagger | TBD |

---

## Results Summary

*(Test results to be added after execution)*

### Drain Completion Times

| Metric | Value |
|--------|-------|
| Total drain time | TBD |
| P50 per-node drain time | TBD |
| P90 per-node drain time | TBD |
| P99 per-node drain time | TBD |

### Node Drainer Metrics

| Metric | Value |
|--------|-------|
| Events received | TBD |
| Events processed (drained) | TBD |
| Events skipped | TBD |
| Processing errors | TBD |
| Peak queue depth | TBD |
| Avg event handling duration | TBD |

### Kubernetes API Impact

| Metric | Baseline | During Test |
|--------|----------|-------------|
| API Server P75 latency | TBD | TBD |
| Eviction API requests/sec | TBD | TBD |
| Eviction API errors | TBD | TBD |

---

## Detailed Observations

### Eviction API Throttling

*(Observations about API throttling behavior)*

### PodDisruptionBudget Interactions

*(Observations about PDB handling)*

### Timeout Behavior

*(Observations about drain timeouts and force deletions)*

---

## Graphs

*(Graphs to be added after test execution)*

- Drain completion timeline
- Node Drainer queue depth over time
- API server latency during test
- Eviction rate over time

---

## Conclusions

*(Conclusions to be added after analysis)*

---

## Test Environment

- **Cluster:** rs3 (1500 nodes)
- **NVSentinel Version:** v0.5.0
- **MongoDB:** 3-replica, 6Gi memory per replica
- **Test Date:** TBD

---

**Last Updated:** December 5, 2025

