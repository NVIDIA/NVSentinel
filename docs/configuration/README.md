# NVSentinel Configuration Documentation

This directory contains technical configuration guides for NVSentinel operators and system administrators.

## Global Configuration

Global settings apply across all NVSentinel modules and are configured under the `global:` section in the Helm values.

### Image Configuration

Image tag for all NVSentinel modules.

```yaml
global:
  image:
    tag: "main"
```

### Init Container Image

Shell image for the helper init containers that NVSentinel modules run before startup: `fix-audit-log-permissions` sets ownership on the audit log directory, and `fix-cert-permissions` copies PostgreSQL client certificates with the permissions the client needs. A module runs them only when [audit logging](../audit-logging.md) is enabled or a client certificate is mounted.

```yaml
global:
  initContainerImage:
    repository: docker.io/bitnamilegacy/os-shell
    tag: "12-debian-12-r30"
    pullPolicy: IfNotPresent
```

Change this when you mirror images into a private registry.

### Dry Run Mode

Run all modules in dry-run mode where actions are logged but not executed.

```yaml
global:
  dryRun: false
```

### Metrics Port

Prometheus metrics port used by all modules.

```yaml
global:
  metricsPort: 2112
```

### Change Stream Resume Tokens

Watcher-based components persist change stream resume tokens so they can resume from the last processed event after a restart. To skip accumulated events and start from the current stream head, scale the component to zero, patch its key in the runtime resume-control ConfigMap from `RESUME` to `CREATE`, then restore its replicas. The component deletes only its own resume token, records a cold-start cutoff timestamp, skips startup cold-start recovery for that run, opens its watcher from the current stream head, and writes its key back to `RESUME`. Future restarts still run cold-start recovery, but only for records newer than the recorded cutoff.

Helm does not create the resume-control ConfigMap. Components create it at runtime if it is missing, so GitOps tools such as Argo CD do not revert operator patches to its data.
When a component starts and its key is missing, it writes its key as `RESUME`; the ConfigMap therefore self-populates with explicit per-component state over time.

Example one-shot reset for node-drainer:

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment node-drainer -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/node-drainer --replicas=0
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"node-drainer":"CREATE"}}'
kubectl -n nvsentinel scale deployment/node-drainer --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/node-drainer --timeout=180s
```

This applies to `fault-quarantine`, `node-drainer`, `fault-remediation`, and `health-events-analyzer`.

### Node Scheduling

Control where NVSentinel pods are scheduled.

```yaml
global:
  # For GPU-bound pods (health monitors, metadata collector)
  nodeSelector: {}
  tolerations: []
  affinity: {}
  
  # For system pods (fault-quarantine, node-drainer etc)
  systemNodeSelector: {}
  systemNodeTolerations: []
```

### Pod Priority

NVSentinel pods set no `priorityClassName` by default, so each takes the priority of the
cluster's `globalDefault` PriorityClass if one is configured, and 0 otherwise. A pod can
preempt another only when its own priority is higher, so on a saturated cluster the
scheduler leaves these pods Pending rather than preempting a lower-priority workload, and a
node can end up with no health monitor while the workload still reports healthy.

Assign a priority class to avoid that. The split matches the node-scheduling values above:
`priorityClassName` covers the node-level agents (the health monitor, metadata collector
and NIC health monitor DaemonSets, the preflight image cache, plus platform-connectors),
`systemPriorityClassName` covers the control-plane components (labeler,
health-events-analyzer, fault-quarantine, node-drainer etc). Both apply only to the
components NVSentinel's own charts render — see [Scope](#scope-nvsentinel-components-only)
below for the datastore, which needs its own key.

```yaml
global:
  priorityClassName: ""        # node-level agents (DaemonSets)
  systemPriorityClassName: ""  # control-plane components (Deployments)
```

Both default to empty, which leaves the field off the pod spec entirely and preserves the
existing behaviour. Any component can also be set individually, and the global takes
precedence when both are set, consistent with how `tolerations` behaves:

```yaml
gpu-health-monitor:
  priorityClassName: my-gpu-agent-priority
```

The priority classes must already exist in the cluster. `system-node-critical` and
`system-cluster-critical` are built in; anything else has to be created first.

A higher priority is necessary but not sufficient: preemption also needs an evictable
lower-priority pod on a node that would then fit, and a class with `preemptionPolicy: Never`
only improves queue order without evicting anything. Both built-in classes above preempt.

#### Scope: NVSentinel components only

Both globals cover the components NVSentinel's own charts render. **They do not reach the datastore.** The MongoDB and PostgreSQL pods come from vendored upstream charts that never read NVSentinel's `global` values, so setting the globals and rendering the release leaves those StatefulSets with no `priorityClassName`.

This matters because the datastore is the component whose eviction hurts most: every module reconciles from it, so a preempted database stops fault detection across the whole cluster while the health monitors it starved keep their own high priority.

Set the priority on the datastore through the upstream chart's own key:

| Datastore | Key |
|---|---|
| Bitnami MongoDB | `mongodb-store.mongodb.priorityClassName` |
| Percona (PSMDB) | `mongodb-store.psmdb-db.replsets.rs0.priorityClass` |
| Bitnami PostgreSQL | `postgresql.primary.priorityClassName` |

```yaml
global:
  priorityClassName: system-node-critical
  systemPriorityClassName: system-cluster-critical

# The datastore needs its own key — the globals above do not apply to it.
mongodb-store:
  mongodb:
    priorityClassName: system-cluster-critical
```

Note that Percona spells it `priorityClass`, without `Name`, and that its key sits under the replica set. The Bitnami MongoDB chart takes separate keys for the arbiter and hidden members, `mongodb-store.mongodb.arbiter.priorityClassName` and `mongodb-store.mongodb.hidden.priorityClassName`, if you run them.

The `create-mongodb-database` initialization Job accepts no priority class from any key. It runs once at install or upgrade and then completes, so it is scheduled against whatever capacity is free at that moment. On a saturated cluster this Job can stay Pending and hold up the install.

### Image Pull Secrets

Credentials for pulling images from private registries.

```yaml
global:
  imagePullSecrets: []
```

### Tracing

Enable OpenTelemetry distributed tracing to get end-to-end visibility into health event processing across all modules.

```yaml
global:
  tracing:
    enabled: false       # Enable/disable tracing for all components
    endpoint: ""         # OTLP gRPC address of your OpenTelemetry Collector (e.g., "alloy.observability.svc.cluster.local:4317")
    insecure: true       # Set to false if the collector endpoint uses TLS
```

For full details, see [Distributed Tracing](../tracing.md).

### Audit logging

Enable file-based audit logs of HTTP write operations (POST, PUT, PATCH, DELETE) to the Kubernetes and CSP APIs, with rotation and optional request-body capture.

```yaml
global:
  auditLogging:
    enabled: true
    logRequestBody: false
    maxSizeMB: 100
    maxBackups: 7
    maxAgeDays: 30
    compress: true
```

For full details, see [Audit Logging](../audit-logging.md).

### Shared Metadata Path

File path where the metadata collector writes GPU metadata and the syslog health monitor reads it. Both components must agree on this path.

```yaml
global:
  metadataPath: /var/lib/nvsentinel/gpu_metadata.json
```

### Client Certificate Rotation

Lets modules pick up rotated datastore client certificates without a pod restart. A file watcher detects the change and supplies the new certificate to subsequent MongoDB connections. Existing connections keep the certificate they opened with.

```yaml
global:
  certificateRotationEnabled: false
```

### Kubernetes Datastore CRDs

Installs the `HealthEvent` custom resource definition used by the Kubernetes datastore backend. Enable it only when you run that backend instead of MongoDB or PostgreSQL.

```yaml
global:
  k8sdatastoreCrds:
    enabled: false
```

### Node Condition Cleanup

Removes node conditions that NVSentinel no longer writes. An upgrade that renames or retires a condition leaves the old one on every node, where it stays `True` forever because nothing updates it any more. The cleanup hook deletes the conditions you name.

```yaml
nodeConditionCleanup:
  enabled: false
  deprecatedConditions: []
  # - "OldConditionType1"
  # - "OldConditionType2"
```

The hook runs as a Helm `post-upgrade` job, so it acts on upgrade rather than on a fresh install, and it takes no action while `deprecatedConditions` is empty. List only conditions you are certain are retired — deleting a live condition discards a node's current health state until the monitor writes it again.

### PodMonitor

Creates a Prometheus Operator `PodMonitor` for every NVSentinel component. Disable it if you scrape with annotation-based discovery instead.

```yaml
podMonitor:
  enabled: true
  interval: 30s        # Scrape interval
  # scrapeTimeout: ""  # Defaults to the Prometheus default
  # metricsPath: "/metrics"
  # labels: {}         # Extra labels on the PodMonitor resource
```

See [Metrics Reference](../METRICS.md) for what each component exposes.

### Platform Connectors Update Strategy

Rolling update strategy for the platform-connectors DaemonSet. `type` accepts `RollingUpdate` or `OnDelete`; `rollingUpdate` is rendered only for `RollingUpdate`.

```yaml
updateStrategy:
  type: RollingUpdate
  rollingUpdate:
    maxUnavailable: 5%
```

`maxUnavailable` accepts a percentage or an absolute count — `maxUnavailable: 34` updates 34 nodes at a time. A larger value drains the fleet's health reporting faster during an upgrade; a smaller one keeps more nodes reporting while the rollout runs.

## Common Module Settings

Several modules expose the same Kubernetes settings. They behave the same everywhere, so each module's own guide documents only the options specific to it.

| Setting | Modules | Purpose |
|---|---|---|
| `replicaCount` | Every Deployment-based module | Number of pod replicas. Every module defaults to `1`. `fault-remediation`, `janitor` and `lifecycle-manager` elect a leader, so further replicas stand by. The event-store consumers have no such guard and would process the same events twice — raise the count only where the module's own guide says it is safe |
| `resources` | All | CPU and memory requests and limits |
| `logLevel` | All | `debug`, `info`, `warn`, or `error` |
| `nodeSelector`, `tolerations`, `affinity` | All | Pod placement. `global.nodeSelector` and `global.systemNodeSelector` set the defaults — see [Node Scheduling](#node-scheduling) |
| `livenessProbe`, `readinessProbe` | `gpu-health-monitor`, `janitor-provider`, `preflight` | Standard Kubernetes probe fields. Raise `initialDelaySeconds` when a component starts slowly on a loaded node. The janitor chart carries these keys but does not apply them — see [Inactive Values](./janitor.md#inactive-values) |
| `updateStrategy` | The DaemonSets: `gpu-health-monitor`, `metadata-collector`, `nic-health-monitor`, `syslog-health-monitor` | Rolling update behavior, same shape as [Platform Connectors Update Strategy](#platform-connectors-update-strategy) |
| `extraEnv` | `janitor`, `janitor-provider`, `lifecycle-manager`, `metadata-collector` | Extra environment variables on the module container, as a standard `env` list |
| `podSecurityContext`, `containerSecurityContext`, `securityContext` | `incluster-file-server`, `preflight` | Pod-level and container-level security context. The defaults run as non-root with a read-only root filesystem; loosen them only for a documented need. The `janitor` and `lifecycle-manager` charts carry these keys but do not apply them |
| `clientCertMountPath` | `csp-health-monitor`, `fault-quarantine`, `fault-remediation`, `health-events-analyzer`, `node-drainer` | Where the datastore client certificate is mounted. Must match the path the datastore chart issues certificates for |
| `volumes` | `kubernetes-object-monitor`, `nvcre-certification-monitor`, `slurm-drain-monitor` | Extra pod volumes, as a standard Kubernetes volume list |

## Module-Specific Configuration

Each module has additional configuration options documented in its dedicated guide:

- [GPU Health Monitor](./gpu-health-monitor.md)
- [Syslog Health Monitor](./syslog-health-monitor.md)
- [CSP Health Monitor](./csp-health-monitor.md)
- [Kubernetes Object Monitor](./kubernetes-object-monitor.md)
- [NVCRE Certification Monitor](./nvcre-certification-monitor.md)
- [Platform Connectors](./platform-connectors.md)
- [Metadata Collector](./metadata-collector.md)
- [Labeler](./labeler.md)
- [Fault Quarantine](./fault-quarantine.md)
- [Lifecycle Manager](./lifecycle-manager.md)
- [Node Drainer](./node-drainer.md)
- [Fault Remediation](./fault-remediation.md)
- [Preflight](./preflight.md)
- [Event Exporter](./event-exporter.md)
