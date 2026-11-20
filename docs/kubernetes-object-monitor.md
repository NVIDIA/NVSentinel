# Kubernetes Object Monitor

## Overview

The Kubernetes Object Monitor watches any Kubernetes resource (for example, nodes, pods, and custom resources). It generates health events when these resources enter unhealthy states. It is a policy-based monitor that uses CEL (Common Expression Language) expressions to detect problems in your cluster resources.

The monitor operates as a customizable watchdog for your Kubernetes cluster. You define what "unhealthy" means for different resources. The monitor then alerts NVSentinel when problems occur.

### Why Do You Need This?

NVSentinel includes specialized monitors for GPUs and system logs. However, your cluster health depends on many other factors:

- **Node conditions**: Nodes can become NotReady, have disk pressure, memory pressure, or network issues
- **Custom resources**: Your application's CRDs (Custom Resource Definitions) can have status fields that indicate failures
- **Application-specific health**: Resources that your operators or controllers manage can also need monitoring
- **Integration with existing systems**: Quickly integrate external monitoring systems or tools into NVSentinel, by exposing their status as Kubernetes resources

The Kubernetes Object Monitor fills these gaps. It lets you define custom health checks for any resource in your cluster with simple CEL expressions. This is a quick way to integrate existing systems into NVSentinel without the need to write custom monitors.

## How It Works

The monitor uses policies that you define:

1. **Watch resources**: Uses Kubernetes controllers to watch specified resource types (for example, Nodes, Pods, Jobs, and CRDs)
2. **Evaluate health state**: Evaluates CEL expressions against resource state
3. **Detect unhealthy state**: When a CEL expression evaluates to **true**, the monitor considers the resource unhealthy and generates an unhealthy health event
4. **Detect recovery**: When a CEL expression evaluates to **false**, the monitor considers the resource healthy and automatically sends a healthy health event
5. **Map to nodes**: Associates the health event with a specific node, with CEL expressions
6. **Publish events**: Sends health events to Platform Connectors, so that the NVSentinel core modules process them

The Helm chart automatically generates Kubernetes RBAC permissions from your policies. These permissions grant read access to the resources that you want to monitor.

## Configuration

To configure the Kubernetes Object Monitor, enable it and define policies in the Helm values:

```yaml
global:
  kubernetesObjectMonitor:
    enabled: true

kubernetes-object-monitor:
  maxConcurrentReconciles: 1
  resyncPeriod: 5m
  
  policies:
    # Example 1: Monitor node readiness
    - name: node-not-ready
      enabled: true
      resource:
        group: ""         # Core API group (empty string)
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
    
    # Example 2: Monitor custom resource with node association
    - name: gpu-job-failed
      enabled: true
      resource:
        group: batch.example.com
        version: v1alpha1
        kind: GPUJob
        namespace: gpu-operator  # Optional: restrict informer cache to one namespace
      predicate:
        # Detect when job fails
        expression: |
          has(resource.status.state) && resource.status.state == "Failed"
      nodeAssociation:
        # Map this job to a specific node
        expression: resource.spec.nodeName
      healthEvent:
        componentClass: GPU
        isFatal: false
        message: "GPU job failed on node"
        recommendedAction: CONTACT_SUPPORT
        errorCode:
          - GPU_JOB_FAILED
```

### Policy Configuration

Each policy has these components:

#### Resource Selection

```yaml
resource:
  group: ""              # API group (empty for core resources)
  version: v1            # API version
  kind: Node             # Resource kind
  # namespace: gpu-operator # Optional, only for namespaced resources
```

To watch all namespaces for that resource kind, leave `namespace` unset. For namespaced resources with large object counts, a namespace value reduces the memory usage of the informer cache. Do not set it for cluster-scoped resources such as `Node`.

#### Predicate (Detection Logic)

```yaml
predicate:
  expression: |
    # CEL expression that returns true when resource is unhealthy
    # When true: unhealthy event is sent
    # When false: healthy event is automatically sent
    resource.status.conditions.filter(c, c.type == "Ready" && c.status == "False").size() > 0
```

Available variables in predicates:

- `resource`: The Kubernetes resource that the monitor evaluates
- `now`: Current timestamp
- `lookup(version, kind, namespace, name)`: Fetch related resources

#### Node Association (Optional)

```yaml
nodeAssociation:
  expression: resource.spec.nodeName  # CEL expression that returns node name
```

For resources that do not directly reference a node, you can use `lookup()` to traverse relationships:

```yaml
nodeAssociation:
  # Get node from a related Pod
  expression: |
    lookup('v1', 'Pod', resource.metadata.namespace, resource.spec.podName).spec.nodeName
```

#### Cached Fields

The monitor serves the `resource` variable from the informer cache. It does not fetch the resource for each evaluation. The monitor reads the policies only one time, at startup. Thus, it derives the fields that each expression touches from the compiled CEL. The cache keeps only those fields, plus the metadata that the informer itself needs (`apiVersion`, `kind`, `metadata.name`, `metadata.namespace`, `metadata.uid`, `metadata.resourceVersion`, `metadata.deletionTimestamp`).

A node object is mostly labels, annotations, managed fields and cached image lists. A typical predicate reads none of these fields. Thus, on large clusters, this pruning cuts the cache to a fraction of its full size. At startup, the monitor logs the retained fields for each resource kind.

A recorded field keeps its whole subtree. Thus, comprehensions and computed map keys need no special care. For example, `resource.status.conditions.filter(c, c.type == "Ready")` keeps every field of every condition. Also, `resource.metadata.labels[resource.spec.nodeName]` keeps all of `metadata.labels`.

If the expression reads a map key as a string literal, the cache keeps only that entry, not the whole map. This also applies to a membership test: `'nvidia.com/gpu.present' in resource.metadata.labels` keeps only that key. A key that the expression computes keeps the whole map, as shown above. Thus, two policies that select the same objects can cache different amounts. Where you have the choice, prefer literal keys.

Some expressions use the resource as a whole, not through a field access, for example `size(resource)`. The monitor cannot reduce such an expression to a set of fields. The monitor then caches that resource kind in full. This is correct, but it loses the saving for every policy on that kind. Prefer to read the specific fields that you need.

The same derivation covers `lookup()`. Some calls give their apiVersion and kind as string literals, as in `lookup('v1', 'Pod', resource.metadata.namespace, resource.spec.podName).spec.nodeName`. For such a call, the monitor also derives the fields that the call reads from the returned object. The monitor then caches that kind cluster-wide, pruned to these fields.

Such a kind needs `get`, `list` and `watch` on it cluster-wide:

- `get` for the calls that read through the API server
- `list` and `watch`, because the cache watches every object of the kind

The generated rules cover only the kinds that policies watch. For a kind that a policy looks up but no policy watches, grant all three in the ClusterRole. If `list` and `watch` are missing, the call reads through the API server instead. The call logs this one time.

The first call that needs the informer for such a kind creates it. The informer lists every object of the kind before it can answer. Until the informer has synced, calls read through the API server. Thus, no evaluation waits for the informer.

A call always reads through the API server in these cases:

- The call computes its apiVersion or kind.
- The expression uses the result of the call as a whole.
- The call is in an expression that uses the watched resource as a whole.
- The call names a kind that the monitor caches only for particular namespaces.

Each read is then one request for each evaluation. Thus, a policy that looks up a literal kind and reads named fields from it is much cheaper.

#### Health Event Template

```yaml
healthEvent:
  componentClass: Node           # Component type (Node, GPU, etc.)
  isFatal: true                 # Severity flag
  message: "Node is not ready"  # Human-readable message
  recommendedAction: CONTACT_SUPPORT  # Action hint
  errorCode:
    - NODE_NOT_READY            # Error codes for classification
  quarantineOverrides:          # Optional: override node cordon behavior
    force: true                 # Or use skip: true; do not set both
  drainOverrides:               # Optional: override pod eviction behavior
    skip: true                  # Or use force: true; do not set both
```

For each override block, `force` and `skip` are mutually exclusive. If this policy must do the action regardless of the normal rules, use `force`. If this policy must bypass the action, use `skip`.

## Testing Policies

The monitor binary can check policies against test cases without a cluster. Pass these flags:

- `--test-policies` with a YAML file of cases
- `--policy-config-path` with the rendered policy config (the `config.toml` key of the `kubernetes-object-monitor` ConfigMap)

The monitor evaluates each case with the same evaluator that it uses in the cluster. It prints one line for each case, and then it exits.

```yaml
# Cluster state that lookup() sees.
objects:
  - apiVersion: v1
    kind: Node
    metadata:
      name: node-a
      labels:
        nvidia.com/gpu.present: "true"
cases:
  - name: Rebooted event on a GPU node
    policy: node-rebooted          # Policy name from the config
    resource:                      # The object the monitor would watch
      apiVersion: v1
      kind: Event
      metadata: {name: node-a.1, namespace: default}
      reason: Rebooted
      involvedObject: {kind: Node, name: node-a}
    expect:
      match: true                  # Required: expected predicate result
      node: node-a                 # Optional: expected node association
```

The example tests this policy, which the Helm values define:

```yaml
kubernetes-object-monitor:
  policies:
    - name: node-rebooted
      enabled: true
      resource: {group: "", version: v1, kind: Event}
      predicate:
        expression: |
          resource.reason == 'Rebooted' &&
          lookup('v1', 'Node', '', resource.involvedObject.name).metadata.labels['nvidia.com/gpu.present'] == 'true'
      nodeAssociation:
        expression: resource.involvedObject.name
      healthEvent:
        componentClass: Node
        isFatal: true
        message: Node rebooted
        recommendedAction: CONTACT_SUPPORT
        errorCode: [NODE_REBOOTED]
```

To render the policy config from the chart and your values:

```bash
helm template nvsentinel oci://ghcr.io/nvidia/nvsentinel --version <version> -f values.yaml \
  --set global.kubernetesObjectMonitor.enabled=true \
  --show-only charts/kubernetes-object-monitor/templates/configmap.yaml \
  | yq 'select(.kind == "ConfigMap") | .data["config.toml"]' > config.toml
```

Then run the tests:

```bash
docker run --rm -v "$PWD:/tests" ghcr.io/nvidia/nvsentinel/kubernetes-object-monitor:<version> \
  --policy-config-path /tests/config.toml --test-policies /tests/policy-tests.yaml
```

```text
PASS  Rebooted event on a GPU node  [node-rebooted] match=true node=node-a

1 passed, 0 failed
```

A case fails in these conditions:

- The predicate result or the node differs from `expect`.
- An expression returns an error.
- The resource is a kind or namespace that the policy does not watch.

Every object and case resource needs `apiVersion`, `kind` and `metadata.name`. The exit code is 0 when every case passes, 1 when any case fails, and 2 when the config or test file is invalid.

`lookup()` only finds the objects that the test file lists. For expressions that use `now`, the tool uses the current time. The tool does not contact the API server. Thus, it does not check that `resource.namespace` is set only for a namespaced kind. The monitor does this check when it starts.

## Key Features

### Policy-Based Monitoring

Define custom health checks for any Kubernetes resource with declarative policies. No code is necessary.

### CEL Expression Language

Use CEL for flexible condition evaluation with access to the full resource object.

### Resource Relationships

The `lookup()` function lets you traverse resource relationships to associate health events with nodes.

### Automatic RBAC

The Helm chart automatically generates the Kubernetes permissions for the kinds that your policies watch. For a kind that a policy looks up but no policy watches, grant `get`, `list` and `watch` yourself. See [Cached Fields](#cached-fields).

### State Tracking

Maintains state for each resource to detect transitions between healthy and unhealthy states.

The monitor saves each unhealthy match in the `nvsentinel.nvidia.com/k8s-object-monitor-policy-matches` annotation on the node. When the monitor starts, it reads these annotations and checks each saved resource again. If a resource became healthy or was deleted while the monitor was stopped, the monitor sends a healthy health event for it. Thus, its node can recover. If the monitor cannot read the node annotations at startup, it does not start, and Kubernetes restarts it.

### Extensible

Monitor any resource: core resources (Nodes, Pods), namespaced resources, cluster-scoped resources, or CRDs.

### Controller-Runtime Based

Uses Kubernetes controller-runtime for efficient, scalable resource watching with caching.
