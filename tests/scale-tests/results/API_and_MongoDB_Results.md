# API Server Impact & MongoDB Performance

**Objective:** Validate that NVSentinel does not negatively impact Kubernetes API server performance or overwhelm MongoDB at realistic production event rates.


## Test Configuration

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.4.0  
**Duration:** 10 minutes per test

| Test | Event Rate | Production Peak Multiplier |
|------|-----------|----------------------------|
| **Light** | 30 events/sec | 3.4× |
| **Medium** | 100 events/sec | 11× |
| **Heavy** | 300 events/sec | 34× |

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

# Heavy load (300 events/sec)
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
| **Heavy load** (300 events/sec) | 854 req/s | 0.010 s (10 ms) | 0.019 s (19 ms) | ≥60s* |

*P95 and P99 are capped at the histogram bucket limit of 60s, indicating the API server has some slow background operations unrelated to NVSentinel.*

**Result:** 
- **Light load:** Request rate +28%, latency stable - no measurable impact
- **Medium load:** Request rate +89%, P50 doubled to 10ms but P75 stable at 20ms - minimal impact
- **Heavy load:** Request rate increased, but **P75 remained stable at 19ms** - excellent scalability

## MongoDB Performance

| Test | Insert Rate | Total Events | Memory (MB) | Connections | Performance |
|------|-------------|--------------|-------------|-------------|-------------|
| **Light** | 1,985 ops/min (~33 events/sec) | ~19,850 events | 2,200 | 4,543 | ✅ Stable |
| **Medium** | 6,061 ops/min (~101 events/sec) | ~60,610 events | 1,934 | 4,549 | ✅ Stable |
| **Heavy** | 18,036 ops/min (~300 events/sec) | ~180,360 events | 2,178 | 4,543 | ✅ Stable |

**Result:** MongoDB successfully processed sustained event loads at all tested rates with stable memory (~2 GB) and connection counts (~4,500). Memory variation of ~13% across tests reflects normal cache management rather than resource accumulation. All writes went to the primary replica (mongodb-0).

## Conclusion

NVSentinel on a 1500-node cluster shows minimal API server impact and MongoDB handles the load without issues:
- **Light load (30 events/sec, 3.4× production peak):** No measurable latency impact
- **Medium load (100 events/sec, 11× production peak):** Minimal latency impact (P75 stable at 20ms)
- **Heavy load (300 events/sec, 34× production peak):** P75 latency remained stable at 19ms - excellent scalability

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

## Extreme Stress Testing

We also tested MongoDB at **1,125 events/sec (127× production peak)** to identify breaking points. At this extreme load, MongoDB required **6Gi memory per replica** to maintain stability. This scenario is well beyond expected production use cases and demonstrates that MongoDB can scale to handle significantly higher loads with appropriate resource allocation.

---

*Cluster:* 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
*NVSentinel Version:* v0.4.0
