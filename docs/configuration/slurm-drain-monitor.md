# Slurm Drain Monitor Configuration

## Overview

The Slurm Drain Monitor watches Slurm workload manager pods (`slurmd`) for drain reasons and converts them into NVSentinel health events. It reads the drain reason from pod labels set by the Slurm node controller and matches patterns to classify the failure. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the slurm-drain-monitor module is deployed in the cluster.

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

Number of `slurmd` pod reconcile loops that run in parallel. Increase only if the monitor is processing a large number of pods and falling behind.

### Resync Period

```yaml
slurm-drain-monitor:
  resyncPeriod: 5m
```

How often the controller re-lists all `slurmd` pods and re-evaluates drain reasons, even when no pod change event is received. Increase to reduce API server load; decrease for faster detection of drain reason changes that did not trigger a watch event.

## Pod Selection

### Namespace

```yaml
slurm-drain-monitor:
  namespace: slurm
```

Kubernetes namespace where `slurmd` pods run. Must match your Slurm deployment.

### Label Selector

```yaml
slurm-drain-monitor:
  labelSelector: "app.kubernetes.io/name=slurmd,app.kubernetes.io/component=worker"
```

Label selector used to identify `slurmd` worker pods. The defaults assume a standard Helm-deployed Slurm. Adjust to match the labels in your Slurm deployment if they differ.

## Drain Reason Parsing

### Reason Delimiter

```yaml
slurm-drain-monitor:
  reasonDelimiter: "; "
```

Delimiter used to split compound drain reasons when a pod label contains multiple reasons in a single string. Each part is matched against patterns independently.

## Pattern Matching

Each pattern maps a drain-reason regex to an NVSentinel health event. Multiple patterns can be defined to classify different failure categories.

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
Unique identifier for the pattern. Used in logs and health event metadata.

#### regex
Regular expression matched against each drain reason string (after splitting on `reasonDelimiter`). All patterns are evaluated independently — if multiple patterns match the same drain reason, each emits a separate health event.

#### checkName
Name of the NVSentinel health check reported in the generated health event.

#### componentClass
Component class associated with the health event. Configurable string forwarded as-is to the health event. Typically `NODE` for Slurm drain events since a drain applies to the whole node, but can be set to any valid component class.

#### isFatal
When `false` (default), the health event triggers `CONTACT_SUPPORT` rather than the full quarantine + drain + remediation pipeline. Set to `true` only if you want the node to be automatically quarantined and remediated when this pattern matches.

#### message
Optional human-readable description included in the health event. Leave empty to use the raw drain reason string.

#### recommendedAction
NVSentinel remediation action associated with this pattern. Typical values: `CONTACT_SUPPORT`, `REBOOT_NODE`, `TERMINATE_NODE`.

## Processing Strategy

```yaml
slurm-drain-monitor:
  processingStrategy: EXECUTE_REMEDIATION
```

Controls how matched health events are handled after pattern classification. `EXECUTE_REMEDIATION` passes the event to the fault-remediation pipeline for action.

## Configuration Examples

### Example 1: Default Pattern for Slurm Health Check Failures

Health check scripts that prefix drain reasons with `[HC]` are classified as non-fatal and routed to support.

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

Use `isFatal: true` and a non-CONTACT_SUPPORT action to trigger the full quarantine and remediation pipeline for a specific failure class.

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
      recommendedAction: REBOOT_NODE
```
