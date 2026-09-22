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

#### skipNodeLabel
Node label, as `key=value`, that marks a node NVSentinel must not act on. Events from a node carrying this label are downgraded to `STORE_ONLY`: NVSentinel records them for audit but performs no remediation and no Kubernetes side effects.

```yaml
platformConnector:
  transformers:
    MetadataAugmentor:
      skipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=false"
```

Leave it empty to disable the behavior. The value must match the key and value that `commons/pkg/managed` defines, so change it only together with that constant. Only the exact value opts a node out; any other value, including an absent label or a typo, leaves the node managed normally. Label a node to hand it to another owner — a hardware team working on it, or an external remediation system — without disabling NVSentinel for the rest of the fleet.

> **Important:** `MetadataAugmentor` enforces this gate, so removing the transformer from `transformers` while `skipNodeLabel` is still set stops the gate from applying. The connector logs a warning at startup and keeps remediating opted-out nodes.

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
- The dedup counter is exposed as `nvsentinel_platform_connector_dedup_store_and_analyse_total{check,err_code}`. It carries no node label, because the deployment platform connector runs the transformer for the whole fleet; the node is in the log line.
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

## gRPC Sink Connector

Forwards each health event to an external gRPC server, which receives the full `HealthEvent` proto with no truncation. The server implements the existing `PlatformConnector.HealthEventOccurredV1` RPC, so no new proto definitions are needed. Use it to feed an organization-specific remediation or analytics pipeline alongside the store and Kubernetes connectors.

```yaml
platformConnector:
  grpcSinkConnector:
    enabled: false
    target: ""        # gRPC server address, e.g. "my-service.example.com:50051"
    maxRetries: 3
    tokenPath: ""
```

### Parameters

#### enabled
Turns the connector on. Disabled by default, like the other optional connectors.

#### target
Address of the receiving gRPC server, as `host:port`. Required when the connector is enabled.

#### maxRetries
Retry attempts with exponential backoff before the connector drops the event. Total send attempts are `1 + maxRetries`. The per-RPC timeout is fixed at 10 seconds; a target that does not answer inside that window counts as a failure and is retried.

#### tokenPath
Path to a projected Kubernetes ServiceAccount token. When set, the connector attaches the token as a Bearer header on every RPC, and the receiving server validates it with the TokenReview API. Empty disables authentication, which matches the other internal NVSentinel gRPC connections.

```yaml
platformConnector:
  grpcSinkConnector:
    tokenPath: "/var/run/secrets/nvsentinel/grpcsink/token"
```

The target must be an external sink, not another NVSentinel platform connector. A connector's own token is bound to the node its pod runs on, and a receiving connector rejects a token whose node claim names a different node, so chaining connectors cannot authenticate. Fan-in belongs to the datastore, which every connector already writes to.

Restrict which pods can reach the target with a network policy. See [ADR-033](../designs/033-grpc-sink-connector.md) for the design rationale.

## Datastore Client Certificates

Where each store client looks for its TLS client certificate. The paths must match what the datastore chart issues certificates for.

```yaml
platformConnector:
  mongodbStore:
    enabled: false
    clientCertMountPath: "/etc/ssl/mongo-client"
    maxRetries: 3
  postgresqlStore:
    clientCertMountPath: "/etc/ssl/client-certs"
```

When a PostgreSQL client certificate is mounted, the platform connector runs a `fix-cert-permissions` init container first, because the PostgreSQL client rejects a key file that is group-readable or world-readable. `mongodbStore.maxRetries` bounds the retries on a failed store write before the event is dropped.

To rotate certificates without restarting pods, see [Client Certificate Rotation](./README.md#client-certificate-rotation).

## Kubernetes Connector

Configures the Kubernetes API client for creating node conditions and events.

```yaml
platformConnector:
  k8sConnector:
    enabled: true
    maxRetries: 25
    maxRetryDuration: 1m
    maxNodeConditionMessageLength: 1024
    qps: 5.0
    burst: 10
```

### Parameters

#### enabled
Enables Kubernetes connector for creating node conditions and events.

#### maxRetries

Maximum retries for each failed Kubernetes write, after its initial attempt. Omission or `0` selects `25`; positive integers override the default. Negative and non-integer values are rejected. An existing explicit value, such as `3`, still limits each write to that retry count.

These settings apply to the node-local Kubernetes queue. On the deployment platform connector the Kubernetes connector runs inside the request as best effort within the `ConditionUpdateTimeout` of the `deployment` object in its config.json; a failure or timeout is counted in `platform_connector_best_effort_failures_total{connector="kubernetes"}` and the batch is acknowledged anyway.

Each node status update and Kubernetes Event write has its own retry state. Successful writes are not repeated when another write fails. Permanent errors are skipped without preventing other writes from retrying. A node status update applies all condition changes for that node together.

Event retries retain the stable fault name and check the persisted timestamp after an uncertain response. An already persisted occurrence is accepted without increasing its count. Later reports refresh the existing Event after suppression expires or recovery clears it. Event timestamps have one-second precision; this counter does not count every monitor report.

Retry delays start at 500 milliseconds, double after each failure, and are capped at 3 seconds. Both the count limit and `maxRetryDuration` apply: whichever is reached first stops that write.

#### maxRetryDuration

Maximum processing time for the whole batch, including Kubernetes API calls and inner retry delays. The default is `1m`. Omission or a zero duration selects the default. Positive duration strings up to `5m` are accepted; negative, invalid, and larger durations are rejected.

The connector holds the current batch while retrying. Newer batches cannot overtake a pending fault or recovery. This pauses consumption of the Kubernetes queue while other connector queues continue independently. Cancellation and connector shutdown interrupt API calls and backoff.

For a five-minute outage window, configure both limits:

```yaml
platformConnector:
  k8sConnector:
    maxRetries: 200
    maxRetryDuration: 5m
```

The default count of 25 allows 69.5 seconds of outer backoff, but the default one-minute deadline stops retries sooner. The five-minute example raises both limits so its deadline controls the window. API calls and client-go retries also consume the time budget. A large batch shares one deadline across its writes.

A write that exhausts its retry count is discarded; other writes can still run within the batch deadline. When the deadline expires, remaining writes are discarded and the connector advances to the next batch. A lost healthy recovery can therefore still leave a condition set. These bounded retries do not guarantee delivery through longer outages or pod restarts. Newer batches accumulate in memory during backpressure; this change does not add a persistent queue or an ingress memory limit.

#### Drop metrics

- `k8s_platform_connector_dropped_writes_total{operation,reason}` counts individual discarded writes, including writes not attempted before deadline or shutdown.
- `k8s_platform_connector_dropped_batches_total{reason}` counts each affected batch once per reason. A batch with multiple failure reasons increments multiple series.

The `operation` label is `node_condition` or `node_event`. The `reason` label is `permanent_error`, `retry_exhausted`, `retry_timeout`, or `shutdown`.

For example, alert when writes are discarded outside shutdown:

```promql
sum(increase(k8s_platform_connector_dropped_writes_total{reason!="shutdown"}[5m])) > 0
```

A failed queue item is explicitly discarded; this operation does not requeue it. Monitor drop counters together with Kubernetes queue depth to detect exhausted retry windows and growing backlogs.

#### maxNodeConditionMessageLength
Maximum length of node condition messages in characters.

#### compactedHealthEventMsgLen
Budget, in bytes, for the part of each event's message that precedes its recommended action. The default is `72` and it must be greater than zero.

One node condition message can carry several health events. The connector compacts only when their combined length exceeds `maxNodeConditionMessageLength`: it first drops messages with a duplicate identity (same error code, entity and recommended action), then shortens each remaining message's free-text diagnostic to this budget while keeping the entity identifiers that recovery needs. If the result still does not fit, the last entry is truncated. Lower this value to fit more events into one condition; raise it to keep more of each event's original text.

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
