# NVSentinel Scale Tests

Comprehensive scale testing for **NVSentinel v0.4.0** validating performance and scalability at production scale (1500+ nodes).

## Overview

This test suite validates NVSentinel's performance and scalability under realistic production conditions:

1. **Does NVSentinel affect the API server load and cause slowdown at scale of 1000+ nodes?**
2. **Does MongoDB function at scale in our use case?**
3. **What is the end-to-end latency of cordoning nodes during mass failure events?**

**Testing Version:** NVSentinel v0.4.0  
**Cluster Scale:** 1500 nodes

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

**Note:** The values file enables MongoDB metrics for Prometheus with the required `release: prometheus` label and includes 6Gi memory per MongoDB replica for burst scenario testing.

### Building Event Generator

The event generator code that we used to create test events is included in this repo. You must build it and push it to a repo your cluster has access to.

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
sed -i 's|ghcr.io/nvidia/nvsentinel|YOUR_REGISTRY|g' event-generator-daemonset.yaml
```

**⚠️ Important:** The placeholder URL `ghcr.io/nvidia/nvsentinel/event-generator` **does not exist** — this is test code only. You must build and push to your own registry.

**Configure test scenarios by changing the ConfigMap:**
- Light load: `kubectl apply -f event-generator-config-light.yaml`
- Medium load: `kubectl apply -f event-generator-config-medium.yaml`  
- Heavy load: `kubectl apply -f event-generator-config-heavy.yaml`

See [event-generator/BUILD.md](event-generator/BUILD.md) for detailed instructions.

## Test Results Summary

See [results/](results/) for detailed test reports.

### 1. API Server Impact & MongoDB Performance

**Objective:** Validate that NVSentinel does not negatively impact Kubernetes API server or overwhelm MongoDB

**Test Scenarios (1500-node cluster):**

**Sustained Load Testing:**
- Light/Medium/Heavy loads: 30-500 events/sec (validates typical production operation)

**Burst Testing:**
- Moderate/High/Extreme bursts: 1,500-4,200 events/sec (1-minute duration based on production incident patterns - validates system limits during major incidents)

**Note:** Event rates are configured for a 1500-node cluster. If your cluster has a different size, you'll need to adjust the `EVENT_RATE` in the ConfigMaps (`manifests/event-generator-config-*.yaml`) proportionally. For example, for a 750-node cluster, use half the event rate per node.

**Key Findings:**
- API server P75 latency stable at ~20ms across sustained loads up to 500 events/sec
- MongoDB successfully processed sustained loads up to 500 events/sec with default configuration
- Additional burst testing validated system behavior under extreme loads to identify operational limits

📊 **[Full Results](results/API_and_MongoDB_Results.md)**

### 2. FQM Latency & Queue Depth

**Objective:** Measure end-to-end latency from fatal event to node cordon at scale

**Test Scenarios (1500-node cluster):**
- 10% cluster failure (150 nodes)
- 25% cluster failure (375 nodes)
- 50% cluster failure (750 nodes)

**Key Findings:**
- 100% cordoning success rate at all scales
- FQM processes ~2.5 nodes/sec consistently across all scales
- Event handling latency consistent: P50 ~0.37s, P90 ~0.48s
- FQM processes events sequentially (one at a time from change stream queue)

📊 **[Full Results](results/FQM_Latency_and_Queue_Depth_Results.md)**

### 3. Concurrent Drain Operations

**Objective:** Validate Node Drainer Manager's ability to handle concurrent drain operations at scale

**Test Scenario (1500-node cluster):**
- Cordon 300 nodes simultaneously
- Each node running 10-20 user pods (not in system namespace)
- Total: ~4,500 pod evictions

**Key Areas:**
- Node Drainer queue depth and processing rate
- Kubernetes Eviction API throttling behavior
- Drain timeout handling
- PodDisruptionBudget interactions

📊 **[Full Results](results/Concurrent_Drain_Results.md)**

### Additional Tests

*(More test results will be added here as testing continues)*

## Running Tests

Each test has specific setup instructions in its results document. General approach:

### 1. Deploy Event Generators

```bash
# Apply the test-specific ConfigMap
kubectl apply -f manifests/event-generator-config-<TEST>.yaml

# Deploy the DaemonSet
kubectl apply -f manifests/event-generator-daemonset.yaml

# Verify deployment (one pod per worker node)
kubectl get pods -n nvsentinel -l app=event-generator
```

### 2. Access Prometheus

```bash
# Port-forward to Prometheus
kubectl port-forward -n monitoring svc/prometheus-server 9090:80

# Access at http://localhost:9090
```

### 3. Monitor & Collect Metrics

Use the Prometheus UI or API to observe metrics during the test. Each test document includes specific queries and monitoring instructions.

## MongoDB Metrics

The test configuration enables MongoDB metrics for Prometheus. If using `kube-prometheus-stack`, ensure the ServiceMonitor has the `release: prometheus` label (or matches your Prometheus Operator's `serviceMonitorSelector`). This is included in `configs/values-v0.4.0-with-mongodb-metrics.yaml`.

## Detailed Results

- 📊 [API Server Impact & MongoDB Performance](results/API_and_MongoDB_Results.md) - Light/Medium/Heavy load test results
- 📊 [FQM Latency & Queue Depth](results/FQM_Latency_and_Queue_Depth_Results.md) - End-to-end cordoning latency at 10-50% cluster failure
- 📊 [Concurrent Drain Operations](results/Concurrent_Drain_Results.md) - Node Drainer scaling with 300 concurrent drains
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
│   ├── FQM_Latency_and_Queue_Depth_Results.md
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

## Summary

Initial scale testing on a 1500-node cluster validates NVSentinel v0.4.0 performance:

- **API Server:** Minimal latency impact with P75 stable at ~20ms even at 500 events/sec sustained load
- **MongoDB:** Successfully handles sustained loads from 30-500 events/sec with default configuration

Additional testing in progress.

---

**Last Updated:** December 5, 2025
