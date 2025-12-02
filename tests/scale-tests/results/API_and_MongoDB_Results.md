# API Server Impact & MongoDB Performance

**Objective:** Validate that NVSentinel does not negatively impact Kubernetes API server performance or overwhelm MongoDB at realistic production event rates.

## Test Overview

This document covers two phases of testing:
1. **Phase 1: Sustained Production Load** - Validates performance at typical production sustained rates
2. **Phase 2: Production Burst Events** - Tests resilience during production-level burst scenarios

*See [PRODUCTION_BASELINE.md](PRODUCTION_BASELINE.md) for production event rate analysis (sustained: 5-373 events/sec, peak bursts: up to 2,033 events/sec)*

---

## Phase 1: Sustained Production Load Testing

### Test Configuration

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.4.0  
**Duration:** 10 minutes per test

| Test | Event Rate | Production Context |
|------|-----------|-------------------|
| **Light** | 30 events/sec | Conservative baseline (below production averages) |
| **Medium** | 100 events/sec | Typical production sustained load |
| **Heavy** | 500 events/sec | Elevated sustained load (above highest production average) |

### Scaling Rationale for 1500-Node Cluster

Our test event rates are based on analysis of production clusters up to ~600 nodes. Large production clusters show sustained event rates of 0.025-0.030 events/node/sec. For a 1500-node cluster, this extrapolates to:
- 1500 nodes × 0.025 events/node/sec = **37.5 events/sec**
- 1500 nodes × 0.030 events/node/sec = **45 events/sec**

Our test scenarios provide validation across this spectrum:
- **Light (30 events/sec):** Below extrapolated baseline - validates conservative operation
- **Medium (100 events/sec):** 2-3× extrapolated baseline - represents elevated but realistic sustained load  
- **Heavy (500 events/sec):** Exceeds the highest observed production average (414 events/sec from Cluster C) - demonstrates headroom beyond current production demands

## Test Setup & Execution

### Deploy Event Generators

For each test scenario:

```bash
# Light load (30 events/sec)
kubectl apply -f manifests/event-generator-config-light.yaml
kubectl apply -f manifests/event-generator-daemonset.yaml

# Medium load (100 events/sec)
kubectl apply -f manifests/event-generator-config-medium.yaml
kubectl apply -f manifests/event-generator-daemonset.yaml

# Heavy load (500 events/sec)
kubectl apply -f manifests/event-generator-config-heavy.yaml
kubectl apply -f manifests/event-generator-daemonset.yaml

# Verify deployment
kubectl get pods -n nvsentinel -l app=event-generator
# Expected: 1500 pods (one per worker node) in Running state
```

### Monitor During Test

#### MongoDB Health
```bash
# Check MongoDB pod status
kubectl get pods -n nvsentinel -l app.kubernetes.io/component=mongodb

# Monitor memory usage
kubectl top pods -n nvsentinel -l app.kubernetes.io/component=mongodb

# Check replica set status
kubectl exec -n nvsentinel mongodb-0 -- mongosh --eval "rs.status()"
```

#### Prometheus Setup
```bash
# Port-forward to Prometheus
kubectl port-forward -n monitoring svc/prometheus-server 9090:80

# Access Prometheus UI at http://localhost:9090
```


### Test Duration

Each test ran for **10 minutes** with metrics collected at the midpoint using 5-minute rate windows.

## API Server Impact

| Test | Request Rate | P50 Latency | P75 Latency | P95/P99 Latency |
|------|-------------|-------------|-------------|-----------------|
| **Baseline** (no load) | 240 req/s | 5 ms | 20 ms | ≥60s* |
| **Light load** (30 events/sec) | 308 req/s | 6 ms | 20 ms | ≥60s* |
| **Medium load** (100 events/sec) | 456 req/s | 10 ms | 20 ms | ≥60s* |
| **Heavy load** (500 events/sec) | 1255 req/s | 14 ms | 21 ms | ≥60s* |

\* *P95 and P99 are capped at the histogram bucket limit of 60s, indicating the API server has some slow background operations unrelated to NVSentinel. This applies to both sustained load and burst testing.*

**Result:** 
- **Light load:** Request rate +28%, latency stable - no measurable impact
- **Medium load:** Request rate +89%, P50 doubled to 10ms but P75 stable at 20ms - minimal impact
- **Heavy load:** Request rate +422%, P50 increased to 14ms but **P75 remained stable at 21ms**.

## MongoDB Performance

| Test | Insert Rate | Total Events | Memory (MB) | Connections | Performance |
|------|-------------|--------------|-------------|-------------|-------------|
| **Light** | 1,985 ops/min (~30 events/sec) | ~19,850 events | 2,200 | 4,543 | ✅ Stable |
| **Medium** | 6,061 ops/min (~100 events/sec) | ~60,610 events | 1,934 | 4,549 | ✅ Stable |
| **Heavy** | 30,032 ops/min (~500 events/sec) | ~300,320 events | 2,637 | 4,543 | ✅ Stable |

**Result:** MongoDB successfully processed sustained event loads at all tested rates with stable memory (2-2.6 GB) and connection counts (~4,500). Memory scales appropriately with load while remaining well within reasonable bounds. Connection counts remain stable across all test scenarios. All writes went to the primary replica (mongodb-0).

## Phase 1 Conclusion

NVSentinel on a 1500-node cluster shows minimal API server impact and MongoDB handles sustained production loads without issues:
- **Light load (30 events/sec):** No measurable latency impact - conservative baseline
- **Medium load (100 events/sec):** Minimal latency impact (P75 stable at 20ms) - typical production sustained load
- **Heavy load (500 events/sec):** P75 latency remained stable at 21ms despite 422% increase in request rate - demonstrates excellent scalability beyond current production demands

**MongoDB Memory Usage:** Scales appropriately from 1.9 GB (light) to 2.6 GB (heavy), remaining well within the 6 GB allocation and providing ample headroom for Phase 2 burst testing.

---

## Phase 2: Production Burst Events Testing

### Test Configuration

**Objective:** Validate NVSentinel resilience during production-level burst scenarios and extreme cluster-wide health events  
**Duration:** 1 minute per burst test (based on observed production spikes)

| Test | Event Rate | Production Context |
|------|-----------|-------------------|
| **Moderate Burst** | 1,500 events/sec | Mid-range production burst scenario |
| **High Burst** | 3,000 events/sec | **Major multi-node health event** |
| **Extreme Burst** | 4,200 events/sec | **Catastrophic cluster-wide event** |

### Production Context

The highest production peak observed is **2,033 events/sec** (see [PRODUCTION_BASELINE.md](PRODUCTION_BASELINE.md)). Our burst tests validate NVSentinel behavior at and beyond production peaks:

- **1,500 events/sec:** Below maximum production peak - validates normal burst handling
- **3,000 events/sec:** ~50% above maximum production peak - tests major incident scenarios  
- **4,200 events/sec:** ~100% above maximum production peak - validates system survival during catastrophic events


### Prerequisites for Burst Testing

MongoDB requires **6Gi memory per replica** for burst scenarios above 1,000 events/sec (validated in previous extreme stress testing). Current deployment already includes this configuration.

### MongoDB Resilience During Burst Testing

During high-stress burst testing, MongoDB demonstrates robust failover capabilities:

- **Automatic failover:** When the primary replica experiences stress, MongoDB automatically promotes a healthy secondary to primary
- **Data integrity:** MongoDB is designed to maintain data consistency during failover, though brief write interruptions may occur during stress-induced restarts
- **Service continuity:** NVSentinel continues operating during failover, with the system recovering once the new primary is established
- **Recovery:** Stressed replicas recover and rejoin the replica set as secondaries

This behavior validates MongoDB's suitability for production deployments where brief service disruptions may occur during major cluster incidents.

**MongoDB Memory Behavior:** MongoDB exhibits aggressive memory caching behavior - once it claims memory during high-load scenarios, it retains that memory for future use rather than releasing it. This means memory usage reflects peak historical load rather than current load. Sequential burst tests show progressively degraded API server performance as MongoDB's memory footprint grows, demonstrating the importance of memory limits and periodic restarts in production environments.

### Phase 2 Results

#### API Server Impact During Burst Testing

| Test | Duration | P50 Latency | P75 Latency | P95/P99 Latency | System Status |
|------|----------|-------------|-------------|-----------------|---------------|
| **Moderate Burst** (1,500 events/sec) | 1 min | 14 ms | 20 ms | ≥60s* | ✅ Stable |
| **Moderate Burst** (1,500 events/sec) | 3 min | 45 ms | 93 ms | ≥60s* | ✅ Stable |
| **High Burst** (3,000 events/sec) | 1 min | 15 ms | 22 ms | ≥60s* | ✅ Stable |
| **High Burst** (3,000 events/sec) | 3 min | 180 ms | 350 ms | ≥60s* | 🟡 Slower |
| **Extreme Burst** (4,200 events/sec) | 1 min | 15 ms | 22 ms | ≥60s* | ✅ Stable |
| **Extreme Burst** (4,200 events/sec) | 3 min | 250 ms | 482 ms | ≥60s* | ⚠️ Degraded |
| **Extended Extreme Burst** (4,200 events/sec) | 5 min | - | - | - | 🔴 Test invalid (primary failover/MongoDB restart) |

\* *P95 and P99 are capped at the histogram bucket limit of 60s, indicating the API server has some slow background operations unrelated to NVSentinel. This applies to both sustained load and burst testing.*

#### MongoDB Performance During Burst Testing

| Test | Insert Rate | Memory (MB) | Connections | Performance |
|------|-------------|-------------|-------------|-------------|
| **Moderate Burst** | ~25,000 ops/min (~417 events/sec) | 2,329 | 4,537 | ✅ Stable |
| **High Burst** | ~50,000 ops/min (~833 events/sec) | 2,754 | 4,538 | ✅ Stable |
| **Extreme Burst** | ~70,000 ops/min (~1,167 events/sec) | 3,630 | 4,538 | ✅ Stable |
| **Extended Extreme Burst** | ~70,000 ops/min (~1,167 events/sec) | 3,710 | 4,538  | 🔴 Test invalid (primary failover/MongoDB restart) |

**Phase 2 Results:**
- **1-Minute Burst Tests:** System handles all production burst scenarios excellently with P75 latency remaining at 20-22ms across all loads (1,500-4,200 events/sec). MongoDB memory scales appropriately (2.3-3.7 GB) with no performance degradation.
- **3-Minute Sustained Bursts:** Duration significantly impacts performance. Moderate burst (1,500 events/sec) shows P75 latency of 93ms, while extreme burst (4,200 events/sec) degrades to 482ms P75 latency. MongoDB primary remains stable throughout.
- **5-Minute Extended Extreme Burst:** **System operational limits reached.** This represents an extreme test scenario - prolonged extreme load (4,200 events/sec for 5 minutes) caused MongoDB primary instability with multiple restarts, invalidating latency measurements. 

**Important context:** Real production incidents show much shorter burst durations (typically 1-2 minutes, see [Real Production Incident Pattern](PRODUCTION_BASELINE.md#real-production-incident-pattern) table), making this test scenario significantly more stressful than observed production patterns. 

**Key finding:** Extended extreme cluster-wide events exceed MongoDB's operational capacity in this configuration - system requires enhanced MongoDB resilience for sustained extreme loads beyond typical production incident patterns.

---

## Prometheus Queries

Data collected using appropriate rate windows for each test duration:

```promql
# API Server Request Rate
# Sustained tests (10-min): [5m] window
# Burst tests (1-3 min): [1m] window or range queries
sum(rate(apiserver_request_duration_seconds_count[5m]))

# P50 Latency
histogram_quantile(0.50, sum(rate(apiserver_request_duration_seconds_bucket[5m])) by (le))

# P75 Latency  
histogram_quantile(0.75, sum(rate(apiserver_request_duration_seconds_bucket[5m])) by (le))

# MongoDB Insert Operations (ops/min)
rate(mongodb_op_counters_total{type="insert",pod="mongodb-0"}[5m])*60

# MongoDB Memory (MB)
mongodb_ss_mem_resident{pod="mongodb-0"}

# MongoDB Connections
mongodb_ss_connections{pod="mongodb-0",conn_type="current"}
```

---


---

*Cluster:* 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
*NVSentinel Version:* v0.4.0
