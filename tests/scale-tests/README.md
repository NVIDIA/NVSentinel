# Scale Tests: API Server & MongoDB Load

Scale testing for **NVSentinel v0.4.0** validating API server impact and MongoDB performance at production scale (1500+ nodes).

## Overview

This test suite validates NVSentinel's performance and scalability under realistic production conditions:

1. **Does NVSentinel affect the API server load and cause slowdown at scale of 1000+ nodes?**
2. **Does MongoDB function at scale or do we need to increase the replica count?**

**Testing Version:** NVSentinel v0.4.0  
**Cluster Scale:** 1500 nodes  
**Test Load:** 30 events/sec (3.4× production peak)

## Quick Start

### Prerequisites

- Kubernetes cluster with 100+ nodes (tested at 1500 nodes)
- NVSentinel v0.4.0 installed
- Prometheus with kube-prometheus-stack for metrics collection
- Access to create DaemonSets for event generation

### Installation

Deploy NVSentinel v0.4.0 via Helm with MongoDB metrics enabled:

```bash
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --namespace nvsentinel \
  --create-namespace \
  --version v0.4.0 \
  --values configs/values-v0.4.0-with-mongodb-metrics.yaml \
  --wait
```

**Note:** The values file enables MongoDB metrics for Prometheus with the required `release: prometheus` label.

### Building Event Generator

The event generator image is **not publicly available**. You must build it yourself.

**One image handles all test scenarios** - everything is configured via ConfigMaps!

```bash
cd event-generator

# Build binary
go mod tidy
go build -o event-generator .

# Build Docker image with YOUR registry
docker build -t YOUR_REGISTRY/event-generator:v1 .
docker push YOUR_REGISTRY/event-generator:v1

# Update all manifests to use your registry (one command!)
cd ../manifests
sed -i 's|nvcr.io/nv-ngc-devops|YOUR_REGISTRY|g' event-generator-daemonset.yaml
```

**Configure test scenarios by changing the ConfigMap:**
- Light load: `kubectl apply -f event-generator-config-light.yaml`
- Medium load: `kubectl apply -f event-generator-config-medium.yaml`  
- Heavy load: `kubectl apply -f event-generator-config-heavy.yaml`

See [event-generator/BUILD.md](event-generator/BUILD.md) for detailed instructions.

## Test Architecture

### Event Generation

To simulate production-scale load, we deploy a DaemonSet of event generators (one pod per node) that inject synthetic health events directly into the platform connector via gRPC. This approach simulates production-style loads without requiring actual hardware failures or DCGM instrumentation.

**Event Distribution (Random Selection):**
- 64% - Healthy GPU events (IsFatal: false, IsHealthy: true)
- 24% - System info events (IsFatal: false, IsHealthy: true)
- 8% - Fatal GPU errors (IsFatal: true, IsHealthy: false)
- 4% - NVSwitch warnings (IsFatal: false, IsHealthy: false)

**Key capabilities:**
- **Communication:** Direct gRPC via Unix socket to platform connector
- **Deployment:** DaemonSet (one pod per worker node)
- **Modes:** Continuous generation with configurable event rates

## Test Results

We validated system performance at a conservative baseline (3.4× production peak):

| Scenario | Event Rate (per node) | Total Cluster Load | Duration | Status |
|----------|----------------------|-------------------|----------|--------|
| **Light Load** | 0.02 events/sec | 30 events/sec | 10 min | ✅ No impact |

See [results/MongoDB_Load_and_API_Server_Impact.md](results/MongoDB_Load_and_API_Server_Impact.md) for detailed metrics.

## Key Findings

### 1. API Server Impact: ✅ None

NVSentinel at 30 events/sec (3.4× production peak) on a 1500-node cluster shows no measurable API server latency impact. Request rate increased 28% as expected from NVSentinel operations, but P50 and P75 latency remained stable.

### 2. MongoDB Performance: ✅ Excellent

MongoDB successfully processed ~33 events/sec sustained load (~1,985 ops/min) with no errors or performance degradation.

## MongoDB Metrics

The test configuration enables MongoDB metrics for Prometheus. If using `kube-prometheus-stack`, ensure the ServiceMonitor has the `release: prometheus` label (or matches your Prometheus Operator's `serviceMonitorSelector`). This is included in `configs/values-v0.4.0-with-mongodb-metrics.yaml`.

## Running the Tests

### Step 1: Deploy Event Generators

```bash
# Light load (30 events/sec cluster-wide for 1500-node cluster)
kubectl apply -f manifests/event-generator-config-light.yaml
kubectl apply -f manifests/event-generator-daemonset.yaml

# Verify deployment
kubectl get pods -n nvsentinel -l app=event-generator

# Expected: One pod per worker node in Running state
```

### Step 2: Monitor System Performance

#### MongoDB Health
```bash
# Check MongoDB pod status
kubectl get pods -n nvsentinel -l app.kubernetes.io/component=mongodb

# Monitor memory usage
kubectl top pods -n nvsentinel -l app.kubernetes.io/component=mongodb

# Check replica set status
kubectl exec -n nvsentinel mongodb-0 -- mongosh --eval "rs.status()"
```

#### API Server Metrics (via Prometheus)

If Prometheus is installed:

```bash
# Port-forward to Prometheus
kubectl port-forward -n monitoring svc/prometheus-server 9090:80

# Access Prometheus UI
# Open http://localhost:9090
```

**Key metrics to monitor:**
- `apiserver_request_duration_seconds` - API server request latency
- `apiserver_request_total` - API server request rate
- `mongodb_op_counters_total` - MongoDB operations per second

### Step 3: Monitor Test

Monitor the test for the desired duration (10 minutes). Use the Prometheus UI or direct API queries to observe API server latency and MongoDB operations in real-time.

## Detailed Results

- 📊 [MongoDB Load & API Server Impact](results/MongoDB_Load_and_API_Server_Impact.md) - 30 events/sec, 10 minutes
- 📊 [Production Baseline Analysis](results/PRODUCTION_BASELINE.md) - Real-world event rate analysis

## Production Recommendations

Based on these scale tests at 30 events/sec (3.4× production peak):

- **API Server Impact:** None - NVSentinel does not affect API server latency at this scale
- **MongoDB:** Handles the load with no issues using default configuration
- **Monitoring:** Enable MongoDB metrics for observability (see configs/values-v0.4.0-with-mongodb-metrics.yaml)

## Directory Structure

```
tests/scale-tests/
├── README.md                        # This file
├── manifests/                       # Kubernetes manifests
│   ├── event-generator-daemonset.yaml
│   ├── event-generator-config-light.yaml
│   ├── event-generator-config-medium.yaml
│   └── event-generator-config-heavy.yaml
├── results/                         # Test results
│   ├── MongoDB_Load_and_API_Server_Impact.md
│   └── PRODUCTION_BASELINE.md
├── event-generator/                 # Event generator source code
│   ├── main.go
│   ├── Dockerfile
│   ├── BUILD.md
│   └── README.md
└── configs/                         # Configuration files
    └── values-v0.4.0-with-mongodb-metrics.yaml
```

## Tools & Technologies

- **Kubernetes:** v1.29+
- **NVSentinel:** v0.4.0
- **MongoDB:** 3-replica StatefulSet
- **Event Generator:** Go + gRPC (Unix socket communication)
- **Metrics:** Prometheus + Kubernetes API

## Conclusion

NVSentinel v0.4.0 is validated for production deployment on 1500-node clusters:

- **API Server:** No measurable latency impact at 30 events/sec (3.4× production peak)
- **MongoDB:** Successfully handles the sustained event load with default configuration
- **Scalability:** System performs well at realistic production event rates

---

**Last Updated:** November 24, 2025

