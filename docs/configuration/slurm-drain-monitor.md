# Slurm Drain Monitor Configuration

## Overview

The slurm-drain-monitor module watches Slurm workload manager pods (`slurmd`) for drain reasons. It converts these reasons into NVSentinel health events. It reads the drain reason from the message of the `SlurmNodeStateDrain` pod condition, which slurm-operator sets. It then matches patterns to classify the failure. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether Helm deploys the slurm-drain-monitor module in the cluster.

```yaml
global:
  slurmDrainMonitor:
    enabled: true
```

### Replica Count

```yaml
slurm-drain-monitor:
  replicaCount: 1
```

### Resources

Defines CPU and memory resource requests and limits for the slurm-drain-monitor pod.

```yaml
slurm-drain-monitor:
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 256Mi
```

### Logging

Sets the verbosity level for slurm-drain-monitor logs.

```yaml
slurm-drain-monitor:
  logLevel: info  # Options: debug, info, warn, error
```

## Reconciler Settings

### Max Concurrent Reconciles

```yaml
slurm-drain-monitor:
  maxConcurrentReconciles: 1
```

Number of `slurmd` pod reconcile loops that run in parallel. If the monitor processes a large number of pods and falls behind, increase this value. Otherwise, keep the default.

### Resync Period

```yaml
slurm-drain-monitor:
  resyncPeriod: 5m
```

How often the controller re-lists all `slurmd` pods and re-evaluates drain reasons, even when it receives no pod change event. To reduce the API server load, increase this value. For faster detection of drain reason changes that did not trigger a watch event, decrease this value.

## Pod Selection

### Namespace

```yaml
slurm-drain-monitor:
  namespace: slurm
```

Kubernetes namespace where `slurmd` pods run. The value must match your Slurm deployment.

### Label Selector

```yaml
slurm-drain-monitor:
  labelSelector: "app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker"
```

Label selector that identifies the `slurmd` worker pods. The defaults assume a standard Helm-deployed Slurm. If the labels in your Slurm deployment are different, change the selector to match them.

## Drain Reason Parsing

### Reason Delimiter

```yaml
slurm-drain-monitor:
  reasonDelimiter: "; "
```

Delimiter that splits compound drain reasons, when the drain reason contains multiple reasons in a single string. The monitor matches each part against the patterns independently.

## Pattern Matching

Each pattern maps a drain-reason regex to an NVSentinel health event. You can define multiple patterns to classify different failure categories.

```yaml
slurm-drain-monitor:
  patterns:
    - name: slurm-healthcheck
      regex: '^\[HC\]'
      checkName: SlurmHealthCheck
      componentClass: NODE
      isFatal: false
      message: ""
      recommendedAction: CONTACT_SUPPORT
```

### Parameters

#### name

Unique identifier for the pattern. The monitor uses it in configuration error messages and as the `pattern_name` label of its metrics.

#### regex

Regular expression that the monitor matches against each drain reason string (after the split on `reasonDelimiter`). The monitor evaluates all patterns independently. If multiple patterns match the same drain reason, each pattern emits a separate health event.

#### checkName

Name of the NVSentinel health check reported in the generated health event.

#### componentClass

Component class associated with the health event. The monitor forwards this configurable string as-is to the health event. The value is typically `NODE` for Slurm drain events, because a drain applies to the whole node. But you can set it to any valid component class.

#### isFatal

When `false` (default), the pattern routes the health event to notification and escalation workflows, not to automated remediation. `isFatal: true` alone does not start automated quarantine, drain or remediation. For these actions, configure all the items in [Default Pattern](../slurm-drain-monitor.md#default-pattern).

#### message

Optional human-readable description included in the health event. To use the raw drain reason string, leave it empty.

#### recommendedAction

NVSentinel remediation action associated with this pattern. The value must be a name of the `RecommendedAction` enum, such as `CONTACT_SUPPORT`, `RESTART_BM`, `RESTART_VM` or `REPLACE_VM`. If the name is not valid, the monitor does not start.

## Processing Strategy

```yaml
slurm-drain-monitor:
  processingStrategy: EXECUTE_REMEDIATION
```

Controls how NVSentinel handles the matched health events after pattern classification. `EXECUTE_REMEDIATION` passes the health event to the fault-remediation pipeline for action.

## Configuration Examples

### Example 1: Default Pattern for Slurm Health Check Failures

If health check scripts prefix drain reasons with `[HC]`, the monitor classifies these drain reasons as non-fatal and routes them to support.

```yaml
slurm-drain-monitor:
  patterns:
    - name: slurm-healthcheck
      regex: '^\[HC\]'
      checkName: SlurmHealthCheck
      componentClass: NODE
      isFatal: false
      message: ""
      recommendedAction: CONTACT_SUPPORT
```

### Example 2: Custom Pattern for Script-Prefixed Failures

Add a pattern for custom health check scripts that write `[FAIL]`-prefixed drain reasons.

```yaml
slurm-drain-monitor:
  patterns:
    - name: slurm-healthcheck
      regex: '^\[HC\]'
      checkName: SlurmHealthCheck
      componentClass: NODE
      isFatal: false
      message: ""
      recommendedAction: CONTACT_SUPPORT
    - name: custom-script-failure
      regex: '^\[FAIL\]'
      checkName: SlurmCustomScriptFailure
      componentClass: NODE
      isFatal: false
      message: "Custom health check script reported failure"
      recommendedAction: CONTACT_SUPPORT
```

### Example 3: Fatal Pattern Triggering Automated Remediation

This example uses `isFatal: true` and the `RESTART_BM` action. NVSentinel quarantines, drains and remediates the node only if a fault-quarantine ruleset also matches these health events. See [Default Pattern](../slurm-drain-monitor.md#default-pattern).

```yaml
slurm-drain-monitor:
  patterns:
    - name: slurm-healthcheck
      regex: '^\[HC\]'
      checkName: SlurmHealthCheck
      componentClass: NODE
      isFatal: false
      message: ""
      recommendedAction: CONTACT_SUPPORT
    - name: gpu-hardware-fault
      regex: '^\[GPU_FAULT\]'
      checkName: SlurmGPUFault
      componentClass: NODE
      isFatal: true
      message: "GPU hardware fault detected by Slurm health check"
      recommendedAction: RESTART_BM
```
