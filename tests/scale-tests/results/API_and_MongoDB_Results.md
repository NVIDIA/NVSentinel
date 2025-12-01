# API Server Impact & MongoDB Performance

**Objective:** Validate that NVSentinel does not negatively impact Kubernetes API server performance or overwhelm MongoDB at realistic production event rates.

## Test Overview

This document covers two phases of testing:
1. **Phase 1: Sustained Production Load** - Validates performance at typical production sustained rates
2. **Phase 2: Production Burst Events** - Tests resilience during production-level burst scenarios

*See [PRODUCTION_BASELINE.md](PRODUCTION_BASELINE.md) for production event rate analysis (sustained: 11-414 events/sec, peak bursts: up to 4,190 events/sec)*

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
| **Baseline** (no load) | 240 req/s | 0.005 s (5 ms) | 0.02 s (20 ms) | ≥60s* |
| **Light load** (30 events/sec) | 308 req/s | 0.006 s (6 ms) | 0.02 s (20 ms) | ≥60s* |
| **Medium load** (100 events/sec) | 456 req/s | 0.010 s (10 ms) | 0.02 s (20 ms) | ≥60s* |
| **Heavy load** (500 events/sec) | 1255 req/s | 0.014 s (14 ms) | 0.021 s (21 ms) | ≥60s* |

*P95 and P99 are capped at the histogram bucket limit of 60s, indicating the API server has some slow background operations unrelated to NVSentinel.*

**Result:** 
- **Light load:** Request rate +28%, latency stable - no measurable impact
- **Medium load:** Request rate +89%, P50 doubled to 10ms but P75 stable at 20ms - minimal impact
- **Heavy load:** Request rate +422%, P50 increased to 14ms but **P75 remained stable at 21ms**.

## MongoDB Performance

| Test | Insert Rate | Total Events | Memory (MB) | Connections | Performance |
|------|-------------|--------------|-------------|-------------|-------------|
| **Light** | 1,985 ops/min (~33 events/sec) | ~19,850 events | 2,200 | 4,543 | ✅ Stable |
| **Medium** | 6,061 ops/min (~101 events/sec) | ~60,610 events | 1,934 | 4,549 | ✅ Stable |
| **Heavy** | 30,032 ops/min (~500 events/sec) | ~300,320 events | 2,637 | 4,543 | ✅ Stable |

**Result:** MongoDB successfully processed sustained event loads at all tested rates with stable memory (2-2.6 GB) and connection counts (~4,500). Memory scales appropriately with load while remaining well within reasonable bounds. Connection counts remain stable across all test scenarios. All writes went to the primary replica (mongodb-0).

## Phase 1 Conclusion

NVSentinel on a 1500-node cluster shows minimal API server impact and MongoDB handles sustained production loads without issues:
- **Light load (30 events/sec):** No measurable latency impact - conservative baseline
- **Medium load (100 events/sec):** Minimal latency impact (P75 stable at 20ms) - typical production sustained load
- **Heavy load (500 events/sec):** P75 latency remained stable at 21ms despite 422% increase in request rate - demonstrates excellent scalability beyond current production demands

---

## Phase 2: Production Burst Events Testing

### Test Configuration

**Objective:** Validate NVSentinel resilience during production-level burst scenarios  
**Duration:** 2-3 minutes per burst test

| Test | Event Rate | Production Context |
|------|-----------|-------------------|
| **Burst Test** | 1,500 events/sec | Mid-range production burst |
| **Peak Burst** | 4,200 events/sec | Maximum observed production burst |

*Phase 2 testing planned for future validation*

---

## Prometheus Queries

Data collected using 5-minute rate windows:

```promql
# API Server Request Rate
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

## Previous Extreme Stress Testing

In earlier testing, we validated MongoDB at **1,125 events/sec** to identify breaking points. At this extreme load, MongoDB required **6Gi memory per replica** to maintain stability. This scenario demonstrated that MongoDB can scale to handle significantly higher loads with appropriate resource allocation.

---

*Cluster:* 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
*NVSentinel Version:* v0.4.0
