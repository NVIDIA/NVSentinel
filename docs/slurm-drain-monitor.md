# Slurm Drain Monitor

## Overview

The Slurm Drain Monitor bridges the Slurm workload manager and NVSentinel. It watches slurmd worker pods for drain reason labels, matches those reasons against configurable regex patterns, and converts matching drains into NVSentinel health events that enter the standard quarantine and notification pipeline.

Think of it as a translator — cluster operators already drain Slurm nodes with structured reason strings from their own health check scripts; the monitor reads those strings and converts them into the health event vocabulary that NVSentinel understands, without requiring a separate health monitor deployment.

### Why Do You Need This?

Slurm operators run custom health check scripts that drain nodes with structured reason prefixes, but that information stays inside Slurm unless something bridges it out:

- **Visibility gap**: Slurm drain events are invisible to Kubernetes and to NVSentinel's notification and audit systems unless explicitly surfaced
- **Duplicate tooling**: Without a bridge, operators must separately configure Slurm alerting and NVSentinel alerting for the same hardware failure
- **Manual triage overhead**: Drained nodes accumulate without automated quarantine or escalation, requiring operators to manually check Slurm state and decide on follow-up actions
- **Inconsistent workflows**: Health check scripts vary across clusters; a pattern-based bridge lets operators standardize response workflows without modifying their scripts

## How It Works

The Slurm Drain Monitor runs as a Deployment (controller) in the cluster:

1. **Watches slurmd pods** matching a configurable label selector (default: `app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker`) in a configurable namespace (default: `slurm`)
2. **Extracts the drain reason** from pod labels set by the Slurm health check scripts when a node is drained
3. **Splits compound reasons** using a configurable delimiter, then evaluates each segment in order
4. **Matches against patterns** — each pattern is a regex mapped to a `checkName`, `componentClass`, `isFatal` flag, `message`, and `recommendedAction`; all patterns are evaluated independently
5. **Emits a health event for every matching pattern**; each event enters the standard NVSentinel pipeline and is subject to Fault Quarantine, Node Drainer, and Fault Remediation processing
6. **Applies the configured processing strategy** — `EXECUTE_REMEDIATION` for live operation or `STORE_ONLY` for shadow-mode observation

If no pattern matches the drain reason, no event is emitted and the pod is ignored.

### Default Pattern

The built-in default pattern matches any drain reason prefixed with `[HC]`, which is the conventional prefix used by Slurm health check scripts:

| Pattern | checkName | componentClass | isFatal | recommendedAction |
|---|---|---|---|---|
| `\[HC\]` | `SlurmHealthCheck` | `NODE` | `false` | `CONTACT_SUPPORT` |

`isFatal: false` routes the event to notification and escalation workflows rather than automated `REPLACE_VM` remediation. Set `isFatal: true` on a pattern to trigger full automated quarantine, drain, and remediation for Slurm-detected failures that warrant it.

## Configuration

See [Slurm Drain Monitor configuration](configuration/slurm-drain-monitor.md) for the full Helm reference, including how to define custom patterns and configure the pod selector.

Enable the module and configure drain reason patterns through Helm values:

```yaml
global:
  slurmDrainMonitor:
    enabled: true

slurm-drain-monitor:
  processingStrategy: EXECUTE_REMEDIATION  # or STORE_ONLY for shadow mode

  namespace: slurm
  labelSelector: "app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker"
  reasonDelimiter: "; "  # Split compound drain reasons on this delimiter

  patterns:
    - regex: "\\[HC\\]"
      checkName: SlurmHealthCheck
      componentClass: NODE
      isFatal: false
      message: "Slurm health check detected a failure"
      recommendedAction: CONTACT_SUPPORT
    - regex: "\\[HC-FATAL\\]"
      checkName: SlurmHealthCheckFatal
      componentClass: NODE
      isFatal: true
      message: "Slurm health check detected a fatal failure"
      recommendedAction: REPLACE_VM
```

All patterns are evaluated independently against each drain reason segment; every matching pattern emits a separate health event. Unmatched drain reasons are silently skipped.

## Key Features

### Pattern-Based Matching
Regex patterns give operators precise control over which drain reasons generate NVSentinel events and which are ignored. Multiple patterns with different `isFatal` values and `recommendedAction` settings let a single deployment handle a range of health check severities.

### Compound Reason Splitting
Slurm health check scripts sometimes concatenate multiple reasons into a single drain string. The configurable delimiter splits compound reasons so each segment is evaluated independently against the pattern list.

### Configurable Escalation
`isFatal: false` (default) triggers `CONTACT_SUPPORT` escalation — a human reviews the node before any destructive action is taken. Set `isFatal: true` on patterns where you want full automated quarantine, workload drain, and maintenance request creation without manual intervention.

### Shadow Mode
Set `processingStrategy: STORE_ONLY` to emit and store health events without triggering quarantine or remediation actions. Use this to validate pattern coverage against real drain reasons before enabling live operation.

### Pipeline Integration
Health events emitted by the Slurm Drain Monitor carry the configured `checkName` and flow through the standard NVSentinel pipeline. Fault Quarantine CEL rules, Node Drainer configuration, and Fault Remediation maintenance templates apply to them in exactly the same way as events from GPU Health Monitor or Syslog Health Monitor.
