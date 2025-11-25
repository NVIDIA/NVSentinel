# MongoDB Load & API Server Impact

**Objective:** Validate that NVSentinel does not negatively impact Kubernetes API server performance or 
overwhelm MongoDB at realistic production event rates.


## Test Configuration

**Cluster:** 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
**NVSentinel Version:** v0.4.0  
**Duration:** 10 minutes per test

| Test | Event Rate | Production Peak Multiplier |
|------|-----------|----------------------------|
| **Light** | 30 events/sec | 3.4× |
| **Medium** | 100 events/sec | 11× |
| **Heavy** | 300 events/sec | 34× |

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

| Test | Insert Rate | Total Events | Performance |
|------|-------------|--------------|-------------|
| **Light** | 1,985 ops/min (~33 events/sec) | ~19,850 events | ✅ No errors |
| **Medium** | 6,061 ops/min (~101 events/sec) | ~60,610 events | ✅ No errors |
| **Heavy** | 18,036 ops/min (~300 events/sec) | ~180,360 events | ✅ No errors |

**Result:** MongoDB successfully processed sustained event loads at all tested rates with all writes going to the primary replica (mongodb-0).

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
```

---

## Extreme Stress Testing

We also tested MongoDB at **1,125 events/sec (127× production peak)** to identify breaking points. At this extreme load, MongoDB required **6Gi memory per replica** to maintain stability. This scenario is well beyond expected production use cases and demonstrates that MongoDB can scale to handle significantly higher loads with appropriate resource allocation.

---

*Cluster:* 1503 nodes (3 system + 1500 customer aws-cpu-m7i.xlarge nodes)  
*NVSentinel Version:* v0.4.0
