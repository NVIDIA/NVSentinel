# Health Events Analyzer Configuration

## Overview

The Health Events Analyzer evaluates MongoDB aggregation pipeline rules against incoming health events and emits derived events with recommended actions (for example, repeated XID 48 errors on the same GPU within 24 hours triggers a `CONTACT_SUPPORT` recommendation). This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the health-events-analyzer module is deployed in the cluster.

```yaml
global:
  healthEventsAnalyzer:
    enabled: false
```

### Replica Count

Number of health-events-analyzer pod replicas to run.

```yaml
health-events-analyzer:
  replicaCount: 1
```

### Resources

Defines CPU and memory resource requests and limits for the health-events-analyzer pod.

```yaml
health-events-analyzer:
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "1"
      memory: "1Gi"
```

### Logging

Sets the verbosity level for health-events-analyzer logs.

```yaml
health-events-analyzer:
  logLevel: info  # Options: debug, info, warn, error
```

### Processing Strategy

Controls whether derived events trigger downstream remediation actions or are recorded only.

```yaml
health-events-analyzer:
  processingStrategy: EXECUTE_REMEDIATION  # Options: EXECUTE_REMEDIATION, STORE_AND_ANALYSE, STORE_ONLY
```

**Options:**

#### EXECUTE_REMEDIATION
Normal operating mode. When a rule fires, downstream modules may update cluster state — applying node conditions, quarantining nodes, draining workloads, or triggering remediations.

#### STORE_AND_ANALYSE
Events are persisted and ingested by the Health Events Analyzer for rule evaluation. The individual event does not directly trigger quarantine or remediation, but HEA rules can still match on it and emit new `EXECUTE_REMEDIATION` synthetic events as a result — for example, a burst of deduplicated `STORE_AND_ANALYSE` events may collectively satisfy a repeated-failure rule. Use this when you want the raw event suppressed from immediate action but still want HEA correlation to fire.

#### STORE_ONLY
Observability-only mode. Derived events are persisted and exported but do not modify any cluster resources. Use this mode to shadow-test new or customised rules in production before enabling full remediation.

### Client Certificate Mount Path

Path inside the container where TLS client certificates are mounted for authenticated MongoDB connections. Certificates are typically provisioned by cert-manager and mounted via a Kubernetes secret volume.

```yaml
health-events-analyzer:
  clientCertMountPath: /etc/ssl/client-certs
```

### Rule Enable/Disable Flags

Each built-in rule can be independently enabled or disabled. All rules are enabled by default. Set a flag to `false` to suppress a rule without removing it from the configuration.

```yaml
health-events-analyzer:
  enableMultipleRemediationsRule: true
  enableRepeatedXIDErrorOnSameGPURule: true
  enableRepeatedXID31OnSameGPURule: true
  enableRepeatedXID31OnDifferentGPURule: true
  enableRepeatedXID13OnSameGPCAndTPCRule: true
  enableRepeatedXID13OnDifferentGPCAndTPCRule: true
  enableXIDErrorSoloNoBurstRule: true
  enableXID74Reg0SoloNVLinkErrorRule: true
  enableXID74Reg0ECCParityErrorRule: true
  enableRepeatedXID74Reg0HardwareIssueRule: true
  enableXID74Reg0SignalIntegrityErrorRule: true
  enableXID74Reg0Bit27Or29SetRule: true
  enableRepeatedXID74Reg2HardwareIssueRule: true
  enableXID74Reg2Bit13SetRule: true
  enableRepeatedXID74Reg2Bit16Or19SetRule: true
  enableRepeatedXID74Reg2Bit17Or18SetRule: true
  enableXID74Reg3UnexpectedErrorRule: true
  enableXID74Reg3Bit18SetRule: true
  enableXID74Reg4HardwareIssueRule: true
  enableXID74Reg4ECCErrorRule: true
  enableRepeatedNICDriverErrorRule: true
  enableRepeatedNICDegradationRule: true
```

The following table summarises every flag, the XID or event type it covers, and the recommended action emitted when the rule fires.

| Flag | Rule | Recommended Action | Description |
|------|------|--------------------|-------------|
| `enableMultipleRemediationsRule` | MultipleRemediations | `CONTACT_SUPPORT` | 5 or more remediations on the same node within 7 days |
| `enableRepeatedXIDErrorOnSameGPURule` | RepeatedXIDErrorOnSameGPU | `CONTACT_SUPPORT` | Any XID except 31 and 45 five or more times within 24 hours on the same GPU (burst window 3 min, sticky XID window 3 h) |
| `enableRepeatedXID31OnSameGPURule` | RepeatedXID31OnSameGPU | `RUN_DCGMEUD` | XID 31 two or more times on the same GPU within 24 hours |
| `enableRepeatedXID31OnDifferentGPURule` | RepeatedXID31OnDifferentGPU | `NONE` | XID 31 on two or more different GPUs within 24 hours |
| `enableRepeatedXID13OnSameGPCAndTPCRule` | RepeatedXID13OnSameGPCAndTPC | `RUN_DCGMEUD` | XID 13 two or more times on the same GPC and TPC within 24 hours |
| `enableRepeatedXID13OnDifferentGPCAndTPCRule` | RepeatedXID13OnDifferentGPCAndTPC | `NONE` | XID 13 on two or more different GPC/TPC combinations within 24 hours |
| `enableXIDErrorSoloNoBurstRule` | XIDErrorSoloNoBurst | `NONE` | XID 13 or 31 appeared exactly once in the most recent burst within 24 hours |
| `enableXID74Reg0SoloNVLinkErrorRule` | XID74Reg0SoloNVLinkError | `CONTACT_SUPPORT` | XID 74 with REG0 bit 1 or 20 set, no other active errors on the same GPU |
| `enableXID74Reg0ECCParityErrorRule` | XID74Reg0ECCParityError | `CONTACT_SUPPORT` | XID 74 with REG0 bit 4 or 5 set, two or more times on the same NVLink and GPU within 24 hours |
| `enableRepeatedXID74Reg0HardwareIssueRule` | RepeatedXID74Reg0HardwareIssue | `CONTACT_SUPPORT` | XID 74 with REG0 bits 8, 9, 12, 16, 17, 24, or 28 set, two or more times on same GPU within 24 hours |
| `enableXID74Reg0SignalIntegrityErrorRule` | XID74Reg0SignalIntegrityError | `CONTACT_SUPPORT` | XID 74 with REG0 bit 21 or 22 set, no other active errors on the same GPU |
| `enableXID74Reg0Bit27Or29SetRule` | XID74Reg0Bit27Or29Set | `CONTACT_SUPPORT` | XID 74 with REG0 bit 27 or 29 set, two or more times on same GPU within 24 hours |
| `enableRepeatedXID74Reg2HardwareIssueRule` | RepeatedXID74Reg2HardwareIssue | `CONTACT_SUPPORT` | XID 74 with REG2 bits 0, 1, 2, or 6 set, two or more times on same NVLink and GPU within 24 hours |
| `enableXID74Reg2Bit13SetRule` | XID74Reg2Bit13Set | `CONTACT_SUPPORT` | XID 74 with REG2 bit 13 set |
| `enableRepeatedXID74Reg2Bit16Or19SetRule` | RepeatedXID74Reg2Bit16Or19Set | `CONTACT_SUPPORT` | XID 74 with REG2 bit 16 or 19 set, two or more times within 24 hours |
| `enableRepeatedXID74Reg2Bit17Or18SetRule` | RepeatedXID74Reg2Bit17Or18Set | `CONTACT_SUPPORT` | XID 74 with REG2 bit 17 or 18 set, two or more times within 24 hours |
| `enableXID74Reg3UnexpectedErrorRule` | XID74Reg3UnexpectedError | `CONTACT_SUPPORT` | XID 74 with an unexpected REG3 value |
| `enableXID74Reg3Bit18SetRule` | XID74Reg3Bit18Set | `CONTACT_SUPPORT` | XID 74 with REG3 bit 18 set |
| `enableXID74Reg4HardwareIssueRule` | XID74Reg4HardwareIssue | `CONTACT_SUPPORT` | XID 74 with a hardware-indicating REG4 value |
| `enableXID74Reg4ECCErrorRule` | XID74Reg4ECCError | `CONTACT_SUPPORT` | XID 74 with an ECC-indicating REG4 value |
| `enableRepeatedNICDriverErrorRule` | RepeatedNICDriverError | `CONTACT_SUPPORT` | Non-fatal NIC driver kernel-log pattern (e.g. TX/RX timeout, NAPI lockup) three or more times on the same node within 1 hour |
| `enableRepeatedNICDegradationRule` | RepeatedNICDegradation | `CONTACT_SUPPORT` | Non-fatal NIC counter degradation three or more times on the same NIC port within 1 hour |

## Rule Configuration

### Rule Structure

Rules are defined as TOML in the `config:` block of `values.yaml`. Each rule entry has the following fields:

```toml
[[rules]]
name        = "RuleName"
description = "Human-readable description"
recommended_action = "CONTACT_SUPPORT"   # or RUN_DCGMEUD, NONE, etc.
evaluate_rule = true                     # Helm template expression; maps to the enable flag
stage = [
  '{ "$match": { ... } }',              # MongoDB aggregation pipeline stages as JSON strings
  '{ "$count": "count" }',
  '{ "$match": { "count": { "$gte": N } } }'
]
```

The full default ruleset — including all aggregation pipeline stage definitions — is in the chart's `values.yaml` at `distros/kubernetes/nvsentinel/charts/health-events-analyzer/values.yaml`. Refer to that file when writing or reviewing custom rules.

### MultipleRemediations Rule

The `MultipleRemediations` rule fires when five or more remediations have been performed on the same node within the preceding 7 days. Unlike other rules, **it applies a node condition that NVSentinel does not automatically clear**, because the rule does not emit healthy events.

After the underlying hardware issue is resolved, remove the condition manually:

```bash
kubectl get node {NODE_NAME} -o json \
  | jq '.status.conditions |= map(select(.type != "MultipleRemediations"))' \
  | kubectl replace -f - --subresource=status
```

Replace `MultipleRemediations` with the actual condition type name if a custom rule applied a different condition.

### XID 74 Register Rules

XID 74 (NVLink error) carries register fields (`REG0`–`REG6`) that encode the specific failure mode. The XID 74 rules inspect these register bit patterns to distinguish three categories of failure:

**Signal integrity issues** — REG0 bits 21 or 22 set (`XID74Reg0SignalIntegrityErrorRule`). Suggests marginal signal levels; check link mechanical connections and run field diagnostics if the issue persists.

**ECC/parity errors** — REG0 bits 4 or 5 set (`XID74Reg0ECCParityErrorRule`), or REG2 bits 0, 1, 2, or 6 set (`RepeatedXID74Reg2HardwareIssueRule`). Repeated occurrences on the same NVLink and GPU indicate a hardware fault.

**Unexpected or unexplained errors** — REG0 bits 1 or 20 set, or REG3 or REG4 anomalies: escalated immediately to `CONTACT_SUPPORT`. REG0 bits 27 or 29 set: escalated to `CONTACT_SUPPORT` after two or more occurrences within 24 hours (`XID74Reg0Bit27Or29SetRule`).

Rules that check REG0 bits 1, 20, 21, or 22 additionally verify that no other XID errors are active on the same GPU before firing, to avoid false positives during multi-error storms.

### XID 31 and XID 13 Rules

Both XID 31 (GPU memory error) and XID 13 (graphics engine fault) have two rule variants:

**Same GPU / same GPC+TPC** — repeated occurrences on the same hardware unit suggest a hardware defect. The recommended action is `RUN_DCGMEUD` (run DCGM End-User Diagnostics), followed by field diagnostics if tests pass.

**Different GPU / different GPC+TPC combinations** — the same error appearing on different hardware units within a short window suggests a software or workload problem. The recommended action is `NONE`; investigate the application process identified in the error PID.

Both variants use a burst-window algorithm (3-minute window for XID 31, 15-second window for XID 13) to avoid counting rapid re-occurrences of the same event as separate bursts.

### NIC Rules

Two NIC rules complement the NIC Health Monitor module:

`enableRepeatedNICDriverErrorRule` — correlates `SysLogsNICDriverError` events from the Syslog Health Monitor. It fires when the same kernel-log error pattern (for example, `mlx5_tx_timeout_detected` or `netdev_watchdog`) occurs three or more times on the same node within one hour, indicating that automatic driver recovery is failing.

`enableRepeatedNICDegradationRule` — correlates `InfiniBandDegradationCheck` and `EthernetDegradationCheck` events from the NIC Health Monitor. It fires when a non-fatal counter degradation occurs three or more times on the same NIC port within one hour, suggesting a persistent physical-layer problem.

See the [NIC Health Monitor configuration](nic-health-monitor.md) for the corresponding check and detection configuration.
