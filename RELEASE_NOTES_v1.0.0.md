# NVSentinel v1.0.0 Release Notes

**Status: Beta / Stable**

With v1.0.0, NVSentinel moves from Experimental to Beta/Stable. We now recommend NVSentinel for production testing and use. The project continues to evolve rapidly and APIs may change between releases, but we follow semantic versioning going forward: breaking changes will increment the major version.

## What's in v1.0.0

This release represents 13 prior releases and 400+ commits since the initial open-source launch in October 2025. The highlights below cover the full arc from v0.1.0 through v1.0.0.

### GPU reset and remediation pipeline

NVSentinel now supports a complete GPU reset workflow as an alternative to full node reboot. The GPU health monitor detects reset-eligible errors, fault remediation creates GPUReset CRDs, and the janitor executes the reset. This reduces remediation time from minutes (reboot) to seconds (reset) for recoverable GPU faults. End-to-end remediation metrics track the full pipeline from fault detection through resolution.

### Kubernetes object monitor

A new policy-based health monitor that watches any Kubernetes resource and evaluates CEL expressions to generate health events. This enables monitoring of custom resources, operator status, and application-level health signals without writing code.

### Event exporter

Health events can now be streamed to external systems in CloudEvents format. This enables integration with existing observability platforms and data pipelines.

### Preflight checks

A new preflight framework validates cluster readiness before GPU workloads are scheduled. Includes DCGM diagnostics and NCCL loopback/all-reduce tests to catch hardware issues before they affect production jobs.

### Slurm drain monitor

A new health monitor for hybrid Kubernetes/Slurm environments. Monitors Slurm drain state and generates health events when nodes are drained by the Slurm scheduler, enabling NVSentinel to coordinate remediation across both schedulers.

### Metadata collector

Automatically gathers GPU and NVSwitch topology information and enriches health events with hardware context. Integrated with both GPU and syslog health monitors.

### PostgreSQL backend

MongoDB is no longer the only storage option. PostgreSQL is now supported as an alternative database backend, with LISTEN/NOTIFY change streams for real-time event processing.

### Slinky (NVIDIA DPU) drain support

Custom drain integration for Slinky-managed nodes, including parallel drain handling and proper annotation coordination.

### NVLink and XID workflow improvements

Dedicated workflows for NVLink failures (XID 13, 31, 154) with GPU-topology-aware fault classification. The syslog health monitor now includes driver-version-dependent parsing for NVL5 decoding rules.

### Cloud provider improvements

- Bare-metal reboot support via sudo in janitor
- Generic CSP plugin with reboot capability
- Configurable IAM role names for EKS
- OCI, Azure, GCP, and AWS all supported with provider-specific janitor configurations

### Operational improvements

- Circuit breaker prevents mass quarantines during cluster-wide events
- Audit logging for all NVSentinel write operations
- Breakfix cancellation via manual uncordon
- Partial drain support in node drainer (per-namespace eviction strategies)
- Custom drain modes with parallel drain handling
- Log collection for diagnostic reports, including AWS SOS and GCP SOS report collection
- Optional TLS for MongoDB connections

### Build and security

- All container images built with ko and attested with SLSA build provenance
- SPDX SBOM attestation on every image
- Daily vulnerability scanning
- Supply chain verification via Sigstore Policy Controller

### Testing and quality

- End-to-end test coverage for all modules
- UAT tests validated on EKS and GKE (including GKE COS environments)
- Scale testing validated on clusters up to 1,100+ nodes
- Slinky drain integration in CI pipeline

## Production validation

NVSentinel is enabled by default with full remediation on NVIDIA internal deployments:

- **Clouds**: AWS, GCP, Azure, OCI, Forge, TogetherAI
- **Teams**: OSMO, NVCF, NIM, RIVA
- **Scale**: ~40,000 GPUs across clusters, largest cluster at 1,173 nodes
- **Volume**: 36,985,246 health events processed in 30 days

## Community

- 213 stars, 54 forks
- Notable external contributors:
  - Miguel Varela Ramos (FluidStack/Cohere) — TLS certificate management
  - Igor Velichkovich (Omniva) — controller-runtime refactor
  - Gyandeep Katiyar (IIIT Lucknow) — started as an intern, continued contributing after internship
  - Vincent Tan (FluidStack) — multi-template fault remediation
  - Avinash Yeddula (Omniva) — Kubernetes datastore
  - Xue Gangjie (echo.tech) — Helm config and syslog support

## Upgrading

```bash
helm upgrade nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --version v1.0.0 \
  --namespace nvsentinel
```

NVSentinel includes a pre-upgrade hook that cleans up deprecated node conditions automatically. Review the [Helm Chart Configuration Guide](distros/kubernetes/README.md) for new configuration options.

## What's next

See the [Roadmap](ROADMAP.md) and [Project Board](https://github.com/orgs/NVIDIA/projects/133/views/1) for planned work toward General Availability.
