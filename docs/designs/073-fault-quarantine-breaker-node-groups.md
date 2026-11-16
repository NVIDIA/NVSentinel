<!--
Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->


# ADR-073: Fault Quarantine — Circuit Breaker Limits for Node Groups and Failure Domains

Status: Proposed (tracking issue [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203))

## Table of Contents

1. [Context](#context)
   - [User journeys](#user-journeys)
   - [Options](#options)
2. [Decision](#decision)
3. [Implementation](#implementation)
   - [Configuration](#configuration)
   - [Membership](#membership)
   - [Counting](#counting)
   - [State, metrics, and reset](#state-metrics-and-reset)
   - [Validation](#validation)
   - [Failure domains](#failure-domains)
   - [Code](#code)
   - [Compatibility](#compatibility)
   - [Rollout](#rollout)
4. [Rationale](#rationale)
5. [Consequences](#consequences)
6. [Alternatives Considered](#alternatives-considered)
7. [Notes](#notes)
8. [References](#references)

## Context

fault-quarantine has one circuit breaker for the cluster. [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) describes two problems with this.

**One breaker for all node types.** The breaker measures its percentages against the nodes that match `gpuNodeLabel` ([#1228](https://github.com/NVIDIA/NVSentinel/issues/1228), [#1482](https://github.com/NVIDIA/NVSentinel/pull/1482)). But it counts the quarantines of every node that fault-quarantine quarantines. Some clusters have more than one node type, for example GPU nodes and nodes with LPU accelerators (LPU nodes). There, a burst of quarantines on one type trips the breaker for all types. The types also share one set of limits, although they can fail in different ways. [ADR-070](070-vendor-neutral-core.md) lists the `gpuNodeLabel` count as GPU logic in a core component. The vendor-neutral work in [#2247](https://github.com/NVIDIA/NVSentinel/issues/2247) continues from there, and [#2421](https://github.com/NVIDIA/NVSentinel/issues/2421) moves the `gpuNodeLabel` default out of the core chart. This ADR supports that work: a node group gives the breaker a way to count nodes that does not depend on a vendor label, and the cluster breaker keeps `gpuNodeLabel` with no change until #2421 moves it.

**Failure domains.** Some nodes fail together. On a rack-scale system, one switch or power fault can make every node of the rack fail at the same time. The comments on [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) describe such failures. A breaker that is sized to stop a bad signal across the cluster trips on one failed rack. Limits that let a full rack be quarantined weaken the protection against all other failures.

[ADR-064](064-node-and-device-selection.md) defines how node-drainer and fault-remediation select groups of nodes with `Node` rules. It lists breaker limits for each group as a Non-goal. This ADR adds them, as a [separate ADR](https://github.com/NVIDIA/NVSentinel/pull/2026#issuecomment-6480632987).

### User journeys

1. **Two node types.** An operator has GPU nodes and LPU nodes in one cluster. The operator adds one node group for the LPU nodes, with its own limits. A burst of quarantines on the LPU nodes then counts only against the LPU limits, and the GPU nodes keep the cluster limits. If the LPU nodes reach their limits, the trip still stops the event stream for all nodes, as today.
2. **No change.** An operator with one node type adds no node groups. The configuration, the breaker state, and the behaviour stay as they are today.
3. **A trip.** A breaker trips. The operator reads which breaker tripped, and which bound, from the `circuit-breaker` ConfigMap and from the metrics. The operator resets the breaker with the procedure of today.
4. **A new node type.** An operator adds nodes of a new type. Until the operator adds a node group for them, the cluster breaker covers them, as it does today.
5. **A failed rack.** An operator of a rack-scale system gives a node group a failure domain label. One rack can then be quarantined completely with no trip and no manual reset. A second rack with a quarantine in the same window trips the breaker.

### Options

1. A breaker for each node group, with optional failure domains (this ADR).
2. A fault-quarantine deployment for each node type.
3. A breaker for each fault-quarantine rule set.
4. A breaker for each `componentClass` or check.
5. Higher limits for the cluster breaker.
6. A static node group for each failure domain.

## Decision

fault-quarantine gets an optional list of node groups. A node group has the common part of the ADR-064 rule set schema, with only `Node` rules. It has one outcome key, `circuitBreaker`, which holds the limits of the group. Each node group has its own counts, limits, and metrics. A node in no group uses the cluster breaker. A node group can also count failure domains, one domain for each value of a node label.

The scope is small:

- Node groups change when a breaker trips. They do not change what a trip stops. A trip of any breaker still stops the event stream for all nodes, and the reset does not change.
- Fault-quarantine rule sets do not change.
- The design fails closed. If fault-quarantine cannot find the node group of a node, it stops, as it does today when a breaker check fails. It does not use other limits for the node.

## Implementation

### Configuration

```yaml
circuitBreaker:                 # the cluster breaker. No change
  enabled: true
  percentage: 50
  maxNodes: 10
  duration: "5m"

nodeGroups:                     # new, optional
  - name: lpu                   # required. Unique DNS-1123 label. "default" is reserved
    enabled: true               # optional. Default: true
    priority: 10                # optional. Default: 0
    match:                      # required. Exactly one of all or any. Only kind: Node
      all:
        - kind: Node
          expression: >-
            "example.com/accelerator" in node.metadata.labels &&
            node.metadata.labels["example.com/accelerator"] == "lpu"
    circuitBreaker:             # the outcome. The limit keys of the cluster breaker
      percentage: 25
      maxNodes: 5
      maxCordonedNodes: 20
      duration: "15m"
```

The values are examples.

- `name`, `enabled`, `priority`, and `match` have the meaning that ADR-064 gives them. This includes `enabled: true` as the default, and exactly one of `all` and `any`.
- The outcome is under one key, as in ADR-064. The limit keys in `circuitBreaker` have the meaning that they have for the cluster breaker. An omitted bound is 0, which turns it off.
- A node group accepts only `kind: Node` rules, because a node group is a set of nodes.
- The key is `nodeGroups` in the Helm values and in the TOML configuration.

A node group is the one place in fault-quarantine that defines a node type. ADR-064 deferred named selectors, and it said that `match` can accept a reference to one later. This ADR defines node groups only in fault-quarantine, and only the breaker reads them. [Future work](#future-work) lists a reference from a rule set to a node group.

### Membership

- A node belongs to one node group. It is the first enabled group that matches, in order of `priority`, the highest first. For equal priorities, the first group in the configuration wins. This is the "one winner" rule of ADR-064.
- A node that matches no enabled group uses the cluster breaker. `enabled: false` on a group moves its nodes to the next group that matches, or to the cluster breaker.
- fault-quarantine evaluates the rules with `commons/pkg/celrule` ([#2299](https://github.com/NVIDIA/NVSentinel/pull/2299)), with the same `node` variable as the `Node` rules of its rule sets. The node informer places each node when the node is added or changes.
- The node cache keeps the label and annotation keys that the group rules read.
- When fault-quarantine starts a quarantine, the log names the breaker that counts the node. A gauge reports the node count of each breaker.
- Match on labels that a node cannot change for itself. A node that can change its own labels can move itself to a group with other limits.

Errors follow ADR-064, with one difference:

- An error in a group before the winner stops the placement of the node. The node does not fall through to a later group or to the cluster breaker, because those can have weaker limits.
- Startup fails for a rule that does not compile. Startup also fails for a rule that reads a label or an annotation with no test for the key. ADR-064 logs a warning for such a rule, because its components can try again. After the node informer has synced, startup places every node, and it fails if a rule fails for a node. The log names the group, the rule, and the node.
- The difference: node-drainer and fault-remediation try a failed decision again. The live event stream of fault-quarantine has no retry for one event. Thus, if a rule fails for a node at run time, fault-quarantine handles it as it handles a failed breaker check today. The stream stops until the pod restarts. A metric counts the rule errors of each group.

### Counting

- **Window.** The window of a group counts each node of the group that received a quarantine that is not forced. A node counts once, as today.
- **Standing bounds.** `maxCordonedNodes` and `maxCordonedPercentage` of a group count the nodes of the group that NVSentinel holds cordoned, with the rule of today. A node in `drain-stopped` or `remediation-stopped` (ADR-064) is still quarantined, so it counts.
- **Forced quarantines and manual changes.** These count as they do today. A forced quarantine does not count in the window, and it counts in the standing bounds. A node that an operator uncordons leaves the standing count.
- **Percentages.** A group measures its percentages against its own nodes. A group does not use `gpuNodeLabel`.
- **Caps.** The node bounds of a group are capped at the number of nodes of the group.
- **The cluster breaker.** When node groups are set, the cluster breaker covers the nodes in no group, and it counts only their quarantines. It measures its percentages against the nodes that it covers and that match `gpuNodeLabel`, as today. Thus a node group for LPU nodes does not change the limits of the GPU nodes.
- **A node that moves.** If a label change moves a node to another group, its window entries stay where they were recorded. Its standing count moves with the node.

A breaker with no nodes:

- With no node groups, the cluster breaker keeps the rule of [#2273](https://github.com/NVIDIA/NVSentinel/pull/2273). When no node matches `gpuNodeLabel`, a quarantine in the window trips the breaker. The rule finds a `gpuNodeLabel` that matches no node.
- With node groups, the cluster breaker can cover no node that matches `gpuNodeLabel` by design, for example when every GPU node is in a group. The rule of #2273 then does not apply. The percentage bounds of the cluster breaker do not apply, and its absolute bounds apply to the nodes that it covers. Thus, when node groups are set, validation requires a positive `maxNodes` or `maxCordonedNodes` on the cluster breaker.
- A node group with no nodes does not trip. A node counts in a group only because it matches the group. Thus a wrong rule gives an empty group, not a quarantine that is not counted. Startup logs a warning for each group with no nodes.

### State, metrics, and reset

- The `circuit-breaker` ConfigMap keeps its keys and their meaning. `status: TRIPPED` halts the event stream for all nodes. `cursor` stays one key, because there is one stream.
- When node groups are set and a breaker trips, fault-quarantine also writes a new key, `trippedBy`. It names the breaker and the bound, for example `lpu/maxNodes`, or `default/percentage` for the cluster breaker. fault-quarantine removes the key when `status` is `CLOSED` at startup.
- The reset is the reset of [ADR-022](022-circuit-breaker-reset-mechanism.md). The runbook gets one new step: read `trippedBy`.
- `fault_quarantine_breaker_state`, `fault_quarantine_breaker_utilization`, and `fault_quarantine_breaker_threshold_nodes` get a `group` label. The cluster breaker uses `group="default"`. The label is always present. The documented alert `fault_quarantine_breaker_state{state="TRIPPED"} == 1` still matches.
- Each breaker sets and deletes only its own series. Today the setters of the state and threshold metrics reset the full vector.
- New gauges report the node count of each breaker. With failure domains, they also report the domain counts and the domain limits. No metric has a label for the domain.

### Validation

When node groups are set, startup fails when:

- a group has a `HealthEvent` rule, or a rule that does not compile,
- a rule reads a label or an annotation with no test for the key,
- a `match` has neither or both of `all` and `any`, or an empty list,
- two groups have the same name, or a group is named `default`,
- a group has an outcome key that is not `circuitBreaker`,
- a bound or the window of a group is not valid, with the checks of the cluster breaker,
- a group has no positive node bound,
- the cluster breaker has no positive `maxNodes` or `maxCordonedNodes`.

Startup logs the node count of each breaker. In dry run, fault-quarantine does not build the breaker, as today. It still validates the node groups, and it logs a rule error on a node as a warning.

### Failure domains

A node group can count failure domains. This is step 2 of the [rollout](#rollout).

```yaml
nodeGroups:
  - name: racks
    match:
      all:
        - kind: Node
          expression: >-
            "nvidia.com/gpu.present" in node.metadata.labels &&
            node.metadata.labels["nvidia.com/gpu.present"] == "true"
    circuitBreaker:
      maxNodes: 40                      # safety net. Above the size of one domain
      maxCordonedNodes: 60              # safety net. Above the size of the domains that can stay cordoned
      duration: "5m"
      failureDomains:
        topologyKey: example.com/rack   # one failure domain for each value of this label
        maxDomains: 2                   # a second domain with a quarantine in the window trips
        maxCordonedDomains: 4
```

- `topologyKey` works like `topologyKey` in Kubernetes topology spread constraints. Each value of the label is one domain.
- `maxDomains` limits the number of domains that have a quarantine in the window. A domain counts once, however many of its nodes are quarantined.
- `maxCordonedDomains` limits the number of domains that have a node that NVSentinel holds cordoned.
- A domain has no limit of its own, so a full domain can be quarantined. The node bounds of the group apply to all its domains together, as a safety net. Set them above the size of one domain.
- A domain bound trips when the count reaches it, as `maxNodes` does. A domain bound of 1 is not valid, because the first quarantine would trip it.
- A domain bound is not a node bound. If the domain labels are wrong or missing, the node bounds stop a bad signal.

With `maxDomains: 2`, one failed domain does not trip, and it needs no reset. A bad signal that hits nodes in many domains trips at the second domain. Two faults in two domains in the same window, which are not related, also trip.

The domain of a node:

- fault-quarantine fixes the domain of a node when it starts the quarantine. It stores the domain in a new quarantine annotation. A later change or loss of the label does not move a quarantined node to another domain, and a restart does not change it. [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) asked for this rule.
- A node that has no label and no stored domain counts in one shared domain for its group, the unknown domain. fault-quarantine logs a warning and reports these nodes in a metric. One domain for each such node would count a rack without labels as many domains, and a failed rack would trip.
- Match the group on a stable label, not on the domain label. A node then stays in its group when it loses its domain label.
- A static rack label is the most stable key. `nvidia.com/gpu.clique` and the [Topograph](../INTEGRATIONS.md#topology-awareness-topograph) domain label are also valid keys, and a cluster with no rack label can use them. A discovery component computes these labels, so they can change or disappear during a fault. The two rules above cover that case.

A cluster that is one domain gets no benefit from domain bounds. A failure of the full domain and a bad signal on every node give the same counts there. The node bounds are capped at the number of nodes of the group, so the breaker trips when every node is quarantined. The operator chooses between that trip and `circuitBreaker.enabled: false`.

### Code

- The node group type is a small type in fault-quarantine with the field names of the ADR-064 common part. It uses `commons/pkg/celrule`, which is on main. It does not need `commons/pkg/ruleset` ([#2209](https://github.com/NVIDIA/NVSentinel/issues/2209), [#2409](https://github.com/NVIDIA/NVSentinel/pull/2409)). If that package merges first, step 1 uses its common part and its validation from the start. If not, the node group type moves to it later, with no change to the configuration. This ADR adds no second rule set layer to `commons`.
- The breaker gets one instance for each node group and one for the cluster. The check before each event reads the counters of every breaker. Fault-quarantine rule sets and their evaluator do not change.
- This work does not change the files of the ADR-064 implementation, except the node cache key list in fault-quarantine.

### Compatibility

- With no node groups, the configuration, the state, and the behaviour do not change. The metrics gain `group="default"`.
- A `status` of `TRIPPED` at upgrade time halts every breaker, as today.
- An older version reads `status` with the same meaning. It ignores `trippedBy`, the domain annotation, and `nodeGroups`, and its cluster breaker then covers all nodes. Before a rollback, remove the node groups, or set the cluster limits for all nodes.
- A running pod reads the breaker state from the ConfigMap only at startup. During a rolling update, the old and the new pod can each process events with their own configuration for a short time. This is true today for each change to the breaker configuration. Turn on node groups in a separate rollout, after the new version runs.

### Rollout

1. Node groups with node bounds: the configuration, membership, counting, `trippedBy`, the `group` label, and validation, with their documentation.
2. Failure domains: `failureDomains`, the domain annotation, and the domain metrics, with their documentation.

Each step is a separate PR. A cluster with no node groups does not change at either step. Step 2 changes only fault-quarantine. It adds no shared code and it does not change the files of the ADR-064 implementation, so step 1 can merge and release without it. The documentation that changes is the [configuration](../configuration/fault-quarantine.md), the [circuit breaker documentation](../circuit-breaker.md), the [runbook](../runbooks/circuit-breaker.md), and [METRICS.md](../METRICS.md).

Tests:

- Unit tests: the winner, equal priorities, a disabled group, and a rule error before and after the winner. The window and the standing count of each group. A group with no nodes, and the cluster breaker with no node that matches `gpuNodeLabel`. The series of each group. The domain of a node at quarantine, stored, and unknown.
- E2E tests: a burst on one node type trips by the limits of its group. A full domain is quarantined with no trip, and a second domain trips. A restart while tripped restores `trippedBy` and the series. With no node groups, nothing changes except the `group` label.

## Rationale

- **One way to select nodes.** A node group uses the `Node` rules and the common part of ADR-064, so operators write one type of rule.
- **One definition of a node type.** The node group is the one place in fault-quarantine that says what a node type is.
- **Small scope.** The stream, the reset, and the rule sets do not change. The work uses only shared code that is on main.
- **Opt-in.** A cluster with no node groups does not change.
- **Fail closed.** A rule error and a node without a domain label never move a node to weaker limits.
- **One deployment.** One component owns the quarantine annotations of each node and reads the event stream.

## Consequences

### Positive

- A burst of quarantines on one node type does not trip the breaker for the other types.
- Each node type gets limits that fit how it fails.
- One failed domain, for example a rack, can be quarantined completely with no trip and no manual reset.
- The reset is the reset of today, and `trippedBy` names the breaker that tripped.

### Negative

- A trip in one group still stops quarantines and recovery for all nodes.
- Operators have more configuration and more metrics.
- A group rule that fails at run time stops the stream for all nodes until the rule or the node is corrected.
- The bounds of a group with domains depend on the size of a domain, so each cluster needs its own values.
- On a cluster that is one domain, the breaker cannot tell a failed domain from a bad signal on every node.
- The membership rule (one winner) is different from the fault-quarantine rule sets (every match applies).

### Mitigations

- Startup places every node, so most rule errors stop startup before the first event.
- The `group` label lets operators make an alert for each group.
- The documentation gives sizing rules for groups with domains.
- The documentation states the membership rule, with a link to ADR-064.

## Alternatives Considered

### Approach

#### A fault-quarantine deployment for each node type

**Rejected** because: two deployments in one cluster share state with fixed names. These are the `circuit-breaker` ConfigMap, the resume position, and the quarantine annotations on the nodes. The standing bounds of each deployment would count the nodes of the other.

#### A breaker for each fault-quarantine rule set

The limits go on each quarantine rule set, and the breaker needs no rules of its own.

**Rejected** because:

- A quarantine has no single owner. Every rule set that matches an event applies, and the breaker records the node once.
- Rule sets select events, not node types. A rule set usually covers every node type, so its breaker would measure against the full cluster.
- One node type usually has more than one rule set. A bad signal would divide its count between their breakers.

#### A breaker for each `componentClass` or check

**Rejected** because: the breaker protects capacity, and capacity is a property of nodes. A node with events of two classes would count in two breakers.

#### Higher limits for the cluster breaker

**Rejected** because: limits that fit a large node group remove the protection for a small one. Limits that let a full rack be quarantined weaken the protection against a bad signal across racks.

#### A static node group for each failure domain

**Rejected** because: a cluster can have many racks. The groups would be written by hand, and they would have no bound across them.

### Details

#### Limit keys next to `match`

**Rejected** because: ADR-064 puts the outcome under one key that names the stage. The node group follows that form.

#### Treat a group rule error as no match

**Rejected** because: the node then falls through to a later group or to the cluster breaker, which can have weaker limits.

## Notes

### Non-goals

- Drain and remediation limits for each node group. ADR-064 rule sets select the drain and the remediation.
- A definition of node groups that other components share.
- A change to the fault-quarantine rule sets.
- A trip that holds only the events of its own group.

### Future work

- **A reference from a rule set to a node group.** A quarantine rule set could name node groups, for example `nodeGroups: [lpu]`, in place of a `Node` rule of its own. The operator would then define a node type one time for the quarantine rules and for the breaker. This is the named selector that ADR-064 deferred. It is not in this ADR, because it changes the rule set layer while the ADR-064 work on that layer is in progress.
- **A trip that holds only its own group.** A held event needs its own recorded status. A replay needs a rule for a node that has recovered in the meantime. A follow-up ADR can design this.
- **The shared rule set package.** The node group moves to `commons/pkg/ruleset` when it is available.

### Open questions

- **Domain bounds on the cluster breaker.** To use domains, a cluster with one node type needs a node group that matches every node. `failureDomains` on the top-level `circuitBreaker` would remove that need.
- **Dry run.** A report-only evaluation in dry run could show the counts of each group. Operators could then size the limits before a live run.
- **Automatic reset.** A trip latches by design, and the operator chooses between replay and discard at the reset (ADR-022). [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) asks for a reset with no operator action after the nodes recover. This ADR does not propose one.
- **Rolling update.** `strategy: Recreate` on the fault-quarantine Deployment would stop two pods from processing events at the same time during a rollout.

## References

- [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203): circuit breaker limits for each node group, and the input on failure domains
- [#1903](https://github.com/NVIDIA/NVSentinel/issues/1903): heterogeneous cluster support
- [ADR-064](064-node-and-device-selection.md): node and device selection in each component
- [ADR-070](070-vendor-neutral-core.md): vendor-neutral core
- [#2247](https://github.com/NVIDIA/NVSentinel/issues/2247), [#2421](https://github.com/NVIDIA/NVSentinel/issues/2421): the vendor-neutral work, and the move of the `gpuNodeLabel` default
- [ADR-022](022-circuit-breaker-reset-mechanism.md): circuit breaker reset mechanism
- [#1228](https://github.com/NVIDIA/NVSentinel/issues/1228), [#1482](https://github.com/NVIDIA/NVSentinel/pull/1482): the GPU nodes as the breaker fleet
- [#2272](https://github.com/NVIDIA/NVSentinel/issues/2272), [#2273](https://github.com/NVIDIA/NVSentinel/pull/2273): the breaker on a cluster with no GPU nodes
- [#2299](https://github.com/NVIDIA/NVSentinel/pull/2299): the shared CEL rule evaluators
- [#2209](https://github.com/NVIDIA/NVSentinel/issues/2209), [#2409](https://github.com/NVIDIA/NVSentinel/pull/2409): the shared rule set package
- [Circuit breaker documentation](../circuit-breaker.md) and [runbook](../runbooks/circuit-breaker.md)
