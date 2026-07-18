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
        # namespace: gpu-operator  # Optional, only for namespaced resources
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
        quarantineOverrides:
          force: true  # Or use skip: true; do not set both
        drainOverrides:
          skip: true   # Or use force: true; do not set both
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

##### namespace
Optional namespace restriction for namespaced resources. When set, the monitor creates the informer cache for this resource kind only in that namespace. Leave unset to watch all namespaces. Do not set this for cluster-scoped resources such as `Node`.

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

##### quarantineOverrides
Optional behavior override for fault-quarantine. `force` forces node cordoning regardless of normal rules; `skip` skips node cordoning for the generated health event. Set at most one of `force` or `skip`.

##### drainOverrides
Optional behavior override for node-drainer. `force` forces immediate pod eviction regardless of configured namespace drain modes; `skip` skips pod eviction and marks the event as already drained. Set at most one of `force` or `skip`.

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

## Monitoring by resource kind

Each policy watches one Kubernetes resource type. Set `resource.group`, `resource.version`,
and `resource.kind` accordingly. Write predicates with the [common CEL patterns](#common-patterns)
on the `resource` object.

### Node

```yaml
resource:
  group: ""
  version: v1
  kind: Node
```

Nodes are cluster-scoped — do not set `resource.namespace`. Do not set `nodeAssociation`; the
monitor uses the Node name automatically ([ADR-011](../designs/011-kubernetes-object-monitor.md)).

### Pod

```yaml
resource:
  group: ""
  version: v1
  kind: Pod
  namespace: default   # optional; omit to watch all namespaces
```

Pods must include `nodeAssociation` (usually `resource.spec.nodeName`; see
[direct reference](#direct-field-reference)). Gate predicates so every matched Pod has a
non-empty node — e.g. `has(resource.spec.nodeName) && resource.spec.nodeName != ""` — so
unscheduled Pods never match; otherwise the reconciler falls back to the Pod name when
association is empty. For production DaemonSet operator monitoring, see
[Monitoring Critical Operators](../monitoring-critical-operators.md).

### Event

```yaml
resource:
  group: events.k8s.io
  version: v1
  kind: Event
```

Events must include `nodeAssociation` that resolves the node from the object the event is
about — typically a `lookup()` on `resource.regarding` ([ADR-011](../designs/011-kubernetes-object-monitor.md)).

### Policy name and output

The policy `name` is sent as the health event `checkName`. Whether that becomes a node
condition or a Kubernetes Event on the node depends on `isFatal` — see
[INTEGRATIONS](../INTEGRATIONS.md#condition-vs-event-behavior).

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

### Example 3: Warning Event on Pod

Event objects do not store the node name directly. Resolve the node by looking up the Pod
referenced by `resource.regarding`.

```yaml
policies:
  - name: pod-warning-event
    enabled: true
    resource:
      group: events.k8s.io
      version: v1
      kind: Event
    predicate:
      # Guard the Pod lookup so a deleted/absent Pod or empty spec.nodeName cannot
      # fall back to the Event name for node association.
      expression: |
        resource.type == 'Warning' &&
        resource.regarding.kind == 'Pod' &&
        has(resource.regarding.namespace) &&
        has(resource.regarding.name) &&
        (now - timestamp(resource.eventTime)) < duration('10m') &&
        lookup('v1', 'Pod', resource.regarding.namespace, resource.regarding.name) != null &&
        has(lookup('v1', 'Pod', resource.regarding.namespace, resource.regarding.name).spec.nodeName) &&
        lookup('v1', 'Pod', resource.regarding.namespace, resource.regarding.name).spec.nodeName != ""
    nodeAssociation:
      expression: |
        lookup('v1', 'Pod', resource.regarding.namespace, resource.regarding.name).spec.nodeName
    healthEvent:
      componentClass: Software
      isFatal: false
      message: "Recent Warning event on pod"
      recommendedAction: CONTACT_SUPPORT
      errorCode:
        - POD_WARNING_EVENT
```

### Example 4: Pod Failed phase

Phase predicate from [Check field value](#common-patterns) plus Pod `nodeAssociation`.

```yaml
policies:
  - name: pod-failed-on-node
    enabled: true
    resource:
      group: ""
      version: v1
      kind: Pod
      namespace: default
    predicate:
      expression: |
        has(resource.spec.nodeName) && resource.spec.nodeName != "" &&
        resource.status.phase == 'Failed'
    nodeAssociation:
      expression: resource.spec.nodeName
    healthEvent:
      componentClass: Pod
      isFatal: false
      message: "Pod entered Failed phase"
      recommendedAction: CONTACT_SUPPORT
      errorCode:
        - POD_FAILED
```

For a fatal GPU-runtime Event policy, see the `NVMLError` example in
[ADR-011](../designs/011-kubernetes-object-monitor.md).

### Observe a policy

```bash
NODE=<test-node>

# Example 4 — Pod exits immediately; the generated Warning events also exercise Example 3
kubectl run kom-pod-fail --restart=Never -n default --image=busybox \
  --overrides='{"spec":{"nodeName":"'"$NODE"'","containers":[{"name":"kom-pod-fail","image":"busybox","command":["sh","-c","exit 1"]}]}}'

kubectl logs -n nvsentinel -l app.kubernetes.io/name=kubernetes-object-monitor --tail=30

# Examples 3 & 4 use isFatal: false — verify Node events ([INTEGRATIONS](../INTEGRATIONS.md#using-events-for-non-fatal-errors))
kubectl get events --field-selector involvedObject.kind=Node,involvedObject.name=$NODE \
  --sort-by='.lastTimestamp' | tail -10

# Cleanup
kubectl delete pod kom-pod-fail -n default --ignore-not-found
```

### One-shot AI prompt

```text
Create a Helm values overlay for NVSentinel kubernetes-object-monitor policies on Node, Pod, or
events.k8s.io/v1/Event. Include nodeAssociation for Pod/Event (omit for Node). Output values
YAML plus kubectl trigger/verify commands.
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
7. **Namespace Scope**: For namespaced resources with many objects, set `resource.namespace` to reduce informer cache memory usage
