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
health-events-analyzer, fault-quarantine, node-drainer etc).

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
- [Node Drainer](./node-drainer.md)
- [Fault Remediation](./fault-remediation.md)
- [Preflight](./preflight.md)
- [Event Exporter](./event-exporter.md)
