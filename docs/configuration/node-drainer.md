# Node Drainer Configuration

## Overview

The Node Drainer module evacuates workloads from quarantined nodes, safely moving pods to healthy nodes. This document covers all Helm configuration options available for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the node-drainer module is deployed in the cluster.

```yaml
global:
  nodeDrainer:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the node-drainer pod.

```yaml
node-drainer:
  resources:
    limits:
      cpu: "2"
      memory: "2Gi"
    requests:
      cpu: "1"
      memory: "1Gi"
```

### Logging

Sets the verbosity level for node-drainer logs.

```yaml
node-drainer:
  logLevel: info  # Options: debug, info, warn, error
```

> Note: This module depends on the results from fault-quarantine. It also depends on the datastore being enabled. Therefore, ensure the datastore and the other modules are also enabled.

### Partial Drain

If enabled, the node-drainer will only drain pods which are leveraging the GPU_UUID impacted entity in COMPONENT_RESET HealthEvents. If disabled, the node-drainer will drain all eligible pods on the impacted node for the configured namespaces regardless of the remediation action. HealthEvents with the COMPONENT_RESET remediation action must include an impacted entity for the unhealthy GPU_UUID or else the drain will fail. 

IMPORTANT: If this setting is enabled, the COMPONENT_RESET action in fault-remediation must map to a custom resource which takes action only against the GPU_UUID. If partial drain was enabled in node-drainer but fault-remediation mapped COMPONENT_RESET to a reboot action, pods which weren't drained would be restarted as part of the reboot.
```yaml
node-drainer:
  partialDrainEnabled: true
```

### Eviction Timeout

Grace period in seconds applied to pod eviction requests in Immediate mode only.

```yaml
node-drainer:
  evictionTimeoutInSeconds: "60"
```

This timeout is passed as the `GracePeriodSeconds` in the Kubernetes eviction API call. Only used for `Immediate` eviction mode. Other modes respect the pod's configured `terminationGracePeriodSeconds`.

### System Namespaces

Regular expression pattern matching system namespaces that are skipped during node drain operations.

```yaml
node-drainer:
  systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator)$"
```

Pods in namespaces matching this regex are not evicted during drain operations.

### Delete After Timeout

Time in minutes from the health event creation after which pods will be force deleted if still running.

```yaml
node-drainer:
  deleteAfterTimeoutMinutes: 60
```

Used with `DeleteAfterTimeout` eviction mode. When the timeout expires, remaining pods are force deleted regardless of their state.

### Not Ready Timeout

Time in minutes after which a pod in NotReady state is skipped from eviction operations.

```yaml
node-drainer:
  notReadyTimeoutMinutes: 5
```

When a pod has been in NotReady state for longer than this timeout, it is excluded from the list of pods to evict. This prevents attempting to evict pods that are already unhealthy and unlikely to respond to eviction requests.

## User Namespaces

Defines eviction behavior for user workloads based on namespace patterns.

### Configuration Structure

```yaml
node-drainer:
  userNamespaces:
    - name: "namespace-pattern"
      mode: "EvictionMode"
```

### Parameters

#### name
Namespace name or pattern to match. Use `*` to match all user namespaces not explicitly configured.

#### mode
Eviction strategy to apply for pods in matching namespaces. Valid values: `Immediate`, `AllowCompletion`, or `DeleteAfterTimeout`.

### Eviction Modes

#### Immediate

Evicts pods with minimal grace period without waiting for completion.

Best for: Stateless applications that can be quickly restarted.

```yaml
userNamespaces:
  - name: "web-frontend"
    mode: "Immediate"
```

#### AllowCompletion

Waits for pods to terminate gracefully, respecting their `terminationGracePeriodSeconds`.

Best for: Most workloads including stateful applications and services.

```yaml
userNamespaces:
  - name: "production"
    mode: "AllowCompletion"
```

#### DeleteAfterTimeout

Waits up to `deleteAfterTimeoutMinutes` (from health event creation), then force deletes remaining pods.

Best for: Long-running training jobs that need time to checkpoint and save state.

```yaml
userNamespaces:
  - name: "ml-training"
    mode: "DeleteAfterTimeout"
```

### Configuration Examples

#### Example 1: Default Configuration for All Namespaces

```yaml
userNamespaces:
  - name: "*"
    mode: "AllowCompletion"
```

#### Example 2: Different Modes for Different Workloads

```yaml
userNamespaces:
  - name: "training"
    mode: "DeleteAfterTimeout"
  - name: "inference-*"
    mode: "Immediate"
```
