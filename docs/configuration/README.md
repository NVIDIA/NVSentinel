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
- [Platform Connectors](./platform-connectors.md)
- [Metadata Collector](./metadata-collector.md)
- [Labeler](./labeler.md)
- [Fault Quarantine](./fault-quarantine.md)
- [Node Drainer](./node-drainer.md)
- [Fault Remediation](./fault-remediation.md)
- [Preflight](./preflight.md)
- [Event Exporter](./event-exporter.md)
