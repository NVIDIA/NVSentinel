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

# ADR-064: Configuration — Node and Device Selection in Each Component

Status: Proposed.

## Table of Contents

1. [Context](#context)
2. [Decision](#decision)
3. [Implementation](#implementation)
   - [Overview](#overview)
     - [Shared rule set schema](#shared-rule-set-schema)
     - [fault-quarantine](#fault-quarantine)
     - [node-drainer](#node-drainer)
     - [fault-remediation](#fault-remediation)
   - [Design details](#design-details)
     - Rule sets: [Rule set evaluation](#rule-set-evaluation), [Validation](#validation), [Metrics](#metrics), [Differences from fault-quarantine](#differences-from-fault-quarantine), [Code](#code)
     - Node data: [Node fields and the node cache](#node-fields-and-the-node-cache)
     - node-drainer: [The drain decision](#the-drain-decision), [One drain method for each session](#one-drain-method-for-each-session), [Already drained](#already-drained), [Custom drain scope](#custom-drain-scope), [Pod policies for each rule set](#pod-policies-for-each-rule-set), [Existing configurations](#existing-configurations), [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector)
     - fault-remediation: [The remediation decision](#the-remediation-decision), [Partial drains and actions on the full node](#partial-drains-and-actions-on-the-full-node)
     - Across components: [Quarantine session](#quarantine-session), [Node state labels](#node-state-labels), [The record on the event](#the-record-on-the-event), [Changes for a fault in progress](#changes-for-a-fault-in-progress)
   - [Phases](#phases)
4. [Rationale](#rationale)
5. [Consequences](#consequences)
6. [Alternatives Considered](#alternatives-considered)
7. [Notes](#notes)
   - [Non-goals](#non-goals)
   - [Open questions](#open-questions)
8. [References](#references)

## Context

Many clusters contain different types of nodes and devices. Examples are bare-metal nodes and virtual machines, Slurm nodes and Kubernetes nodes, different GPU models, and accelerators that are not GPUs. Operators must set a different behaviour for each group ([#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904)). Typical cases are:

1. The operator enables automated remediation for one group of nodes and monitors the results. Then the operator enables it for more groups.
2. NVSentinel drains a group of nodes, and an external system repairs them ([ADR-040](040-external-remediation-request.md)).
3. NVSentinel resets bare-metal nodes and replaces virtual machines.
4. NVSentinel drains Slurm nodes with a drain plugin and drains all other nodes with the eviction API ([#1857](https://github.com/NVIDIA/NVSentinel/issues/1857)).
5. NVSentinel reboots the node after a fault on one device type or GPU model, but not after a fault on a different one.
6. A group of nodes only gets monitoring, or only gets monitoring and a quarantine.

Each component that acts on a fault already reads the node, and two of them can already select by node:

| Stage | Control for each node at this time | Limitation that this ADR addresses |
|---|---|---|
| platform-connectors | `managed=false` changes events to `STORE_ONLY`. Override rules ([ADR-021](021-health-event-property-overrides.md)) can change `recommendedAction` | None |
| fault-quarantine | CEL `Node` and `HealthEvent` rules in each rule set ([ADR-003](003-rule-based-node-quarantine.md)) | None |
| node-drainer | `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) and `podDrainPolicies` ([pod label drain policies ADR](055-pod-drain-policies.md)) | Cannot stop a group of nodes before the drain. `podDrainPolicies` can leave pods out of the drain, but the drain still completes and remediation starts. `userNamespaces` and `podDrainPolicies` apply to the full cluster. Checks the selector again on each pass, so a label change during a drain can change the drain method. Records the method only in a log |
| fault-remediation | None | Cannot stop remediation or select a different action for a group of nodes |

Thus, case 4 and the monitoring-only form of case 6 work at this time. The gap is mostly in fault-remediation, which has no selection.

This ADR adds the missing controls to node-drainer and fault-remediation, with one rule set schema that both components use. The schema starts from the fault-quarantine rule sets.

## Decision

node-drainer and fault-remediation get rule sets in the form of the fault-quarantine rule sets. An operator can then select a different drain or remediation for each group of nodes and each type of fault. After fault-quarantine quarantines a node, each later stage decides for each event with its own rule sets:

| Stage | Rule sets | Outcome | No match |
|---|---|---|---|
| fault-quarantine | The rule sets of this time. No change | Taints, labels, and the cordon | No quarantine |
| node-drainer | New `ruleSets` | `drain`: `stop`, or the drain method and its pod policies | `BuiltIn` with the top-level lists, or `Custom` when the custom drain is not scoped |
| fault-remediation | New `ruleSets` | `remediation`: `stop`, or the action | The action of the event |

1. **A shared rule set schema for node-drainer and fault-remediation.** A rule set has a common part (`name`, `enabled`, `priority`, and `match`) and one outcome key that is named for the component (`drain` or `remediation`). The common part, the CEL variables, the meaning of `priority`, and the result of an error are the same in both components. An operator learns one format, and a rule set moves from one component to the other with a change to the outcome only. The exception is a `Node` rule that reads the status, which node-drainer does not keep (see [Node fields and the node cache](#node-fields-and-the-node-cache)). The types and the evaluator are in `commons`.
2. **Each component keeps its own configuration.** There is no shared profile and no shared definition of node groups.
3. **fault-quarantine:** no change. Its rule sets already select by node and by event. The shared schema is based on its rule sets and its node cache, but this ADR does not change its configuration or its behaviour (see [Differences from fault-quarantine](#differences-from-fault-quarantine)).
4. **node-drainer:** add `ruleSets` with the `drain` outcome. The outcome is `stop: true` or a `method`. `BuiltIn` is the eviction by node-drainer. `Custom` is the custom drain CR. A `BuiltIn` outcome can set its own `userNamespaces` or `podDrainPolicies`, which replace the top-level list for the events that use the rule set. The top-level lists are the default, and they are not deprecated. An event that no rule set matches uses `BuiltIn` with the top-level lists, or `Custom` when the custom drain is not scoped (see [Custom drain scope](#custom-drain-scope)).
5. **fault-remediation:** add `ruleSets` with the `remediation` outcome. The outcome is `stop: true` or an `action` from `maintenance.actions`. No match gives the action of the event. fault-remediation decides before it uses the action of the event, so the selected entry applies in each later step, including the equivalence group and `maxRemediationAttempts` (see [The remediation decision](#the-remediation-decision)).
6. **A stop is not a failure.** A stop is recorded on the event, and the node state is `drain-stopped` or `remediation-stopped`, not a failure state (see [Node state labels](#node-state-labels)).
7. **Each decision is made one time and recorded on the event.** Later passes read the record, so a label or configuration change does not change a decision that is in progress. The exception is the pod policies of a `BuiltIn` drain, which are not in the record and follow the configuration. node-drainer selects the drain method one time for each [quarantine session](#quarantine-session), when it starts the first drain of the session. Each later event of the session uses the same method and rule set, but can still stop (see [One drain method for each session](#one-drain-method-for-each-session)). node-drainer records the rule set, the method or the stop, and the scope. fault-remediation records the rule set and the action (see [The record on the event](#the-record-on-the-event)).
8. **Device selection reads the health event.** A `HealthEvent` rule can read each field of the event. Device information that a later change adds to the event is available to the rules with no change to this ADR. Changes that add device information to the event are not part of this ADR.
9. **Existing configurations continue to work.** Without `ruleSets`, each component behaves as at this time, except for two node-drainer changes (see [Existing configurations](#existing-configurations)). `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) is deprecated. It continues to work as a `Custom` rule set with the lowest priority (see [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector)).

| Case | Configuration |
|---|---|
| 1. Staged rollout | fault-remediation rule set: `remediation: { stop: true }` for nodes that are not in the wave |
| 2. External repair | fault-remediation rule set: an action entry for an `ExternalRemediationRequest` |
| 3. Reset bare metal, replace VMs | fault-remediation rule set: `COMPONENT_RESET` on VM nodes selects a `TerminateNode` entry |
| 4. Slurm drain plugin | node-drainer rule set: `drain: { method: Custom }` for Slurm nodes. `customDrain.nodeSelector` works at this time |
| 5. Device type or model | A rule set on `event.componentClass`, `event.entitiesImpacted`, or other device fields of the event, in any of the three components |
| 6. Monitoring only | fault-quarantine rule sets whose `Node` rule excludes the group, at this time |
| 6. Monitoring and quarantine | node-drainer rule set: `drain: { stop: true }` |

## Implementation

The [overview](#overview) shows what changes in each component, with an example. The [design details](#design-details) describe each decision in full, and each detail section can be read by itself.

### Overview

#### Shared rule set schema

node-drainer and fault-remediation use one rule set schema. Both components read it from the top-level key `ruleSets` of their configuration.

```yaml
ruleSets:
  - name: vm-nodes              # required. Unique DNS-1123 label in the component
    enabled: true               # optional. Default: true
    priority: 10                # optional. Default: 0. The highest priority wins a conflict
    match:                      # required. Exactly one of all or any
      all:                      # every rule must match. With any: at least one rule must match
        - kind: Node
          expression: >-
            "example.com/platform" in node.metadata.labels &&
            node.metadata.labels["example.com/platform"] == "vm"
        - kind: HealthEvent
          expression: 'event.componentClass == "GPU"'
    <outcome>:                  # exactly one outcome key: drain or remediation
      stop: true                # or the field of the component: method or action
```

Each rule set has one outcome key. The key is the name of the stage that reads it. The outcome has `stop: true`, or the field that selects the behaviour of the stage. A component accepts only its own outcome key, and startup fails for a different key.

| Key | Component | `stop: true` | Selection field | No match |
|---|---|---|---|---|
| `drain` | node-drainer | No drain. Node state `drain-stopped`, unless another event of the session has already moved the node to a later state (see [Node state labels](#node-state-labels)) | `method`: `BuiltIn` or `Custom`. `BuiltIn` can also set `userNamespaces` or `podDrainPolicies` | `BuiltIn` with the top-level lists. `Custom` when the custom drain is enabled and not scoped (see [Custom drain scope](#custom-drain-scope)) |
| `remediation` | fault-remediation | No CR. Node state `remediation-stopped`, unless another event of the session has already moved the node to a later state (see [Node state labels](#node-state-labels)) | `action`: an entry of `maintenance.actions` | The action of the event |

The component evaluates the enabled rule sets in order of `priority`, and the first rule set that matches wins. If a rule set before the winner cannot be evaluated, the component does not decide, and it tries again (see [Rule set evaluation](#rule-set-evaluation)).

In node-drainer, when another event of the [quarantine session](#quarantine-session) has already selected the method, an event that does not stop uses that method, not the method of its own winner (see [One drain method for each session](#one-drain-method-for-each-session)).

#### fault-quarantine

This ADR does not change fault-quarantine. Its rule sets and its node cache are the basis of the shared schema.

At this time, the fault-quarantine rule sets select by node labels and by event fields. All rule sets that match apply, and `priority` resolves conflicts between their taints and labels. The cordon-reason label lists the rule sets that matched.

**Monitoring only.** An operator excludes a group from quarantine with a `Node` rule, for example `!("example.com/pool" in node.metadata.labels && node.metadata.labels["example.com/pool"] in ["legacy-a", "legacy-b"])`. Health events and node conditions continue for these nodes. No later stage runs, because node-drainer starts only for a quarantined node.

The shared schema differs from the fault-quarantine rule sets in seven ways (see [Differences from fault-quarantine](#differences-from-fault-quarantine)).

#### node-drainer

```yaml
ruleSets:
  - name: quarantine-only
    priority: 100
    match:
      all:
        - kind: Node
          expression: >-
            "example.com/pool" in node.metadata.labels &&
            node.metadata.labels["example.com/pool"] == "quarantine-only"
    drain:
      stop: true
  - name: lpu-no-drain
    priority: 90
    match:
      all:
        - kind: HealthEvent
          expression: 'event.componentClass == "LPU"'
    drain:
      stop: true
  - name: slurm
    priority: 10
    match:
      all:
        - kind: Node
          expression: >-
            "scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"
    drain:
      method: Custom
  - name: inference-fast
    priority: 10
    match:
      all:
        - kind: Node
          expression: >-
            "example.com/pool" in node.metadata.labels &&
            node.metadata.labels["example.com/pool"] == "inference"
    drain:
      method: BuiltIn
      userNamespaces:                       # replaces the top-level list on these nodes
        - name: "*"
          mode: Immediate
userNamespaces:                             # the default for BuiltIn. Does not change
  - name: "*"
    mode: AllowCompletion
customDrain:
  enabled: true
  # Deprecated. Use a rule set with drain.method: Custom.
  # nodeSelector: "scheduler=slurm"
  templateFileName: ...                     # the template and CR settings do not change
```

| Node or event | Winner | Drain |
|---|---|---|
| `example.com/pool=quarantine-only` | `quarantine-only` | Stop |
| An LPU event on any node | `lpu-no-drain` | Stop |
| `scheduler=slurm` | `slurm` | Custom drain CR |
| `example.com/pool=inference` | `inference-fast` | Eviction by node-drainer, `*` in `Immediate` mode |
| Other nodes | None | Eviction by node-drainer, with the top-level `userNamespaces`. The `slurm` rule set selects `Custom`, so the custom drain is scoped (see [Custom drain scope](#custom-drain-scope)) |

**Use of the schema.** The outcome key is `drain`. A `Node` rule reads the Node from the node informer of node-drainer. A `HealthEvent` rule reads the event, so a rule set can also select by fault type. A label selector cannot. `method: Custom` is valid only when `customDrain.enabled` is true. The Helm chart and startup validation check this.

**The decision.** Before node-drainer drains for an event, it applies `drainOverrides.skip`, then `stop`, then the check for an earlier drain that covers the event, then the method of the session, and then the winner or the default. It records the decision on the event before it acts. The details are:

- The order of the decision, `stop`, and errors: [The drain decision](#the-drain-decision).
- One method for all events of a quarantine session: [One drain method for each session](#one-drain-method-for-each-session).
- Full and partial drains of earlier events: [Already drained](#already-drained).
- The default for an event that no rule set matches: [Custom drain scope](#custom-drain-scope).
- Eviction modes for each group: [Pod policies for each rule set](#pod-policies-for-each-rule-set).
- Upgrades and `customDrain.nodeSelector`: [Existing configurations](#existing-configurations) and [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector).

#### fault-remediation

```yaml
maintenance:
  actions:
    COMPONENT_RESET: { kind: RebootNode, ... }       # unchanged
    REPLACE_VM: { kind: TerminateNode, ... }         # unchanged
    EXTERNAL_REPAIR: { kind: ExternalRemediationRequest, ... }
ruleSets:
  - name: lpu-no-remediation
    priority: 100
    match:
      all:
        - kind: HealthEvent
          expression: 'event.componentClass == "LPU"'
    remediation:
      stop: true
  - name: outside-rollout
    priority: 90
    match:
      all:
        - kind: Node
          expression: >-
            !("example.com/remediation-wave" in node.metadata.labels &&
              node.metadata.labels["example.com/remediation-wave"] == "1")
    remediation:
      stop: true
  - name: vm-replace          # see "Partial drains and actions on the full node"
    priority: 10
    match:
      all:
        - kind: Node
          expression: >-
            "example.com/platform" in node.metadata.labels &&
            node.metadata.labels["example.com/platform"] == "vm"
        - kind: HealthEvent
          expression: 'event.recommendedAction == 2'   # COMPONENT_RESET
    remediation:
      action: REPLACE_VM
  - name: partner-repair
    priority: 10
    match:
      all:
        - kind: Node
          expression: >-
            "example.com/repair" in node.metadata.labels &&
            node.metadata.labels["example.com/repair"] == "partner"
    remediation:
      action: EXTERNAL_REPAIR
```

**Use of the schema.** The outcome key is `remediation`. A `Node` rule reads the Node that fault-remediation gets from the API server for the event. fault-remediation has no Node cache ([#1758](https://github.com/NVIDIA/NVSentinel/pull/1758)), so the labels are current. Each `action` must be a key of `maintenance.actions`. The Helm chart and startup validation check this.

**The decision.** fault-remediation evaluates the rule sets one time for each event, after the checks for `NONE` and for an event that is already remediated, and before it uses the action of the event. The selected entry replaces the action of the event in each later step. A stopped event gets no CR, and the node state is `remediation-stopped`, unless another event of the session has already moved the node to a later state. The details are in [The remediation decision](#the-remediation-decision) and [Partial drains and actions on the full node](#partial-drains-and-actions-on-the-full-node).

### Design details

The sections below describe each decision in full. They are grouped by area:

- **Rule sets:** [Rule set evaluation](#rule-set-evaluation), [Validation](#validation), [Metrics](#metrics), [Differences from fault-quarantine](#differences-from-fault-quarantine), [Code](#code).
- **Node data:** [Node fields and the node cache](#node-fields-and-the-node-cache).
- **node-drainer:** [The drain decision](#the-drain-decision), [One drain method for each session](#one-drain-method-for-each-session), [Already drained](#already-drained), [Custom drain scope](#custom-drain-scope), [Pod policies for each rule set](#pod-policies-for-each-rule-set), [Existing configurations](#existing-configurations), [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector).
- **fault-remediation:** [The remediation decision](#the-remediation-decision), [Partial drains and actions on the full node](#partial-drains-and-actions-on-the-full-node).
- **Across components:** [Quarantine session](#quarantine-session), [Node state labels](#node-state-labels), [The record on the event](#the-record-on-the-event), [Changes for a fault in progress](#changes-for-a-fault-in-progress).

#### Rule set evaluation

**The common part.** `name`, `enabled`, `priority`, and `match` have the same meaning in each component:

- `enabled` is true when it is not set. A rule set with `enabled: false` is not evaluated.
- `match` has exactly one of `all` and `any`. With `all`, the rule set matches when every rule matches. With `any`, it matches when at least one rule matches.
- A condition that needs `all` and `any` together is written as CEL in one rule, or as more than one rule set. In one rule, `&&` and `||` combine conditions of the same kind. An `any` across kinds, for example "the node is in the legacy pool, or the event is from an LPU", can be two rule sets with the same outcome.
- Each rule has a `kind` and a CEL `expression` that returns a boolean:
  - `kind: HealthEvent` reads `event`, which has all fields of the health event, including `componentClass` and `entitiesImpacted`. The evaluator converts each field of the event, so a field that is added to the event later is available with no change to the schema. Enum fields, such as `recommendedAction`, are numbers, as they are in fault-quarantine.
  - `kind: Node` reads `node`, the Node object. In node-drainer, the fields that the rules read decide what the node cache keeps. See [Node fields and the node cache](#node-fields-and-the-node-cache).
- A CEL map lookup of a missing label fails. A rule must test for the key first.

**Priority.** When the outcomes of two rule sets that match are in conflict, the rule set with the higher `priority` applies. If the priorities are equal, the first in the configuration applies. Each outcome defines what a conflict is:

| Outcome | A conflict is |
|---|---|
| `drain` | Any two rule sets that match. The outcome is one choice |
| `remediation` | Any two rule sets that match. The outcome is one choice |
| fault-quarantine (for reference, not part of this schema) | Two rule sets that match and set the same taint key and value, or the same label key. The other fields combine |

**One winner.** The `drain` and `remediation` outcomes are each one choice, so one rule set applies. The component evaluates the enabled rule sets in order of `priority`, the highest first, and in the sequence of the configuration for equal priorities. The first rule set that matches wins, and the component does not evaluate the rule sets after it. Only the outcome of the winner applies.

**An error stops the decision.** If a rule set before the winner cannot be evaluated, the component does not use the result of the other rule sets, and it does not use the default. The failed rule set could have won, for example with `stop` for the node. A rule set after the winner is not evaluated, so it cannot stop the decision. Thus, a rule set with an error stops only the decisions in which no rule set with a higher priority matches. The component logs the rule set name and the error, and a metric counts the error for each rule set. What each component does then is in [The drain decision](#the-drain-decision) and [The remediation decision](#the-remediation-decision).

Startup validation cannot find all errors. For example, a rule that reads a label without a test for the key fails on each node that does not have the label. The component tries the decision again with its existing requeue backoff, and the metric shows the error on each attempt.

#### Validation

**The common part.** The Helm chart checks the structure, and `helm install` or `helm upgrade` fails for an error:

- Rule set names are unique DNS-1123 labels.
- `match` has exactly one of `all` and `any`, and that list has at least one rule.
- Each rule set has exactly one outcome key, and the key is the key of the component.
- The outcome has exactly one of `stop: true` and the selection field.

The component checks all of the items above again at startup, and also checks the CEL, which Helm cannot do. Startup fails for an error:

- Each `expression` compiles and returns a boolean.

The component also logs a warning at startup, but continues, for:

- A rule that reads `node.metadata.labels["<key>"]` but does not test `"<key>" in node.metadata.labels`. The rule fails at runtime on each node that does not have the label.
- In node-drainer, a `Node` rule that makes the node cache keep all labels ([Node fields and the node cache](#node-fields-and-the-node-cache)).
- In node-drainer, a `Node` rule that reads the status. The node cache does not keep the status, so the rule reads an empty status.

**node-drainer.** The Helm chart and startup validation fail for:

- A `drain` outcome with both `userNamespaces` and `podDrainPolicies`.
- `userNamespaces` or `podDrainPolicies` with `method: Custom` or `stop: true`. These outcomes do not evict, so the list has no effect.
- A `method` other than `BuiltIn` and `Custom`.
- `method: Custom` when `customDrain.enabled` is false.
- An eviction mode that does not exist.

Startup validation also fails for:

- A `podSelector` that does not parse.
- A `BuiltIn` outcome without its own list when both top-level lists are empty. node-drainer would mark the drain complete and evict no pods.
- Top-level lists that the [custom drain scope](#custom-drain-scope) does not accept.
- A rule set with `method: Custom` when `customDrain.nodeSelector` is also set (see [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector)).

**fault-remediation.** The Helm chart and startup validation fail for an `action` that is not a key of `maintenance.actions`.

#### Metrics

Each component counts the wins and the errors of each rule set, with the same metric names and labels. A win is counted for the winner of the evaluation of an event, also when node-drainer uses the method of the session for the event. Thus, a rule set that another rule set always hides has a count of zero. node-drainer also counts the events that use the method of the session. The rule set name is a metric label. The number of rule sets in the configuration limits its cardinality, not the number of nodes or events.

#### Differences from fault-quarantine

This section describes what the shared schema takes from fault-quarantine and where it differs. The differences are not changes to fault-quarantine, and this ADR does not implement them.

**What the shared schema takes from fault-quarantine.** The `name` and `match` fields, the meaning of `priority` (the higher priority wins a conflict), the `HealthEvent` and `Node` rule kinds, the CEL environments, the `event` and `node` variables, and the pruning of the node cache. A rule that works in fault-quarantine works in the shared schema with no change.

**Where the shared schema differs.**

| fault-quarantine | Shared schema |
|---|---|
| Key `rule-sets` in TOML, `ruleSets` in Helm values | Key `ruleSets` in TOML and in Helm values |
| `enabled` is false when it is not set | `enabled` is true when it is not set |
| `taint`, `label`, and `cordon` are next to `match` | The outcome is under one key that names the stage |
| A rule set can have `all` and `any`, and it applies if either one matches | A rule set has exactly one of `all` and `any` |
| A rule set that fails does not match | An error in a rule set before the winner stops the decision |
| A `version` field on each rule set | No `version` field |
| A conflict at equal priority: for a label, the last rule set in the configuration wins. For a taint effect, the first rule set that the evaluation finishes wins, and the rule sets are evaluated concurrently | A conflict at equal priority: the first rule set in the configuration wins |

#### Code

The schema is in `commons/pkg/ruleset`:

- The common part is a Go type with `toml` and `json` tags. Each component adds its outcome type to it. The Helm chart can make a `values.schema.json` from these types, and other NVSentinel code can use them.
- The CEL rule evaluators for `HealthEvent` and `Node` move from `fault-quarantine/pkg/evaluator` to `commons`. The CEL environments and the conversion of the event and the Node do not change. Thus, a rule that works in fault-quarantine works in the shared schema.
- A `Node` rule gets the Node from an interface, not from the fault-quarantine informer. node-drainer gives its node informer lister. fault-remediation gives the Node that it has already read for the event. fault-quarantine gives its informer lister and the direct API read that it uses at cold start.
- The rule set layer (`enabled`, `match`, the winner, and the errors) is new in `commons`. fault-quarantine keeps its own rule set layer.

#### Node fields and the node cache

**node-drainer at this time.** The node informer keeps only the name, the [`quarantineHealthEvent` annotation](#quarantine-session), and the label keys that `customDrain.nodeSelector` reads (`node-drainer/pkg/informers/informers.go`). It drops the spec, the status, and the other annotations.

**node-drainer with rule sets.** The informer prunes each Node to what the `Node` rules read, in the same way as the fault-quarantine node cache (`fault-quarantine/pkg/nodecache`). At startup, the component finds the fields that each `Node` rule reads, with `commons/pkg/celfields`. The informer keeps:

- The spec and the identity metadata, such as the name. The spec is new, as the fault-quarantine node cache keeps it.
- The labels and the annotations whose keys the rules name with a string literal, as `node.metadata.labels["<key>"]` or `"<key>" in node.metadata.labels`.
- The keys that node-drainer reads for its own use, such as the [`quarantineHealthEvent` annotation](#quarantine-session).

The informer does not keep the status. A rule that reads the status gets an empty status, as in fault-quarantine. node-drainer logs a warning at startup for each such rule, with the rule set name and the expression. The same rule reads the full status in fault-remediation.

If the component cannot name the label keys that a rule reads, the informer keeps all labels of each node. For example:

| Expression | The informer keeps |
|---|---|
| `"scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"` | The `scheduler` label |
| `"example.com/team" in node.metadata.annotations` | The `example.com/team` annotation |
| `node.spec.unschedulable` | The spec is always kept |
| `node.metadata.labels.exists(k, k.startsWith("example.com/"))` | All labels |
| `size(node.metadata.labels) > 0` | All labels |
| `node.metadata.labels[node.metadata.name]` | All labels. The key is not a literal |

**The cost of keeping all labels.** A node can have about a hundred labels, and on a large cluster the labels are most of the size of the cache. Thus, one change to one rule can increase the memory of node-drainer from a few label keys for each node to all labels of all nodes, and the configuration does not show it. To make the cause visible:

- At startup, node-drainer logs a warning for each rule that makes the cache keep all labels, with the rule set name and the expression.
- The documentation describes this cost, lists the forms of rule that keep the cache small, and shows how to replace a label prefix with a label that has a known key.

**fault-remediation.** fault-remediation has no Node cache. It reads the full Node from the API server for each event, so a `Node` rule can read any field, and the rules do not change the memory of fault-remediation. The reads are in [The remediation decision](#the-remediation-decision).

#### The drain decision

**The method names.** `BuiltIn` is the eviction that node-drainer does itself. `Custom` is the custom drain CR, which an external controller completes. The controller can also evict pods, so the names tell who drains the node, not how.

**The order.** Before node-drainer evicts the first pod or creates the custom drain CR for an event, it selects the drain method:

1. `drainOverrides.skip` on the event applies first, as it does at this time. node-drainer marks the drain `AlreadyDrained` and does not evaluate the rule sets, so remediation continues. This is different from `stop: true`, which also stops remediation. An override on one event is more specific than a rule set. `drainOverrides.force` does not select `stop` or the method. It applies on the `BuiltIn` path, as it does at this time.
2. If the winning rule set gives `stop: true`, node-drainer stops the drain for the event (see **Stop** below). An event can stop when another event of the session already drains the node.
3. If an earlier drain of the session covers the event, node-drainer marks the drain `AlreadyDrained`, as it does at this time (see [Already drained](#already-drained)).
4. If another event of the [quarantine session](#quarantine-session) has a recorded method, the event uses that method and the rule set of that record, so the pod policies are also the same. The drain scope still comes from the event (see [One drain method for each session](#one-drain-method-for-each-session)).
5. Otherwise, the winning rule set gives the method. For `BuiltIn`, the pod policies are those of the rule set, or the top-level lists if the rule set has none. The winner can be the `custom-drain-node-selector` rule set, which node-drainer makes from the deprecated `customDrain.nodeSelector` (see [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector)).
6. If no rule set matches, and the custom drain is enabled and not scoped, the method is `Custom`, as at this time (see [Custom drain scope](#custom-drain-scope)).
7. Otherwise, the method is `BuiltIn` with the top-level lists.

node-drainer records the rule set, the method or the stop, and the drain scope (the full node, or the entity type and value for a partial drain) on the event status before it acts. Later passes for the same event read the record and do not evaluate the rule sets again. A cold start reads the record.

**Stop.** node-drainer records `userPodsEvictionStatus: Stopped`, a new value, and the node state `drain-stopped` (see [Node state labels](#node-state-labels)). `Stopped` is different from `AlreadyDrained`. fault-remediation starts only for `Succeeded` and `AlreadyDrained`, so it does not start for this event. The node stays in quarantine until the [quarantine session](#quarantine-session) ends, when the last failure recovers or the operator removes the quarantine. For a fault that does not recover by itself, this is until the operator acts.

**An error.** node-drainer does not drain, does not record a decision, and does not change the node state. It logs the rule set name and the error, increments the metric, and tries again with backoff, as it does at this time when it cannot evaluate an event. The next pass evaluates the rule sets again, so a label or configuration change can correct the failure.

**No Node.** If the informer cannot return the Node, node-drainer does not evaluate the rule sets and does not set a node state. It waits and tries again, as it does for `customDrain.nodeSelector` at this time. The `custom-drain-node-selector` rule set behaves the same way.

#### One drain method for each session

The first event of the [quarantine session](#quarantine-session) that node-drainer drains selects the method, and the later events of the session use it (steps 4 to 7 of [the order](#the-drain-decision)). Two events of a session do not drain the node with different methods, for example a custom drain CR for one event and the eviction by node-drainer for another. The exception is a drain that is in progress at the upgrade, which has no record and can change its method one time (see [Existing configurations](#existing-configurations)). A stopped event records `stopped` and no method, so it does not select the method of the session.

node-drainer finds the other events in the datastore, as described in [Quarantine session](#quarantine-session), and not in the `quarantineHealthEvent` annotation. An event that recovers leaves the annotation, but its drain can still be in progress.

When the event uses the method of another event, the record names that event in `reusedFrom`. Because later passes read the record, a label change during a drain, or between two events of a session, does not change the method.

The record does not copy the pod policies. A later pass reads the policies of the recorded rule set from the configuration. Thus, a configuration change during a drain changes the policies that apply to the pods that remain, as a change to the top-level lists does at this time. If the recorded rule set is no longer in the configuration, node-drainer does not evaluate the rule sets again, so the method does not change. For `BuiltIn`, it uses the top-level lists, logs a warning, and increments a metric. If both top-level lists are empty, as they are when the custom drain is not scoped (see [Custom drain scope](#custom-drain-scope)), node-drainer does not complete the drain, because it would evict no pods. It logs an error, increments the metric, and tries again with backoff. The node state stays `draining` until the rule set is in the configuration again or the [quarantine session](#quarantine-session) ends. node-drainer does the same for an event that uses the method of another event, when the rule set of that record is no longer in the configuration.

#### Already drained

Step 3 of [the order](#the-drain-decision) keeps the check of this time (`commons/pkg/drain`) for an event that fault-quarantine marks `AlreadyQuarantined`. If an earlier event of the session has completed a drain that covers the event, node-drainer marks the event `AlreadyDrained` and does not drain:

| Earlier completed drain | This event: partial drain, same entity | This event: partial drain, other entity | This event: full drain |
|---|---|---|---|
| Full | `AlreadyDrained` | `AlreadyDrained` | `AlreadyDrained` |
| Partial | `AlreadyDrained` | Drain | Drain |

- A completed drain is `Succeeded`, or `AlreadyDrained` that does not come from `drainOverrides.skip`, as at this time. A `Stopped` event did not drain, so it does not count.
- The check comes after `stop`. Thus, a `stop` rule set applies to an event when an earlier drain is complete, as it does when that drain is in progress.
- The check reads the scope of the earlier event from its record. At this time, node-drainer derives the scope again from the action and the entities of the event, with the current `partialDrainEnabled`. If `partialDrainEnabled` changes to false after a partial drain, that drain then counts as a full drain, and it covers a later full drain of pods that it did not evict. The record removes this error. An event that has no record, because it is from before the upgrade, uses the derivation, as at this time.
- The check reads the earlier events from the `quarantineHealthEvent` annotation, as at this time. Thus, the drain of an event that has recovered does not count. The method in step 4 reads the events of the session from the datastore instead.

#### Custom drain scope

At this time, `customDrain.nodeSelector` scopes the custom drain (`IsScoped` in `node-drainer/pkg/config/custom_drain.go`). With a selector, the nodes that the selector does not match use the eviction by node-drainer. Without a selector, all nodes use the custom drain. With rule sets, the custom drain is scoped when `customDrain.enabled` is true and either:

- `customDrain.nodeSelector` is set. node-drainer converts it to the `custom-drain-node-selector` rule set.
- A rule set selects `Custom`. A rule set with `enabled: false` also counts. Thus, disabling a rule set turns off only its own match. It does not change the method of the nodes that no rule set matches.

The scope decides the method for an event that no rule set matches, and the top-level lists that startup validation accepts:

| Custom drain | An event that no rule set matches | Top-level `userNamespaces` and `podDrainPolicies` |
|---|---|---|
| `customDrain.enabled` is false | `BuiltIn` with the top-level lists | As at this time |
| Enabled and scoped | `BuiltIn` with the top-level lists | One is required. Startup fails if both are empty, because node-drainer would mark the drain complete and evict no pods. This check exists at this time for `customDrain.nodeSelector` |
| Enabled and not scoped | `Custom`, as at this time | Both must be empty. Startup fails if one is set, as at this time |

In the [example](#node-drainer), the `slurm` rule set selects `Custom`. Thus, the custom drain is scoped, and the other nodes use `BuiltIn` with the top-level `userNamespaces`.

An operator who uses the custom drain on all nodes has empty top-level lists. When the operator adds the first rule set that selects `Custom`, the custom drain becomes scoped, and the nodes that no rule set matches change from `Custom` to `BuiltIn`. Startup fails until the operator sets a top-level list, so this change is not silent.

#### Pod policies for each rule set

At this time, `userNamespaces` and `podDrainPolicies` decide how node-drainer evicts each pod, and they apply to the full cluster. A `BuiltIn` outcome can set one of them, so that a group of nodes uses different eviction modes:

```yaml
drain:
  method: BuiltIn
  podDrainPolicies:                 # or userNamespaces. Not both
    - name: checkpointing-jobs
      namespace: training-*
      podSelector: "example.com/checkpoint=true"
      mode: AllowCompletion
    - name: training-workers
      namespace: training-*
      ownerKinds: ["Job"]
      mode: DeleteAfterTimeout
```

- The list of the rule set replaces the top-level list for the events that use the rule set: the events that it wins, and the later events of the same [quarantine session](#quarantine-session). The two lists are not merged, so one rule set decides the pod policies of a drain.
- The list has the same fields, modes, and validation as the top-level list, including `ownerKinds` ([ADR-063](063-pod-drain-policies-owner-kinds.md)). The first `podDrainPolicies` entry that matches a pod applies, as at this time. The checks for the lists in rule sets are in [Validation](#validation).
- The top-level `userNamespaces` and `podDrainPolicies` do not change. They are the default for an event that no rule set matches, and for a `BuiltIn` rule set without its own list. They are not deprecated.
- `partialDrainEnabled`, `deleteAfterTimeoutMinutes`, and the eviction timeout stay at the top level. `partialDrainEnabled` must agree with fault-quarantine, and with the `COMPONENT_RESET` entry of fault-remediation, so it cannot change for each group.

#### Existing configurations

Without `ruleSets`, each configuration that is valid at this time gives the same drain:

| Configuration at this time | Drain at this time | With this ADR, without `ruleSets` |
|---|---|---|
| `userNamespaces` (the chart default) | Eviction with `userNamespaces` | `BuiltIn` with the top-level `userNamespaces` |
| `podDrainPolicies`, `userNamespaces: []` | Eviction with `podDrainPolicies` | `BuiltIn` with the top-level `podDrainPolicies` |
| `customDrain.enabled`, no `nodeSelector` | Custom drain on all nodes | `Custom`. The custom drain is not scoped |
| `customDrain.enabled` with a `nodeSelector`, and one list | Custom drain on the nodes that match, eviction on the others | The `custom-drain-node-selector` rule set gives `Custom`. The other nodes use `BuiltIn` with the top-level list. A deprecation warning |

Two changes apply without `ruleSets`:

- The method is recorded, so a label change during a drain, or between two events of a quarantine session, no longer changes the method for an operator who uses `customDrain.nodeSelector`.
- A drain that is in progress at the upgrade has no record. The first pass after the upgrade evaluates it one time. Without a configuration change, the result is the method of the current pass. If the same upgrade adds `ruleSets`, a drain in progress can change its method one time. The documentation recommends that an operator adds `ruleSets` in a separate upgrade.

#### Deprecation of `customDrain.nodeSelector`

`customDrain.nodeSelector` merged in [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) on 2026-09-28 and is in releases since v1.26.0. This ADR deprecates it soon after its release. The costs are:

- **Operators who adopted it must migrate.** The field continues to work, but the documentation, the Helm values, and the examples for #1857 change to rule sets. An operator who reads the current documentation for v1.26.0 learns a form that is deprecated in a later release.
- **Two forms exist during the deprecation period.** node-drainer must support the label selector and the rule sets together, with a defined order between them, and the tests must cover both.
- **A more verbose configuration for the simple case.** `scheduler=slurm` becomes a rule set with a CEL expression that must test for the key first.
- **A visible change of direction.** Contributors to #1871 chose a label selector for this field. This ADR replaces that choice soon after it merged.

To limit these costs:

- node-drainer converts `customDrain.nodeSelector` to a rule set at startup when `customDrain.enabled` is true, with the name `custom-drain-node-selector`, the outcome `drain: { method: Custom }`, and a priority lower than all configured rule sets. When `customDrain.enabled` is false, node-drainer ignores the selector, as it does at this time. The selection, the record, and the metrics are the same for both forms, and the record and the metrics use the rule set name `custom-drain-node-selector`.
- node-drainer logs a deprecation warning at startup when the field is set. The Helm chart prints a deprecation note.
- If `ruleSets` contains a rule set with `method: Custom` and `customDrain.nodeSelector` is also set, startup fails. One configuration must not use both forms for the same selection.
- The field is removed at least two minor releases after the release that adds `ruleSets`. The release notes give the equivalent rule set.

#### The remediation decision

**At this time.** fault-remediation uses the action of the event in each step: the check for an action that has no entry, the equivalence group, the check for an existing CR, the `maxRemediationAttempts` count, the log collector, and the CR. It reads the Node from the API server for the owner reference of the CR, after these steps. It also reads the Node to check if the node is still quarantined, for an action that has no entry and for an event that may already be resolved.

**With rule sets.** fault-remediation reads the Node and evaluates the rule sets first, after the checks for `NONE` and for an event that is already remediated. The selected entry then replaces the action of the event in each step. The CR uses the same Node, so the change does not add an API call for an event that gets a CR. An event that does not get a CR, for example a stopped event, adds one read.

| Result | Action |
|---|---|
| The winner has `action` | fault-remediation uses that entry of `maintenance.actions` instead of the entry for the recommended action, in each step |
| The winner has `stop: true` | The log collector runs, as for other events. No CR. The node state is `remediation-stopped`, unless another event of the session has already moved the node to a later state (see [Node state labels](#node-state-labels)). The event does not count against `maxRemediationAttempts` |
| No rule set matches | The action of the event, as at this time. An action that has no entry is recorded as `remediation-failed`, as at this time |
| A rule set cannot be evaluated | No CR. The node state does not change, as in node-drainer. fault-remediation logs the rule set name and the error, increments the metric, and tries again with backoff, as it does at this time when it cannot create a CR. The event does not count against `maxRemediationAttempts` |

Thus, a rule set can select an entry, or `stop`, for an event whose action has no entry in `maintenance.actions`.

**The record.** fault-remediation records the decision before the log collector runs. Later passes for the same event read the record and do not evaluate the rule sets again. The record has the name of the entry, not only the recommended action. fault-remediation uses that name to find the CR and its `completeConditionType`, and the equivalence group of the entry. Thus, a rule set change does not lose a CR that is in progress. When fault-remediation computes the node label again from the remaining [active failures](#quarantine-session), it also reads the record of each event.

#### Partial drains and actions on the full node

With `partialDrainEnabled`, node-drainer drains only the pods that use the failed GPU for a `COMPONENT_RESET` event. If a rule set changes that event to an action on the full node, as `vm-replace` does, fault-remediation terminates or reboots a node whose other pods were not drained. The node-drainer Helm values already describe this risk for a `COMPONENT_RESET` entry that reboots the node. Rule sets make it easier to hit, because the action can change for each group of nodes.

This ADR does not add a check. The documentation of rule sets describes the risk, next to the examples that change the action. The drain scope in the [record on the event](#the-record-on-the-event) gives fault-remediation the information for a later check.

#### Quarantine session

A quarantine session is the time in which fault-quarantine keeps a node quarantined. A session can have more than one event. A node has at most one session at a time.

- **Start.** fault-quarantine quarantines a node that is not quarantined. The event that causes this gets the status `nodeQuarantined: Quarantined`. Each later event of the session gets `AlreadyQuarantined`.
- **End.** fault-quarantine removes the quarantine. This occurs when the last failure recovers, and the event that recovers it gets `UnQuarantined`. It also occurs when the operator removes the quarantine manually, and fault-quarantine marks each event of the session `Cancelled`.
- **Events of the session.** While the node is quarantined, the events of the current session are the events of the node that the datastore created (`createdAt`) at or after the newest event of the node with `nodeQuarantined: Quarantined`.

A component finds the events of the current session with two datastore queries:

1. The newest event of the node with `nodeQuarantined: Quarantined`. Its `createdAt` is the start of the session.
2. The events of the node with a `createdAt` at or after the start.

fault-quarantine uses the same definition at this time. When an operator removes a quarantine manually, it finds the events of the current session in this way and marks them `Cancelled` (`fault-quarantine/pkg/eventwatcher/event_watcher.go`).

The `quarantineHealthEvent` annotation on the Node is not the list of the events of the session. It lists only the active failures of the session, which are the events that have not recovered. When an event recovers, fault-quarantine removes it from the annotation, but the event stays in the session. fault-remediation computes the node state label from the active failures at this time.

#### Node state labels

Two new values join the node state labels: `drain-stopped` and `remediation-stopped`. A [quarantine session](#quarantine-session) can have more than one event, and each event has its own decision. Thus, the state label can move between a stopped state and the other states:

| From | New expected transitions | Example |
|---|---|---|
| `quarantined` | `drain-stopped` | The first event matches a rule set with `drain: { stop: true }` |
| `drain-stopped` | `draining`, `drain-succeeded` | A later event in the session does not match a `stop` rule set, and node-drainer drains the node, or marks it `AlreadyDrained` |
| `drain-succeeded` | `remediation-stopped` | The event matches a rule set with `remediation: { stop: true }` |
| `remediation-stopped` | `remediating`, `remediation-succeeded`, `remediation-failed` | A later event in the session is remediated |
| `remediating`, `remediation-succeeded`, `remediation-failed` | `remediation-stopped` | fault-remediation computes the label again from the remaining [active failures](#quarantine-session), and all of them are stopped |

fault-remediation includes `remediation-stopped` when it computes the node label from the [active failures](#quarantine-session). The label shows the state of the node. The record on each event shows the decision for that event.

node-drainer sets `drain-stopped` only when the node state is `quarantined` or `drain-stopped`. If another event of the [session](#quarantine-session) has already moved the node to a later state, node-drainer records the stop on the event and does not change the label.

fault-remediation sets `remediation-stopped` only when the node state is `drain-succeeded` or `remediation-stopped`. If another event of the session has already moved the node to a later state, for example `remediating`, fault-remediation records the stop on the event and does not change the label. The label can still become `remediation-stopped` later, when fault-remediation computes it again from the active failures.

A node in `drain-stopped` or `remediation-stopped` is still quarantined, so it counts against the circuit breaker bounds `maxCordonedNodes` and `maxCordonedPercentage`. A gauge counts the nodes in `drain-stopped` and `remediation-stopped`, so that the operator can alert on nodes that stay in these states.

When the [quarantine session](#quarantine-session) ends, the existing removal of the state label removes `drain-stopped` and `remediation-stopped`, as it removes the other values. The removal accepts any state, so it needs no change.

#### The record on the event

```proto
message DrainDecision {
  string ruleSet = 1;    // rule set that applies: the winner, or the rule set of the reusedFrom event. Empty for the default
  string method = 2;     // BuiltIn | Custom; empty when stopped
  bool stopped = 3;
  Entity scope = 4;      // entity type and value of a partial drain; empty for a drain of the full node
  string reusedFrom = 5; // event of the session whose method this event uses; empty when this event selected it
}

message RemediationDecision {
  string ruleSet = 1; // rule set that won; empty when no rule set matched
  string action = 2;  // entry of maintenance.actions; empty when stopped
  bool stopped = 3;
}

message HealthEventStatus {
  // ... fields 1-7 unchanged ...
  DrainDecision drainDecision = 8;
  RemediationDecision remediationDecision = 9;
}
```

fault-quarantine already records its decision in the cordon-reason label and the [`quarantineHealthEvent` annotation](#quarantine-session).

The records are new fields of `HealthEventStatus` (fields 1 to 7 are in use at this time). The datastore keeps them with the event. A reader that does not know the fields ignores them, so the change is compatible with earlier versions.

At this time, fault-quarantine finds out if a drain was partial from a copy of `partialDrainEnabled` ([#1960](https://github.com/NVIDIA/NVSentinel/pull/1960)). The recorded scope makes it possible to remove that copy later. This ADR does not change it.

#### Changes for a fault in progress

A fault can take hours, from the event to the end of remediation. Node labels and configuration can change during this time. Each stage decides one time, when it starts, with the current state:

| Stage | Decides | After the decision |
|---|---|---|
| fault-quarantine | When it evaluates the event | Unchanged by a label change. It applies until the [quarantine session](#quarantine-session) ends |
| node-drainer | Before the first eviction or custom drain CR | Uses the recorded method until the drain ends. The later events of the [quarantine session](#quarantine-session) use the same method |
| fault-remediation | Before the equivalence group, the log collector, and the CR | Uses the recorded entry. The CR continues. A change does not stop it |

Thus, a label change applies to the stages of a fault that have not started. A change does not undo an action that a stage already did.

A rule set change is a configuration change. As at this time, the component reads it when it starts. The records keep the decisions of events that are in progress, so a restart does not change them. The pod policies of a `BuiltIn` drain are not in the record, so they follow the new configuration (see [One drain method for each session](#one-drain-method-for-each-session)).

### Phases

Each phase is compatible with earlier versions. Phases 2 and 3 require phase 1, but not each other. Empty configuration gives the current behaviour, except for the two node-drainer changes that apply without `ruleSets` (see [Existing configurations](#existing-configurations)).

1. The shared rule set schema in `commons/pkg/ruleset`. The CEL rule evaluators move to `commons`. fault-quarantine uses them with no change in behaviour.
2. node-drainer: the drain decision and its record. Then `ruleSets`, the pod policies for each rule set, `drain-stopped`, and the deprecation of `customDrain.nodeSelector`.
3. fault-remediation: `ruleSets`, the record, and `remediation-stopped`.

## Rationale

- **Most of the selection exists.** fault-quarantine and node-drainer can already select by node. A shared profile adds a second way to configure them.
- **Small changes in the right place.** The gap is in fault-remediation. It already reads the Node for each event that gets a CR. The rule sets use that read, so they select with current labels, and they add one read only for an event that does not get a CR.
- **A simpler configuration for the operator.** node-drainer and fault-remediation use the same rule set fields, variables, winner, and error result. An operator learns one format and one set of rules for priority and errors. A rule set moves from one component to the other with a change to the outcome only. Both components can also select by event, which a label selector cannot do.
- **The outcome key shows the stage.** Each rule set states which stage it configures. A rule set that is in the configuration of the wrong component fails validation at startup, with a clear message.
- **Reusable types.** The schema is in `commons`, so other NVSentinel code can read and validate rule sets with the same types.
- **Tested code.** The CEL rule evaluators already run the quarantine and the validation rule sets. The rules use the same CEL as fault-quarantine.
- **Scale.** No new cache for the full cluster. fault-remediation reads the Node for each event, and the CR uses the same read. node-drainer keeps its pruned informer, and prunes it in the same way as fault-quarantine.

## Consequences

### Positive

- Each component can be changed, released, and reviewed by itself.
- Existing configuration continues to work, including `customDrain.nodeSelector` during its deprecation period. Operators add rule sets only where they need them.
- One rule set schema for node-drainer and fault-remediation, and the same CEL rules as fault-quarantine.
- "Off" is a separate result that the operator can see, for drain and for remediation.
- The drain method no longer changes during a drain, and all events of a quarantine session use one method.
- A group of nodes can use different eviction modes, and the top-level `userNamespaces` and `podDrainPolicies` are not deprecated.

### Negative

- Each component has its own group definitions. A group that applies to more than one stage is written once in each component.
- fault-quarantine rule sets differ from the shared schema in seven ways (see [Differences from fault-quarantine](#differences-from-fault-quarantine)). An operator must know both forms.
- `customDrain.nodeSelector` is deprecated soon after its release in v1.26.0. Operators who adopted it must migrate, and node-drainer supports two forms during the deprecation period. See [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector).
- A simple node selection, such as `scheduler=slurm`, is longer as a CEL rule set than as a label selector.
- No single field shows the full behaviour for a node. The operator reads the cordon-reason label and the two records on the event.
- A rule set with a high priority hides the rule sets with a lower priority that match the same events.
- A rule set that fails before the winner stops the decision in node-drainer and fault-remediation, but does not match in fault-quarantine.
- The move of the CEL rule evaluators changes fault-quarantine code, although its behaviour does not change.
- A later event of a quarantine session cannot change the drain method. For example, a more severe event cannot move the drain to a faster method, or to the `Immediate` policies of another rule set. The first event that node-drainer drains selects the method, and it is not always the first event of the session.
- Pod policies can be in the top-level configuration and in each `BuiltIn` rule set. The operator must find the winning rule set to know which policies apply to a node.
- On a cluster that uses the custom drain on all nodes, the first rule set that selects `Custom` changes the nodes that no rule set matches from `Custom` to `BuiltIn`.
- A fault-remediation rule set can change a `COMPONENT_RESET` event after a partial drain to an action on the full node. The pods that were not drained then stop without a drain.
- In node-drainer, a `Node` rule that reads labels by a prefix or by a computed key makes the node cache keep all labels of all nodes. Only a startup warning shows it.
- In node-drainer, a `Node` rule that reads the status reads an empty status. The same rule reads the full status in fault-remediation. Only a startup warning shows it.

### Mitigations

- A metric counts the wins for each drain and remediation rule set. A rule set that another rule set always hides has a count of zero.
- The existing fault-quarantine evaluator tests move with the CEL rule evaluators and run in `commons`.
- Document one example per case, with the configuration of each component that it needs.
- Startup fails when the custom drain is scoped and both top-level lists are empty. Thus, the change from `Custom` to `BuiltIn` for the nodes that no rule set matches needs a top-level list, and it is not silent.
- The documentation describes the risk of an action on the full node after a partial drain. The drain scope in the record on the event makes a later check possible.
- The startup warning for a `Node` rule names the rule set and the expression. The documentation describes the memory cost and shows how to replace a label prefix with a label that has a known key.

## Alternatives Considered

The alternatives are in two groups. [Approach](#approach) has the alternatives to the overall design. [Details](#details) has the alternatives to individual design details.

### Approach

#### Behaviour profiles

One shared profile, selected by ordered routes, that enables or disables each stage. **Rejected** because: fault-quarantine and node-drainer can already select by node. A profile is a shared definition of node groups and their behaviour for all stages. The drain method, the external repair, and the action for each group still need changes in each component.

#### platform-connectors override rules

Use the override rules of [ADR-021](021-health-event-property-overrides.md) to change `recommendedAction` by node label. **Rejected** because: the labels come from a cache that can be 10 minutes old. A rule that fails is skipped, so the event keeps the action that the rule excluded. The override changes the event for all stages and the node condition, not only remediation. It also cannot disable remediation, or select a custom action or an external repair.

#### An ordered list of CEL expressions in fault-remediation

A new `maintenance.rules` list, with one `expression` for each rule, where the first rule that matches wins. **Rejected** because: it is a third CEL rule format, after the fault-quarantine rule sets and the ADR-021 overrides. The position of a rule in the list decides the result, and fault-quarantine uses `priority` for the same purpose.

#### Use the fault-quarantine decision in fault-remediation

Add a remediation field to each quarantine rule set. fault-remediation reads which rule sets matched the event and does not evaluate rules itself. **Rejected** because:

- The decision occurs at quarantine time, which can be hours before remediation. Node labels can change in that time (see [Changes for a fault in progress](#changes-for-a-fault-in-progress)).
- A group that needs a different action must also have its own quarantine rule set.
- Events with `quarantineOverrides.force` skip the rule sets, so they have no record of a match.

#### `managed=false` or `STORE_ONLY` to disable remediation

**Rejected** because: these stop node conditions and all later stages. `managed=false` shows that a different system owns the node ([ADR-040](040-external-remediation-request.md)).

#### Named selectors

Define each node group one time, with a name, and refer to the name from the rule sets of each component. **Deferred** because: it is a shared definition of node groups, which this ADR does not add. The `match` field can accept a reference to a named selector later, without a change to the outcome keys.

#### Keep label selectors in node-drainer

Add `stopDrain.nodeSelector` in the syntax of `customDrain.nodeSelector`, and keep both. **Rejected** because: node-drainer would then use a different syntax from the other two components, and it could not select by event. The label selector is simpler for one label, but a group that applies to drain and to remediation would be written in two syntaxes. This alternative avoids the deprecation of `customDrain.nodeSelector`, which is its main advantage.

### Details

#### Outcome fields next to `match`

Put `stop`, `method`, and `action` next to `match`, as fault-quarantine puts `taint`, `label`, and `cordon`. **Rejected** because: the fields of the common part and the fields of the stage are mixed. A rule set that is copied from one component to the other needs changes in more than one location. A rule set in the configuration of the wrong component is also harder to find, because its fields are not under one key that names the stage.

#### Select the drain method on each pass

The behaviour at this time. **Rejected** because: a label change during a drain can send later passes to a different method, after a custom drain CR already exists.

#### Select the drain method for each event

Each event selects its own method, and the record keeps it for the passes of that event. **Rejected** because: two events of a quarantine session can select different methods, from a `HealthEvent` rule or from a label change between the events. node-drainer drains the node for each event, so the custom drain CR of one event and the eviction by node-drainer for another event then run on the same node at the same time.

#### Only `Node` rules select the method

A rule set with a `HealthEvent` rule can only stop. **Rejected** because: a label change between two events still gives them different methods, and a rule set can no longer select the method by fault type.

#### `Evict` as the name of the built-in method

**Rejected** because: `Evict` and `Custom` look like opposites, but a custom drain can also evict pods. The difference is who drains the node, which `BuiltIn` and `Custom` show.

#### `skip` as the name of the outcome

**Rejected** because: `drainOverrides.skip` on the event also skips the drain, but remediation continues. The outcome stops the pipeline at its stage, and the node stays in quarantine. `stop` shows this difference, and it has the same meaning in the `drain` and the `remediation` outcomes.

#### An eviction mode in the drain outcome

`drain: { method: BuiltIn, mode: Immediate }`, one mode for all pods on the node. **Rejected** because: it removes the selection by namespace and by pod label, and it is not clear how it combines with `podDrainPolicies`. A full list in the rule set uses the existing format and validation.

#### Named eviction profiles

Define named eviction profiles at the top level, and select a profile by name in the `drain` outcome. **Deferred** because: it adds a new top-level field and one more indirection. A list in the rule set gives the same behaviour. Profiles can be added later if the same list is repeated in many rule sets.

#### Node selection in `podDrainPolicies`

Add a node or event match to each pod policy. **Rejected** because: it adds a second form of node selection to node-drainer, with different semantics, and it cannot select the custom drain.

#### Fail at startup when a rule reads more than named labels

Accept a `Node` rule only if it reads the name and labels with a literal key, so that the node-drainer cache can never keep all labels. **Rejected** because: fault-quarantine and kubernetes-object-monitor accept any rule. When they cannot name what a rule reads, fault-quarantine keeps all labels, and kubernetes-object-monitor keeps the whole object. A different behaviour in node-drainer adds a difference between components and limits the rules that an operator can write. The memory cost is documented and shown with a startup warning instead.

## Notes

### Non-goals

- A shared definition of node groups across components, or named selectors.
- Circuit breaker limits for each group. The circuit breaker continues to apply to the full cluster.
- Validation tests and thresholds for each group ([#1922](https://github.com/NVIDIA/NVSentinel/pull/1922)).
- A change to the monitors that run on each node.
- MIG instances in rules.
- New device information in the health event, for example the GPU model or architecture. A different ADR adds it. The rules of this ADR can read it when it is in the event.
- A stop of a maintenance CR when a rule set change disables remediation.
- A check that prevents an action on the full node after a partial drain.
- A change to the rule set format or the behaviour of fault-quarantine.

### Open questions

- **Enum names in rules.** A rule compares `event.recommendedAction` with a number, not with a name such as `COMPONENT_RESET`. Proposed answer: keep the numbers in this ADR, so that existing fault-quarantine rules do not change. A later change to the shared CEL rule evaluators can add the names for all three components.

## References

- [#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904): heterogeneous cluster support (node and device dimensions)
- [#1857](https://github.com/NVIDIA/NVSentinel/issues/1857), [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871): custom drain for a subset of nodes
- [#1960](https://github.com/NVIDIA/NVSentinel/pull/1960): validation after a full drain of a `COMPONENT_RESET` event
- [#1758](https://github.com/NVIDIA/NVSentinel/pull/1758): removal of the Node cache from fault-remediation
- [ADR-003](003-rule-based-node-quarantine.md): rule-based node quarantine (CEL rules)
- [ADR-021](021-health-event-property-overrides.md): health event property overrides
- [ADR-036](036-custom-remediation-actions.md): custom remediation actions
- [ADR-040](040-external-remediation-request.md): external remediation request
- [Node Drainer — Pod label policies](055-pod-drain-policies.md): pod label drain policies
- [ADR-063](063-pod-drain-policies-owner-kinds.md): owner kinds in pod drain policies
