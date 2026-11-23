# Health Events Analyzer

For operator-verified recovery of configured derived conditions, see the [recovery guide](health-events-analyzer-recovery.md).

## Overview

The Health Events Analyzer runs MongoDB aggregation pipelines on the health events collection. These pipelines detect failure patterns that one raw health event cannot show alone:

- Repeated hardware failures on the same node
- Errors that cluster on a specific GPU die
- Multiple remediations in a short window
- NVLink signal integrity issues encoded in the XID 74 register fields

The Analyzer is a health monitor, like the GPU Health Monitor or the Syslog Health Monitor. It recognizes patterns. It does not react to one abnormal reading. Instead, it correlates health events over hours or days before it makes a conclusion.

### Why Do You Need This?

Individual health events record what happened at a specific time. Many hardware problems become visible only across a sequence of health events:

- **Recurring failures**: A GPU that fails, recovers, and fails again in one week gives a different type of signal from a one-time transient error
- **Die-level clustering**: Errors on the same GPC or TPC in a GPU indicate a localized hardware defect that per-event monitoring cannot see
- **NVLink register ambiguity**: XID 74 has seven register fields (reg0–reg6). These fields distinguish signal integrity errors, ECC parity errors, definite hardware failures, and unexpected conditions. The decoding of these fields needs more analysis than the raw event stream gives
- **Repair effectiveness**: If NVSentinel remediates a node more than once in a short window, the automated repair does not correct the underlying problem. An operator must then intervene

## How It Works

The Health Events Analyzer reads MongoDB change stream events from the health events collection. It does these steps:

1. **Ingests raw events** from any other health monitor registered in the cluster. Examples are the GPU Health Monitor, Syslog Health Monitor, NIC Health Monitor and CSP Health Monitor. Platform Connectors stores these events in the collection
2. **Runs aggregation pipelines** over configurable time windows (hours to days) to find patterns across health events on the same node or GPU
3. **Evaluates rules** that the Helm `config:` block supplies as TOML-encoded MongoDB aggregation stages. Each rule targets a specific pattern, for example repeated failures, die-level clustering, XID 74 register decoding or multiple remediations
4. **Emits derived events** when a rule matches. Each derived event has its own `checkName` (the rule name). It follows the standard Fault Quarantine → Node Drainer → Fault Remediation pipeline, the same as a health event from any other health monitor
5. **Applies the configured processing strategy** — `EXECUTE_REMEDIATION` for live operation or `STORE_ONLY` for shadow-mode observation with no side effects

The Analyzer does not change or delete raw events. It only publishes new derived events to platform-connectors over gRPC, the same as any other health monitor.

**Loop prevention**: The Analyzer excludes its own events at two layers. First, the change-stream ingestion filter drops each event where `agent == "health-events-analyzer"`, so the Analyzer never ingests its derived events again. Second, the aggregation pipeline of each rule starts with a guard `$match` stage. This stage also removes the events that `health-events-analyzer` produced. Thus, the Analyzer never counts its own derived events when it evaluates rules.

### XID 74 Register Decoding

XID 74 is the NVLink error XID. The error registers in the health event (reg0–reg6) encode the root cause. The Analyzer decodes these fields to distinguish these types of errors:

- **Signal integrity errors**: Transient NVLink link issues
- **ECC parity errors**: Memory-level errors on the NVLink interface
- **Hardware failures**: Conditions that need a `CONTACT_SUPPORT` recommended action
- **Unexpected errors**: Conditions that the Analyzer escalates with a `CONTACT_SUPPORT` recommended action

Without register-level decoding, all XID 74 health events look the same. The Analyzer sets the recommended action that fits the pattern. The default XID 74 rules recommend `CONTACT_SUPPORT` or `NONE`, and Fault Remediation creates no maintenance CR for these actions. For `CONTACT_SUPPORT`, Fault Remediation increments `fault_remediation_unsupported_actions_total`. If the node is still quarantined for this fault, it also sets the `dgxc.nvidia.com/nvsentinel-state` label of the node to `remediation-failed`.

### MultipleRemediations Rule

The MultipleRemediations rule matches when NVSentinel remediated the same node five or more times in a 7-day window. This shows that the automated repair does not correct the underlying hardware problem. Unlike for a standard quarantine condition, the Analyzer does not emit a matching healthy derived event for this rule. After an investigation, you must clear the node condition manually.

### NIC Rules

The RepeatedNICDriverError rule detects patterns in NIC driver health events from the Syslog Health Monitor. The RepeatedNICDegradation rule detects patterns in NIC counter health events from the NIC Health Monitor. For details about the underlying event sources, see [NIC Health Monitor](nic-health-monitor.md).

## Configuration

For the full Helm reference, see [Health Events Analyzer configuration](configuration/health-events-analyzer.md). It includes how to add, change, enable and disable individual rules.

Use these Helm values to enable the module and to enable or disable individual rules:

```yaml
global:
  healthEventsAnalyzer:
    enabled: true

health-events-analyzer:
  processingStrategy: EXECUTE_REMEDIATION  # or STORE_ONLY for shadow mode

  # Individual rule flags — all enabled by default
  enableMultipleRemediationsRule: true
  enableRepeatedXID74Reg0HardwareIssueRule: true
  enableXID74Reg0ECCParityErrorRule: true
  enableXID74Reg0SignalIntegrityErrorRule: true
  enableRepeatedNICDegradationRule: true
  enableRepeatedNICDriverErrorRule: true
```

Most operators use the default aggregation pipeline stages and only enable or disable individual rules. You can supply custom aggregation stages in the Helm `config:` block as TOML-encoded MongoDB pipeline documents.

## Key Features

### Time-Window Correlation

Aggregation pipelines operate over configurable windows of hours to days. Thus, the Analyzer can detect slow hardware degradation that instantaneous monitoring cannot show.

### Die-Level Failure Clustering

Groups errors by GPC and TPC identifiers in a GPU to identify failures on a specific hardware die. This grouping distinguishes a die defect from a node-wide or driver-level issue.

### Register-Level XID Decoding

Decodes XID 74 error register fields to assign accurate recommended actions (`CONTACT_SUPPORT`, `NONE`). Thus, the downstream modules receive the correct recommended action for each XID 74 pattern.

### Shadow Mode

To run the Analyzer in observation mode, set `processingStrategy: STORE_ONLY`. In this mode, the Analyzer evaluates the rules and NVSentinel stores the derived events, but NVSentinel does not start quarantine or remediation actions. Use this mode to validate the rule behavior before you enable live operation.

### Pipeline Integration

Each derived event from the Analyzer has a `checkName` equal to the rule name. Derived events enter the standard NVSentinel pipeline. The same Fault Quarantine CEL rules, Node Drainer configuration, and Fault Remediation actions apply to them as to health events from other health monitors.
