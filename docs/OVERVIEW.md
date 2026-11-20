# NVSentinel Overview

## What it is

NVSentinel is a GPU fault detection and remediation system for Kubernetes. It monitors GPU health, system logs, and cloud provider maintenance events. Then it takes action: it cordons faulty nodes, drains workloads, and starts break-fix workflows. The full pipeline runs without human intervention.

NVSentinel runs on AWS, GCP, Azure, and OCI. The largest clusters that run NVSentinel have more than 1,100 nodes and approximately 40,000 GPUs. NVSentinel processes tens of millions of health events each month. NVIDIA internal deployments run with full remediation enabled by default.

## The problem

GPU faults in large clusters are expensive and disruptive. A single faulty GPU can silently corrupt training results, crash a multi-day distributed job, or leave hundreds of GPUs idle while an operator investigates. Traditional monitoring detects the problem but does not fix it. In the gap between detection and remediation, the cluster wastes GPU-hours and operators receive pages.

With NVSentinel, detection, quarantine, drain, and remediation occur automatically. They typically complete in minutes, compared to the hours or days that manual intervention requires.

## How it works

NVSentinel is a set of independent modules that coordinate through a shared datastore and the Kubernetes API. No module communicates directly with another.

**Health monitors** detect faults and send structured health events to the system:

- **GPU health monitor** watches for thermal issues, ECC errors, and XID events through DCGM
- **Syslog health monitor** parses system logs for kernel panics, driver crashes, and NVLink errors
- **CSP health monitor** polls cloud provider APIs (AWS, GCP, Azure, OCI, Akamai Cloud) for scheduled maintenance and hardware events
- **Kubernetes object monitor** evaluates CEL expressions against any Kubernetes resource to generate health events from custom signals

**Platform connectors** receive health events through gRPC, validate them, persist them to the datastore, and update Kubernetes node conditions.

**Preflight** (optional) uses a mutating webhook that injects GPU diagnostic init containers at pod admission. These containers find bad hardware before the workload starts. Each check publishes its result as a health event through platform connectors. If fault quarantine is enabled and the health event is fatal, a default fault quarantine ruleset cordons the node. Multi-node checks can use **gang discovery** (native Kubernetes gang APIs or PodGroup-style schedulers) and ConfigMap coordination. See [Preflight configuration](./configuration/preflight.md).

**Core modules** watch the datastore or the Kubernetes API, and act independently:

- **Fault quarantine** cordons nodes based on configurable CEL rules, with a circuit breaker that prevents mass quarantines during cluster-wide incidents
- **Node drainer** gracefully evicts workloads with per-namespace eviction strategies (immediate, allow-completion, or delete-after-timeout)
- **Fault remediation** creates maintenance CRs (GPU reset or node reboot) after the drain completes, and collects diagnostic logs
- **Janitor** executes the maintenance action through cloud provider APIs or direct node commands
- **Health events analyzer** identifies patterns across health events and generates recommended actions
- **Labeler** labels nodes with their DCGM and driver versions, so that other modules can configure themselves

## GPU reset

For recoverable GPU faults, NVSentinel can reset individual GPUs instead of rebooting the entire node. The GPU health monitor identifies reset-eligible errors. Then fault remediation creates a GPUReset CR, and the janitor executes it. A reset reduces the remediation time from minutes (reboot) to seconds (reset). The other GPUs on the node stay operational.

## Storage

NVSentinel supports MongoDB and PostgreSQL as datastore backends. Both provide change streams for real-time processing of health events. MongoDB uses native change streams. PostgreSQL uses LISTEN/NOTIFY, with polling as a fallback. NVSentinel persists all health events with full audit trails.

## GPU support

NVSentinel is validated on the following NVIDIA GPU architectures:

| Architecture | Example GPUs |
|---|---|
| Volta | V100 |
| Ampere | A100 |
| Hopper | H100, H200 |
| Ada Lovelace | L4 Tensor Core GPU, L40, L40S |
| Blackwell | B200, B300, GB200, GB300, RTX Pro 6000 |

NVSentinel is designed to work with any GPU that the NVIDIA GPU Operator supports. The architectures and GPUs that are not in the table above are not formally validated. However, they can work in your environment.

> **Note on nccl-loopback preflight check**: Most NVSentinel components do not compile GPU code. They work on all validated architectures. The optional nccl-loopback preflight check (NCCL bandwidth test) compiles GPU kernels. It targets only Ampere, Ada Lovelace, Hopper, and Blackwell. It does not support Volta (V100). On Volta nodes, disable or skip this check in the [preflight configuration](./configuration/preflight.md).

## Getting started

```bash
helm install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --version v1.32.0 \
  --namespace nvsentinel \
  --create-namespace
```

By default, NVSentinel enables only health monitoring. This default is safe to deploy in any cluster, because it only observes and reports. Use Helm values to enable fault quarantine, node drainer, and fault remediation. As your confidence in the behavior of NVSentinel in your environment increases, enable them.

**Preflight** is disabled by default. If you want admission-time GPU checks, enable it with `global.preflight.enabled` ([configuration guide](./configuration/preflight.md)).

For all options, see the Helm Chart Configuration Guide (`distros/kubernetes/README.md`). To see the full pipeline in a KIND cluster without GPU hardware, refer to the local fault injection demo (`demos/local-fault-injection-demo/README.md`).

## Security and supply chain

All container images are built with ko and attested with SLSA build provenance. They include SPDX SBOMs. You can verify the images in the cluster with Sigstore Policy Controller. For details, see `SECURITY.md` in the repository root.

## Learn more

- [Architecture and data flow](./DATA_FLOW.md)
- [Preflight](./configuration/preflight.md) for admission-time GPU checks and gang discovery
- [Integration guide](./INTEGRATIONS.md) for taints, node conditions, and custom remediation triggers
- [Metrics reference](./METRICS.md) for Prometheus dashboards and alerts
- Component configuration — see the Configuration section for the setup of each module
- Runbooks — see the Runbooks section for troubleshooting
- Development guide — see `DEVELOPMENT.md` in the repository root for information about how to contribute
