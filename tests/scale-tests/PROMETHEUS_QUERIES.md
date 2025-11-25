# Prometheus Queries

## Access Prometheus

```bash
kubectl port-forward -n monitoring prometheus-prometheus-kube-prometheus-prometheus-0 9090:9090
# Open http://localhost:9090
```

## Queries Used for Testing

### API Server Metrics

```bash
# Request Rate (req/s)
curl -s 'http://localhost:9090/api/v1/query?query=sum(rate(apiserver_request_duration_seconds_count[5m]))&time=2025-11-24T23:30:00Z' | jq -r '.data.result[0].value[1]'

# P50 Latency (seconds)
curl -s 'http://localhost:9090/api/v1/query?query=histogram_quantile(0.50,%20sum(rate(apiserver_request_duration_seconds_bucket[5m]))%20by%20(le))&time=2025-11-24T23:30:00Z' | jq -r '.data.result[0].value[1]'

# P75 Latency (seconds)
curl -s 'http://localhost:9090/api/v1/query?query=histogram_quantile(0.75,%20sum(rate(apiserver_request_duration_seconds_bucket[5m]))%20by%20(le))&time=2025-11-24T23:30:00Z' | jq -r '.data.result[0].value[1]'
```

### MongoDB Metrics

```bash
# Insert Operations (ops/min)
curl -s 'http://localhost:9090/api/v1/query?query=rate(mongodb_op_counters_total{type="insert",pod="mongodb-0"}[5m])*60&time=2025-11-24T23:30:00Z' | jq -r '.data.result[0].value[1]'
```

Replace the timestamp with your test time.
