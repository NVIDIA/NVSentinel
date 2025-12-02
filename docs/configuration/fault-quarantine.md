# Fault Quarantine Configuration

## Overview

The Fault Quarantine module isolates nodes with detected hardware or software failures by cordoning and/or tainting them. This document covers all Helm configuration options available for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the fault-quarantine module is deployed in the cluster.

```yaml
global:
  faultQuarantine:
    enabled: true
```

> Note: This module depends on the datastore being enabled. Therefore, ensure the datastore is also enabled.

### Resources

Defines CPU and memory resource requests and limits for the fault-quarantine pod.

```yaml
fault-quarantine:
  resources:
    limits:
      cpu: "1"
      memory: "1Gi"
    requests:
      cpu: "1"
      memory: "1Gi"
```

### Logging

Sets the verbosity level for fault-quarantine logs.

```yaml
fault-quarantine:
  logLevel: info  # Options: debug, info, warn, error
```

### Label Prefix

Defines the prefix for all node labels created by the module to track cordon/uncordon lifecycle.

```yaml
fault-quarantine:
  labelPrefix: "k8saas.nvidia.com/"
```

Generated labels:
- `<labelPrefix>cordon-by` - Service that cordoned the node
- `<labelPrefix>cordon-reason` - Reason for cordoning
- `<labelPrefix>cordon-timestamp` - Cordon timestamp (format: 2006-01-02T15-04-05Z)
- `<labelPrefix>uncordon-by` - Service that uncordoned the node
- `<labelPrefix>uncordon-timestamp` - Uncordon timestamp (format: 2006-01-02T15-04-05Z)

## Circuit Breaker

Prevents too many nodes from being quarantined simultaneously, protecting against cluster-wide cascading failures.

### Configuration

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true
    percentage: 50
    duration: "5m"
```

### Parameters

#### enabled
Enables or disables circuit breaker protection. When disabled, unlimited nodes can be quarantined.

#### percentage
Maximum percentage of total cluster nodes that can be quarantined within the time window. When exceeded, the circuit breaker trips and blocks all new quarantine actions.

#### duration
Time window for tracking cordon events. The circuit breaker counts unique node cordons within this sliding window.

### Configuration Examples

Aggressive:
```yaml
circuitBreaker:
  enabled: true
  percentage: 20
  duration: "10m"
```

Conservative:
```yaml
circuitBreaker:
  enabled: true
  percentage: 75
  duration: "3m"
```

Disabled:
```yaml
circuitBreaker:
  enabled: false
```

## Rule Sets

Rule sets define conditions for quarantining nodes using CEL expressions. Each rule set specifies match conditions (when to trigger) and actions (what to do).

### Rule Set Structure

```yaml
fault-quarantine:
  ruleSets:
    - version: "1"
      name: "ruleset-name"
      priority: 100
      
      match:
        all:
          - kind: "HealthEvent"
            expression: "event.agent == 'gpu-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
          - kind: "Node"
            expression: |
              !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
        
        any:
          - kind: "HealthEvent"
            expression: "event.agent == 'syslog-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
      
      cordon:
        shouldCordon: true
      
      taint:
        key: "nvidia.com/gpu-error"
        value: "fatal"
        effect: "NoSchedule"
```

### Parameters

#### version
Rule set format version for future compatibility.

#### name
Unique identifier used in logs, metrics, and as part of the cordon-reason label.

#### priority
Optional integer for resolving conflicts when multiple rule sets apply the same taint key-value pair. Higher values take precedence.

#### match
Defines conditions that must be satisfied for the rule set to trigger. Supports `all` (AND) and `any` (OR) logic.

#### kind
Specifies the object type to evaluate in the CEL expression. Valid values: `HealthEvent` (evaluates against health event data) or `Node` (evaluates against Kubernetes node object).

#### expression
CEL (Common Expression Language) expression that evaluates to true or false. For `HealthEvent` kind, access fields via `event` variable. For `Node` kind, access fields via `node` variable.

#### cordon
Specifies whether to mark the node as unschedulable when the rule matches.

#### taint
Optional Kubernetes taint to apply. Taints can prevent pod scheduling or evict existing pods based on the effect.

### Example Rule Sets

#### Example 1: Fatal GPU Errors from GPU Health Monitor AND node not labeled with k8saas.nvidia.com/ManagedByNVSentinel=false

```yaml
ruleSets:
  - version: "1"
    name: "GPU fatal error ruleset"
    match:
      all:
        - kind: "HealthEvent"
          expression: "event.agent == 'gpu-health-monitor' && event.componentClass == 'GPU' && event.isFatal == true"
        - kind: "Node"
          expression: |
            !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && 
              node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
    cordon:
      shouldCordon: true
```

#### Example 2: Syslog Fatal Errors Excluding XID 45 AND node not labeled with k8saas.nvidia.com/ManagedByNVSentinel=false

```yaml
ruleSets:
  - version: "1"
    name: "Syslog fatal error ruleset"
    match:
      all:
        - kind: "HealthEvent"
          expression: |
            event.agent == 'syslog-health-monitor' && 
            event.componentClass == 'GPU' && 
            event.isFatal == true && 
            (event.errorCode == null || !event.errorCode.exists(e, e == '45'))
        - kind: "Node"
          expression: |
            !('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels && 
              node.metadata.labels['k8saas.nvidia.com/ManagedByNVSentinel'] == "false")
    cordon:
      shouldCordon: true
```
