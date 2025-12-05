# Concurrent Drain Operations Scale Test Results

**Issue:** #385  
**Version:** NVSentinel v0.4.0
**Cluster:** 1500 nodes

---

## Test Overview

**Objective:** Validate Node Drainer Manager's ability to handle concurrent drain operations at scale

**Description:** When nodes are cordoned due to fatal health events, Node Drainer evicts user pods to allow maintenance or repair. This test measures NDM performance when draining many nodes concurrently, using two workload patterns: inference clusters (many small pods) and training clusters (few large pods).

### Test Matrix

| Test | Workload | Scale | Nodes | Pods/Node | Total Pods |
|------|----------|-------|-------|-----------|------------|
| **A1** | Inference (small) | 10% | 150 | 15 | ~2,250 |
| **A2** | Inference (small) | 25% | 375 | 15 | ~5,625 |
| **B1** | Training (large) | 10% | 150 | 3 | ~450 |
| **B2** | Training (large) | 25% | 375 | 3 | ~1,125 |

*Scale = percentage of the 1500-node cluster experiencing simultaneous failures requiring drain (e.g., 10% = 150 nodes cordoned and drained concurrently).*

---

## Results Summary

### Workload A: Inference Cluster (15 small pods/node)

| Test | Nodes | Pods Evicted | Drain Time | Peak Queue | Avg Rate |
|------|-------|--------------|------------|------------|----------|
| **A1** | 150 | ~2,250 | TBD | TBD | TBD |
| **A2** | 375 | ~5,625 | TBD | TBD | TBD |

### Workload B: Training Cluster (3 large pods/node)

| Test | Nodes | Pods Evicted | Drain Time | Peak Queue | Avg Rate |
|------|-------|--------------|------------|------------|----------|
| **B1** | 150 | ~450 | TBD | TBD | TBD |
| **B2** | 375 | ~1,125 | TBD | TBD | TBD |

---

## Node Drainer Metrics

| Metric | A1 | A2 | B1 | B2 |
|--------|----|----|----|----|
| Events received | TBD | TBD | TBD | TBD |
| Events processed (drained) | TBD | TBD | TBD | TBD |
| Processing errors | TBD | TBD | TBD | TBD |
| Peak queue depth | TBD | TBD | TBD | TBD |
| Event handling P50 | TBD | TBD | TBD | TBD |
| Event handling P90 | TBD | TBD | TBD | TBD |

---

## Kubernetes API Impact

| Metric | Baseline | A1 | A2 | B1 | B2 |
|--------|----------|----|----|----|----|
| API Server P75 latency | TBD | TBD | TBD | TBD | TBD |
| Eviction requests/sec | - | TBD | TBD | TBD | TBD |
| Eviction errors | - | TBD | TBD | TBD | TBD |

---

## Detailed Observations

### Workload A: Inference Cluster

*(Observations about many small pod evictions)*

### Workload B: Training Cluster

*(Observations about large pod evictions and resource pressure)*

### Eviction API Throttling

*(Observations about API throttling behavior)*

### Timeout Behavior

*(Observations about drain timeouts and force deletions)*

---

## Graphs

*(Graphs to be added after test execution)*

- Drain completion timeline (all 4 tests overlay)
- Node Drainer queue depth over time
- API server latency during tests
- Eviction rate comparison (Workload A vs B)

---

## Conclusions

*(Conclusions to be added after analysis)*

---

## Test Environment

- **Cluster:** rs3 (1500 nodes)
- **NVSentinel Version:** v0.4.0
- **MongoDB:** 3-replica, 6Gi memory per replica
- **Workload A:** inference-sim (15 pods/node, 30s termination)
- **Workload B:** training-sim (3 pods/node, 60s termination)
- **Test Date:** TBD

---

**Last Updated:** December 5, 2025

