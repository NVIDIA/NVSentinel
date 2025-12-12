# Platform Connectors

## Overview

Platform Connectors is the central hub that receives health events from all health monitors and distributes them to the appropriate destinations. It acts as a translator and router, ensuring health events are persisted to the datastore and reflected in Kubernetes node status.

Think of it as a post office - it receives messages (health events) from various senders (health monitors) and routes them to the right destinations (datastore, Kubernetes API).

### Why Do You Need This?

Platform Connectors provides the glue that connects monitoring to action:

- **Centralized ingestion**: Single endpoint for all health events
- **Data persistence**: Stores events in the datastore for the remediation pipeline
- **Kubernetes integration**: Updates node conditions and events based on health status
- **Metadata enrichment**: Optionally augments events with node metadata (cloud provider info, labels, etc.)
- **Decoupling**: Keeps health monitors independent from platform-specific implementations

Without Platform Connectors, health monitors would need to directly integrate with each platform's storage and APIs, creating tight coupling and complexity.

## How It Works

Platform Connectors runs as a deployment in the cluster:

1. Exposes gRPC service for health monitors to send events
2. Receives health events via gRPC (`HealthEventOccurredV1` API)
3. Processes events through the transformer pipeline:
   - **Metadata Augmentor**: Augments events with node metadata (cloud provider, labels, topology)
   - **Override Transformer**: Applies CEL-based rules to modify event properties
4. Queues transformed events in ring buffers for parallel processing
5. Processes events through multiple connectors:
   - **Store Connector**: Persists events to the datastore
   - **Kubernetes Connector**: Updates node conditions and Kubernetes events
6. Each connector processes events independently for resilience

The transformer pipeline processes events in order, allowing each transformer to build on previous enrichments. The ring buffer architecture ensures events are processed reliably even under high load, with retry logic for transient failures.

## Configuration

Configure Platform Connectors through Helm values:

```yaml
platformConnector:
  enabled: true
  
  # Transformer pipeline - defines execution order
  pipeline:
    - name: MetadataAugmentor
      enabled: false
      config: /etc/config/metadata.toml
    - name: OverrideTransformer
      enabled: false
      config: /etc/config/overrides.toml
  
  # Transformer configurations
  transformers:
    # Metadata enrichment
    MetadataAugmentor:
      cacheSize: 50
      cacheTTLSeconds: 3600
      allowedLabels:
        - "topology.kubernetes.io/zone"
        - "topology.kubernetes.io/region"
        - "node.kubernetes.io/instance-type"
    
    # Health event property overrides
    OverrideTransformer:
      rules:
        - name: "suppress-xid-109"
          when: 'event.agent == "syslog-health-monitor" && "109" in event.errorCode'
          override:
            isFatal: false
            recommendedAction: "NONE"
```

### Configuration Options

- **Pipeline**: Configure transformer execution order and enable/disable individual transformers
- **Transformers**: Transformer-specific configurations (MetadataAugmentor, OverrideTransformer)
- **Metadata Augmentor**: Configure node metadata enrichment, cache settings, and allowed labels
- **Override Transformer**: Define CEL-based rules to modify event properties
- **Kubernetes API Rate Limits**: Configure QPS and burst for Kubernetes API calls

For complete configuration reference, see [Platform Connectors Configuration](configuration/platform-connectors.md).

## What It Does

### Health Event Ingestion
Receives health events from all monitors via gRPC:
- GPU Health Monitor (DCGM-based checks)
- Syslog Health Monitor (log-based checks)
- CSP Health Monitor (cloud provider events)
- Kubernetes Object Monitor (resource-based checks)
- Any custom health monitors

### Event Transformation
Processes events through configurable transformer pipeline:
- **Metadata Augmentor**: Adds cloud provider IDs, topology labels, custom node labels
- **Override Transformer**: Applies CEL-based rules to modify event severity and recommendations
- **Extensible**: Support for custom transformers via factory pattern
- Transformers execute in configured order with non-blocking error handling

### Data Persistence
Stores health events in the datastore:
- Atomic insertion with proper timestamps
- Preserves all event metadata and transformations
- Triggers change streams for downstream modules

### Kubernetes Integration
Updates cluster state based on health events:
- **Node Conditions**: Updates node conditions for fatal failures
- **Node Events**: Creates Kubernetes events for non-fatal issues
- Event correlation and deduplication

## Transformer Pipeline

The transformer pipeline processes health events before they reach storage or Kubernetes. Transformers run in a configurable order, with each transformer able to modify events based on the enrichments from previous transformers.

### Available Transformers

#### Metadata Augmentor
Enriches health events with node information from Kubernetes:
- Cloud provider ID (AWS, GCP, Azure, OCI)
- Node labels (topology, instance type, custom labels)
- Caches metadata to minimize Kubernetes API calls

#### Override Transformer
Applies CEL-based rules to modify health event properties:
- **isFatal**: Change whether an error is considered fatal
- **isHealthy**: Override health status
- **recommendedAction**: Modify the recommended remediation action

Use cases:
- Suppress known non-critical errors in your environment
- Change recommended actions during maintenance windows
- Apply different policies based on node labels

### Transformer Configuration

Transformers are configured through Helm values with two sections:

1. **pipeline** - defines which transformers run and in what order
2. **transformers** - contains transformer-specific configurations

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
      allowedLabels: [...]
    
    OverrideTransformer:
      rules: [...]
```

### Error Handling

Transformer failures log warnings but don't block event processing. If a transformer fails, the event still reaches storage and Kubernetes with whatever transformations were successfully applied. This ensures system resilience - monitoring continues even if enrichment features fail.

## Key Features

### gRPC API
Standard gRPC interface for health monitors to report events - protocol buffer-based for efficiency and type safety.


### Ring Buffer Architecture
Parallel event processing with independent queues:
- Store connector queue for datastore writes
- Kubernetes connector queue for API updates
- Failure in one connector doesn't block the other

### Metadata Caching
Caches node metadata to reduce Kubernetes API load:
- Configurable cache size and TTL
- Automatic cache invalidation
- Reduces latency for event processing

### Resilient Processing
Built-in retry and error handling:
- Transient failures don't lose events
- Backpressure handling via ring buffers
- Detailed metrics for monitoring
