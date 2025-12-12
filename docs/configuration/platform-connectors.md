# Platform Connectors Configuration

## Overview

The Platform Connectors module acts as the central communication hub for NVSentinel. It receives health events from monitors via gRPC, processes them through a transformer pipeline, stores them in the database, and propagates them to Kubernetes. This document covers all Helm configuration options for system administrators.

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

## Transformer Pipeline

Configures the event transformation pipeline that processes health events before storage and Kubernetes propagation.

```yaml
platformConnector:
  pipeline:
    - name: MetadataAugmentor
      enabled: false
      config: /etc/config/metadata.toml
    - name: OverrideTransformer
      enabled: false
      config: /etc/config/overrides.toml
  
  transformers:
    MetadataAugmentor:
      cacheSize: 50
      cacheTTLSeconds: 3600
      allowedLabels:
        - "topology.kubernetes.io/zone"
    
    OverrideTransformer:
      rules: []
```

### Parameters

#### pipeline
Array of transformers to execute in order:
- **name**: Transformer identifier (`MetadataAugmentor`, `OverrideTransformer`)
- **enabled**: Enable/disable the transformer
- **config**: Path to transformer-specific configuration file

#### transformers
Transformer-specific configurations, nested by transformer name.

**Note:** Transformers execute sequentially. `MetadataAugmentor` should run first to provide node metadata for subsequent transformers.

## Metadata Augmentor Configuration

Enriches health events with node labels and metadata from Kubernetes.

```yaml
platformConnector:
  transformers:
    MetadataAugmentor:
      cacheSize: 50
      cacheTTLSeconds: 3600
      allowedLabels:
        - "topology.kubernetes.io/zone"
        - "topology.kubernetes.io/region"
        - "node.kubernetes.io/instance-type"
```

### Parameters

#### cacheSize
Number of node metadata entries to cache in memory.

#### cacheTTLSeconds
Time-to-live for cached node metadata entries in seconds.

#### allowedLabels
List of node label keys to include in health event enrichment. Only labels in this list are read from nodes and added to events.

> Note: The complete default list is defined in `distros/kubernetes/nvsentinel/values.yaml`

### Example

```yaml
platformConnector:
  transformers:
    MetadataAugmentor:
      cacheSize: 100
      cacheTTLSeconds: 3600
      allowedLabels:
        - "topology.kubernetes.io/zone"
        - "topology.kubernetes.io/region"
        - "custom.company.com/rack-id"
```

## Override Transformer Configuration

Applies CEL-based rules to modify health event properties (isFatal, isHealthy, recommendedAction).

```yaml
platformConnector:
  transformers:
    OverrideTransformer:
      rules:
        - name: "suppress-xid-109"
          when: 'event.agent == "syslog-health-monitor" && "109" in event.errorCode'
          override:
            isFatal: false
            recommendedAction: "NONE"
```

### Parameters

#### rules
Array of override rules evaluated in order (first match wins):
- **name**: Human-readable rule name for logging
- **when**: CEL expression that evaluates to boolean
- **override**: Properties to modify (isFatal, isHealthy, recommendedAction)

### CEL Expression Context

CEL expressions have access to the `event` object with the following fields:

| Field | Type | Description |
|-------|------|-------------|
| `event.nodeName` | string | Node where event occurred |
| `event.agent` | string | Health monitor that generated event |
| `event.componentClass` | string | Component class (e.g., "GPU", "Network") |
| `event.checkName` | string | Name of the health check |
| `event.message` | string | Human-readable error message |
| `event.errorCode` | []string | Array of error codes |
| `event.entitiesImpacted` | []Entity | Affected entities (GPUs, NICs, etc.) |
| `event.isFatal` | bool | Whether error is fatal |
| `event.isHealthy` | bool | Overall health status |
| `event.recommendedAction` | string | Recommended remediation action |
| `event.metadata` | map | Node metadata from MetadataAugmentor |

**Entity fields:** Each entity in `entitiesImpacted` has:
- `entityType` - Type of entity (e.g., "GPU", "NIC")
- `entityValue` - Entity identifier (e.g., GPU UUID, PCI address)

### Examples

**Suppress known errors:**
```yaml
transformers:
  OverrideTransformer:
    rules:
      - name: "suppress-xid-109"
        when: 'event.agent == "syslog-health-monitor" && "109" in event.errorCode'
        override:
          isFatal: false
          recommendedAction: "NONE"
```


## Kubernetes Connector

Configures the Kubernetes API client for creating node conditions and events.

```yaml
platformConnector:
  k8sConnector:
    enabled: true
    maxNodeConditionMessageLength: 1024
    qps: 5.0
    burst: 10
```

### Parameters

#### enabled
Enables Kubernetes connector for creating node conditions and events.

#### maxNodeConditionMessageLength
Maximum length of node condition messages in characters.

#### qps
Queries per second allowed to the Kubernetes API server.

#### burst
Maximum burst of queries allowed to the Kubernetes API server.

### Example

```yaml
platformConnector:
  k8sConnector:
    enabled: true
    maxNodeConditionMessageLength: 1024
    qps: 10.0
    burst: 20
```
