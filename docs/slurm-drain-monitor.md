# Slurm Drain Monitor

## Overview

The Slurm Drain Monitor connects the Slurm workload manager and NVSentinel. It watches slurmd worker pods for drain reasons. It matches these reasons against configurable regex patterns. It then converts the matching drains into NVSentinel health events, which enter the standard quarantine and notification pipeline.

The monitor operates as a translator. Cluster operators already drain Slurm nodes with structured reason strings from their own health check scripts. The monitor reads those strings and converts them into the health event vocabulary that NVSentinel understands. A separate health monitor deployment is not necessary.

### Why Do You Need This?

Slurm operators run custom health check scripts that drain nodes with structured reason prefixes. But this information stays inside Slurm, unless a bridge exports it:

- **Visibility gap**: Kubernetes and the NVSentinel notification and audit systems cannot see Slurm drain events, unless something explicitly exposes them
- **Duplicate tooling**: Without a bridge, operators must separately configure Slurm alerting and NVSentinel alerting for the same hardware failure
- **Manual triage overhead**: Drained nodes accumulate without automated quarantine or escalation. Operators must then manually check the Slurm state and decide on follow-up actions
- **Inconsistent workflows**: Health check scripts vary across clusters. A pattern-based bridge lets operators standardize response workflows without changes to their scripts

## How It Works

The Slurm Drain Monitor runs as a Deployment (controller) in the cluster:

1. **Watches slurmd pods** that match a configurable label selector (default: `app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker`) in a configurable namespace (default: `slurm`)
2. **Extracts the drain reason** from the message of the `SlurmNodeStateDrain` pod condition, which slurm-operator sets when the Slurm node is in the DRAIN state
3. **Splits compound reasons** with a configurable delimiter, and then evaluates each segment in order
4. **Matches against patterns**: each pattern is a regex that maps to a `checkName`, `componentClass`, `isFatal` flag, `message`, and `recommendedAction`. The monitor evaluates all patterns independently
5. **Emits a health event for every matching pattern**. Each health event enters the standard NVSentinel pipeline, where Fault Quarantine, Node Drainer, and Fault Remediation process it
6. **Applies the configured processing strategy**: `EXECUTE_REMEDIATION` for live operation or `STORE_ONLY` for shadow-mode observation

If no pattern matches the drain reason, the monitor emits no health event and ignores the pod.

### Default Pattern

The built-in default pattern matches any drain reason with the prefix `[HC]`. Slurm health check scripts conventionally use this prefix:

| Pattern | checkName | componentClass | isFatal | recommendedAction |
|---|---|---|---|---|
| `^\[HC\]` | `SlurmHealthCheck` | `NODE` | `false` | `CONTACT_SUPPORT` |

`isFatal: false` routes the health event to notification and escalation workflows, not to automated remediation. `isFatal: true` alone does not start automated remediation. For Slurm-detected failures that need automated quarantine, drain and remediation, configure all these items:

- Set `isFatal: true` on the pattern.
- Set `recommendedAction` to an action that has an entry in `fault-remediation.maintenance.actions`, such as `RESTART_VM` or `REPLACE_VM`. The default chart has no entry for `CONTACT_SUPPORT`, the action of the default pattern.
- Add a fault-quarantine ruleset that matches the health events of this monitor, for example with `event.agent == 'slurm-drain-monitor'`. The default rulesets do not match them. See [Rule Sets](configuration/fault-quarantine.md#rule-sets).

## Configuration

For the full Helm reference, see [Slurm Drain Monitor configuration](configuration/slurm-drain-monitor.md). It includes how to define custom patterns and configure the pod selector.

Use Helm values to enable the module and to configure drain reason patterns:

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
    - name: slurm-healthcheck
      regex: "\\[HC\\]"
      checkName: SlurmHealthCheck
      componentClass: NODE
      isFatal: false
      message: "Slurm health check detected a failure"
      recommendedAction: CONTACT_SUPPORT
    - name: slurm-healthcheck-fatal
      regex: "\\[HC-FATAL\\]"
      checkName: SlurmHealthCheckFatal
      componentClass: NODE
      isFatal: true
      message: "Slurm health check detected a fatal failure"
      recommendedAction: REPLACE_VM
```

The monitor evaluates all patterns independently against each drain reason segment. Every matching pattern emits a separate health event. The monitor silently skips drain reasons that do not match.

## Key Features

### Pattern-Based Matching

Regex patterns give operators precise control over the drain reasons that generate NVSentinel health events and the drain reasons that the monitor ignores. Multiple patterns with different `isFatal` values and `recommendedAction` settings let a single deployment handle a range of health check severities.

### Compound Reason Splitting

Slurm health check scripts sometimes concatenate multiple reasons into a single drain string. The configurable delimiter splits compound reasons. Thus, the monitor evaluates each segment independently against the pattern list.

### Configurable Escalation

The default pattern sets `isFatal: false` and `recommendedAction: CONTACT_SUPPORT`. Thus, a human reviews the node before any destructive action occurs. For automated quarantine, workload drain and maintenance CR creation, configure the items in [Default Pattern](#default-pattern). `isFatal: true` alone is not enough.

### Shadow Mode

To emit and store health events without quarantine or remediation actions, set `processingStrategy: STORE_ONLY`. Use this mode to validate pattern coverage against real drain reasons before you enable live operation.

### Pipeline Integration

The health events that the Slurm Drain Monitor emits carry the configured `checkName`. They follow the standard NVSentinel pipeline. Fault Quarantine CEL rules, Node Drainer configuration, and Fault Remediation maintenance templates apply to them. They apply in exactly the same way as to health events from the GPU Health Monitor or the Syslog Health Monitor.
