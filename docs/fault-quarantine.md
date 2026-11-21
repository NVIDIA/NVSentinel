# Fault Quarantine

## Overview

The Fault Quarantine module is the first protection that NVSentinel gives against faulty GPU nodes. When health monitors detect a problem with a GPU node, Fault Quarantine decides whether to quarantine the node. Quarantine isolates the node from the cluster, so that the problem does not affect your workloads.

Fault Quarantine acts as a security checkpoint. It stops new work on nodes that can be dangerous. A hospital isolates patients who have contagious diseases to protect other patients. In the same way, Fault Quarantine isolates GPU nodes that have problems, to protect your cluster and workloads.

### Why Do You Need This?

If you do not correct a GPU hardware failure, it can cause serious problems:

- **Silent data corruption**: Faulty GPUs can produce incorrect results in AI training or simulations. This wastes days or weeks of compute time.
- **Cascading job failures**: One faulty GPU can crash a full multi-GPU distributed training job.
- **Wasted resources**: Other healthy GPUs stay idle while they wait for the faulty node to reach the same step.
- **Poor cluster utilization**: Kubernetes continues to schedule work on broken nodes, because it does not know that they are faulty.

Fault Quarantine solves these problems in these ways:

- **Prevents new workloads** on faulty nodes. It cordons each faulty node, so that Kubernetes does not schedule new workloads on it.
- **Marks nodes** with specific failure information. It applies taints and labels to the nodes.
- **Gives visibility** through annotations. The annotations show what is wrong and when it occurred.

## How It Works

Fault Quarantine continuously watches the datastore for health events from the health monitors (GPU Health Monitor, Syslog Health Monitor, CSP Health Monitor). When a health event arrives, Fault Quarantine does these steps:

1. **Recovers unresolved health events on startup**, before it processes live health events
2. **Evaluates the health event** against configurable CEL rules, to decide if the node needs quarantine
3. **Checks the circuit breaker** (if enabled), to make sure that it is safe to take action
4. **Takes quarantine action** if the rules match:
   - **Cordons the node**: Sets the node to "unschedulable", so that Kubernetes places no new pods on it
   - **Applies taints**: Adds Kubernetes taints, which can prevent scheduling or evict running pods, based on their effect (optional)
   - **Updates annotations**: Records detailed information about why and when Fault Quarantine quarantined the node
   - **Sets labels**: Adds searchable labels for cluster operators

**When Fault Quarantine quarantines a node:**

- Kubernetes does not schedule any new pods on the node
- Existing pods continue to run, unless taints force them to leave the node
- The node stays part of the cluster and is fully visible
- Fault Quarantine attaches detailed diagnostic information to the node for troubleshooting

**When a health event reports a healthy status for a quarantined node:**

- If all health checks are healthy again, Fault Quarantine automatically removes the quarantine
- Fault Quarantine uncordons the node, and the node returns to normal scheduling
- Fault Quarantine removes the quarantine annotations

### Rule-Based Decision Making with CEL

Fault Quarantine uses CEL (Common Expression Language) rules to decide when to quarantine a node. CEL is a simple expression language. With CEL, you write conditions such as "if this happens, then quarantine the node."

**Key feature**: The CEL evaluator can read the `metadata` and `spec` of the Kubernetes Node. Fault Quarantine does not cache the Node `status`, and rules cannot read it.

**Example rule capabilities:**

- Quarantine nodes with fatal XID errors: `event.errorCode.contains("XID-48")`
- Check node labels: `node.metadata.labels["node-type"] == "training"`
- Evaluate multiple conditions: `event.isFatal && node.metadata.labels["environment"] == "production"`
- Check scheduling state: `node.spec.unschedulable == false`
- Skip quarantine for certain nodes or environments

## Configuration

For the full Helm reference, see [Fault Quarantine configuration](configuration/fault-quarantine.md). It also shows how to add, modify, and verify rulesets.

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

You define rules in rulesets. A ruleset evaluates CEL expressions. Each ruleset has these parts:

**Match Conditions**: CEL expressions that decide when the ruleset triggers

- `kind: "HealthEvent"` - Expressions that Fault Quarantine evaluates against the health event (agent, isFatal, checkName and other fields)
- `kind: "Node"` - Expressions that Fault Quarantine evaluates against the Kubernetes Node `metadata` and `spec`

**Actions**: What occurs when the conditions match

- `cordon.shouldCordon: true` - Cordon (mark unschedulable) the node
- `taint` (optional) - Apply Kubernetes taints to the node
- `label` (optional) - Apply a Kubernetes label until all tracked faults recover

**Configuration options:**

- **Dry Run**: Test rules without cordoning nodes
- **Circuit Breaker**: Prevents cordoning too many nodes at the same time. See [Circuit Breaker documentation](circuit-breaker.md)
- **Label Prefix**: Customize the prefix for tracking labels and annotations on nodes
- **Multiple Rulesets**: Define different rules for different failure types. Their CEL expressions access either the health event or the Node metadata and spec.

#### Cached Node Fields

The `node` variable comes from the informer cache. Fault Quarantine does not fetch the node again for each evaluation. Fault Quarantine reads the rules one time, at startup. From the compiled CEL, it finds the label and annotation keys that each expression reads. The cache keeps only those keys.

The cache clears `node.status`. It keeps `node.spec` and the identity metadata whole, because Fault Quarantine reads them when it cordons a node. At startup, Fault Quarantine logs the retained keys. Use this log to see the effect of a rule change without reading the code.

Fault Quarantine also keeps the keys that it uses itself, whatever your rules say:

- the quarantine annotations
- the node state label
- the GPU node label
- the six cordon and uncordon tracking labels
- any label that a ruleset applies

**How you write a rule changes what the cache keeps.** Two rules that select the same nodes can prune the cache differently. Thus the form of a rule changes memory use, even when the result is identical.

Each of these forms keeps one key, which is the cheapest shape:

```text
'k8saas.nvidia.com/ManagedByNVSentinel' in node.metadata.labels
node.metadata.labels['nvidia.com/gpu.present'] == 'true'
node.metadata.annotations['maintenance'] == 'false'
```

These forms keep the whole label map, because the key is unknown until the rule runs:

```text
size(node.metadata.labels) > 0
node.metadata.labels.exists(k, k.startsWith('nvidia.com/'))
node.metadata.labels[node.spec.nodeName] == 'true'
node.metadata.labels['nvidia.com/' + 'gpu.present'] == 'true'
```

Look carefully at the last form. It builds the key at run time, so it keeps the whole map, although both halves are string literals. To keep one entry, write the key as a single literal.

These forms disable all pruning for the node, because no set of keys describes what they read:

```text
size(node)
```

An expression that does not compile has the same effect.

Two results follow from this. Both can surprise you:

- The retained set is the union of all rulesets, and "keep everything" has priority. **One unprunable expression in one ruleset disables label pruning for every node and every rule.** If the startup log shows `retainedLabels="<all>"`, find the expression that caused it.
- Fault Quarantine reads all rulesets, enabled or disabled. A disabled ruleset with an unprunable expression still disables pruning. If you want the saving, remove such a ruleset. Do not only disable it.

In all cases, prefer the guarded literal-key form. Correct rules also need this form. When an expression reads an absent label, the evaluation raises an error and does not return false. Thus, read an optional label only behind an `in` guard.

## Key Features

### Entity-Level Tracking

Fault Quarantine tracks health issues at the entity level (for example, individual GPUs), and not only at the node level:

- Fine-grained visibility into the specific components that are faulty
- Track multiple issues on the same node
- Partial recovery scenarios, where some GPUs recover while others stay faulty

### Flexible CEL Rules with Node Context

Each CEL expression receives either the health event or the Kubernetes Node's metadata and spec:

- Cordon based on node labels (for example, environment, node type or GPU model)
- Check scheduling and taint configuration
- Skip quarantine for nodes with specific annotations
- Different thresholds based on any node metadata

### Automatic Recovery

When all health checks return to a healthy state, Fault Quarantine does these actions:

- Uncordons the node automatically
- Removes the taints
- Removes the quarantine annotations
- Returns the node to normal scheduling

### Circuit Breaker Integration

Fault Quarantine has built-in protection against mass cordoning. For details, see [Circuit Breaker documentation](circuit-breaker.md).
