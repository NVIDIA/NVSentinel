# Kubernetes Object Monitor Configuration

## Overview

The Kubernetes Object Monitor module watches Kubernetes resources and generates health events when resources enter unhealthy states. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the kubernetes-object-monitor module is deployed in the cluster.

```yaml
global:
  kubernetesObjectMonitor:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the kubernetes-object-monitor pod.

```yaml
kubernetes-object-monitor:
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### Logging

Sets the verbosity level for kubernetes-object-monitor logs.

```yaml
kubernetes-object-monitor:
  logLevel: info  # Options: debug, info, warn, error
```

### Controller Configuration

Controls behavior of the Kubernetes controller watching resources.

```yaml
kubernetes-object-monitor:
  maxConcurrentReconciles: 1
  resyncPeriod: 5m
```

#### maxConcurrentReconciles
Maximum number of concurrent reconciliation workers. Higher values allow parallel processing of multiple resources.

#### resyncPeriod
How often the controller re-evaluates all watched resources even without changes.

## Policies Configuration

Policies define which Kubernetes resources to monitor and when to generate health events.

### Policy Structure

```yaml
kubernetes-object-monitor:
  policies:
    - name: "policy-name"
      enabled: true
      resource:
        group: ""
        version: v1
        kind: Node
      predicate:
        expression: |
          resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0
      nodeAssociation:
        expression: resource.spec.nodeName
      healthEvent:
        componentClass: Node
        isFatal: true
        message: "Error message"
        recommendedAction: CONTACT_SUPPORT
        errorCode:
          - ERROR_CODE
```

### Parameters

#### name
Unique identifier for the policy used in logs and metrics.

#### enabled
Enables or disables the policy. Disabled policies are not compiled or evaluated.

#### resource
Specifies the Kubernetes resource type to monitor.

##### group
API group of the resource. Use empty string `""` for core resources (Pod, Node, Service, etc.).

##### version
API version of the resource (e.g., `v1`, `v1beta1`).

##### kind
Kubernetes Kind of the resource (e.g., `Node`, `Pod`, `Deployment`).

#### predicate
CEL expression that evaluates to true when the resource is in an unhealthy state. Evaluated with `resource` variable containing the full resource object.

##### expression
CEL expression accessing the resource via `resource` variable.

#### nodeAssociation
Optional CEL expression that maps the resource to a specific Kubernetes node name.

##### expression
CEL expression that returns a string node name.

#### healthEvent
Defines the health event to generate when the predicate matches.

##### componentClass
Component type for the health event (e.g., `Node`, `GPU`, `Pod`).

##### isFatal
Boolean indicating if this is a fatal error that should trigger quarantine.

##### message
Human-readable error message included in the health event.

##### recommendedAction
Action code from health event proto (see [health_event.proto](https://github.com/NVIDIA/NVSentinel/blob/main/data-models/protobufs/health_event.proto)).

##### errorCode
Array of error code strings for categorization and filtering.

## CEL Expressions

### Predicate Expressions

Access the resource object via the `resource` variable.

#### Common Patterns

Check if condition exists and is True:
```yaml
expression: |
  resource.status.conditions.filter(c, c.type == "Ready" && c.status == "True").size() > 0
```

Check field value:
```yaml
expression: |
  has(resource.status.phase) && resource.status.phase == "Failed"
```

Check label exists:
```yaml
expression: |
  'failure' in resource.metadata.labels && resource.metadata.labels['failure'] == 'true'
```

### Node Association Expressions

Map resources to nodes using CEL expressions.

#### Direct Field Reference

```yaml
nodeAssociation:
  expression: resource.spec.nodeName
```

#### Using lookup() Function

The `lookup()` function retrieves other Kubernetes resources during evaluation.

##### Signature:
```text
lookup(version, kind, namespace, name) -> resource object
```

##### Parameters:
- `version` (string) - API version (e.g., "v1", "apps/v1")
- `kind` (string) - Resource Kind (e.g., "Pod", "Node")
- `namespace` (string) - Namespace (use empty string `""` for cluster-scoped resources)
- `name` (string) - Resource name

##### Examples:

Get node from pod reference:
```yaml
nodeAssociation:
  expression: |
    lookup('v1', 'Pod', resource.metadata.namespace, resource.spec.podName).spec.nodeName
```

## Policy Examples

### Example 1: Node Not Ready

Monitor nodes that are not in Ready state.

```yaml
policies:
  - name: node-not-ready
    enabled: true
    resource:
      group: ""
      version: v1
      kind: Node
    predicate:
      expression: |
        resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0
    healthEvent:
      componentClass: Node
      isFatal: true
      message: "Node is not ready"
      recommendedAction: CONTACT_SUPPORT
      errorCode:
        - NODE_NOT_READY
```

### Example 2: Node Needs Repair

Monitor custom node conditions.

```yaml
policies:
  - name: NodeNeedsRepair
    enabled: true
    resource:
      group: ""
      version: v1
      kind: Node
    predicate:
      expression: |
        resource.status.conditions.filter(c, c.type == "kubernetes.acme.com/NeedsRepair" && c.status == "True").size() > 0
    healthEvent:
      componentClass: Node
      isFatal: true
      message: "Node needs repair"
      recommendedAction: REPLACE_VM
      errorCode:
        - NODE_NEEDS_REPAIR
```

## RBAC Permissions

RBAC permissions are automatically generated based on configured policies:

- **Node resources**: Get write permissions (patch/update) for annotations
- **All other resources**: Get read-only permissions (get/list/watch)

When adding a new policy for a Custom Resource, ensure the CRD is installed before deploying the kubernetes-object-monitor.

## Policy Design Guidelines

1. **Predicate Specificity**: Write predicates that clearly identify unhealthy states
2. **Node Association**: Provide `nodeAssociation` for non-Node resources to enable quarantine
3. **Error Codes**: Use descriptive error codes for filtering and categorization
4. **Fatal vs NonFatal**: Set `isFatal: true` only for errors requiring node quarantine
5. **Testing**: Use dry-run mode to test policy expressions before production deployment
6. **Performance**: Avoid expensive operations in predicates (e.g., multiple nested lookups)
