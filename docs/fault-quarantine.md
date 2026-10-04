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

1. **Recovers unresolved events on startup** before moving to live events
2. **Evaluates the event** against configurable CEL rules to determine if quarantine is needed
3. **Checks the circuit breaker** (if enabled) to ensure it's safe to take action
4. **Takes quarantine action** if rules match:
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

**Key feature**: The CEL evaluator has access to the Kubernetes Node's `metadata` and `spec`. Node `status` is not cached or available to Fault Quarantine rules.

**Example rule capabilities:**
- Quarantine nodes with fatal XID errors: `event.errorCode.contains("XID-48")`
- Check node labels: `node.metadata.labels["node-type"] == "training"`
- Evaluate multiple conditions: `event.isFatal && node.metadata.labels["environment"] == "production"`
- Check scheduling state: `node.spec.unschedulable == false`
- Skip quarantine for certain nodes or environments

## Configuration

See [Fault Quarantine configuration](configuration/fault-quarantine.md) for the full Helm reference, including how to add, modify, and verify rule sets.

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
      # Optional label configuration
      # label:
      #   key: "nvidia.com/gpu-fault"
      #   value: "active"
```

### Defining CEL Rules

Rules are defined using rulesets that evaluate CEL expressions. Each ruleset has:

**Match Conditions**: CEL expressions that determine when the ruleset triggers
- `kind: "HealthEvent"` - Expressions evaluated against the health event (agent, isFatal, checkName, etc.)
- `kind: "Node"` - Expressions evaluated against the Kubernetes Node `metadata` and `spec`

**Actions**: What happens when conditions match
- `cordon.shouldCordon: true` - Cordon (mark unschedulable) the node
- `taint` (optional) - Apply Kubernetes taints to the node
- `label` (optional) - Apply a Kubernetes label until all tracked faults recover

**Configuration options:**
- **Dry Run**: Test rules without cordoning nodes
- **Circuit Breaker**: Prevents cordoning too many nodes at once. See [Circuit Breaker documentation](circuit-breaker.md)
- **Label Prefix**: Customize the prefix for tracking labels and annotations on nodes
- **Multiple Rulesets**: Define different rules for different failure types with CEL expressions that access either the health event or Node metadata/spec

#### Cached Node Fields

The `node` variable is served from the informer cache, not fetched per evaluation. Rules are read once at startup, so the label and annotation keys each expression reads are derived from the compiled CEL, and the cache keeps only those keys. `node.status` is cleared. `node.spec` and the identity metadata are kept whole, because the cordon path reads them. The retained keys are logged at startup, so you can see the effect of a rule change without reading the code.

Fault Quarantine also keeps the keys it uses itself, whatever your rules say: the quarantine annotations, the node state label, the GPU node label, the six cordon and uncordon tracking labels, and any label a ruleset applies.

**How you write a rule changes what the cache keeps.** Two rules that select the same nodes can prune differently, so the form matters for memory even when the result is identical.

These forms keep one key each, which is the cheapest shape:

```text
'k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels
node.metadata.labels['nvidia.com/gpu.present'] == 'true'
node.metadata.annotations['maintenance'] == 'false'
```

These forms keep the whole label map, because the key is not known until the rule runs:

```text
size(node.metadata.labels) > 0
node.metadata.labels.exists(k, k.startsWith('nvidia.com/'))
node.metadata.labels[node.spec.nodeName] == 'true'
node.metadata.labels['nvidia.com/' + 'gpu.present'] == 'true'
```

The last one is worth noting: the key is built at run time, so it keeps the whole map even though both halves are string literals. Write the key as a single literal to keep one entry.

These forms give up pruning for the node altogether, because no set of keys describes what they read:

```text
size(node)
```

An expression that does not compile has the same effect.

Two properties follow from this, and both surprise people:

- The retained set is a union across every ruleset, and "keep everything" wins. **One unprunable expression in one ruleset turns off label pruning for every node and every rule.** If the startup log shows `retainedLabels="<all>"`, look for the expression that caused it.
- Rulesets are read whether or not they are enabled. A disabled ruleset with an unprunable expression still turns pruning off. Remove such a ruleset rather than disabling it if you want the saving.

Prefer the guarded literal-key form in any case. It is also what correctness needs: reading a label that is absent raises an error rather than returning false, so an optional label has to be read behind an `in` guard.

## Key Features

### Entity-Level Tracking
Tracks health issues at the entity level (e.g., individual GPUs), not just at the node level:
- Fine-grained visibility into which specific components are failing
- Track multiple issues on the same node
- Partial recovery scenarios where some GPUs recover while others remain faulty

### Flexible CEL Rules with Node Context
CEL expressions receive either the health event or the Kubernetes Node's metadata and spec:
- Cordon based on node labels (e.g., environment, node type, GPU model)
- Check scheduling and taint configuration
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
