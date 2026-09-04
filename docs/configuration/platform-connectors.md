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

### Scheduling

Controls where platform-connectors pods can be scheduled.

```yaml
platformConnector:
  tolerations: []
  affinity: {}
```

## Event Processing Pipeline

Configures the event processing pipeline that processes health events before storage and Kubernetes propagation. Transformers mutate events in order before connector fan-out.

```yaml
platformConnector:
  pipeline:
    - name: MetadataAugmentor
      enabled: false
      config: /etc/config/metadata.toml
    - name: OverrideTransformer
      enabled: false
      config: /etc/config/overrides.toml
  
  dedup:
    enabled: true
    suppressionWindow: "3m"
    cleanupInterval: "60s"
    includeChecks:
      - SysLogsXIDError
      - SysLogsSXIDError

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
Array of transformer stages to execute in order:
- **name**: Transformer identifier (`MetadataAugmentor`, `OverrideTransformer`)
- **enabled**: Enable/disable the transformer
- **config**: Path to transformer-specific configuration file

The chart appends the `Deduplicator` transformer stage from `platformConnector.dedup`; operators normally configure deduplication through the `dedup` block rather than adding it manually to `pipeline`.

#### dedup
Deduplication transformer configuration. See [Deduplication Transformer Configuration](#deduplication-transformer-configuration).

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

## Deduplication Transformer Configuration

Suppresses repeated health events within a burst window before they are written to the datastore or propagated to Kubernetes. The dedup key is derived from:

```text
(nodeName, checkName, canonical entitiesImpacted, canonical errorCode, processingStrategy, isHealthy)
```

`message`, `pid`, timestamps, and other fields outside the key do not distinguish events. If a producer needs those fields to create distinct faults, it should include them in `entitiesImpacted` or `errorCode`. `processingStrategy` is included so a `STORE_ONLY` observation cannot suppress a later `EXECUTE_REMEDIATION` event for the same fault identity.

```yaml
platformConnector:
  dedup:
    enabled: true
    suppressionWindow: "3m"
    cleanupInterval: "60s"
    includeChecks:
      - SysLogsXIDError
      - SysLogsSXIDError
```

### Parameters

#### enabled
Enables the deduplication transformer. When disabled, every event that reaches platform-connectors keeps its original processing strategy.

#### suppressionWindow
Go duration string that controls how long repeated events with the same key are downgraded to `STORE_AND_ANALYSE`. After the window expires, the next matching event remains `EXECUTE_REMEDIATION`.

#### cleanupInterval
Go duration string that controls how often the in-memory tracker removes expired keys that have not recurred.

#### includeChecks
List of `checkName` values eligible for platform-connector deduplication. Keep this focused on high-volume repeated signal streams, such as `SysLogsXIDError` and `SysLogsSXIDError`; every other check passes through unchanged.

### Healthy Event Behavior

Healthy events are not downgraded by deduplication. Before they continue downstream, they clear any matching unhealthy entries from the in-memory tracker. This keeps recovery and baseline events reliable even when a previous healthy event did not update every downstream consumer, while repeated unhealthy fault observations are still deduplicated.

### Operational Notes

- Dedup state is in-memory only and is cleared on platform-connectors pod restart.
- The dedup counter is exposed as `nvsentinel_platform_connector_dedup_store_and_analyse_total{check,node,err_code}`.
- `entitiesImpacted` and `errorCode` are canonicalized as sets for keying; ordering differences do not create distinct events.

## Prometheus Connector

Records every health event reaching the platform connector as a Prometheus counter. It has
no sink and no external dependency: it only observes, so it cannot fail or block the
connectors it shares the event fan-out with.

```yaml
platformConnector:
  promConnector:
    enabled: false
```

### Parameters

#### enabled
Registers `health_events_total{node, agent, check_name, recommended_action, is_fatal, is_healthy}`
on the existing metrics endpoint. Defaults to `false`, like the other optional connectors.

### Why it lives here rather than in each monitor

Every monitor publishes through the platform connector, so one connector covers
`gpu-health-monitor`, `syslog-health-monitor`, `nic-health-monitor`, `csp-health-monitor`,
`kubernetes-object-monitor` and `health-events-analyzer` at once. It is also already the
authority on severity, since `isFatal` is derived from `recommendedAction`.

On a fleet where actionable events are rare, this is the metric that answers "what needs
attention right now" without querying the datastore. See
[Prometheus Connector Metrics](../METRICS.md#prometheus-connector-metrics) for the label
rationale and example queries.

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

## Kubernetes Authentication

Platform Connectors uses in-cluster Kubernetes authentication by default. In that mode it authenticates with the pod ServiceAccount and no extra flags are required.

For host-managed deployments, pass a kubeconfig file explicitly:

```bash
platform-connectors \
  --socket=/var/run/nvsentinel.sock \
  --config=/etc/config/config.json \
  --kubeconfig=/var/lib/kubelet/kubeconfig
```

When `--kubeconfig` is set:
- The Kubernetes connector uses that kubeconfig instead of `InClusterConfig()`
- `MetadataAugmentor` uses the same kubeconfig for node metadata lookups

When `--kubeconfig` is unset, existing in-cluster behavior is unchanged.

The bundled Helm chart continues to rely on in-cluster authentication and does not need to set this flag.
