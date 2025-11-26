# Fault Quarantine

## Overview

The Fault Quarantine module is NVSentinel's first line of defense against faulty GPU nodes. When health monitors detect a problem with a GPU node, this module decides whether the node should be quarantined (isolated from the cluster) to prevent the issue from impacting your workloads.

Think of it as a security checkpoint that prevents potentially dangerous nodes from receiving new work. Similar to how a hospital isolates patients with contagious diseases to protect others, Fault Quarantine isolates problematic GPU nodes to protect your cluster and workloads.

### Why Do You Need This?

GPU hardware failures can cause serious problems if left unaddressed:

- **Silent data corruption**: Faulty GPUs can produce incorrect results in AI training or simulations, wasting days or weeks of compute time
- **Cascading job failures**: One bad GPU can crash an entire multi-GPU distributed training job
- **Wasted resources**: Other healthy GPUs sit idle waiting for the faulty node to catch up
- **Poor cluster utilization**: Kubernetes continues scheduling work on broken nodes because it doesn't know they're faulty

The Fault Quarantine module solves this by:
- **Preventing new workloads** from being scheduled on faulty nodes (cordoning)
- **Marking nodes** with specific failure information (tainting and labeling)
- **Providing visibility** through annotations about what's wrong and when it happened

## How It Works

The Fault Quarantine module continuously watches the datastore for health events reported by the health monitors (GPU Health Monitor, Syslog Health Monitor, CSP Health Monitor). When a health event arrives, the module:

1. **Evaluates the event** against configurable CEL rules to determine if quarantine is needed
2. **Checks the circuit breaker** (if enabled) to ensure it's safe to take action
3. **Takes quarantine action** if rules match:
   - **Cordons the node**: Sets the node to "unschedulable" so no new pods are placed on it
   - **Applies taints**: Adds Kubernetes taints to repel running pods (optional)
   - **Updates annotations**: Records detailed information about why and when the node was quarantined
   - **Sets labels**: Adds searchable labels for cluster operators

**When a node is quarantined:**
- Kubernetes will not schedule any new pods on the node
- Existing pods continue running (unless taints force them to evacuate)
- The node remains part of the cluster and is fully visible
- Detailed diagnostic information is attached to the node for troubleshooting

**When a healthy event arrives for a quarantined node:**
- If all health checks have recovered, the module automatically removes the quarantine
- The node is uncordoned and returned to normal scheduling
- Quarantine annotations are cleaned up

### Rule-Based Decision Making with CEL

The Fault Quarantine module uses CEL (Common Expression Language) to define flexible rules for when nodes should be quarantined. CEL is a simple expression language that lets you write conditions like "if this happens, then quarantine the node."

**Key feature**: The CEL evaluator has access to the complete Kubernetes Node object, giving you flexibility to make quarantine decisions based on any node attribute - labels, annotations, conditions, capacity, or any other node metadata.

**Example rule capabilities:**
- Quarantine nodes with fatal XID errors: `event.errorCode.contains("XID-48")`
- Check node labels: `node.metadata.labels["node-type"] == "training"`
- Evaluate multiple conditions: `event.isFatal && node.metadata.labels["environment"] == "production"`
- Access node capacity: `node.status.capacity["nvidia.com/gpu"] > 8`
- Skip quarantine for certain nodes or environments

## Configuration

Configure the Fault Quarantine module through Helm values:

```yaml
fault-quarantine:
  enabled: true           # Enable the module
  dryRun: false          # Live mode - execute actions; set to true to log actions without executing
  
  circuitBreaker:
    enabled: true        # Safety feature to prevent mass cordoning
    percentage: 50       # Max % of nodes that can be cordoned
    duration: "5m"       # Time window for percentage calculation
  
  labelPrefix: "k8saas.nvidia.com/"  # Prefix for node labels and annotations
  
  ruleSets:
    - version: "1"
      name: "GPU fatal error ruleset"
      match:
        all:
          - kind: "HealthEvent"
            expression: "event.agent == 'gpu-health-monitor' && event.isFatal == true"
          - kind: "Node"
            expression: "!('k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels)"
      cordon:
        shouldCordon: true
      # Optional taint configuration
      # taint:
      #   key: "nvidia.com/gpu-error"
      #   value: "fatal"
      #   effect: "NoSchedule"
```

### Defining CEL Rules

Rules are defined using rulesets that evaluate CEL expressions. Each ruleset has:

**Match Conditions**: CEL expressions that determine when the ruleset triggers
- `kind: "HealthEvent"` - Expressions evaluated against the health event (agent, isFatal, checkName, etc.)
- `kind: "Node"` - Expressions evaluated against the Kubernetes Node object (labels, capacity, conditions, etc.)

**Actions**: What happens when conditions match
- `cordon.shouldCordon: true` - Cordon (mark unschedulable) the node
- `taint` (optional) - Apply Kubernetes taints to the node

**Configuration options:**
- **Dry Run**: Test rules without cordoning nodes
- **Circuit Breaker**: Prevents cordoning too many nodes at once. See [Circuit Breaker documentation](circuit-breaker.md)
- **Label Prefix**: Customize the prefix for tracking labels and annotations on nodes
- **Multiple Rulesets**: Define different rules for different failure types with CEL expressions that have access to both the health event and full Node object

## Key Features

### Entity-Level Tracking
Tracks health issues at the entity level (e.g., individual GPUs), not just at the node level:
- Fine-grained visibility into which specific components are failing
- Track multiple issues on the same node
- Partial recovery scenarios where some GPUs recover while others remain faulty

### Flexible CEL Rules with Node Context
CEL expressions receive both the health event and the complete Kubernetes Node object:
- Cordon based on node labels (e.g., environment, node type, GPU model)
- Access node capacity and conditions
- Skip quarantine for nodes with specific annotations
- Different thresholds based on any node metadata

### Automatic Recovery
When all health checks return to healthy state:
- Node is automatically uncordoned
- Taints are removed
- Quarantine annotations are cleaned up
- Node returns to normal scheduling

### Circuit Breaker Integration
Built-in protection against mass cordoning. See [Circuit Breaker documentation](circuit-breaker.md) for details.
