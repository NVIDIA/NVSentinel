# Architecture Decision Records

Each record states one decision: the context that forced it, what was decided, what it costs, and which alternatives were rejected and why. That reasoning is rarely recoverable from the code alone.

## Conventions

- The file name carries the number: `NNN-short-description.md`, and the record's `# ADR-NNN:` heading matches it.
- New records follow [template.md](template.md): Context, Decision, Implementation, Rationale, Consequences (Positive / Negative / Mitigations), Alternatives Considered, Notes, References.
- Take the next free number. **Never reuse a number**, even if a record was withdrawn — a reused number silently breaks every reference pointing at the original.
- Superseding a decision is normal; deleting the record is not. Leave the old record in place, mark it superseded at the top, and have the new record name what it supersedes in its References section. The decision trail is the point.
- **Do not edit an existing record to describe a new feature.** A record captures what was decided and why at the time it was written; rewriting it to cover later work destroys that history and leaves the rationale describing a system that no longer matches it. Build on a record instead: put user-facing behaviour in [docs/](../) and the configuration reference, and write a new record only when the change alters a decision an existing one made. Correcting a factual error or a broken link in an old record is fine.
- Numbers are allocated, not contiguous. Gaps are expected.

## Index

| ADR | Title |
|---|---|
| 001 | [Architecture — Health Event Detection Interface](001-health-event-detection-interface.md) |
| 002 | [Infrastructure — Storage Layer Selection](002-storage-layer-selection.md) |
| 003 | [Behavior — Rule-Based Node Quarantine](003-rule-based-node-quarantine.md) |
| 004 | [Behavior — Workload Eviction Strategies](004-workload-eviction-strategies.md) |
| 005 | [API — Kubernetes-Native Maintenance API](005-maintenance-api-design.md) |
| 006 | [Reliability — Platform Connector Event Buffering](006-platform-connector-reliability.md) |
| 007 | [Intelligence — Health Event Correlation](007-event-correlation-and-analysis.md) |
| 008 | [Integration — Cloud Provider Maintenance Events](008-cloud-provider-integration.md) |
| 009 | [Behavior — Fault Remediation Triggering](009-fault-remediation-triggering.md) |
| 010 | [Metadata Retrieval](010-metadata-retrieval.md) |
| 011 | [Kubernetes Object Monitor](011-kubernetes-object-monitor.md) |
| 012 | [Observability — Health Events Exporter](012-health-events-exporter.md) |
| 013 | [MongoDB Migration from Bitnami](013-mongodb-bitnami-migration.md) |
| 014 | [Implementation Plan: WORKFLOW_XID_13 and WORKFLOW_XID_31](014-workflow-XID-13-and-31.md) |
| 015 | [Behavior — Node Drain Extensibility](015-custom-drain-extensibility.md) |
| 016 | [Audit Logging for NVSentinel Write Operations](016-audit-logging.md) |
| 017 | [Architecture — Remediation Plugins](017-remediation-plugins.md) |
| 018 | [Syslog Health Monitor Support for Pre-installed Drivers](018-syslog-monitor-preinstalled-driver-support.md) |
| 019 | [Janitor Support for GPU Reset](019-janitor-gpu-reset.md) |
| 020 | [NVSentinel Support for GPU Reset](020-nvsentinel-gpu-reset.md) |
| 021 | [Configuration — Health Event Property Overrides](021-health-event-property-overrides.md) |
| 022 | [Circuit Breaker — Reset Mechanism](022-circuit-breaker-reset-mechanism.md) |
| 023 | [Architecture — Health Event Transformer Pipeline](023-health-event-transformer-pipeline.md) |
| 024 | [Implement WORKFLOW_NVLINK_ERR](024-workflow-nvlink-err.md) |
| 025 | [Health Event Processing Strategy](025-processing-strategy-for-health-checks.md) |
| 026 | [Feature — Preflight Checks](026-preflight-checks.md) |
| 027 | [Kubernetes Data Store (CRD) for HealthEvent](027-kubernetes-data-store.md) |
| 028 | [Janitor — Generic Bare-Metal Reboot Provider](028-generic-baremetal-reboot-provider.md) |
| 029 | [Slurm External Drain Health Monitor](029-slurm-external-drain-health-monitor.md) |
| 030 | [Security — gRPC TLS and Authentication for Janitor-Provider Connection](030-grpc-tls-authentication.md) |
| 031 | [OpenTelemetry Tracing for NVSentinel](031-OTEL-traces.md) |
| 032 | [Feature Flag Tracking via Metric](032-feature-flag-tracking.md) |
| 033 | [gRPC Sink Connector for Platform-Connectors](033-grpc-sink-connector.md) |
| 034 | [Feature — Per-Pod Preflight Check Selection](034-preflight-check-selection.md) |
| 035 | [Inline DCGM Config into Init Container](035-preflight-inline-dcgm-config.md) |
| 036 | [Data Model — Custom Remediation Actions](036-custom-remediation-actions.md) |
| 037 | [Janitor — TTL-Based Cleanup of Maintenance CRs](037-janitor-cr-ttl-cleanup.md) |
| 038 | [Health Monitors — Health Event Cancellation Rules](038-health-monitor-cancellation-rules.md) |
| 039 | [Platform Connector — Health Event Deduplication](039-health-event-deduplication.md) |
| 040 | [API — External Remediation Request (ERR)](040-external-remediation-request.md) |
| 041 | [Node Drainer — Priority Queue](041-node-drainer-priority-queue.md) |
| 042 | [GPU Health Monitor — GPU Thermal Margin (DCGM Field 153)](042-gpu-temp-limit-field-monitoring.md) |
| 043 | [Labeler - Expected Device Count Labels](043-expected-device-count-labels.md) |
| 044 | [GPU Health Monitor - DCGM Source Modes](044-dcgm-source-modes.md) |
| 045 | [NIC Health Monitor Design](045-nic-monitor-overview.md) |
| 046 | [NIC Health Monitor: Link Counter Detection](046-link-counter-detection.md) |
| 047 | [NIC Health Monitor: Link State Detection](047-link-state-detection.md) |
| 048 | [NIC Health Monitor: Syslog Detection](048-syslog-detection-correlation.md) |
| 049 | [Node Validation](049-node-validation.md) |
| 051 | [API — MaintenanceRequest (MR)](051-maintenance-request.md) |
| 052 | [Deployment Platform Connector](052-deployment-platform-connector.md) |
| 053 | [Monitoring — Integrate Default NPD Node Conditions](053-npd-checks-integration.md) |
| 054 | [`Observability` — change stream consumer lag metrics](054-changestream-lag-metrics.md) |
| 055 | [Health Monitors — NVCRE Certification Monitor](055-nvcre-certification-monitor.md) |
| 056 | [Health Events Analyzer — Derived-Condition Recovery](056-derived-condition-recovery.md) |
