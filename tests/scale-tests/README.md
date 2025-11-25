# Scale Tests: API Server & MongoDB Load

Scale testing for **NVSentinel v0.4.0** validating API server impact and MongoDB performance at production scale (1500+ nodes).

## Overview

This test suite validates NVSentinel's performance and scalability under realistic production conditions:

1. **Does NVSentinel affect the API server load and cause slowdown at scale of 1000+ nodes?**
2. **Does MongoDB function at scale or do we need to increase the replica count?**

**Testing Version:** NVSentinel v0.4.0  
**Cluster Scale:** 1500 nodes  
**Test Loads:** 30, 100, 300 events/sec (3.4×, 11×, 34× production peak)

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

| Scenario | Event Rate | Production Peak Multiplier | Duration | API Server Impact |
|----------|-----------|----------------------------|----------|-------------------|
| **Light** | 30 events/sec | 3.4× | 10 min | ✅ No impact |
| **Medium** | 100 events/sec | 11× | 10 min | ✅ Minimal (P75 stable at 20ms) |
| **Heavy** | 300 events/sec | 34× | 10 min | ✅ Excellent (P75 stable at 19ms) |

See [results/API_and_MongoDB_Results.md](results/API_and_MongoDB_Results.md) for detailed metrics and Prometheus queries.

## Key Findings

**API Server Impact:** NVSentinel shows minimal impact even at heavy loads (34× production peak). The critical P75 latency metric remained stable at ~20ms across all test scenarios.

**MongoDB Performance:** Successfully processed sustained event loads ranging from 33 to 300 events/sec with no errors or performance degradation.

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

- 📊 [API Server Impact & MongoDB Performance](results/API_and_MongoDB_Results.md) - Light/Medium/Heavy load test results
- 📊 [Production Baseline Analysis](results/PRODUCTION_BASELINE.md) - Real-world event rate analysis

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
│   ├── API_and_MongoDB_Results.md
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

- **API Server:** Minimal latency impact with P75 stable at ~20ms even at 300 events/sec (34× production peak)
- **MongoDB:** Successfully handles sustained loads from 30-300 events/sec with default configuration
- **Scalability:** Excellent - system maintains stable performance across a wide range of event rates

---

**Last Updated:** November 24, 2025

