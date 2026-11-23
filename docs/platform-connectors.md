# Platform Connectors

## Overview

Platform Connectors is the central hub that receives health events from all health monitors and sends them to the correct destinations. It works as a translator and a router. It persists the health events to the datastore and shows them in the Kubernetes node status.

Platform Connectors works like a post office. It receives messages (health events) from different senders (health monitors). It routes the messages to the correct destinations (the datastore and the Kubernetes API).

### Why Do You Need This?

Platform Connectors connects monitoring to action:

- **Centralized ingestion**: One endpoint for all health events
- **Data persistence**: Stores health events in the datastore for the remediation pipeline
- **Kubernetes integration**: Updates node conditions and Kubernetes events based on the health status
- **Metadata enrichment**: Optionally adds node metadata to health events (for example, cloud provider information and labels)
- **Burst deduplication**: Marks repeated health events with the same fault identity as `STORE_AND_ANALYSE` before the downstream fan-out
- **Decoupling**: Keeps health monitors independent from platform-specific implementations

Without Platform Connectors, each health monitor must integrate directly with the storage and the APIs of each platform. This causes tight coupling and complexity.

## How It Works

Platform Connectors usually runs as a DaemonSet in the cluster. It works as follows:

1. Exposes a gRPC service that health monitors use to send health events
2. Receives health events through gRPC (the `HealthEventOccurredV1` API)
3. Processes the health events through the transformer pipeline:
   - **Metadata Augmentor**: Adds node metadata to health events (cloud provider, labels, topology)
   - **Override Transformer**: Applies CEL-based rules to change the properties of health events
4. Runs deduplication as a transformer:
   - **Deduplicator**: Marks repeated health events with the same node, check, impacted entities, error code, and health state as `STORE_AND_ANALYSE`
5. Queues the health events in ring buffers for parallel processing
6. Processes the health events through multiple connectors:
   - **Store Connector**: Persists health events to the datastore
   - **Kubernetes Connector**: Updates node conditions and Kubernetes events
7. Each connector processes the health events independently, for resilience

The event processing pipeline runs the transformers in order, so each transformer can build on the enrichments of the transformers before it. The ring buffer architecture makes sure that Platform Connectors processes health events reliably, even under high load. It has retry logic for transient failures.

The same binary can also run as a central Deployment, the deployment platform connector. A monitor with `publishTo: deployment` publishes directly to it over TLS, with a bearer token. The deployment platform connector runs the pipeline and both connectors inside each request. It acknowledges a batch only after it stores the batch. See [Deployment Platform Connector](configuration/platform-connectors.md#deployment-platform-connector).

## Configuration

Configure Platform Connectors through Helm values:

```yaml
platformConnector:
  daemonset:
    enabled: true
  
  # Transformer pipeline - defines execution order
  pipeline:
    - name: MetadataAugmentor
      enabled: false
      config: /etc/config/metadata.toml
    - name: OverrideTransformer
      enabled: false
      config: /etc/config/overrides.toml

  # Health event burst deduplication transformer
  dedup:
    enabled: true
    suppressionWindow: "3m"
    cleanupInterval: "60s"
    includeChecks:
      - SysLogsXIDError
      - SysLogsSXIDError
  
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

- **Pipeline**: Configure the execution order of the transformers, and enable or disable each transformer
- **Deduplication**: Configure how Platform Connectors downgrades repeated health events before the fan-out to the datastore and Kubernetes
- **Transformers**: Transformer-specific configurations (MetadataAugmentor, OverrideTransformer)
- **Metadata Augmentor**: Configure node metadata enrichment, cache settings, and allowed labels
- **Override Transformer**: Define CEL-based rules that change the properties of health events
- **Kubernetes API Rate Limits**: Configure QPS and burst for Kubernetes API calls

For the complete configuration reference, see [Platform Connectors Configuration](configuration/platform-connectors.md).

### Kubernetes Authentication Modes

Platform Connectors supports two Kubernetes authentication modes:

- **In-cluster (default)**: Uses the pod ServiceAccount through `InClusterConfig()`
- **Out-of-cluster**: Uses an explicit kubeconfig file, set with `--kubeconfig=/path/to/kubeconfig`

The `--kubeconfig` flag is for host-managed deployments. For example, you can run `platform-connectors` under `systemd` together with the other runtime components. If you set the flag, both the Kubernetes connector and `MetadataAugmentor` use that kubeconfig.

Example host-managed invocation:

```bash
platform-connectors --socket=/var/run/nvsentinel.sock --config=/etc/config/config.json --kubeconfig=/var/lib/kubelet/kubeconfig
```

## What It Does

### Health Event Ingestion

Platform Connectors receives health events from all health monitors through gRPC:

- gpu-health-monitor (DCGM-based checks)
- syslog-health-monitor (log-based checks)
- csp-health-monitor (cloud provider events)
- kubernetes-object-monitor (resource-based checks)
- Any custom health monitors

### Event Transformation

Platform Connectors processes health events through a configurable transformer pipeline:

- **Metadata Augmentor**: Adds cloud provider IDs, topology labels and custom node labels
- **Override Transformer**: Applies CEL-based rules to change the severity and the recommended actions of health events
- **Extensible**: Supports custom transformers through a factory pattern
- Transformers run in the configured order, with non-blocking error handling

### Event Deduplication

Platform Connectors marks repeated health events for the configured checks as `STORE_AND_ANALYSE` within a configurable burst window. It does this before it sends the health events to the connectors. The deduplication key uses `nodeName`, `checkName`, sorted `entitiesImpacted`, sorted `errorCode`, `processingStrategy`, and `isHealthy`. Health events that differ only in their message do not create distinct faults.

The deduplicator uses `STORE_AND_ANALYSE`, not `STORE_ONLY`, on purpose. With `STORE_AND_ANALYSE`, health-events-analyzer still ingests the deduplicated health events for rule evaluation. As a result, repeated occurrences can together trigger a correlation rule that emits a new `EXECUTE_REMEDIATION` derived health event. With `STORE_ONLY`, health-events-analyzer does not get these health events at all.

### Data Persistence

Platform Connectors stores health events in the datastore:

- Atomic insertion with correct timestamps
- Keeps all metadata and transformations of the health event
- Triggers change streams for downstream modules

### Kubernetes Integration

Platform Connectors updates the cluster state based on health events:

- **Node Conditions**: Updates node conditions for fatal failures
- **Node Events**: Creates Kubernetes events for non-fatal problems
- Event correlation and deduplication

#### What the condition timestamps mean

If a condition does not exist, Platform Connectors always creates it. Platform Connectors rewrites an existing condition only when its status, reason or message changes. Both timestamps come from the health event that Platform Connectors processes, not from the time of the write. Different triggers move the two timestamps:

| Field | What it means |
|---|---|
| `status` | the state that the most recent health event reported when it wrote this condition. It is not proof that the fault is present or absent *at the current time* |
| `lastTransitionTime` | set when Platform Connectors creates the condition, and again each time `status` changes |
| `lastHeartbeatTime` | the timestamp of the health event that wrote the condition most recently. Despite its name, it is not a liveness signal |

A change of only the reason or the message therefore moves `lastHeartbeatTime` but not `lastTransitionTime`, and the two timestamps can differ. A check that reports healthy and stays healthy writes its condition one time. After that, neither timestamp moves again, even across a node reboot and a restart of every NVSentinel pod.

There is one exception. If a health event arrives with no `generatedTimestamp`, Platform Connectors stamps it with its arrival time before any other step sees it. Platform Connectors also logs a `HealthEvent has nil GeneratedTimestamp` warning. The condition timestamps then carry that arrival time. This time is close to the write time, not to the time when the check observed something. As a result, if a monitor omits the field, its condition timestamps look like write times, but they are not write times.

Because of this, a node condition tells you *what was last reported*. It never tells you *whether the check still runs*, and its age is not evidence in either direction. This matches how Kubernetes treats `lastHeartbeatTime` in general. Kubernetes uses the `Lease` API for node liveness. When nothing changes, `kubelet` posts the node status only every `nodeStatusReportFrequency` (5m by default). But this behavior means that the Node object carries no freshness for these conditions.

#### Checking that a check is still running

Use the metrics, not the Node object. No metric in this table proves that a component does useful work at this time. Each metric is a staleness signal. Before you alert on these metrics, learn what each one measures:

| Metric | What it actually reports |
|---|---|
| `up{namespace="nvsentinel", container="gpu-health-monitor"}` | Prometheus scrapes the pod, and the metrics endpoint of the pod answers. The same applies to the other monitors. The exact labels depend on your scrape configuration |
| `change_stream_lag_seconds{client="..."}` | the seconds since the consumer last had evidence that it was up to date. **An empty batch counts as that evidence.** Thus a rising value means that the loop is stuck or that it truly lags. A low value means that the loop ran recently and found nothing to do |
| `change_stream_lag_known` | only that the lag can be calculated at this point. Before the first empty batch or event read, `change_stream_lag_seconds` is absent, not zero. Thus a threshold rule alone cannot catch a consumer that never completed a first read |
| `datastore_connected` | PostgreSQL watcher readiness. The value is `1` while LISTEN is connected and subscribed, or after polling succeeds, unless a database read failed. After a failed read, it stays `0` until another read succeeds. With an active subscription, the watcher can report ready before its first poll finishes. It can also report ready while a catch-up of more than one batch continues. Readiness does not mean that the catch-up is complete. It also does not guarantee the replay of rows whose notifications the watcher missed before it subscribed. The watcher reads poll batches in `(changed_at, id)` order. The cursor advances only after the watcher delivers a row to the channel, or after the filter consumes the row. PostgreSQL assigns `BIGSERIAL` IDs and `changed_at DEFAULT NOW()` before a transaction commits, so a long-running transaction can become visible after a poll advanced past its sort position. A later catch-up cannot find that row from the forward cursor. If the watcher missed the NOTIFY of that row while it was disconnected, the watcher can skip the row. If LISTEN is active, the NOTIFY of the row can fetch it by ID and deliver it after newer rows. As a result, the watcher does not guarantee the global commit order. A TRIPPED fault-quarantine breaker stops event dequeuing independently, so this metric can stay `1` while the breaker stops the component on purpose. Other watchers stay ready after they register, unless they expose a readiness state. This metric does not measure the progress of event processing |

For the full set, see [METRICS.md](METRICS.md). A component that stopped silently keeps its pod `Running`, answers `/healthz` and never restarts. It also leaves its node conditions exactly as they were. Thus these metrics, read together and with attention to their limits, show the difference between a stopped component and a component with nothing to report.

## Event Processing Pipeline

The event processing pipeline processes health events before they reach the datastore or Kubernetes. Transformers run in a configurable order. Each transformer can change health events based on the enrichments from the transformers before it.

### Available Transformers

#### Metadata Augmentor

Adds node information from Kubernetes to health events:

- Cloud provider ID (AWS, GCP, Azure, OCI)
- Node labels (topology, instance type, custom labels)
- Caches metadata to minimize Kubernetes API calls

#### Override Transformer

Applies CEL-based rules to change the properties of health events:

- **isFatal**: Change whether NVSentinel treats an error as fatal
- **isHealthy**: Override the health status
- **recommendedAction**: Change the recommended remediation action

Use cases:

- Suppress known non-critical errors in your environment
- Change recommended actions during maintenance windows
- Apply different policies based on node labels

### Transformer Configuration

Helm values configure the transformers in these sections:

1. **pipeline** - defines which transformers run and in what order
2. **transformers** - contains transformer-specific configurations
3. **dedup** - configures the deduplication transformer that the chart appends

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

  dedup:
    enabled: true
    suppressionWindow: "3m"
    cleanupInterval: "60s"
    includeChecks:
      - SysLogsXIDError
      - SysLogsSXIDError
```

### Error Handling

Platform Connectors logs a warning for a transformer failure, but the failure does not block event processing. If a transformer fails, the health event still reaches the datastore and Kubernetes, with all transformations that succeeded. This keeps the system resilient: monitoring continues even if enrichment features fail.

## Key Features

### gRPC API

Platform Connectors exposes a standard gRPC interface that health monitors use to report health events. It uses protocol buffers for efficiency and type safety.

### Health Event Node Binding

Every health event names the node that it concerns. No downstream component derives that name again: fault-quarantine cordons the `nodeName` verbatim, and fault-remediation writes it onto the `RebootNode` CR. If a publisher names the wrong node, this mistake goes straight through to a cordon, a drain or a reboot of that node. For this reason, Platform Connectors is the one place that can check the claim against the publisher that makes it.

Two classes of publisher share the per-node Unix socket:

| Class | Components | Names other nodes? |
|-------|------------|--------------------|
| Node-local | gpu-health-monitor, syslog-health-monitor, nic-health-monitor, preflight checks, custom monitors | No. Always their own node |
| Cluster-scoped | csp-health-monitor, kubernetes-object-monitor, nvcre-certification-monitor, slurm-drain-monitor, health-events-analyzer, lifecycle-manager (MaintenanceRequest controller) | Yes, by design |

Every first-party monitor presents a credential. This credential is a projected Kubernetes ServiceAccount token, minted for a dedicated audience. Platform Connectors validates it with the TokenReview API. On Kubernetes 1.30+, the API server writes the **node** of the bound pod into the token when it issues the token. TokenReview reports this node back (`authentication.kubernetes.io/node-name`). As a result, the token carries a verified statement of *where its holder runs*, and the holder cannot change it.

The table gives the rule for each caller under `mode: enforce`, the default. Under `mode: audit`, Platform Connectors records each violation that the table describes and lets the request through.

| Caller presents | Decision |
|-----------------|----------|
| Valid token whose node claim names a **different** node | Platform Connectors rejects the request with `PermissionDenied` (`node_claim_mismatch`), for **every** caller, including allowlisted callers. A mismatch means that the caller presents the credential on a node other than the node for which the API server issued it. Cross-node reach is permission to *name* other nodes, not to *present the credential from* other nodes. |
| Valid token, ServiceAccount on the allowlist, claim matches | Platform Connectors accepts the supplied node names as sent. The token must also be bound to a running pod on a scheduled node, with both a pod UID and a node claim. A token minted with `kubectl create token` (no `--bound-object-ref`) has neither. Platform Connectors refuses this token outright, also for own-node events. |
| Valid token, any other ServiceAccount, claim matches | Platform Connectors scopes the caller to the node where Platform Connectors runs. It writes this node name into a blank `nodeName`. It rejects a *different* node name (`node_mismatch`). |
| No token, or a token without a node claim | Platform Connectors scopes the caller to the node where Platform Connectors runs, the same as a tokenless caller. A caller that can reach the socket already has this scope. Exception: Platform Connectors rejects a token without a node claim from an allowlisted ServiceAccount (`cross_node_claim_absent`, or `unbound_cross_node_token` if the token has no pod binding). |

Platform Connectors rejects a violating event and does not rewrite it. This is deliberate. A silent redirect of an event about node B onto node A turns a misdirected event into a real outage on the wrong node. It also hides the misconfiguration that caused the event.

**Missing node claims.** Below Kubernetes 1.30, or with the pod-node-info feature disabled, tokens carry no node claim. A node-local caller that presents such a token authenticates normally. Platform Connectors pins the caller to the node where Platform Connectors runs, which is the same scope that the caller gets with no token. As a result, node-local monitors continue to work with no change. Platform Connectors counts each such case in `platform_connector_auth_node_claim_total{result="absent"}`.

No setting rejects claimless tokens. An allowlisted caller already requires a claim (`cross_node_claim_absent`). For node-local callers, a refusal gains nothing, because a caller that reaches the socket already has that scope. NVSentinel requires Kubernetes 1.34, and pod-node info is GA in Kubernetes 1.32 and later. In practice, the token of every scheduled pod therefore carries a claim.

For **preflight checks**, this works without any allowlisting. The preflight webhook injects the check container into the customer's pod. The check container presents a token minted against the customer's own ServiceAccount, whatever that ServiceAccount is. Platform Connectors enforces only the node claim of the token.

Platform Connectors validates the whole batch before it forwards any part of it, so it accepts a batch in full or rejects it in full.

Platform Connectors keeps its availability in three ways: a cache of verified tokens, retries inside the call, and no TokenReview for a tokenless caller. It **caches verified tokens for a flat two minutes**, for every caller and not only for elevated callers. As a result, a steady-state publisher costs one TokenReview per token every two minutes, not one per batch. The cache TTL is deliberately not the expiry of the token. TokenReview is the only check that notices the deletion of the bound pod. Thus this window is how long the token of a deleted pod continues to work.

A cache TTL equal to `exp` makes this window the whole token lifetime, which is one hour by default. The accepted trade-off is that an authenticated caller stays accepted for up to two minutes after its token expires or its pod is deleted.

If the API server is unreachable, Platform Connectors **retries inside the call** with exponential backoff for a few seconds before it reports anything. After that time, Platform Connectors **fails closed** with `Unavailable` by default. Every publisher treats `Unavailable` as retryable.

`global.platformConnectorAuth.failOpenOnUnavailable: true` changes this behavior. The caller then gets a degraded node-local scope, and Platform Connectors does not reject it. An outage says nothing about the credential of the caller. In either `mode`, Platform Connectors accepts and stamps a blank or matching node name as usual.

With a degraded scope, the `mode` matters for an event that names a different node. Under `mode: enforce` (the default), Platform Connectors keeps the event retryable. It does not silently accept the event or count it as a mismatch. Under `mode: audit`, Platform Connectors records that same outcome and then lets the event through, like any other audited violation. As a result, it forwards the event as-is. For the detail, see [Authentication Configuration](configuration/authentication.md).

A tokenless caller never causes a TokenReview, so custom monitors continue to work with no dependency on the API server.

#### Configuration

All settings are under `global.platformConnectorAuth`, so the server-side allowlist and the projected tokens of the publishers stay consistent:

| Key | Default | Purpose |
|-----|---------|---------|
| `enabled` | `true` | Master switch, and a real boolean. A `null` or non-boolean value, such as a quoted `"false"`, fails the render. Platform Connectors does not start if the ConfigMap omits it. If you disable it, any caller can name any node. NVSentinel does not support this configuration in production. |
| `audience` | `platform-connector.nvsentinel.nvidia.com` | The audience for which the projected tokens are minted. TokenReview must echo it back. |
| `tokenExpirationSeconds` | `3600` | Token lifetime. It must be a whole number in `600 <= x <= 2^32`. Kubernetes rejects any other value. |
| `tokenMountPath` | `/var/run/secrets/nvsentinel/platform-connector` | Where publishers mount the token. |
| `crossNodeServiceAccounts` | `[]` | **Additional** fully-qualified usernames that can name other nodes. The chart derives the usernames of the enabled bundled monitors from the release namespace. Do not list them here. |
| `mode` | `enforce` | `enforce` rejects a violating request. `audit` validates every request and still increments `platform_connector_auth_violations_total` by reason, but lets the request through. Use `audit` to roll out node binding against real traffic. Confirm that the violation counters stay at zero. Then switch to `enforce` with evidence. One exception applies in both modes. Platform Connectors always rejects an event with no node name from a caller with verified cross-node scope, because no downstream component can handle it. This exception does not apply to a caller with a degraded scope under `failOpenOnUnavailable`. Platform Connectors never verified that caller as cross-node. |
| `failOpenOnUnavailable` | `false` | Must be a real YAML boolean. The chart fails the render for a quoted `"false"`. It does not read it as false. If `true` and the validator reaches no verdict (the API server is unreachable, or the call times out), the caller gets a degraded node-local scope. Platform Connectors then does not reject the request, because an outage says nothing about the credential of the caller. Under `mode: enforce`, a rejected credential (an invalid or malformed token) always causes a rejection, whatever the value of this setting. Under `mode: audit`, Platform Connectors records it and lets it through, like any other violation. |

There is no `requireNodeClaim` setting. You cannot configure whether Platform Connectors checks a claim. You can configure only what happens after Platform Connectors finds a violation (`mode`), and how it treats an unavailable validator (`failOpenOnUnavailable`). For the full detail on both, see [Authentication Configuration](configuration/authentication.md). That page also tells how a degraded scope treats a blank node name and a different node name.

A **custom cluster-scoped monitor** needs three things:

- a projected token volume with the configured audience
- its token path, passed to the client dial
- its fully-qualified username in `crossNodeServiceAccounts`

The monitor can be in any namespace. There is one list, and it always takes the complete `system:serviceaccount:<namespace>:<name>` form. A custom *node-local* monitor needs no changes, because it already reports its own node.

At the ConfigMap level (this applies only if you do not use the chart), a config that omits `enableNodeBindingAuth` **fails startup**. There is no default. A value that is neither `true` nor `false` also fails startup.

#### Where to run the cluster-scoped monitors

There are six cluster-scoped publishers:

- `csp-health-monitor`
- `kubernetes-object-monitor`
- `nvcre-certification-monitor`
- `slurm-drain-monitor`
- `health-events-analyzer`
- the MaintenanceRequest controller of `lifecycle-manager`

They are the only bundled identities that can name a node other than their own. All six ship **disabled by default** (`global.<monitor>.enabled: false`). For `lifecycle-manager`, both `global.lifecycleManager.enabled` and `lifecycle-manager.controllers.maintenanceRequest.enabled` are `false`. The chart derives the usernames of the enabled publishers from the release namespace. As a result, when you enable a publisher, you give it cross-node reach. This is a deliberate act, because that ServiceAccount can then have any node in the cluster cordoned, drained and rebooted.

Run them on nodes that do not also run tenant workloads. Use a system or control plane node pool, not a GPU node that serves tenants. The reason is the credential, not the code. Their pods hold a projected token that Platform Connectors accepts for any node name. Every container in the pod can read that token file. If you keep those pods away from tenant workloads, the one credential with cluster-wide reach stays off the machines where untrusted code runs.

To set this placement, use `nodeSelector`, `affinity` or tolerations for a tainted system pool, whichever your cluster already uses. Node-local monitors (gpu-health-monitor, syslog-health-monitor, nic-health-monitor and preflight checks) need no such placement. Their tokens attest the node that they run on and grant nothing more, so they are safe to run on all nodes.

#### Enabling node binding on a live cluster

Platform Connectors and the publishers are subcharts of one release. As a result, one `helm upgrade` moves them together, and you cannot upgrade one before the other. You can control *when enforcement starts*.

When you set `global.platformConnectorAuth.enabled` to `true` on a running release, the pods restart on their own schedule. As a result, there is a window in which a restarted Platform Connectors pod enforces node binding. In that window, it can see an old publisher pod that still sends no token.

That exposure is narrow. Platform Connectors does not reject a tokenless publisher outright. It pins the publisher to the publisher's own node, so there is no effect on node-local publishers in either case. The exposure affects only a *cluster-scoped* publisher, and only for the seconds between the restart of Platform Connectors and the replacement of the publisher pod. In that window, Platform Connectors rejects the cross-node events of the publisher with `node_mismatch`. The next attempt of the publisher succeeds.

To prevent rejections during the rollout, set `global.platformConnectorAuth.mode` to `audit` first. When the violation counters stay at zero, switch to `enforce`. See [`mode`](configuration/authentication.md#mode).

Watch the rollout with:

```text
sum(rate(platform_connector_auth_violations_total{reason="node_mismatch"}[5m]))
```

Related metrics:

| Metric | Labels | Meaning |
|--------|--------|---------|
| `platform_connector_auth_decisions_total` | `decision` | Batches by granted scope (`node_local`, `cross_node`) |
| `platform_connector_auth_violations_total` | `reason` | Batches that violated the node-binding rule. Under `mode: enforce`, Platform Connectors rejects them. Under `mode: audit`, it records them but lets them through. Exception: Platform Connectors always rejects a `missing_node_name` violation from a caller with verified cross-node scope. |
| `platform_connector_auth_node_claim_total` | `result` | Whether the token carried a node claim (`verified`, `absent`) |

A `reason` does not always mean that Platform Connectors rejected a caller. `validator_unavailable`, `validator_timeout` and `validator_error` mean that the validator reached no verdict. An unreachable API server increments them for every in-flight request. If you alert on suspected credential abuse, exclude these reasons. Otherwise, a short control-plane problem looks like an attack.

`validator_error` also counts a permanent TokenReview failure, such as a missing `create` permission on `tokenreviews`. Under `mode: enforce`, a `validator_error` always causes a rejection of the request. For the full list, see [METRICS.md](METRICS.md).

### Ring Buffer Architecture

Parallel processing of health events, with independent queues:

- Store connector queue for datastore writes
- Kubernetes connector queue for API updates
- A failure in one connector does not block the other

### Metadata Caching

Caches node metadata to reduce the load on the Kubernetes API:

- Configurable cache size and TTL
- Automatic cache invalidation
- Reduces the latency of event processing

### Resilient Processing

Built-in retry and error handling:

- Platform Connectors does not lose health events on transient failures
- Backpressure handling through ring buffers
- Detailed metrics for monitoring
