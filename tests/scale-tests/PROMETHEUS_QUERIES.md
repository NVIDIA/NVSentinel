# Prometheus Queries

## Access Prometheus

```bash
kubectl port-forward -n monitoring prometheus-prometheus-kube-prometheus-prometheus-0 9090:9090
# Open http://localhost:9090
```

## Queries Used for Testing

### API Server Metrics

#### Sustained Load Tests (10-minute tests)
```bash
# Request Rate (req/s) - 5-minute window
curl -s 'http://localhost:9090/api/v1/query?query=sum(rate(apiserver_request_duration_seconds_count[5m]))&time=2025-12-02T00:30:00Z' | jq -r '.data.result[0].value[1]'

# P50 Latency (seconds)
curl -s 'http://localhost:9090/api/v1/query?query=histogram_quantile(0.50,%20sum(rate(apiserver_request_duration_seconds_bucket[5m]))%20by%20(le))&time=2025-12-02T00:30:00Z' | jq -r '.data.result[0].value[1]'

# P75 Latency (seconds)
curl -s 'http://localhost:9090/api/v1/query?query=histogram_quantile(0.75,%20sum(rate(apiserver_request_duration_seconds_bucket[5m]))%20by%20(le))&time=2025-12-02T00:30:00Z' | jq -r '.data.result[0].value[1]'
```

#### Burst Tests (1-3 minute tests)
```bash
# Current latency (1-minute window)
curl -s 'http://localhost:9090/api/v1/query?query=histogram_quantile(0.75,%20sum(rate(apiserver_request_duration_seconds_bucket[1m]))%20by%20(le))' | jq -r '.data.result[0].value[1]'

# Range query for test window (example: 1-minute test)
curl -s 'http://localhost:9090/api/v1/query_range?query=histogram_quantile(0.75,%20sum(rate(apiserver_request_duration_seconds_bucket%5B1m%5D))%20by%20(le))&start=2025-12-02T00:45:30Z&end=2025-12-02T00:46:30Z&step=30s' | jq '.data.result[0].values[] | .[1]' | sort -nr | head -1
```

### MongoDB Metrics

```bash
# Insert Operations (ops/min) - check primary
curl -s 'http://localhost:9090/api/v1/query?query=rate(mongodb_op_counters_total{instance="mongodb-1.mongodb.nvsentinel.svc.cluster.local:9216",type="insert"}[1m])*60' | jq -r '.data.result[0].value[1]'

# Memory Usage (MB)
curl -s 'http://localhost:9090/api/v1/query?query=mongodb_ss_mem_resident{instance="mongodb-1.mongodb.nvsentinel.svc.cluster.local:9216"}' | jq -r '.data.result[0].value[1]'

# Connection Count
curl -s 'http://localhost:9090/api/v1/query?query=mongodb_ss_connections{instance="mongodb-1.mongodb.nvsentinel.svc.cluster.local:9216",state="current"}' | jq -r '.data.result[0].value[1]'
```

**Note:** Replace timestamps with your actual test times. Use `mongodb-0`, `mongodb-1`, or `mongodb-2` depending on which is the primary during your test.
