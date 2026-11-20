# Kubernetes Object Monitor Configuration

## Overview

The kubernetes-object-monitor module watches Kubernetes resources. It generates health events when resources enter unhealthy states. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether Helm deploys the kubernetes-object-monitor module in the cluster.

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

Controls the behavior of the Kubernetes controller that watches resources.

```yaml
kubernetes-object-monitor:
  maxConcurrentReconciles: 1
  resyncPeriod: 5m
  cacheSyncTimeout: 10m
```

#### maxConcurrentReconciles

Maximum number of concurrent reconciliation workers. Higher values allow parallel processing of multiple resources.

#### resyncPeriod

How often the controller re-evaluates all watched resources, even without changes.

#### cacheSyncTimeout

Maximum time to wait for informer caches to synchronize at startup. For large clusters where the initial cache population takes longer, increase this value.

## Policies Configuration

Policies define which Kubernetes resources to monitor and when to generate health events.

The default chart configures only the `ReplaceNotReadyNode` policy. NPD condition policies are intentionally opt-in, because NVSentinel does not install or own the NPD configuration of the cluster. Before you make NPD conditions actionable, read the [NPD integration tutorial](../tutorials/integrating-node-problem-detector.md) and the NPD remediation values. The NVSentinel chart contains these values in `values-npd-remediation.yaml`.

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

Unique identifier for the policy, used in logs and metrics.

#### enabled

Enables or disables the policy. The monitor does not compile or evaluate disabled policies.

#### resource

Specifies the Kubernetes resource type to monitor.

##### group

API group of the resource. For core resources (for example, Pod, Node, and Service), use the empty string `""`.

##### version

API version of the resource (for example, `v1`, `v1beta1`).

##### kind

Kubernetes Kind of the resource (for example, `Node`, `Pod`, `Deployment`).

##### namespace

Optional namespace restriction for namespaced resources. If you set it, the monitor creates the informer cache for this resource kind only in that namespace. To watch all namespaces, leave it unset. Do not set this for cluster-scoped resources such as `Node`.

#### predicate

CEL expression that evaluates to true when the resource is in an unhealthy state. The monitor evaluates it with the `resource` variable, which contains the full resource object.

##### expression

CEL expression that accesses the resource through the `resource` variable.

#### nodeAssociation

Optional CEL expression that maps the resource to a specific Kubernetes node name.

##### expression

CEL expression that returns a string node name.

#### healthEvent

Defines the health event to generate when the predicate matches.

##### componentClass

Component type for the health event (for example, `Node`, `GPU`, `Pod`).

##### isFatal

Boolean that indicates if this is a fatal error that requires quarantine.

##### message

Human-readable error message included in the health event.

##### recommendedAction

Action code from the HealthEvent `RecommendedAction` enum (see [HealthEvent Data Model Reference](../health-event-data-model.md#recommendedaction-enum)).

##### errorCode

Array of error code strings for categorization and filtering.

##### quarantineOverrides

Optional behavior override for fault-quarantine. `force` forces node cordoning regardless of the normal rules. `skip` skips node cordoning for the generated health event. Set at most one of `force` or `skip`.

##### drainOverrides

Optional behavior override for node-drainer. `force` forces immediate pod eviction regardless of the configured namespace drain modes. `skip` skips pod eviction and marks the health event as already drained. Set at most one of `force` or `skip`.

## CEL Expressions

### Predicate Expressions

Access the resource object through the `resource` variable.

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

Map resources to nodes with CEL expressions.

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

- `version` (string) - API version (for example, "v1", "apps/v1")
- `kind` (string) - Resource Kind (for example, "Pod", "Node")
- `namespace` (string) - Namespace (for cluster-scoped resources, use the empty string `""`)
- `name` (string) - Resource name

##### Examples:

Get node from pod reference:

```yaml
nodeAssociation:
  expression: |
    lookup('v1', 'Pod', resource.metadata.namespace, resource.spec.podName).spec.nodeName
```

## Monitoring by resource kind

Each policy watches one Kubernetes resource type. Set `resource.group`, `resource.version`, and `resource.kind` accordingly. Write predicates with the [common CEL patterns](#common-patterns) on the `resource` object.

### Node

```yaml
resource:
  group: ""
  version: v1
  kind: Node
```

Nodes are cluster-scoped. Do not set `resource.namespace`. Do not set `nodeAssociation`, because the monitor automatically uses the Node name.

### Pod

```yaml
resource:
  group: ""
  version: v1
  kind: Pod
  namespace: default   # optional; omit to watch all namespaces
```

Pods must include `nodeAssociation` (usually `resource.spec.nodeName`; see [direct reference](#direct-field-reference)). Gate the predicates so that every matched Pod has a non-empty node, for example with `has(resource.spec.nodeName) && resource.spec.nodeName != ""`. Then unscheduled Pods never match. Otherwise, if the association is empty, the monitor uses the Pod name instead. For production DaemonSet operator monitoring, see [Monitoring Critical Operators](../monitoring-critical-operators.md).

### Event

```yaml
resource:
  group: events.k8s.io
  version: v1
  kind: Event
```

Events must include a `nodeAssociation` that resolves the node from the object that the Event is about. Typically, this is a `lookup()` on `resource.regarding`.

### Policy name and output

The monitor sends the policy `name` as the `checkName` of the health event. `isFatal` decides whether this becomes a node condition or a Kubernetes Event on the node. See [INTEGRATIONS](../INTEGRATIONS.md#condition-and-event-behavior).

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

Event objects do not store the node name directly. To resolve the node, use a lookup of the Pod that `resource.regarding` references.

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

### Observe a policy

```bash
NODE={test-node}

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

The Helm chart automatically generates RBAC permissions from the configured policies:

- **Node resources**: Get write permissions (patch/update) for annotations
- **All other resources**: Get read-only permissions (get/list/watch)

For a new policy on a Custom Resource, make sure that the CRD is installed before you deploy the module.

## Policy Design Guidelines

1. **Predicate Specificity**: Write predicates that identify unhealthy states precisely
2. **Node Association**: Provide `nodeAssociation` for non-Node resources to enable quarantine
3. **Error Codes**: Use descriptive error codes for filtering and categorization
4. **Fatal or NonFatal**: Set `isFatal: true` only for errors that require node quarantine
5. **Testing**: Test policy expressions with `--test-policies` before production deployment (see [Testing Policies](../kubernetes-object-monitor.md#testing-policies))
6. **Performance**: Avoid expensive operations in predicates (for example, multiple nested lookups)
7. **Namespace Scope**: For namespaced resources with many objects, set `resource.namespace` to reduce the memory usage of the informer cache
