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
- **Burst deduplication**: Marks repeated health events with the same fault identity as `STORE_AND_ANALYSE` before downstream fan-out
- **Decoupling**: Keeps health monitors independent from platform-specific implementations

Without Platform Connectors, health monitors would need to directly integrate with each platform's storage and APIs, creating tight coupling and complexity.

## How It Works

Platform Connectors typically runs as a deployment in the cluster:

1. Exposes gRPC service for health monitors to send events
2. Receives health events via gRPC (`HealthEventOccurredV1` API)
3. Processes events through the transformer pipeline:
   - **Metadata Augmentor**: Augments events with node metadata (cloud provider, labels, topology)
   - **Override Transformer**: Applies CEL-based rules to modify event properties
4. Runs deduplication as a transformer:
   - **Deduplicator**: Marks repeated events with the same node, check, impacted entities, error code, and health state as `STORE_AND_ANALYSE`
5. Queues events in ring buffers for parallel processing
6. Processes events through multiple connectors:
   - **Store Connector**: Persists events to the datastore
   - **Kubernetes Connector**: Updates node conditions and Kubernetes events
7. Each connector processes events independently for resilience

The event processing pipeline runs transformers in order, allowing each transformer to build on previous enrichments. The ring buffer architecture ensures events are processed reliably even under high load, with retry logic for transient failures.

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

- **Pipeline**: Configure transformer execution order and enable/disable individual transformers
- **Deduplication**: Configure repeated event downgrading before datastore/Kubernetes fan-out
- **Transformers**: Transformer-specific configurations (MetadataAugmentor, OverrideTransformer)
- **Metadata Augmentor**: Configure node metadata enrichment, cache settings, and allowed labels
- **Override Transformer**: Define CEL-based rules to modify event properties
- **Kubernetes API Rate Limits**: Configure QPS and burst for Kubernetes API calls

For complete configuration reference, see [Platform Connectors Configuration](configuration/platform-connectors.md).

### Kubernetes Authentication Modes

Platform Connectors supports two Kubernetes authentication modes:

- **In-cluster (default)**: Uses the pod ServiceAccount via `InClusterConfig()`
- **Out-of-cluster**: Uses an explicit kubeconfig file via `--kubeconfig=/path/to/kubeconfig`

The `--kubeconfig` flag is intended for host-managed deployments, such as running `platform-connectors` under `systemd` alongside the other runtime components. When set, both the Kubernetes connector and `MetadataAugmentor` use that kubeconfig.

Example host-managed invocation:
```bash
platform-connectors --socket=/var/run/nvsentinel.sock --config=/etc/config/config.json --kubeconfig=/var/lib/kubelet/kubeconfig
```

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

### Event Deduplication
Marks repeated events for configured checks as `STORE_AND_ANALYSE` within a configurable burst window before they are sent to connectors. The key uses `nodeName`, `checkName`, sorted `entitiesImpacted`, sorted `errorCode`, `processingStrategy`, and `isHealthy`; message-only variations do not create distinct faults.

`STORE_AND_ANALYSE` (not `STORE_ONLY`) is used intentionally: deduplicated events are still ingested by the Health Events Analyzer for rule evaluation, so repeated occurrences can collectively trigger a correlation rule that emits a new `EXECUTE_REMEDIATION` synthetic event. `STORE_ONLY` would suppress them from HEA entirely.

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

## Event Processing Pipeline

The event processing pipeline processes health events before they reach storage or Kubernetes. Transformers run in a configurable order, with each transformer able to modify events based on the enrichments from previous transformers.

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

Transformers are configured through Helm values with these sections:

1. **pipeline** - defines which transformers run and in what order
2. **transformers** - contains transformer-specific configurations
3. **dedup** - configures the deduplication transformer appended by the chart

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

Transformer failures log warnings but don't block event processing. If a transformer fails, the event still reaches storage and Kubernetes with whatever transformations were successfully applied. This ensures system resilience - monitoring continues even if enrichment features fail.

## Key Features

### gRPC API
Standard gRPC interface for health monitors to report events - protocol buffer-based for efficiency and type safety.

### Health Event Node Binding

Every health event names the node it concerns, and nothing downstream re-derives
that name: fault-quarantine cordons `nodeName` verbatim and fault-remediation
stamps it onto the `RebootNode` CR. A publisher that names the wrong node has
that carried straight through to a cordon, drain or reboot of that node, so
platform-connector is the one place the claim can be checked against the
publisher making it.

Two classes of publisher share the per-node Unix socket:

| Class | Components | Names other nodes? |
|-------|------------|--------------------|
| Node-local | gpu, syslog, nic, preflight checks, custom monitors | No — always their own node |
| Cluster-scoped | csp-health-monitor, kubernetes-object-monitor, slurm-drain-monitor, health-events-analyzer | Yes, by design |

Every first-party monitor presents a credential: a projected Kubernetes
ServiceAccount token minted for a dedicated audience, validated with the
TokenReview API — the same mechanism
[ADR-030](designs/030-grpc-tls-authentication.md) chose for the janitor to
janitor-provider channel. On Kubernetes 1.30+ the API server writes the bound
pod's **node** into the token at issuance and reports it back from TokenReview
(`authentication.kubernetes.io/node-name`), so the token carries a verified
statement of *where its holder runs* that the holder cannot alter.

The rule, per caller:

| Caller presents | Decision |
|-----------------|----------|
| Valid token whose node claim names a **different** node | Rejected with `PermissionDenied` (`node_claim_mismatch`), for **every** caller including allowlisted ones. A mismatch means the credential is being presented somewhere other than where it was issued. Cross-node reach is permission to *name* other nodes, not to *present the credential from* other nodes. |
| Valid token, ServiceAccount on the allowlist, claim matches | Supplied node names accepted as sent. The token must additionally be bound to a running pod on a scheduled node — both a pod UID and a node claim. A token minted with `kubectl create token` (no `--bound-object-ref`) has neither and is refused outright, for own-node events too. |
| Valid token, any other ServiceAccount, claim matches | Scoped to the connector's own node. A blank `nodeName` is filled in; a *different* one is rejected (`node_mismatch`). |
| No token, or a token without a node claim | Scoped to the connector's own node, exactly as a tokenless caller would be. Reaching the socket already granted that much. |

Rejecting rather than rewriting is deliberate: silently redirecting an event
about node B onto node A would turn a misdirected event into a real outage on
the wrong node, and would hide the misconfiguration that produced it.

**Missing node claims.** Below Kubernetes 1.30, or with the pod-node-info
feature disabled, tokens carry no node claim. A node-local caller presenting
such a token authenticates normally and is pinned to the connector's own node —
the same scope it would get with no token at all — so node-local monitors keep
working with no change. Every such case is counted in
`platform_connector_auth_node_claim_total{result="absent"}`.

There is no setting to reject claimless tokens: an allowlisted caller already
requires one (`cross_node_claim_absent`), and refusing them for node-local
callers would gain nothing, since reaching the socket already grants that scope.
NVSentinel requires Kubernetes 1.34 and pod-node info has been GA since 1.32, so
in practice every scheduled pod's token carries a claim.

For **preflight checks** this works without any allowlisting: the check
container is injected into the customer's pod and presents a token minted
against the customer's own ServiceAccount — whatever it is — and only the
token's node claim is enforced. The whole batch is validated before any of it
is forwarded, so a batch is either accepted in full or rejected in full.

Availability is preserved three ways. Verified tokens are **cached for a flat
two minutes**, for every caller rather than only elevated ones, so a steady-state
publisher costs one TokenReview per token per two minutes instead of one per
batch. The TTL is deliberately not the token's own expiry: TokenReview is the
only check that notices the bound pod being deleted, so this window is how long
a deleted pod's token keeps working, and honouring `exp` would stretch that to
the whole token lifetime — an hour by default. The accepted trade is that an
authenticated caller stays accepted for up to two minutes after its token
expires or its pod is deleted. An unreachable API server is
**retried inside the call** with exponential backoff for a few seconds before
anything is surfaced. Past that the connector **fails closed** with
`Unavailable`, which every publisher treats as retryable; tokenless callers
never trigger a TokenReview at all, so custom monitors keep working with no
API-server dependency whatsoever.

#### Configuration

All settings live under `global.platformConnectorAuth` so the server-side
allowlist and the publishers' projected tokens cannot drift apart:

| Key | Default | Purpose |
|-----|---------|---------|
| `enabled` | `true` | Master switch, and a real boolean — a quoted `"false"` is refused rather than read as true. Must be set explicitly; the connector fails to start if the ConfigMap omits it. Disabling lets any caller name any node; not supported in production. |
| `audience` | `platform-connector.nvsentinel.nvidia.com` | Audience the projected tokens are minted for and TokenReview must echo back. |
| `tokenExpirationSeconds` | `3600` | Token lifetime. Must be a whole number in `600 <= x <= 2^32`; Kubernetes rejects anything else. |
| `tokenMountPath` | `/var/run/secrets/nvsentinel/platform-connector` | Where publishers mount the token. |
| `crossNodeServiceAccounts` | `[]` | **Additional** fully-qualified usernames allowed to name other nodes. The bundled monitors are derived from the release namespace for whichever are enabled — do not list them here. |

There is no `mode` and no `requireNodeClaim`. A setting that is "on but
not enforcing" cannot be reasoned about from its configuration alone, so
enforcement is not separable from enablement.

A **custom cluster-scoped monitor** needs three things: a projected token volume
with the configured audience, its token path passed to the client dial, and its
fully-qualified username added to `crossNodeServiceAccounts` — in any namespace;
there is one list and it always takes the complete
`system:serviceaccount:<namespace>:<name>` form. A custom *node-local* monitor
needs no changes: it already reports its own node.

At the ConfigMap level (relevant only when bypassing the chart): a config that
omits `enableNodeBindingAuth` entirely **fails startup** — there is no default.
A ConfigMap without the key predates this version and is missing the audience
and allowlist too, so guessing the intended behaviour from it would be worse than
refusing. A value that is neither `true` nor `false` fails startup as well.

#### Where to run the cluster-scoped monitors

The four cluster-scoped publishers — `csp-health-monitor`,
`kubernetes-object-monitor`, `slurm-drain-monitor` and `health-events-analyzer` —
are the only bundled identities allowed to name a node other than their own, and
all four ship **disabled by default** (`global.<monitor>.enabled: false`). Their
usernames are derived from the release namespace for whichever are enabled, so
enabling one is what grants it cross-node reach — a deliberate act, because it
lets that ServiceAccount have any node in the cluster cordoned, drained and
rebooted.

Run them on nodes that do not also run user workloads — a system or control
plane node pool rather than a GPU node serving tenants. The reason is the
credential, not the code: their pods hold a projected token that platform-connector
accepts for any node name, and that token file is readable by every container in
the pod. Keeping those pods away from tenant workloads keeps the one credential
with cluster-wide reach off the machines where untrusted code runs.

Express it with `nodeSelector`, `affinity` or tolerations for a tainted system
pool, whichever your cluster already uses. Node-local monitors (gpu, syslog,
nic, preflight checks) need no such placement: their tokens attest the node
they run on and grant nothing beyond it, which is why they are safe to run
everywhere.

#### Staging a rolling upgrade

platform-connector and the publishers are subcharts of one release, so a single
`helm upgrade` moves them together and you cannot sequence one before the other.
What you can control is *when enforcement starts*.

Pods restart on their own schedule after an upgrade, so there is a window in
which a connector that has already restarted (and is enforcing) sees a publisher
pod that has not yet been replaced and is therefore still sending no token.

That exposure is narrow. A tokenless publisher is not rejected outright — it is
pinned to its own node, so node-local publishers are unaffected either way. Only
a *cluster-scoped* publisher is affected, and only for the seconds between the
connector restarting and its own pod being replaced: its cross-node events are
rejected with `node_mismatch` for that window, and its next attempt succeeds.

**If you need zero exposure**, upgrade in two steps rather than one:

```bash
# 1. New images everywhere, enforcement off. No token volumes are mounted and
#    no --platform-connector-token-path flag is passed.
helm upgrade nvsentinel <chart> -n nvsentinel --set global.platformConnectorAuth.enabled=false

# 2. Wait until every publisher pod is Running on the new image, then enable.
helm upgrade nvsentinel <chart> -n nvsentinel --set global.platformConnectorAuth.enabled=true
```

Step 2 still restarts the publishers to add their token volumes, but by then
every pod is on an image that knows how to send the token, so the only gap is
the pod restart itself.

Watch either approach with:

```text
sum(rate(platform_connector_auth_violations_total{reason="node_mismatch"}[5m]))
```

The other direction — old chart, new images — does not start at all: the
connector requires `enableNodeBindingAuth` to be present, and an old chart does
not write it.

Related metrics:

| Metric | Labels | Meaning |
|--------|--------|---------|
| `platform_connector_auth_decisions_total` | `decision` | Batches by granted scope (`node_local`, `cross_node`) |
| `platform_connector_auth_violations_total` | `reason` | Batches rejected by the interceptor |
| `platform_connector_auth_node_claim_total` | `result` | Whether the token carried a node claim (`verified`, `absent`) |

Not every `reason` means a caller was rejected. `validator_unavailable`,
`validator_timeout` and `validator_error` mean no verdict could be reached — an
unreachable API server increments them for every in-flight request. Exclude them
when alerting on suspected credential abuse, or a control-plane blip reads as an
attack. See [METRICS.md](METRICS.md) for the full list.

#### Chart and image versions must move together

The chart passes `--platform-connector-token-path` to each publisher when
`global.platformConnectorAuth.enabled` is true. That flag does not exist in
images built before this feature, and Go's flag parser rejects unknown flags at
startup, so a chart that is newer than the images it deploys puts those
publishers into `CrashLoopBackOff` with:

```text
flag provided but not defined: -platform-connector-token-path
```

This is a startup failure, not a health-event failure: the pod never serves. It
is also not limited to the connector — it hits every publisher the chart passes
the flag to.

Upgrade the images together with (or before) the chart. `global.image.tag` must
resolve to an image that contains this feature. If the chart has already been
rolled out ahead of the images, the fastest way back to a serving cluster is to
turn the feature off rather than to roll the chart back:

```bash
helm upgrade nvsentinel <chart> --reuse-values --set global.platformConnectorAuth.enabled=false
```

That drops the flag, the token volume and the connector's enforcement in one
step, leaving the pre-feature behavior. Re-enable it once the images are current.

The other direction, old chart with new images, does not start at all. The
connector requires `enableNodeBindingAuth` to be present in its ConfigMap and
refuses to start when it is absent, and an old chart does not write that key.
Roll the chart and the images together.



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
