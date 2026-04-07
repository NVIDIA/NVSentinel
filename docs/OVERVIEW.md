# NVSentinel Overview

## What it is

NVSentinel is a GPU fault detection and remediation system for Kubernetes. It monitors GPU health, system logs, and cloud provider maintenance events, then takes action: cordoning faulty nodes, draining workloads, and triggering break-fix workflows. The full pipeline runs without human intervention.

NVSentinel is running across AWS, GCP, Azure, and OCI on clusters up to 1,100+ nodes and ~40,000 GPUs, processing tens of millions of health events per month. NVIDIA internal deployments run with full remediation enabled by default.

## The problem

GPU failures in large clusters are expensive and disruptive. A single faulty GPU can corrupt training results silently, crash a multi-day distributed job, or leave hundreds of GPUs idle while an operator investigates. Traditional monitoring detects the problem but does not fix it. The gap between detection and remediation is where GPU-hours are wasted and operators get paged.

NVSentinel closes that gap. Detection, quarantine, drain, and remediation happen automatically, typically completing in minutes rather than the hours or days that manual intervention requires.

## How it works

NVSentinel is built as a set of independent modules that coordinate through a shared data store and the Kubernetes API. No module communicates directly with another.

**Health monitors** detect faults and send structured health events to the system:

- **GPU health monitor** watches for thermal issues, ECC errors, and XID events via DCGM
- **Syslog health monitor** parses system logs for kernel panics, driver crashes, and NVLink errors
- **CSP health monitor** polls cloud provider APIs (AWS, GCP, Azure, OCI) for scheduled maintenance and hardware events
- **Kubernetes object monitor** evaluates CEL expressions against any Kubernetes resource to generate health events from custom signals

**Platform connectors** receive health events via gRPC, validate them, persist them to the data store, and update Kubernetes node conditions.

**Preflight** (optional) is not part of the health-event pipeline. A mutating webhook injects GPU diagnostic init containers at pod admission so bad hardware is caught before the workload starts. Multi-node checks can use **gang discovery** (native Workload API or PodGroup-style schedulers) and ConfigMap coordination. See [Preflight configuration](./configuration/preflight.md) and [ADR-026](./designs/026-preflight-checks.md).

**Core modules** watch the data store for new events and act independently:

- **Fault quarantine** cordons nodes based on configurable CEL rules, with a circuit breaker to prevent mass quarantines during cluster-wide events
- **Node drainer** gracefully evicts workloads with per-namespace eviction strategies (immediate, allow-completion, or delete-after-timeout)
- **Fault remediation** creates maintenance CRDs (GPU reset or node reboot) after drain completes, and collects diagnostic logs
- **Janitor** executes the maintenance action via cloud provider APIs or direct node commands
- **Health events analyzer** identifies patterns across events and generates recommended actions
- **Labeler** tags nodes with DCGM and driver versions so other modules can self-configure

## GPU reset

For recoverable GPU faults, NVSentinel can reset individual GPUs instead of rebooting the entire node. The GPU health monitor identifies reset-eligible errors, fault remediation creates a GPUReset CRD, and the janitor executes it. This reduces remediation time from minutes (reboot) to seconds (reset) while keeping other GPUs on the node operational.

## Storage

NVSentinel supports MongoDB and PostgreSQL as database backends. Both provide change streams for real-time event processing. MongoDB uses native change streams. PostgreSQL uses LISTEN/NOTIFY. All health events are persisted with full audit trails.

## Getting started

```bash
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --version v1.0.0 \
  --namespace nvsentinel \
  --create-namespace
```

By default, only health monitoring is enabled. This is safe to deploy in any cluster as it only observes and reports. Enable fault quarantine, node drainer, and fault remediation via Helm values as you build confidence in the system's behavior in your environment. **Preflight** is disabled by default; turn it on with `global.preflight.enabled` when you want admission-time GPU checks ([configuration guide](./configuration/preflight.md)).

See the [Helm Chart Configuration Guide](../distros/kubernetes/README.md) for all options, and the [local fault injection demo](../demos/local-fault-injection-demo/README.md) to see the full pipeline in a KIND cluster without GPU hardware.

## Security and supply chain

All container images are built with ko, attested with SLSA build provenance, and include SPDX SBOMs. In-cluster verification is supported via Sigstore Policy Controller. See [Security](../SECURITY.md) for details.

## Learn more

- [Architecture and data flow](./DATA_FLOW.md)
- [Preflight](./configuration/preflight.md) for admission-time GPU checks and gang discovery
- [Integration guide](./INTEGRATIONS.md) for taints, node conditions, and custom remediation triggers
- [Metrics reference](./METRICS.md) for Prometheus dashboards and alerts
- [Component configuration](./configuration/) for per-module setup
- [Runbooks](./runbooks/) for troubleshooting
- [Development guide](../DEVELOPMENT.md) for contributing
