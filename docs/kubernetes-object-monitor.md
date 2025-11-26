# Kubernetes Object Monitor

## Overview

The Kubernetes Object Monitor watches any Kubernetes resource (nodes, pods, custom resources, etc.) and generates health events when they enter unhealthy states. It's a policy-based monitor that uses CEL (Common Expression Language) expressions to detect problems in your cluster resources.

Think of it as a customizable watchdog for your Kubernetes cluster - you define what "unhealthy" means for different resources, and it alerts NVSentinel when problems occur.

### Why Do You Need This?

While NVSentinel includes specialized monitors for GPUs and system logs, your cluster health depends on many other factors:

- **Node conditions**: Nodes can become NotReady, have disk pressure, memory pressure, or network issues
- **Custom resources**: Your application's CRDs (Custom Resource Definitions) may have status fields indicating failures
- **Application-specific health**: Resources managed by your operators or controllers may need monitoring
- **Integration with existing systems**: Quickly integrate external monitoring systems or tools into NVSentinel by exposing their status as Kubernetes resources

The Kubernetes Object Monitor fills these gaps by letting you define custom health checks for any resource in your cluster using simple CEL expressions. This provides a quick way to integrate existing systems into NVSentinel without writing custom monitors.

## How It Works

The monitor operates using policies that you define:

1. **Watch resources**: Uses Kubernetes controllers to watch specified resource types (Nodes, Pods, Jobs, CRDs, etc.)
2. **Evaluate health state**: Evaluates CEL expressions against resource state
3. **Detect unhealthy state**: When a CEL expression evaluates to **true**, the resource is considered unhealthy and an unhealthy event is generated
4. **Detect recovery**: When a CEL expression evaluates to **false**, the resource is considered healthy and a healthy event is automatically sent
5. **Map to nodes**: Associates the health event with a specific node using CEL expressions
6. **Publish events**: Sends health events to Platform Connectors for processing by NVSentinel core modules

The monitor automatically creates Kubernetes RBAC permissions based on your policies, granting read access to the resources you want to monitor.

## Configuration

Configure the Kubernetes Object Monitor through Helm values by defining policies:

```yaml
kubernetes-object-monitor:
  enabled: true
  
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
```

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
- `resource`: The Kubernetes resource being evaluated
- `now`: Current timestamp
- `lookup(version, kind, namespace, name)`: Fetch related resources

#### Node Association (Optional)
```yaml
nodeAssociation:
  expression: resource.spec.nodeName  # CEL expression that returns node name
```

For resources that don't directly reference a node, you can use `lookup()` to traverse relationships:

```yaml
nodeAssociation:
  # Get node from a related Pod
  expression: |
    lookup('v1', 'Pod', resource.metadata.namespace, resource.spec.podName).spec.nodeName
```

#### Health Event Template
```yaml
healthEvent:
  componentClass: Node           # Component type (Node, GPU, etc.)
  isFatal: true                 # Severity flag
  message: "Node is not ready"  # Human-readable message
  recommendedAction: CONTACT_SUPPORT  # Action hint
  errorCode:
    - NODE_NOT_READY            # Error codes for classification
```

## Key Features

### Policy-Based Monitoring
Define custom health checks for any Kubernetes resource using declarative policies - no code required.

### CEL Expression Language
Use CEL for flexible, powerful condition evaluation with access to the full resource object.

### Resource Relationships
The `lookup()` function lets you traverse resource relationships to associate health events with nodes.

### Automatic RBAC
Kubernetes permissions are automatically generated based on your policies - you don't manage RBAC manually.

### State Tracking
Maintains state for each resource to detect transitions between healthy and unhealthy states.

### Extensible
Monitor any resource: core resources (Nodes, Pods), namespaced resources, cluster-scoped resources, or CRDs.

### Controller-Runtime Based
Uses Kubernetes controller-runtime for efficient, scalable resource watching with caching.
