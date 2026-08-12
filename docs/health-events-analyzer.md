# Health Events Analyzer

## Overview

The Health Events Analyzer runs MongoDB aggregation pipelines against the health events collection to detect failure patterns that no single raw event can represent alone — repeated hardware failures on the same node, errors clustering on a specific GPU die, multiple remediations within a short window, and NVLink signal integrity issues encoded inside XID 74 register fields.

Think of it as a pattern-recognition layer between your raw telemetry and your response pipeline — similar to how a clinician looks across a patient's history rather than reacting to a single abnormal reading, the Analyzer correlates events over hours or days before drawing a conclusion.

### Why Do You Need This?

Individual health events capture what happened at a point in time, but many hardware problems only become visible across a sequence of events:

- **Recurring failures**: A GPU that fails, recovers, and fails again within a week is a fundamentally different signal than a one-time transient error
- **Die-level clustering**: Errors concentrated on the same GPC or TPC inside a GPU indicate a localized hardware defect that per-event monitoring cannot see
- **NVLink register ambiguity**: XID 74 carries five register fields (reg0–reg4) that distinguish signal integrity errors, ECC parity errors, outright hardware failures, and unexpected conditions — decoding them requires analysis beyond what the raw event stream provides
- **Repair effectiveness**: When a node has been remediated more than once in a short window, automated repair is not resolving the underlying problem and operator intervention is needed

## How It Works

The Health Events Analyzer consumes MongoDB change stream events from the health events collection:

1. **Ingests raw events** from any health monitor registered in the cluster (GPU Health Monitor, Syslog Health Monitor, NIC Health Monitor, CSP Health Monitor, etc.), stored in the collection by Platform Connectors
2. **Runs aggregation pipelines** over configurable time windows (hours to days) to look for patterns across events on the same node or GPU
3. **Evaluates rules** shipped as TOML-encoded MongoDB aggregation stages in the Helm `config:` block; each rule targets a specific pattern (e.g., repeated failures, die-level clustering, XID 74 register decoding, multiple remediations)
4. **Emits synthetic events** when a rule matches: each derived event carries its own `checkName` (the rule name) and flows through the standard Fault Quarantine → Node Drainer → Fault Remediation pipeline exactly as if a health monitor had reported it
5. **Applies the configured processing strategy** — `EXECUTE_REMEDIATION` for live operation or `STORE_ONLY` for shadow-mode observation with no side effects

The Analyzer does not modify or delete raw events. It only appends new synthetic events into the same pipeline.

**Loop prevention**: The Analyzer excludes its own events at two layers. First, the change-stream ingestion filter drops any event where `agent == "health-events-analyzer"`, so derived events are never re-ingested. Second, every rule's aggregation pipeline opens with a guard `$match` stage that also filters out events produced by `health-events-analyzer`, so the Analyzer never counts its own synthetic events when evaluating rules.

### XID 74 Register Decoding

XID 74 is the NVLink error XID. The error registers embedded in the event (reg0–reg4) encode the root cause. The Analyzer decodes these fields to distinguish:

- **Signal integrity errors**: Transient NVLink link issues
- **ECC parity errors**: Memory-level errors on the NVLink interface
- **Hardware failures**: Conditions that warrant a `REPLACE_VM` recommended action
- **Unexpected errors**: Conditions escalated with a `CONTACT_SUPPORT` recommended action

Without register-level decoding, all XID 74 events look identical. The Analyzer promotes the appropriate recommended action so that Fault Remediation can dispatch the correct maintenance request.

### MultipleRemediations Rule

The MultipleRemediations rule fires when the same node has been remediated more than once within a 7-day window. This signals that automated repair is not resolving the underlying hardware problem. Unlike a standard quarantine condition, the Analyzer does not emit a corresponding healthy event for this rule — the node condition must be cleared manually by a cluster operator after investigation.

### NIC Rules

The RepeatedNICDegradation and RepeatedNICDriverError rules detect patterns in NIC health events from the Syslog Health Monitor. See [NIC Health Monitor](nic-health-monitor.md) for details on the underlying event sources.

## Configuration

See [Health Events Analyzer configuration](configuration/health-events-analyzer.md) for the full Helm reference, including how to add, modify, and enable or disable individual rules.

Enable the module and toggle individual rules through Helm values:

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

Most operators use the default aggregation pipeline stages and only toggle individual rules on or off. Custom aggregation stages can be supplied in the Helm `config:` block as TOML-encoded MongoDB pipeline documents.

## Key Features

### Time-Window Correlation
Aggregation pipelines operate over configurable windows spanning hours to days, enabling detection of slow-burn hardware degradation that instantaneous monitoring cannot surface.

### Die-Level Failure Clustering
Groups errors by GPC and TPC identifiers within a GPU to identify failures localized to a specific hardware die, distinguishing a die defect from a node-wide or driver-level issue.

### Register-Level XID Decoding
Decodes XID 74 error register fields to assign accurate recommended actions (`CONTACT_SUPPORT`, `REPLACE_VM`), ensuring the correct downstream remediation workflow is triggered.

### Shadow Mode
Set `processingStrategy: STORE_ONLY` to run the Analyzer in observation mode. Rules evaluate and synthetic events are stored, but no quarantine or remediation actions are triggered. Use this to validate rule behavior before enabling live operation.

### Pipeline Integration
Synthetic events emitted by the Analyzer carry a `checkName` equal to the rule name. They enter the standard NVSentinel pipeline and are subject to all the same Fault Quarantine CEL rules, Node Drainer configuration, and Fault Remediation actions as events from health monitors.
