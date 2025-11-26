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
3. Optionally enriches events with node metadata (cloud provider, labels, topology)
4. Queues events in ring buffers for parallel processing
5. Processes events through multiple connectors:
   - **Store Connector**: Persists events to the datastore
   - **Kubernetes Connector**: Updates node conditions and Kubernetes events
6. Each connector processes events independently for resilience

The ring buffer architecture ensures events are processed reliably even under high load, with retry logic for transient failures.

## Configuration

Configure Platform Connectors through Helm values:

```yaml
platform-connectors:
  enabled: true
  
  # Node metadata enrichment
  nodeMetadata:
    enabled: true
    cacheSize: 50
    cacheTTLSeconds: 3600
    allowedLabels:
      - "topology.kubernetes.io/zone"
      - "topology.kubernetes.io/region"
      - "node.kubernetes.io/instance-type"
      - "nvidia.com/cuda.driver-version.major"
      - "nvidia.com/cuda.driver-version.minor"
      - "nvidia.com/cuda.driver-version.revision"
      - "nvidia.com/cuda.driver-version.full"
      # Add cloud-specific labels as needed
```

### Configuration Options

- **Node Metadata**: Enable/disable metadata enrichment and configure which labels to include
- **Cache Settings**: Configure metadata cache size and TTL for performance
- **Kubernetes API Rate Limits**: Configure QPS and burst for Kubernetes API calls

## What It Does

### Health Event Ingestion
Receives health events from all monitors via gRPC:
- GPU Health Monitor (DCGM-based checks)
- Syslog Health Monitor (log-based checks)
- CSP Health Monitor (cloud provider events)
- Any custom health monitors

### Data Persistence
Stores health events in the datastore:
- Atomic insertion with proper timestamps
- Preserves all event metadata
- Triggers change streams for downstream modules

### Kubernetes Integration
Updates cluster state based on health events:
- **Node Conditions**: Updates node conditions for fatal failures
- **Node Events**: Creates Kubernetes events for non-fatal issues
- Event correlation and deduplication

### Metadata Enrichment
Augments health events with node information:
- Cloud provider ID (AWS, GCP, Azure, OCI)
- Topology labels (zone, region, rack)
- Custom node labels
- Cached for performance

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
