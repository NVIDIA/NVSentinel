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
   - [Options](#options)
2. [Decision](#decision)
   - [User journeys](#user-journeys)
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

**One breaker for all node types.** The breaker measures its percentages against the nodes that match `gpuNodeLabel` ([#1228](https://github.com/NVIDIA/NVSentinel/issues/1228), [#1482](https://github.com/NVIDIA/NVSentinel/pull/1482)). But it counts the quarantines of every node that fault-quarantine quarantines. Some clusters have more than one node type, for example GPU nodes and nodes with LPU accelerators (LPU nodes). There, this has two effects:

- **The counts are mixed.** The quarantines of all node types count together against one set of limits. Thus a burst of quarantines on one node type can trip the breaker with limits that were sized for a different type. The types also share one set of limits, although they can fail in different ways.
- **A trip stops all nodes.** A trip stops the event stream, so no node of any type is quarantined or released until the reset.

This ADR is about the first effect. The second effect does not change: see [Decision](#decision) and [Future work](#future-work).

[ADR-070](070-vendor-neutral-core.md) lists the `gpuNodeLabel` count as GPU logic in a core component. The vendor-neutral work in [#2247](https://github.com/NVIDIA/NVSentinel/issues/2247) continues from there, and [#2421](https://github.com/NVIDIA/NVSentinel/issues/2421) moves the `gpuNodeLabel` default out of the core chart. This ADR supports that work: a node group gives the breaker a way to count nodes that does not depend on a vendor label, and the cluster breaker keeps `gpuNodeLabel` with no change until #2421 moves it.

**Failure domains.** Some nodes fail together. On a rack-scale system, one switch or power fault can make every node of the rack fail at the same time. The comments on [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) describe such failures. A breaker that is sized to stop a bad signal across the cluster trips on one failed rack. Limits that let a full rack be quarantined weaken the protection against all other failures.

[ADR-064](064-node-and-device-selection.md) defines how node-drainer and fault-remediation select groups of nodes with `Node` rules. It lists breaker limits for each group as a Non-goal. This ADR adds them, as a [separate ADR](https://github.com/NVIDIA/NVSentinel/pull/2026#issuecomment-6480632987).

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

- Node groups change when a breaker trips. They do not change what a trip stops. A trip of any breaker still stops the event stream for all nodes, and the reset does not change. There is one ordered event stream with one resume position, so a trip that holds only the events of its own group is a larger change. It is in [Future work](#future-work).
- Fault-quarantine rule sets do not change.
- The design fails closed. If a rule error stops the placement of a node, fault-quarantine does not quarantine that node until it has a placement. It continues with the other nodes. It does not use other limits for the node.

### User journeys

1. **Two node types.** An operator has GPU nodes and LPU nodes in one cluster. The operator adds one node group for the LPU nodes, with its own limits. A burst of quarantines on the LPU nodes then counts only against the LPU limits, and the GPU nodes keep the cluster limits. A trip then needs the LPU nodes to reach their own limits. Such a trip still stops the event stream for all nodes, as today.
2. **No change.** An operator with one node type adds no node groups. The configuration, the breaker state, and the behaviour stay as they are today.
3. **A trip.** A breaker trips. The operator reads which breaker tripped, and which bound, from the `circuit-breaker` ConfigMap and from the metrics. The operator resets the breaker with the procedure of today.
4. **A new node type.** An operator adds nodes of a new type. Until the operator adds a node group for them, the cluster breaker covers them, as it does today.
5. **A failed rack.** An operator of a rack-scale system gives a node group a failure domain label. With a domain bound of 2, one rack can then be quarantined completely with no trip and no manual reset. A second rack with a quarantine in the same window trips the breaker.

## Implementation

### Configuration

```yaml
# The cluster breaker. No change to the keys. With node groups, each of its bounds counts only the nodes that match no group.
circuitBreaker:
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
- `circuitBreaker.enabled: false` at the top level turns off every breaker, as today. fault-quarantine still validates the node groups. A node group cannot turn off only its own breaker.
- The key is `nodeGroups` in the Helm values and in the TOML configuration.

A node group is the one place in fault-quarantine that defines a node type. ADR-064 deferred named selectors, and it said that `match` can accept a reference to one later. This ADR defines node groups only in fault-quarantine, and only the breaker reads them. [Future work](#future-work) lists a reference from a rule set to a node group.

### Membership

- A node belongs to one node group. It is the first enabled group that matches, in order of `priority`, the highest first. For equal priorities, the first group in the configuration wins. This is the "one winner" rule of ADR-064.
- A node that matches no enabled group uses the cluster breaker. `enabled: false` on a group moves its nodes to the next group that matches, or to the cluster breaker.
- fault-quarantine evaluates the rules with `commons/pkg/celrule` ([#2299](https://github.com/NVIDIA/NVSentinel/pull/2299)), with the same `node` variable as the `Node` rules of its rule sets. The node informer places each node when the node is added or changes.
- The node cache keeps the label and annotation keys that the group rules read, and the `topologyKey` label of each group.
- When fault-quarantine starts a quarantine, the log names the breaker that counts the node. A gauge reports the node count of each breaker.
- Match on labels that a node cannot change for itself. A node that can change its own labels can move itself to a group with other limits. It can also cause a rule error for itself, and fault-quarantine then does not quarantine the node.

Errors follow ADR-064, with two differences. Startup is stricter, and fault-quarantine does not try a skipped event again:

- An error in a group before the winner stops the placement of the node. The node does not fall through to a later group or to the cluster breaker, because those can have weaker limits. The node then has no placement until a change of the node, or a restart with a corrected rule, gives it one. No breaker counts a node with no placement.
- Startup fails for a rule that does not compile. Startup also fails for a rule that reads a label or an annotation with no test for the key. ADR-064 logs a warning for such a rule, because its components can try again. After the node informer has synced, startup places every node. A rule error at this step does not stop startup, for one node or for many nodes. fault-quarantine handles it as a rule error at run time, so that an error on a node cannot stop a restart.
- node-drainer and fault-remediation try a failed decision again. The live event stream of fault-quarantine has no retry for one event, and a stop of the stream is a stop for all nodes. Thus, when a rule error stops the placement of a node, fault-quarantine logs an error that names the group, the rule, and the node, and it increments a counter of the rule errors of each group. While the node has no placement, fault-quarantine does not start a quarantine for it, and it does not add a cordon to it. This includes a forced quarantine, because a forced quarantine counts in the standing bounds, and no breaker counts a node with no placement. fault-quarantine skips each event that would start a quarantine for the node. For each event that it skips, it logs an error that names the node and the event, and it increments `fault_quarantine_processing_errors_total` with a new `error_type` value. It then continues with the next event. A skipped event is not tried again. After the node has a placement, a later event for the node starts the quarantine. fault-quarantine does not start it from the events that it skipped.
- This rule applies only while the breaker is on. With `circuitBreaker.enabled: false` or in dry run, no breaker counts a node, so a rule error does not stop a quarantine.
- For a node that is quarantined when the rule fails, the window entries stay where they were recorded, as for a node that moves. The node leaves the standing count of its breaker, for the node bounds and for the domain bounds, because the standing count follows the placement. It counts again in the breaker of its next placement. Its other events are processed as today: a later unhealthy event is added to the quarantine with no new cordon, and a healthy event releases the node.

### Counting

- **Window.** The window of a group counts each node of the group that received a quarantine that is not forced. A node counts once, as today.
- **Standing bounds.** `maxCordonedNodes` and `maxCordonedPercentage` of a group count the nodes of the group that NVSentinel holds cordoned, with the rule of today. A node in `drain-stopped` or `remediation-stopped` (ADR-064) is still quarantined, so it counts.
- **Forced quarantines and manual changes.** These count as they do today. A forced quarantine does not count in the window, and it counts in the standing bounds. A node that an operator uncordons leaves the standing count.
- **Percentages.** A group measures its percentages against its own nodes. A group does not use `gpuNodeLabel`.
- **Caps.** The node bounds of a group are capped at the number of nodes of the group. For the standing bounds, this cap is new. The cluster breaker caps only its window bounds, as today, because its standing count can include nodes that do not match `gpuNodeLabel`. A group counts and measures the same nodes.
- **The cluster breaker.** When node groups are set, the cluster breaker covers the nodes that match no enabled group. Each of its bounds, in the window and standing, counts only these nodes. It measures its percentages against the nodes that it covers and that match `gpuNodeLabel`, as today. Thus a node group for LPU nodes does not change the limits of the GPU nodes.
- **A node that moves.** If a label change moves a node to another group, its window entries stay where they were recorded, with their domain results. Its standing count moves with the node. For the domain bounds of the new group, the rules for the domain of a node in [Failure domains](#failure-domains) apply.
- **A node with no placement.** A node with a rule error has no placement (see [Membership](#membership)). It is in the node count of no breaker: not of a node group, and not of the cluster breaker. Thus no percentage and no cap includes it. Membership gives the rule for its window entries and its standing count.

A breaker with no nodes:

- With no node groups, the cluster breaker keeps the rule of [#2273](https://github.com/NVIDIA/NVSentinel/pull/2273). When no node matches `gpuNodeLabel`, a quarantine in the window of a node that is still in the cluster trips the breaker. The rule finds a `gpuNodeLabel` that matches no node.
- With node groups, the cluster breaker can cover no node that matches `gpuNodeLabel` by design, for example when every GPU node is in a group. The rule of #2273 then does not apply. The percentage bounds of the cluster breaker do not apply, and its absolute bounds apply to the nodes that it covers. Thus, when node groups are set, validation requires a positive `maxNodes` or `maxCordonedNodes` on the cluster breaker.
- A node group with no nodes does not trip. A node counts in a group only because it matches the group. Thus a rule that matches no node gives an empty group, not a quarantine that is not counted. Startup logs a warning for each group with no nodes.
- Deleted nodes. The cluster breaker keeps its rule of today: it forgets the window entries of deleted nodes when the last node that matches `gpuNodeLabel` is deleted. A node group forgets the window entries of its deleted nodes when the last node of the group is deleted. A group that has no nodes because its nodes moved to a different group, or have no placement, forgets nothing. In each other case, the entry of a deleted node stays until it expires.

### State, metrics, and reset

- The `circuit-breaker` ConfigMap keeps its keys and their meaning. `status: TRIPPED` halts the event stream for all nodes. `cursor` stays one key, because there is one stream.
- When node groups are set and a breaker trips, fault-quarantine also writes a new key, `trippedBy`. It names the breaker and the bound, for example `lpu/maxNodes`, or `default/percentage` for the cluster breaker. The bound names are those of the `bound` label of `fault_quarantine_breaker_threshold_nodes` today, and `maxDomains` and `maxCordonedDomains`. If more than one breaker has reached a bound at the same check, `trippedBy` names the first one in the order of membership, with the cluster breaker last. In one breaker, the window bounds come before the standing bounds, as today, and a node bound comes before a domain bound. fault-quarantine removes the key when `status` is `CLOSED` at startup.
- The reset is the reset of [ADR-022](022-circuit-breaker-reset-mechanism.md). The runbook gets one new step: read `trippedBy`.
- `fault_quarantine_breaker_state`, `fault_quarantine_breaker_utilization`, and `fault_quarantine_breaker_threshold_nodes` get a `group` label. The cluster breaker uses `group="default"`. The label is always present. The documented alert `fault_quarantine_breaker_state{state="TRIPPED"} == 1` still matches.
- Each breaker sets and deletes only its own series. Today the setters of the state and threshold metrics reset the full vector.
- A new counter reports the rule errors of each group. New gauges report the node count of each breaker and the number of nodes with no placement. With failure domains, they also report the domain counts and the domain limits. No metric has a label for the domain.

### Validation

When node groups are set, startup fails when:

- a group has a `HealthEvent` rule, or a rule that does not compile,
- a rule reads a label or an annotation with no test for the key,
- a `match` has neither or both of `all` and `any`, or an empty list,
- two groups have the same name, or a group is named `default`,
- a group has an outcome key that is not `circuitBreaker`,
- a bound or the window of a group is not valid, with the checks of the cluster breaker,
- a group has no positive node bound,
- `failureDomains` has no `topologyKey`, or a `topologyKey` that is not a valid label key,
- a domain bound is negative or 1, or `failureDomains` has no positive domain bound,
- the cluster breaker has no positive `maxNodes` or `maxCordonedNodes`.

A rule error on a node does not stop startup (see [Membership](#membership)). Startup logs the node count of each breaker and the number of nodes with no placement. In dry run, fault-quarantine does not build the breaker, as today. It still validates the node groups, and it logs a rule error on a node as a warning, because no breaker counts the node.

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
      maxNodes: 40                      # safety net. (maxDomains - 1) domains, and a small margin
      maxCordonedNodes: 120             # safety net. (maxCordonedDomains - 1) domains, and a small margin
      duration: "5m"
      failureDomains:
        topologyKey: example.com/rack   # one failure domain for each value of this label
        maxDomains: 2                   # a second domain with a quarantine in the window trips
        maxCordonedDomains: 4
```

- `topologyKey` works like `topologyKey` in Kubernetes topology spread constraints. Each value of the label is one domain.
- `maxDomains` limits the number of different domain results among the window entries of the group. A domain counts once, however many of its nodes have an entry.
- `maxCordonedDomains` limits the number of domains that have a node that NVSentinel holds cordoned.
- A domain has no limit of its own, so a full domain can be quarantined. The node bounds of the group apply to all its domains together, as a safety net. A domain bound of N lets N - 1 full domains be quarantined. Thus set `maxNodes` to `maxDomains` - 1 times the size of the largest domain, plus a small margin, and not higher. Set `maxCordonedNodes` from `maxCordonedDomains` in the same way. With correct labels, a higher node bound gives no benefit, because the domain bound stops the next domain. A higher node bound also weakens the protection for nodes with a missing or wrong domain label. The example is for domains of 36 nodes.
- A domain bound trips when the count reaches it, as `maxNodes` does. A domain bound of 1 is not valid, because the first quarantine would trip it. An omitted domain bound is 0, which turns it off.
- A domain bound is not a node bound. If the domain labels are wrong or missing, the node bounds stop a bad signal.

With `maxDomains: 2`, one failed domain does not trip, and it needs no reset. A bad signal that hits nodes in many domains trips at the second domain. Two faults in two domains in the same window, which are not related, also trip.

The domain of a node:

- A domain result has one of two kinds: a label value, or no label. No label is its own kind, and it is not a label value. Thus a node whose label has the value `unknown` is in an ordinary domain with that name. A label with an empty value counts as no label.
- fault-quarantine fixes the domain of a node when it starts the quarantine. It reads the label of the `topologyKey` of the group one time, at that time, and it stores the result in a new quarantine annotation. It does this for each quarantine, also for a forced quarantine and for a quarantine with no cordon. A later change, loss, or arrival of the label does not change a stored result, and a restart does not change it. [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) asked for this rule.
- **The annotation.** The annotation is `quarantineHealthEventFailureDomains`. It holds one result for each `topologyKey`, with its kind, for example `{"example.com/rack": {"value": "r1"}}` and `{"example.com/rack": {"noLabel": true}}`. A stored result does not change while the quarantine continues. At the start of a quarantine, fault-quarantine replaces the full annotation. If the breaker of the node has no `topologyKey`, it removes the annotation. It does this in the node update that writes the other quarantine annotations, so a node is never quarantined with a result that is not yet stored. When the quarantine ends, fault-quarantine removes the annotation with the other quarantine annotations. It also removes the annotation when an operator uncordons the node. Thus a quarantine that this version starts never uses a result from an earlier quarantine of the node. [Compatibility](#compatibility) gives the one exception, after a rollback.
- **A missing result.** A quarantined node can have no stored result for the `topologyKey` of its group. Examples: the quarantine started before the group had failure domains, the `topologyKey` of the group changed, or the node moved from a group with a different `topologyKey` or with no failure domains. fault-quarantine then reads the label of that key when it first needs the result of the node in that group, and it adds the result for that key to the annotation. It does not replace or use a result for a different key. It also does not replace a result that is already stored for the same key: if a different writer stored one first, fault-quarantine uses that result. If the write fails, fault-quarantine counts the node with the result that it read, and it tries the write again. Thus a rack value never counts as a domain in a group that counts zones, and quarantines in many racks that are older than the domain bounds do not count as one domain.
- **The standing count.** `maxCordonedDomains` counts the stored results of the nodes of the group that NVSentinel holds cordoned.
- **The window.** `maxDomains` counts the results of the window entries of the group. As today, fault-quarantine records a node in the window when a quarantine that is not forced starts, and when a later event adds the cordon to a node that is already quarantined. Each time, the entry holds the stored result of the node for the `topologyKey` of the group at that time. The window is in memory, as today, and a restart does not keep it. An entry does not change after it is recorded: a later change of the annotation, the end of the quarantine, or a quarantine in a different group does not change it. A node has one entry in a window, as today: when fault-quarantine records the node again in the same group and the same window, the new entry replaces the first one. Thus, if the label of the node changed between two quarantines, the window counts the node in its new domain only. A node is in one domain, and its latest result is that domain. An entry stays until it expires, or until the breaker forgets a deleted node ([Counting](#counting)).
- A node with the result no label counts in one shared domain for its group, the unknown domain. The name is not a label value. fault-quarantine logs a warning and reports these nodes in a metric. One domain for each such node would count a rack without labels as many domains, and a failed rack would trip.
- The unknown domain has a cost. Nodes without a label can be in different racks, but they all count as one domain, so the domain bound cannot find the second rack. Only the node bounds protect against a failure or a bad signal on such nodes. The breaker then trips at the node bound. With labels, it trips at the domain bound, which is the first quarantine in the domain that reaches the bound. The metric for nodes without a domain label shows this exposure before a fault occurs.
- Match the group on a stable label, not on the domain label. A node then stays in its group when it loses its domain label.
- A static rack label is the most stable key. `nvidia.com/gpu.clique` and the [Topograph](../INTEGRATIONS.md#topology-awareness-topograph) domain label are also valid keys, and a cluster with no rack label can use them. A discovery component computes these labels, so they can change or disappear during a fault. The stored result covers a node from the start of its quarantine. It does not cover a node that loses or changes its label before its quarantine, and it does not cover a result that fault-quarantine reads later because it was missing.
- fault-quarantine cannot verify the domain label. A label that is missing, wrong, or changed before the quarantine can make the domain bound count too many domains or too few. Too many: a node of a failed domain counts in the unknown domain or in a new domain, one failed domain counts as two, and the breaker can trip early. Too few: a node gets the label value of a different domain, two domains count as one, and the domain bound does not find the second domain. In each case the node bounds apply with no change, and they are the protection. `trippedBy`, the domain annotations, and the metric for nodes without a domain label show the cause of a trip. A static label prevents these cases. [Future work](#future-work) lists ways to keep the domain of a node.

A cluster that is one domain gets no benefit from domain bounds. A failure of the full domain and a bad signal on every node give the same counts there. The node bounds are capped at the number of nodes of the group, so the breaker trips when every node is quarantined. The operator chooses between that trip and `circuitBreaker.enabled: false`.

### Code

- The node group type is a small type in fault-quarantine with the field names of the ADR-064 common part. It uses `commons/pkg/celrule`, which is on main. It does not need `commons/pkg/ruleset` ([#2209](https://github.com/NVIDIA/NVSentinel/issues/2209), [#2409](https://github.com/NVIDIA/NVSentinel/pull/2409)). If that package merges first, step 1 uses its common part and its validation from the start. If not, the node group type moves to it later, with no change to the configuration. This ADR adds no second rule set layer to `commons`.
- The breaker gets one instance for each node group and one for the cluster. The check before each event reads the counters of every breaker. Fault-quarantine rule sets and their evaluator do not change.
- This work does not change the files of the ADR-064 implementation, except the node cache key list in fault-quarantine.

### Compatibility

- With no node groups, the configuration, the state, and the behaviour do not change. The metrics gain `group="default"`. There are no group rules, so no node has a rule error.
- A `status` of `TRIPPED` at upgrade time halts every breaker, as today.
- An older version reads `status` with the same meaning. It ignores `trippedBy`, the domain annotation, and `nodeGroups`, and its cluster breaker then covers all nodes. Before a rollback, remove the node groups, or set the cluster limits for all nodes. An older version does not remove the domain annotation when a quarantine ends. At startup, a newer version removes the domain annotation from each node that is not quarantined. One case remains: a node that an older version quarantined again keeps the result of its earlier quarantine until the quarantine ends. The domain bounds can then count that node in the wrong domain, and the node bounds apply with no change. To prevent this case, remove the `quarantineHealthEventFailureDomains` annotation from all nodes after a rollback.
- A running pod reads the breaker state from the ConfigMap only at startup. During a rolling update, the old and the new pod can each process events with their own configuration for a short time. This is true today for each change to the breaker configuration. Turn on node groups in a separate rollout, after the new version runs.

### Rollout

1. Node groups with node bounds: the configuration, membership and its rule errors, counting, `trippedBy`, the `group` label, the rule error and node count metrics, and validation, with their documentation.
2. Failure domains: `failureDomains`, the domain annotation, and the domain metrics, with their documentation.

Each step is a separate PR. A cluster with no node groups does not change at either step. Step 2 changes only fault-quarantine. It adds no shared code and it does not change the files of the ADR-064 implementation, so step 1 can merge and release without it. The documentation that changes is the [configuration](../configuration/fault-quarantine.md), the [circuit breaker documentation](../circuit-breaker.md), the [runbook](../runbooks/circuit-breaker.md), and [METRICS.md](../METRICS.md).

Tests:

- Unit tests: the winner, equal priorities, a disabled group, and a rule error before and after the winner. A node with a rule error before the winner has no placement: an event for it starts no quarantine, forced or not, the event is not tried again, the stream continues, and a later event starts the quarantine after the node gets a placement. The rule error counter of the group increases, and the gauge for nodes with no placement counts the node until it gets a placement. A quarantined node that loses its placement on a rule error: its window entries stay, it leaves the standing count, a later unhealthy event adds no cordon, and a healthy event releases it. When the node gets a placement again, it counts in the standing count of that breaker. A rule error at startup, for one node and for every node, does not stop startup. With the breaker off, a rule error does not stop a quarantine. The window and the standing count of each group. A group with no nodes, and the cluster breaker with no node that matches `gpuNodeLabel`. The series of each group. The domain of a node at quarantine, stored, and unknown. A node with no label and a node whose label has the value `unknown` count as two domains. A label with an empty value counts as no label. A quarantined node with no stored result for the `topologyKey` of its group: an older quarantine, a changed key, and a move to a group with a different key. After a move, the window entries of the first group keep their domain. A node that is quarantined in one group, released, and quarantined in a second group with the same `topologyKey` and a different label value: the window entry of the first group keeps the first value, and the second quarantine stores the second value. A second record of a node in the same group and window replaces its entry. When the label of the node changed between the two quarantines, the domain count has only the new result. A forced quarantine stores a result and has no window entry. A quarantine with no cordon that gets a cordon later: the new entry holds the stored result, also when the label changed. A quarantined node that moves between groups with the same `topologyKey` keeps its stored result. A quarantined node that moves from a group with no failure domains gets a result when it is first needed. The start of a quarantine replaces a domain annotation that remains from an earlier quarantine, and removes it when the breaker of the node has no `topologyKey`. Startup removes the domain annotation from a node that is not quarantined. A manual uncordon removes the domain annotation. A missing result that a different writer stored first is used, and a failed write is tried again. `trippedBy` for a domain bound, and for two breakers that reach a bound at the same check. A group forgets the entries of deleted nodes when its last node is deleted. The validation of `failureDomains`. A node that is quarantined with no label keeps the unknown domain when its label appears and after a restart. Nodes without a domain label, some in one rack and some in a second rack: they count as one domain, and the node bound trips. A node that loses its label before the quarantine counts in the unknown domain. A node whose label changes to the value of a different domain counts in that domain. The node bounds for a domain bound of 2 and of 3.
- E2E tests: a burst on one node type trips by the limits of its group. A full domain is quarantined with no trip, and a second domain trips. A restart while tripped restores `trippedBy` and the series. A rule error on one node: that node is not quarantined, a second node is quarantined, and a restart completes. With no node groups, nothing changes except the `group` label.

## Rationale

- **One way to select nodes.** A node group uses the `Node` rules and the common part of ADR-064, so operators write one type of rule.
- **One definition of a node type.** The node group is the one place in fault-quarantine that says what a node type is.
- **Small scope.** The effect of a trip, the reset, and the rule sets do not change. There is still one event stream with one resume position. The work uses only shared code that is on main.
- **Opt-in.** A cluster with no node groups does not change.
- **Fail closed.** A rule error never moves a node to weaker limits. fault-quarantine does not quarantine a node with a rule error until the node has a placement, and it continues with the other nodes. A node with a missing or wrong domain label keeps the node bounds of its group. Only the domain bound can count such nodes incorrectly, and the node bounds limit that case.
- **One deployment.** One component owns the quarantine annotations of each node and reads the event stream.

## Consequences

### Positive

- A burst of quarantines on a node type that has its own node group counts only against the limits of that group. It does not count against the limits of the other node types.
- Each node type gets limits that fit how it fails.
- One failed domain, for example a rack, can be quarantined completely with no trip and no manual reset.
- The reset is the reset of today, and `trippedBy` names the breaker that tripped.

### Negative

- A trip of any breaker still stops quarantines and releases for all nodes.
- Operators have more configuration and more metrics.
- Startup rejects a rule that does not compile or that reads a key with no test (see [Validation](#validation)). But a rule can still fail at run time, for one node or for many nodes. fault-quarantine then does not quarantine those nodes until the rule or the nodes are corrected. This includes a forced quarantine. The nodes stay in service with their faults, and an event that would start a quarantine in that time is skipped and not tried again. A quarantined node with a rule error is not in a standing count until it has a placement, so the standing bounds of its group count fewer nodes in that time. The event stream and the nodes that have a placement are not affected.
- The bounds of a group with domains depend on the size of a domain, so each cluster needs its own values.
- On a cluster that is one domain, the breaker cannot tell a failed domain from a bad signal on every node.
- For nodes without a domain label, the domain bound is weaker, because it counts them as one domain. The node bounds limit that case.
- With a domain label that a discovery component computes, a label that disappears or changes during a fault can make the domain bound count too many domains or too few. The node bounds limit both cases.
- The membership rule (one winner) is different from the fault-quarantine rule sets (every match applies).
- A node type is defined in two places: the `Node` rules of the quarantine rule sets and the node group. If the two differ, the quarantines of those nodes count against a different breaker than the operator expects. A node that has a placement always counts against one breaker: the group that the node matches, or the cluster breaker. A group whose nodes no quarantine rule set covers never counts a node.

### Mitigations

- Startup rejects a rule that does not compile or that reads a key with no test, so most incorrect rules stop startup before the first event.
- The gauge for nodes with no placement shows that fault-quarantine does not quarantine a node, and the documentation gives an alert for a value above zero. The rule error counter shows the group, and the log names the node.
- The `group` label lets operators make an alert for each group.
- The documentation gives sizing rules for groups with domains.
- The documentation states the membership rule, with a link to ADR-064.
- The node count of each breaker, in the startup log and in the metrics, shows a difference between the two definitions of a node type. A reference from a rule set to a node group removes the second definition (see [Future work](#future-work)).

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

#### Stop the event stream on a group rule error

**Rejected** because: an error on one node would then stop the quarantine and the release of all nodes.

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
- **Domain bounds on the cluster breaker.** To use domains, a cluster with one node type needs a node group that matches every node. `failureDomains` on the top-level `circuitBreaker` would remove that need.
- **The domain of a node that loses or changes its label before its quarantine.** This ADR counts the node in the unknown domain or in the domain of the new value, so the domain bound can count too many domains or too few. A later change can correct this in one of two ways. In the first, fault-quarantine writes the domain to an annotation on each node of the group when it first reads the label. The value is visible and a restart does not change it, but fault-quarantine then writes to nodes that are not quarantined. In the second, the domain bound does not count the unknown domain, and only the node bounds apply to its nodes. This needs no state, but the domain bound is weaker for a node without a label.
- **A report in dry run.** A report-only evaluation in dry run could show the counts of each group. Operators could then size the limits before a live run.
- **Automatic reset.** A trip latches by design, and the operator chooses between replay and discard at the reset (ADR-022). [#2203](https://github.com/NVIDIA/NVSentinel/issues/2203) asks for a reset with no operator action after the nodes recover. A follow-up ADR can propose one.
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
