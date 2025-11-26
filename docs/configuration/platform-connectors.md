# Platform Connectors Configuration

## Overview

The Platform Connectors module acts as the central communication hub for NVSentinel. It receives health events from monitors via Unix socket, stores them in the database, and enriches events with node metadata from Kubernetes. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Resources

Defines CPU and memory resource requests and limits for the platform-connectors pod.

```yaml
platformConnector:
  resources:
    limits:
      cpu: 200m
      memory: 512Mi
    requests:
      cpu: 200m
      memory: 512Mi
```

### Logging

Sets the verbosity level for platform-connectors logs.

```yaml
platformConnector:
  logLevel: info  # Options: debug, info, warn, error
```

## Kubernetes Connector

Configures the Kubernetes API client used for node metadata enrichment.

```yaml
platformConnector:
  k8sConnector:
    enabled: true
    qps: 5.0
    burst: 10
```

### Parameters

#### enabled
Enables Kubernetes connector for creating node conditions and events.

#### qps
Queries per second allowed to the Kubernetes API server.

#### burst
Maximum burst of queries allowed to the Kubernetes API server.

### Example

```yaml
platformConnector:
  k8sConnector:
    enabled: true
    qps: 10.0
    burst: 20
```

## Node Metadata Enrichment

Enriches health events with node labels and metadata from Kubernetes.

```yaml
platformConnector:
  nodeMetadata:
    enabled: false
    cacheSize: 50
    cacheTTLSeconds: 3600
    allowedLabels:
      - "topology.kubernetes.io/zone"
      - "topology.kubernetes.io/region"
```

### Parameters

#### enabled
Enables node metadata enrichment for health events.

#### cacheSize
Number of node metadata entries to cache in memory.

#### cacheTTLSeconds
Time-to-live for cached node metadata entries in seconds.

#### allowedLabels
List of node label keys to include in health event enrichment. Only labels in this list are read from nodes and added to events.

### Default Allowed Labels

The default configuration includes common topology and infrastructure labels:
- `topology.kubernetes.io/zone`
- `topology.kubernetes.io/region`
- `node.kubernetes.io/instance-type`

> Note: The complete default list is defined in `distros/kubernetes/nvsentinel/values.yaml`

### Example

```yaml
platformConnector:
  nodeMetadata:
    enabled: true
    cacheSize: 100
    cacheTTLSeconds: 7200
    allowedLabels:
      - "topology.kubernetes.io/zone"
      - "custom.label/rack-id"
```
