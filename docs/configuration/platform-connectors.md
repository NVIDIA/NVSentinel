# Platform Connectors Configuration

## Overview

The platform-connectors module is the central communication hub of NVSentinel. It receives health events from health monitors through gRPC and processes them through a transformer pipeline. Then it stores them in the datastore and propagates them to Kubernetes. This document covers all Helm configuration options for system administrators.

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

Controls the nodes on which Kubernetes can schedule platform-connectors pods.

```yaml
platformConnector:
  tolerations: []
  affinity: {}
```

## Kubernetes API Permissions

`platformConnector.k8sConnector.enabled` controls the Kubernetes sink in both platform-connectors roles. When it is disabled, their ClusterRoles omit the `nodes/status` and `events` permissions. When it is enabled, the sink can update node conditions and create or update Kubernetes Events. Neither role needs `pods/eviction`, because node-drainer owns workload eviction.

Authentication still needs `create` on `tokenreviews`. The Metadata Augmentor needs `get` on `nodes`. The node-local role keeps its read permissions for node and pod discovery. Other components, including labeler, metadata-collector and the enabled action controllers, have separate permissions. A disabled Kubernetes sink does not make the whole installation read-only. It also does not remove host access.

## Event Processing Pipeline

Configures the event processing pipeline. This pipeline processes health events before platform-connectors stores them and propagates them to Kubernetes. Transformers change the health events in order, before the fan-out to the connectors.

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

Array of transformer stages to run in order:

- **name**: Transformer identifier (`MetadataAugmentor`, `OverrideTransformer`)
- **enabled**: Enable or disable the transformer
- **config**: Path to the transformer-specific configuration file

The chart appends the `Deduplicator` transformer stage from `platformConnector.dedup`. You normally configure deduplication through the `dedup` block, not by a manual addition of the stage to `pipeline`.

#### dedup

Deduplication transformer configuration. See [Deduplication Transformer Configuration](#deduplication-transformer-configuration).

#### transformers

Transformer-specific configurations, nested by transformer name.

**Note:** Transformers run sequentially. Put `MetadataAugmentor` first, so that it supplies node metadata to the transformers after it.

## Metadata Augmentor Configuration

Adds node labels and metadata from Kubernetes to health events.

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

List of node label keys to include in health event enrichment. The Metadata Augmentor reads only the labels in this list from nodes, and adds them to health events.

> Note: `distros/kubernetes/nvsentinel/values.yaml` defines the complete default list.

#### skipNodeLabel

Node label, as `key=value`, that marks a node that NVSentinel must not act on. The Metadata Augmentor downgrades the health events from a node with this label to `STORE_ONLY`. NVSentinel records these health events for audit, but does no remediation and causes no Kubernetes side effects.

```yaml
platformConnector:
  transformers:
    MetadataAugmentor:
      skipNodeLabel: "nvsentinel.dgxc.nvidia.com/managed=false"
```

To disable the behavior, leave it empty. Do not set a different value, because janitor and labeler also use this label with a fixed key and value. Only the exact value excludes a node. With any other value, including an absent label or a typo, NVSentinel manages the node normally.

To give a node to another owner, label the node. The owner can be a hardware team that works on the node, or an external remediation system. NVSentinel stays enabled for the rest of the fleet.

A new label takes effect within the metadata cache time, after platform-connectors can read the node. For each health event of an excluded node, platform-connectors reads the node again. As a result, the removal of the label takes effect on the next health event of the node. If that read fails, the node stays excluded.

> **Important:** `MetadataAugmentor` enforces this gate. If you remove the transformer from `transformers` while `skipNodeLabel` still has a value, the gate stops. In this case, platform-connectors logs a warning at startup, and the remediation of excluded nodes continues.

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

Applies CEL-based rules to change the properties of health events (isFatal, isHealthy, recommendedAction, customRecommendedAction).

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

Array of override rules that the transformer evaluates in order (the first match wins):

- **name**: Human-readable rule name for logging
- **when**: CEL expression that evaluates to a boolean
- **override**: Properties to change (isFatal, isHealthy, recommendedAction, customRecommendedAction)

To send a fault to a custom remediation action, set `recommendedAction: "CUSTOM"` and set `customRecommendedAction` to the action name. Each of the two needs the other: platform-connectors does not start if a rule sets only one of them, or sets an empty name. An override to a built-in action clears the `customRecommendedAction` of the health event.

### CEL Expression Context

CEL expressions can use the `event` object, which has these fields:

| Field | Type | Description |
|-------|------|-------------|
| `event.nodeName` | string | Node where the health event occurred |
| `event.agent` | string | Health monitor that generated the health event |
| `event.componentClass` | string | Component class (for example, "GPU" or "Network") |
| `event.checkName` | string | Name of the health check |
| `event.message` | string | Human-readable error message |
| `event.errorCode` | []string | Array of error codes |
| `event.entitiesImpacted` | []Entity | Affected entities (for example, GPUs and NICs) |
| `event.isFatal` | bool | Whether the error is fatal |
| `event.isHealthy` | bool | Overall health status |
| `event.recommendedAction` | string | Recommended remediation action |
| `event.customRecommendedAction` | string | Custom action name, as sent. Also check `event.recommendedAction == 'CUSTOM'`, because a health event with a built-in action can also carry a name |
| `event.metadata` | map | Node metadata from MetadataAugmentor |

**Entity fields:** Each entity in `entitiesImpacted` has:

- `entityType` - Type of entity (for example, "GPU" or "NIC")
- `entityValue` - Entity identifier (for example, a GPU UUID or a PCI address)

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

**Send a fault to a custom action:**

```yaml
transformers:
  OverrideTransformer:
    rules:
      - name: "nvlink-to-hardware-repair"
        when: 'event.checkName == "GpuNvlinkWatch" && event.isFatal == true'
        override:
          recommendedAction: "CUSTOM"
          customRecommendedAction: "hardware-repair"
```

fault-remediation must have an entry for the action name in `maintenance.actions`. See [Custom remediation plugin](../tutorials/plugin-custom-remediation.md).

## Deduplication Transformer Configuration

Suppresses repeated health events within a burst window, before platform-connectors writes them to the datastore or propagates them to Kubernetes. The deduplicator derives the dedup key from:

```text
(nodeName, checkName, canonical entitiesImpacted, canonical errorCode, processingStrategy, isHealthy)
```

`message`, `pid`, timestamps, and other fields outside the key do not distinguish health events. If a producer needs those fields to create distinct faults, it must include them in `entitiesImpacted` or `errorCode`. The key includes `processingStrategy`, so that a `STORE_ONLY` observation cannot suppress a later `EXECUTE_REMEDIATION` health event for the same fault identity.

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

Enables the deduplication transformer. When it is disabled, every health event that reaches platform-connectors keeps its original processing strategy.

#### suppressionWindow

Go duration string that controls how long the deduplicator downgrades repeated health events with the same key to `STORE_AND_ANALYSE`. After the window expires, the next matching health event stays `EXECUTE_REMEDIATION`.

#### cleanupInterval

Go duration string that controls how often the in-memory tracker removes expired keys that did not recur.

#### includeChecks

List of `checkName` values that platform-connectors can deduplicate. Keep this list focused on high-volume repeated signal streams, such as `SysLogsXIDError` and `SysLogsSXIDError`. Every other check passes through unchanged.

### Healthy Event Behavior

Deduplication does not downgrade healthy events. Before they continue downstream, they clear any matching unhealthy entries from the in-memory tracker. This keeps recovery and baseline events reliable, even when a previous healthy event did not update every downstream consumer. The deduplicator still deduplicates repeated unhealthy fault observations.

### Operational Notes

- Dedup state is in memory only. A restart of the platform-connectors pod clears it.
- platform-connectors exposes the dedup counter as `nvsentinel_platform_connector_dedup_store_and_analyse_total{check,err_code}`. It carries no node label, because the deployment platform connector runs the transformer for the whole fleet. The node is in the log line.
- For the key, the deduplicator canonicalizes `entitiesImpacted` and `errorCode` as sets. Differences in order do not create distinct health events.

## Prometheus Connector

Records every health event that reaches platform-connectors as a Prometheus counter. It has no sink and no external dependency. It only observes, so it cannot fail or block the connectors that share the event fan-out with it.

```yaml
platformConnector:
  promConnector:
    enabled: false
```

### Parameters

#### enabled

Registers `health_events_total{node, agent, check_name, recommended_action, is_fatal, is_healthy}` on the existing metrics endpoint. The default is `false`, as for the other optional connectors.

### When to use it

Every monitor publishes through platform-connectors, so one connector covers `gpu-health-monitor`, `syslog-health-monitor`, `nic-health-monitor`, `csp-health-monitor`, `kubernetes-object-monitor` and `health-events-analyzer` at the same time.

On a fleet where actionable health events are rare, this metric answers "what needs attention right now" without a query to the datastore. For the label rationale and example queries, see [Prometheus Connector Metrics](../METRICS.md#prometheus-connector-metrics).

## gRPC Sink Connector

Forwards each health event to an external gRPC server. The server receives the full `HealthEvent` proto, with no truncation. The server implements the existing `PlatformConnector.HealthEventOccurredV1` RPC, so you need no new proto definitions. Use the connector to send health events to an organization-specific remediation or analytics pipeline. It runs in addition to the store and Kubernetes connectors.

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

Enables the connector. It is disabled by default, as are the other optional connectors.

#### target

Address of the receiving gRPC server, as `host:port`. Required when you enable the connector.

#### maxRetries

Retry attempts with exponential backoff before the connector drops the health event. The total number of send attempts is `1 + maxRetries`. The per-RPC timeout is fixed at 10 seconds. If the target does not answer in that time, the attempt counts as a failure, and the connector retries it.

The connector holds a failed batch and retries it before it takes the next batch. As a result, a newer batch cannot overtake a pending fault or recovery. The connector also retries a batch that fails while the queues drain at shutdown.

If the target refuses a batch with InvalidArgument, PermissionDenied or Unimplemented, it gives the same answer on every attempt. As a result, the connector does not retry such a batch. It drops the batch at once and logs it at Error.

If its loop is not finished 10 seconds after the shutdown starts, the connector logs at Error the number of batches still in its queue. The store connector does the same. The deployment platform connector does not use this queue. It sends each batch one time, so `maxRetries` has no effect there.

#### tokenPath

Path to a projected Kubernetes ServiceAccount token. If you set it, the connector attaches the token as a Bearer header on every RPC. The receiving server validates the token with the TokenReview API. An empty value disables authentication, which matches the other internal NVSentinel gRPC connections.

```yaml
platformConnector:
  grpcSinkConnector:
    tokenPath: "/var/run/secrets/nvsentinel/grpcsink/token"
```

The target must be an external sink, not another NVSentinel platform-connectors pod. A receiving platform-connectors pod accepts health events only for the node that the token of the sending pod names. It rejects health events about other nodes (`node_mismatch`). As a result, chained platform-connectors pods cannot forward the health events of the whole cluster. Fan-in belongs to the datastore, which every platform-connectors pod already writes to.

Use a network policy to restrict which pods can reach the target.

## Datastore Client Certificates

Where each store client looks for its TLS client certificate. The paths must match the paths for which the datastore chart issues certificates.

```yaml
platformConnector:
  mongodbStore:
    enabled: false
    clientCertMountPath: "/etc/ssl/mongo-client"
    maxRetries: 3
  postgresqlStore:
    clientCertMountPath: "/etc/ssl/client-certs"
```

If a PostgreSQL client certificate is mounted, platform-connectors first runs a `fix-cert-permissions` init container. The reason is that the PostgreSQL client rejects a key file that is group-readable or world-readable. `mongodbStore.maxRetries` limits the retries of a failed store write before the store connector drops the health event.

The store connector holds a failed batch and retries it before it takes the next batch. As a result, a newer batch cannot overtake a pending fault or recovery.

The datastore can reject a health event for its content, for example a check name that is longer than its PostgreSQL column. Such a health event gets the same answer on every attempt. The store connector does not retry it. It drops that health event, logs it at Error, and stores the health events after it in the batch.

At shutdown, the loops get up to 10 seconds to finish before the datastore client closes. The store connector logs at Error each batch that it drops during a write. It logs the batches that are still in the queue when its loop stops as one Error line with their count. Shutdown does not wait for a connector that does not stop within 5 seconds after that. Shutdown then closes the connections and logs at Error the number of batches that the connector still holds.

The deployment platform connector does not use this queue. It writes each batch one time per request, so the retries above and `mongodbStore.maxRetries` do not apply there.

To rotate certificates without a restart of the pods, see [Client Certificate Rotation](./README.md#client-certificate-rotation).

## Kubernetes Connector

Configures the Kubernetes API client that creates node conditions and Kubernetes events.

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

Enables the Kubernetes connector, which creates node conditions and Kubernetes events.

#### maxRetries

Maximum retries for each failed Kubernetes write, after its initial attempt. If you omit it or set `0`, the value is `25`. A positive integer overrides the default. Negative and non-integer values are rejected.

On the node-local DaemonSet, these settings control the Kubernetes queue. The deployment platform connector retries each write the same way inside the request. But it retries only up to the `ConditionUpdateTimeout` of the `deployment` object in its config.json, and `maxRetryDuration` cannot extend this time. If a write still fails, the deployment platform connector counts it in `platform_connector_best_effort_failures_total{connector="kubernetes"}` and `k8s_platform_connector_dropped_writes_total`. It acknowledges the batch anyway.

Each node status update and each Kubernetes Event write has its own retry state. If one write fails, the connector does not repeat the successful writes. The connector skips permanent errors, and the other writes can still retry. A node status update applies all condition changes for that node together.

Event retries keep the stable fault name. After an uncertain response, they check the persisted timestamp. The connector accepts an occurrence that is already persisted, and does not increase its count. Later reports refresh the existing Event after suppression expires or recovery clears it. Event timestamps have one-second precision. The count of the Kubernetes Event does not include every monitor report.

Retry delays start at 500 milliseconds and double after each failure, up to a maximum of 3 seconds. Both the count limit and `maxRetryDuration` apply. The limit that the connector reaches first stops that write.

#### maxRetryDuration

Maximum processing time for the whole batch, including Kubernetes API calls and inner retry delays. The default is `1m`. If you omit it or set a zero duration, the default applies. Positive duration strings up to `5m` are valid. Negative, invalid, and larger durations are rejected.

The connector holds the current batch while it retries. Newer batches cannot overtake a pending fault or recovery. This pauses the consumption of the Kubernetes queue, while the other connector queues continue independently. Cancellation interrupts API calls and backoff.

At shutdown, the connector writes the batches that its queue holds, as the other connectors do. If the loops are not finished after 10 seconds, shutdown interrupts the API calls and the backoff. This is true even when the queue is empty. The reason is that the loop can still have a write in progress for a batch that it already took from the queue.

For a five-minute outage window, configure both limits:

```yaml
platformConnector:
  k8sConnector:
    maxRetries: 200
    maxRetryDuration: 5m
```

The default count of 25 allows 69.5 seconds of outer backoff, but the default one-minute deadline stops retries sooner. The five-minute example raises both limits, so that its deadline controls the window. API calls and client-go retries also use the time budget. All writes of a large batch share one deadline.

The connector discards a write that uses all its retries. Other writes can still run within the batch deadline. When the deadline expires, the connector discards the remaining writes and goes to the next batch. A lost healthy recovery can therefore still leave a condition set.

These bounded retries do not guarantee delivery through longer outages or pod restarts. Newer batches collect in memory during backpressure. There is no persistent queue and no ingress memory limit.

#### Drop metrics

- `k8s_platform_connector_dropped_writes_total{operation,reason,is_healthy}` counts each discarded write, including writes that the connector did not try before the deadline or the shutdown.
- `k8s_platform_connector_dropped_batches_total{reason}` counts each affected batch one time per reason. A batch with more than one failure reason increments more than one series.

The `operation` label is `node_condition` or `node_event`. The `reason` label is `permanent_error`, `retry_exhausted`, `retry_timeout`, or `shutdown`. The `is_healthy` label is `true` if the discarded write contains at least one recovery, and `false` if it contains none. A lost recovery can leave a fault in place with nothing to clear it. The next change reports a lost fault again.

For example, alert when the connector discards writes outside a shutdown:

```promql
sum(increase(k8s_platform_connector_dropped_writes_total{reason!="shutdown"}[5m])) > 0
```

The connector explicitly discards a failed queue item. This operation does not add the item to the queue again. Monitor the drop counters together with the Kubernetes queue depth, to find exhausted retry windows and growing backlogs.

#### maxNodeConditionMessageLength

Maximum length of node condition messages in characters.

#### compactedHealthEventMsgLen

Budget, in bytes, for the part of the message of each health event that comes before its recommended action. The default is `72`, and the value must be greater than zero.

One node condition message can carry several health events. The connector compacts the message only when their combined length exceeds `maxNodeConditionMessageLength`. First, it drops messages with a duplicate identity (same error code, entity and recommended action). Then it shortens the free-text diagnostic of each remaining message to this budget. It keeps the entity identifiers that recovery needs.

If the result still does not fit, the connector removes diagnostic text. It also shortens GPU UUID values to 8 characters plus `...` when the same entry also has a GPU or PCI identifier. This form keeps the error code, entity tokens and recommended action. The connector uses this form only when it saves bytes.

A healthy GPU event clears an entry that names its GPU index and PCI address, or its complete GPU UUID. A shortened UUID does not match, so a shortened entry clears only through its GPU index and PCI address.

If the list still exceeds the limit, a byte-level cut is the final fallback. Lower this value to fit more health events into one condition. Raise it to keep more of the original text of each health event.

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

## Deployment Platform Connector

The platform-connectors binary can also run as a central Deployment (`--mode=deployment`). This page calls it the deployment platform connector. A publisher on `publishTo: deployment` sends its health events directly to the deployment platform connector over TLS, with a bearer token. It does not use the socket of the node-local DaemonSet.

The deployment platform connector is disabled by default. To move to it, do these steps in this order:

1. Set `platformConnector.deployment.enabled: true` on the umbrella chart. Keep every publisher on `publishTo: socket`. The chart then renders the Deployment, its Service, its certificate and its network policy. The replicas need the idempotency index. The datastore setup creates it, like every other index. On MongoDB, the setup Job creates it. On PostgreSQL, the components that create the tables also create the index when they start: fault-quarantine, fault-remediation, health-events-analyzer and node-drainer. Unlike these components, platform-connectors does not create it. A replica exits after it waits five minutes for the index. Thus a missing index shows as CrashLoopBackOff, not as a pod that is quietly not ready. The replica tries again after each restart until the index exists. Wait until the replicas are ready.
2. Move the publishers to the deployment platform connector. Then disable the DaemonSet:
   1. Set `publishTo: deployment` on each enabled monitor subchart: `gpu-health-monitor`, `syslog-health-monitor`, `nic-health-monitor`, `kubernetes-object-monitor`, `slurm-drain-monitor`, `csp-health-monitor`, `nvcre-certification-monitor` and `health-events-analyzer`.
   2. If the MaintenanceRequest controller of lifecycle-manager is enabled, set `lifecycle-manager.publishTo: deployment`. See [Lifecycle Manager](lifecycle-manager.md#maintenancerequest-controller).
   3. If preflight is enabled, set `preflight.publishTo: deployment`. The webhook then gives each injected check the target, the token path and the CA bundle. It also puts the publisher label on the tenant pod. The preflight controller keeps a copy of the CA bundle in a ConfigMap named `nvsentinel-platform-connector-ca`, in each namespace that it injects into. It updates the copies when cert-manager rotates the CA. See [Preflight](preflight.md#publishing-to-the-deployment-platform-connector).
   4. Set `platformConnector.daemonset.enabled: false` last. This removes the DaemonSet and its RBAC. The chart refuses this value while an enabled publisher still uses `publishTo: socket`. Also, gpu-health-monitor still mounts the socket directory `/var/run/nvsentinel` of the node, and this directory must exist. The DaemonSet creates it. After a node restart, the directory is gone, because `/var/run` is not persistent. Thus, while gpu-health-monitor runs, keep the DaemonSet or create the directory on each node in another way.

A publisher on `publishTo: deployment` gets its connection settings from the chart as `HEALTH_PUBLISH_*` environment variables: the target, the token path and the CA bundle. With `global.platformConnectorDeployment.tls.mode: insecureDevelopmentMode`, the publisher gets `HEALTH_PUBLISH_INSECURE=true` instead of the CA bundle. The token is the same projected token that the publisher uses on the socket path. See [Authentication](authentication.md#deployment-platform-connector). The client in the publisher works as follows:

- The retry window is 5 minutes. This is a client default. To change it, set `HEALTH_PUBLISH_RETRY_WINDOW` on the pod.
- The request timeout is fixed at 30 seconds.
- The client sends one batch at a time. Its publish call returns only after the deployment platform connector stores the batch. Thus a monitor marks a health event as reported only after the deployment platform connector stores it. The node-local socket connector, by contrast, acknowledges a batch when it queues it.
- If the deployment platform connector is unreachable, the client retries until the retry window ends. Then it drops the batch and reports the failure. The monitor sends the health event again on its next cycle.
- The pause between two attempts is at most 30 seconds. The client retries the connection at most 10 seconds apart. Thus the client delivers a batch within about 30 seconds after the deployment platform connector is available again.
- A monitor can stop waiting earlier, because its context ended or its own timeout expired. The client then withdraws the batch at once, even when the batch is already on the wire.
- An attempt can be cut or can time out after the deployment platform connector stores the batch. This can leave a duplicate next to the health event that the monitor sends again. The deployment platform connector tolerates this duplicate.
- The deployment platform connector can refuse a batch permanently. It does this if the batch is invalid, or if the datastore rejects the batch for its content. It also does this if the batch names a node that the monitor has no permission to report on. The client then reports the batch as rejected at once and does not retry. If the datastore rejects a health event for its content, the health events before it in the batch can already be in the datastore. The deployment platform connector does not store the health events after it. For each permanent rejection, syslog-health-monitor skips the journal entry. Both nic-health-monitor and health-events-analyzer drop the health event and count it.
- `nvsentinel_health_events_publisher_dropped_total` counts the drops by reason. See [METRICS.md](../METRICS.md#health-event-publisher).

The deployment platform connector has its own network policy for the gRPC port. The metrics port is open through the `metrics-access` policy of the chart, as for the other components. The gRPC port admits these pods:

- Every pod in the release namespace.
- Pods with the label `nvsentinel.nvidia.com/health-publisher: "true"` in the namespace of each entry in `global.platformConnectorAuth.crossNodeServiceAccounts`. A publisher that this chart does not ship must put the label on its pods itself.
- Pods with the same label in the namespaces that the preflight webhook injects into, while `preflight.publishTo` is `deployment`. The policy uses `preflight.namespaceSelector`, so the webhook and the policy select the same namespaces. The webhook puts the label on the tenant pods.

Two publishers can run on the host network:

- gpu-health-monitor, when `useHostNetworking` is set or the DCGM mode (`global.dcgm.mode`) is `external-hostengine`.
- The preflight checks, when their workload pods use the host network (`preflight.hostNetworkWorkloads: true`).

A CNI that enforces network policies identifies such a pod by its node, not by its labels. Thus, while such a publisher uses the deployment platform connector, the chart opens the gRPC rule to every source. The chart annotates the policy to say so. It also prints the same note at install and upgrade time. In all cases, the token of the caller decides who can publish. On a CNI that does not enforce network policies, the policy has no effect.

The server settings are under `platformConnector.deployment` in `values.yaml`. The chart puts them in three places:

- The Deployment spec gets the replicas, the placement and the resources.
- The `DATASTORE_MAX_CONNECTIONS` environment variable gets the datastore pool size, `datastore.maxPoolSize`.
- The config file of the deployment platform connector, in the ConfigMap `platform-connector-deployment`, gets the other settings:
  - the TokenReview cache and rate limit
  - the condition update timeout
  - the connection ages and buffers
  - the Kubernetes client rate limit
  - the node metadata cache

The chart renders this config file from the same template as the config file of the DaemonSet, so both roles read the same keys. The file of the deployment platform connector has fleet sizing and a `deployment` object with its own settings. The gRPC port and the TLS mode are under `global.platformConnectorDeployment` (`grpcPort`, `tls.mode`), because the publisher subcharts read them too. The container args carry only these values:

- `--mode=deployment`
- the config path
- the metrics port
- the listener settings (`--listen-addr`, and `--tls-cert-dir` or `--tls-insecure-development-mode`)
- the datastore client certificate flag

The replicas verify the idempotency index one time, before they become ready. To restore the datastore from a backup:

1. Scale the Deployment to zero.
2. Restore the datastore.
3. Run the datastore setup again, so that the index exists. On MongoDB, delete the completed setup Job. Then upgrade or sync. On PostgreSQL, restart a component that creates the tables.
4. Scale the Deployment up again. The replicas verify the index again when they start.

## Kubernetes Authentication

platform-connectors uses in-cluster Kubernetes authentication by default. In that mode, it authenticates with the pod ServiceAccount and needs no extra flags.

For host-managed deployments, pass a kubeconfig file explicitly:

```bash
platform-connectors \
  --socket=/var/run/nvsentinel.sock \
  --config=/etc/config/config.json \
  --kubeconfig=/var/lib/kubelet/kubeconfig
```

If you set `--kubeconfig`:

- The Kubernetes connector uses that kubeconfig instead of `InClusterConfig()`
- `MetadataAugmentor` uses the same kubeconfig for node metadata lookups

If you do not set `--kubeconfig`, both use the in-cluster configuration.

The bundled Helm chart uses in-cluster authentication and does not set this flag.
