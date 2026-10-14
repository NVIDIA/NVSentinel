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
   - [Shared rule set schema](#shared-rule-set-schema)
   - [fault-quarantine](#fault-quarantine)
   - [node-drainer](#node-drainer)
   - [fault-remediation](#fault-remediation)
   - [Node state labels](#node-state-labels)
   - [The record on the event](#the-record-on-the-event)
   - [Phases](#phases)
4. [Changes for a fault in progress](#changes-for-a-fault-in-progress)
5. [Rationale](#rationale)
6. [Consequences](#consequences)
7. [Alternatives Considered](#alternatives-considered)
8. [Notes](#notes)
   - [Non-goals](#non-goals)
   - [Open questions](#open-questions)
9. [References](#references)

## Context

Many clusters contain different types of nodes and devices. Examples are bare-metal nodes and virtual machines, Slurm nodes and Kubernetes nodes, different GPU models, and accelerators that are not GPUs. Operators must set a different behaviour for each group ([#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904)). Typical cases are:

1. The operator enables automated remediation for one group of nodes and monitors the results. Then the operator enables it for more groups.
2. NVSentinel drains a group of nodes, and an external system repairs them ([ADR-040](040-external-remediation-request.md)).
3. NVSentinel resets bare-metal nodes and replaces virtual machines.
4. NVSentinel drains Slurm nodes with a drain plugin and drains all other nodes with the eviction API ([#1857](https://github.com/NVIDIA/NVSentinel/issues/1857)).
5. NVSentinel reboots the node after a fault on one device type or GPU model, but not after a fault on a different one.
6. A group of nodes only gets monitoring, or only gets monitoring and a quarantine.

Each component that acts on a fault already reads the node, and two of them can already select by node:

| Stage | Control for each node at this time | Limitation |
|---|---|---|
| platform-connectors | `managed=false` changes events to `STORE_ONLY`. Override rules ([ADR-021](021-health-event-property-overrides.md)) can change `recommendedAction` | `STORE_ONLY` also stops node conditions. Override rules read node labels from a cache that can be 10 minutes old, skip a rule that fails, and change the event for all later stages |
| fault-quarantine | CEL `Node` and `HealthEvent` rules in each rule set ([ADR-003](003-rule-based-node-quarantine.md)) | None. A later event on a quarantined node also applies its taints and cordon, which are released when the session ends ([ADR-062](062-fault-quarantine-release-at-session-end.md)) |
| node-drainer | `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) and `podDrainPolicies` ([pod label drain policies ADR](055-pod-drain-policies.md)) | Cannot skip the drain for a group. Checks the selector again on each pass, so a label change during a drain can change the drain method. Records the method only in a log |
| fault-remediation | None | One map from action to CR applies to the full cluster. An action that is not in the map is recorded as `remediation-failed` |

Thus, case 4 and case 6 work at this time. The gap is mostly in fault-remediation, which has no selection.

This ADR adds the missing controls to node-drainer and fault-remediation, with one rule set schema that both components use. The schema starts from the fault-quarantine rule sets.

## Decision

1. **A shared rule set schema for node-drainer and fault-remediation.** A rule set has a common part (`name`, `enabled`, `priority`, and `match`) and one outcome key that is named for the component (`drain` or `remediation`). The common part, the CEL variables, the selection of the winner, and the result of an error are the same in both components. An operator learns one format, and a rule set moves from one component to the other with a change to the outcome only. The types and the evaluator are in `commons`.
2. **Each component keeps its own configuration.** There is no shared profile and no shared definition of node groups.
3. **fault-quarantine:** no change. Its rule sets already select by node and by event. The shared schema keeps their `match` lists, rule kinds, and CEL variables, so the schema can also describe a quarantine outcome (see [fault-quarantine](#fault-quarantine)).
4. **node-drainer:** add `ruleSets` with the `drain` outcome.
   - The outcome is `skip: true` or a `method` (`Custom` or `Evict`). No match gives `Evict`.
   - node-drainer selects the method one time, when the drain for an event starts. It records the rule set, the method, and the scope on the event status.
   - `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) is deprecated. It continues to work as a `Custom` rule set with the lowest priority.
5. **fault-remediation:** add `ruleSets` with the `remediation` outcome.
   - The outcome is `skip: true` or an `action` from `maintenance.actions`. No match gives the action of the event.
   - fault-remediation evaluates the rule sets immediately before it creates the CR, with the Node that it already reads. It records the rule set and the action on the event status.
   - A skipped remediation is recorded as `remediation-skipped`, not `remediation-failed`.
6. **Device selection reads the health event.** A `HealthEvent` rule can read each field of the event. Device information that a later change adds to the event is available to the rules with no change to this ADR. Changes that add device information to the event are not part of this ADR.

| Case | Configuration |
|---|---|
| 1. Staged rollout | fault-remediation rule set: `remediation: { skip: true }` for nodes that are not in the wave |
| 2. External repair | fault-remediation rule set: an action entry for an `ExternalRemediationRequest` |
| 3. Reset bare metal, replace VMs | fault-remediation rule set: `COMPONENT_RESET` on VM nodes selects a `TerminateNode` entry |
| 4. Slurm drain plugin | node-drainer rule set: `drain: { method: Custom }` for Slurm nodes. `customDrain.nodeSelector` works at this time |
| 5. Device type or model | A rule set on `event.componentClass`, `event.entitiesImpacted`, or other device fields of the event, in any of the three components |
| 6. Monitoring only | fault-quarantine rule sets whose `Node` rule excludes the group, at this time |
| 6. Monitoring and quarantine | node-drainer rule set: `drain: { skip: true }` |

## Implementation

### Shared rule set schema

node-drainer and fault-remediation use one rule set schema. Both components read it from the top-level key `ruleSets` of their configuration.

```yaml
ruleSets:
  - name: vm-nodes              # required. Unique DNS-1123 label in the component
    enabled: true               # optional. Default: true
    priority: 10                # optional. Default: 0. The highest priority match wins
    match:                      # required. Exactly one of all or any
      all:                      # every rule must match. With any: at least one rule must match
        - kind: Node
          expression: >-
            "example.com/platform" in node.metadata.labels &&
            node.metadata.labels["example.com/platform"] == "vm"
        - kind: HealthEvent
          expression: 'event.componentClass == "GPU"'
    <outcome>:                  # exactly one outcome key: drain or remediation
      skip: true                # or the field of the component: method or action
```

**The common part.** `name`, `enabled`, `priority`, and `match` have the same meaning in each component:

- `enabled` is true when it is not set. A rule set with `enabled: false` is not evaluated.
- `match` has exactly one of `all` and `any`. With `all`, the rule set matches when every rule matches. With `any`, it matches when at least one rule matches.
- A condition that needs `all` and `any` together is written as CEL in one rule, or as more than one rule set. In one rule, `&&` and `||` combine conditions of the same kind. An `any` across kinds, for example "the node is in the legacy pool, or the event is from an LPU", can be two rule sets with the same outcome.
- Each rule has a `kind` and a CEL `expression` that returns a boolean:
  - `kind: HealthEvent` reads `event`, which has all fields of the health event, including `componentClass` and `entitiesImpacted`. The evaluator converts each field of the event, so a field that is added to the event later is available with no change to the schema. Enum fields, such as `recommendedAction`, are numbers, as they are in fault-quarantine.
  - `kind: Node` reads `node`. A `Node` rule can read only `node.metadata.name` and labels with a literal key, as `node.metadata.labels["example.com/pool"]` or `"example.com/pool" in node.metadata.labels`. See [Node rules read only named labels](#node-rules-read-only-named-labels).
- A CEL map lookup of a missing label fails. A rule must test for the key first.

**The outcome.** Each rule set has one outcome key. The key is the name of the stage that reads it. The outcome has `skip: true`, or the field that selects the behaviour of the stage. A component accepts only its own outcome key, and startup fails for a different key.

| Key | Component | `skip: true` | Selection field | No match |
|---|---|---|---|---|
| `drain` | node-drainer | No drain. Node state `drain-skipped` | `method`: `Custom` or `Evict` | `Evict` |
| `remediation` | fault-remediation | No CR. Node state `remediation-skipped` | `action`: an entry of `maintenance.actions` | The action of the event |

**One winner.** The component evaluates all enabled rule sets. Of the rule sets that match, the one with the highest `priority` wins. If two have the same priority, the first in the configuration wins. Only the outcome of the winner applies.

**An error stops the decision.** If a rule set cannot be evaluated, the component does not use the result of the other rule sets, and it does not use the default. The failed rule set can be the one that selects `skip` for the node. The component records the rule set name and the error, and a metric counts the error for each rule set.

Startup validation cannot find all errors. For example, a rule that reads a label without a test for the key fails on each node that does not have the label. The component tries the decision again with its existing requeue backoff, and the metric shows the error on each attempt.

**Validation.** The Helm chart checks the structure, and `helm install` or `helm upgrade` fails for an error:

- Rule set names are unique DNS-1123 labels.
- `match` has exactly one of `all` and `any`, and that list has at least one rule.
- Each rule set has exactly one outcome key, and the key is the key of the component.
- The outcome has exactly one of `skip: true` and the selection field.

The component checks all of the items above again at startup, and also checks the CEL, which Helm cannot do. Startup fails for an error:

- Each `expression` compiles and returns a boolean.
- Each `Node` rule reads only `node.metadata.name` and labels with a literal key.

The component also logs a warning at startup, but continues, for:

- Two enabled rule sets with the same `priority`. The sequence in the configuration then selects the winner.
- A rule that reads `node.metadata.labels["<key>"]` but does not test `"<key>" in node.metadata.labels`. The rule fails at runtime on each node that does not have the label.

**Metrics.** Each component counts the wins and the errors of each rule set, with the same metric names and labels. A rule set that another rule set always hides has a count of zero. The rule set name is a metric label. The number of rule sets in the configuration limits its cardinality, not the number of nodes or events.

#### Node rules read only named labels

A component must know, at startup, which label keys its `Node` rules read. node-drainer keeps Nodes in an informer, and the informer keeps only the labels that the rules read. On a large cluster, this keeps the cache small. If a rule reads labels that the component cannot name at startup, the component must keep all labels of all nodes. Thus, one change to one rule can increase the memory of the component by a large amount, and nothing tells the operator why.

To prevent this, startup validation finds the fields that each `Node` rule reads, with `commons/pkg/celfields`. The rule is accepted only if each field is one of these:

- `node.metadata.name`.
- `node.metadata.labels["<key>"]`, where `<key>` is a string literal.
- `"<key>" in node.metadata.labels`, where `<key>` is a string literal.

Startup fails for any other read, with the rule set name, the expression, and the field that is not permitted. For example:

| Expression | Result |
|---|---|
| `"scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"` | Accepted. Reads the `scheduler` label |
| `node.metadata.labels.exists(k, k.startsWith("example.com/"))` | Rejected. Reads all labels |
| `size(node.metadata.labels) > 0` | Rejected. Reads all labels |
| `node.metadata.labels[node.metadata.name]` | Rejected. The key is not a literal |
| `node.spec.unschedulable` | Rejected. Reads a field other than the name and labels |
| `"example.com/team" in node.metadata.annotations` | Rejected. Reads an annotation |

To select by a label prefix or by another field, the operator adds a label with a known key to the nodes, and the rule reads that label.

fault-remediation has no Node cache, but it applies the same validation. Thus, a rule set that works in one component works in the other.

**Code.** The schema is in `commons/pkg/ruleset`:

- The common part is a Go type with `toml` and `json` tags. Each component adds its outcome type to it. The Helm chart can make a `values.schema.json` from these types, and other NVSentinel code can use them.
- The CEL rule evaluators for `HealthEvent` and `Node` move from `fault-quarantine/pkg/evaluator` to `commons`. The CEL environments and the conversion of the event and the Node do not change. Thus, a rule that works in fault-quarantine works in the shared schema.
- A `Node` rule gets the Node from an interface, not from the fault-quarantine informer. node-drainer gives its node informer lister. fault-remediation gives the Node that it has already read for the event. fault-quarantine gives its informer lister and the direct API read that it uses at cold start.
- The rule set layer (`enabled`, `match`, the winner, and the errors) is new in `commons`. fault-quarantine keeps its own rule set layer.

### fault-quarantine

The configuration does not change in this ADR. The rule sets already select by node labels and by event fields. All rule sets that match apply, and `priority` resolves label conflicts. The cordon-reason label lists the rule sets that matched.

[ADR-062](062-fault-quarantine-release-at-session-end.md) fixed the case of a later event on a quarantined node ([#1439](https://github.com/NVIDIA/NVSentinel/issues/1439), [#2021](https://github.com/NVIDIA/NVSentinel/pull/2021)). fault-quarantine now applies the taints and the cordon of each rule set that a later event matches. It releases them when the last event of the session recovers, not when each event recovers.

**Monitoring only.** An operator excludes a group from quarantine with a `Node` rule, for example `!("example.com/pool" in node.metadata.labels && node.metadata.labels["example.com/pool"] in ["legacy-a", "legacy-b"])`. Health events and node conditions continue for these nodes. No later stage runs, because node-drainer starts only for a quarantined node.

**Common parts.** The shared schema starts from the fault-quarantine rule sets. These parts are the same: `name`, `priority`, the `all` and `any` lists, the `HealthEvent` and `Node` rule kinds, the CEL environments, and the `event` and `node` variables. A `HealthEvent` rule, and a `Node` rule that reads only named labels, works in both with no change. The quarantine fields (`taint`, `label`, and `cordon`) fit under one outcome key, `quarantine`. Thus, the shared schema can describe fault-quarantine rule sets.

**Differences from the shared schema.** Each difference changes the behaviour of an existing fault-quarantine configuration:

| fault-quarantine at this time | Shared schema |
|---|---|
| Key `rule-sets` in TOML, `ruleSets` in Helm values | Key `ruleSets` in TOML and in Helm values |
| `enabled` is false when it is not set | `enabled` is true when it is not set |
| `taint`, `label`, and `cordon` are next to `match` | The outcome is under one key (`quarantine`) |
| A rule set can have `all` and `any`. Each list is a separate evaluator, and the rule set applies if either one matches: `all` OR `any`. The metrics count each list separately. No shipped configuration uses `any` | A rule set has exactly one of `all` and `any` |
| A rule set that fails does not match | An error stops the decision |
| A `version` field on each rule set | No `version` field |
| A `Node` rule can read any field. If the keys cannot be found, the node cache keeps all labels and annotations | A `Node` rule reads only the name and labels with a literal key. Other reads fail at startup |
| No outcome key | Outcome key `quarantine`, with the taint, label, and cordon fields. The matches combine, as at this time, and do not select one winner |

### node-drainer

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
      skip: true
  - name: lpu-no-drain
    priority: 90
    match:
      all:
        - kind: HealthEvent
          expression: 'event.componentClass == "LPU"'
    drain:
      skip: true
  - name: slurm
    priority: 10
    match:
      all:
        - kind: Node
          expression: >-
            "scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"
    drain:
      method: Custom
customDrain:
  enabled: true
  # Deprecated. Use a rule set with drain.method: Custom.
  # nodeSelector: "scheduler=slurm"
  templateFileName: ...                     # the template and CR settings do not change
```

**Use of the schema.** The outcome key is `drain`. A `Node` rule reads the Node from the node informer of node-drainer. A `HealthEvent` rule reads the event, so a rule set can also select by fault type. A label selector cannot. `method: Custom` is valid only when `customDrain.enabled` is true. The Helm chart and startup validation check this.

**One decision for each event.** Before node-drainer evicts the first pod or creates the custom drain CR for an event, it selects the drain method:

1. `drainOverrides` on the event apply first, as they do at this time.
2. The winning rule set gives `skip` or the method.
3. If no rule set matches and `customDrain.nodeSelector` matches, the method is `Custom`. This is the deprecated form.
4. Otherwise, the method is `Evict`.

node-drainer records the rule set, the method, and the drain scope (`Node`, or the entity for a partial drain) on the event status before it acts. Later passes for the same event read the record and do not evaluate the rule sets again. Thus, a label change during a drain does not change the method. A cold start reads the record.

**An error.** node-drainer does not drain and does not record a decision. It sets the node state `drain-failed`, with the rule set name in the message. The next pass evaluates the rule sets again, so a label or configuration change can correct the failure.

**Skip.** node-drainer records `userPodsEvictionStatus: Skipped` and the node state `drain-skipped`. `Skipped` is different from `AlreadyDrained`. fault-remediation starts only for `Succeeded` and `AlreadyDrained`, so it does not start for this event. The node stays in quarantine until the event recovers or the operator cancels it. For a fault that does not recover by itself, this is until the operator acts. A gauge counts the nodes in `drain-skipped` and `remediation-skipped`, so that the operator can alert on nodes that stay in these states.

The recorded scope also replaces the configuration that [#1960](https://github.com/NVIDIA/NVSentinel/pull/1960) copies into fault-quarantine to find out if a drain was partial.

**Node cache.** The node informer keeps the name, the quarantine annotation, and only the label keys that its selectors read (`node-drainer/pkg/informers/informers.go`). It does not keep the spec, the status, or other annotations. Startup validation gives node-drainer the label keys of each rule set ([Node rules read only named labels](#node-rules-read-only-named-labels)), and the informer keeps those keys. A rule that needs a different field or all labels fails at startup. It does not change the size of the cache.

**`podDrainPolicies` do not change.** They select pods, not nodes, and they apply inside the `Evict` method.

#### Deprecation of `customDrain.nodeSelector`

`customDrain.nodeSelector` merged in [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) on 2026-09-28 and is in releases since v1.26.0. This ADR deprecates it soon after its release. The costs are:

- **Operators who adopted it must migrate.** The field continues to work, but the documentation, the Helm values, and the examples for #1857 change to rule sets. An operator who reads the current documentation for v1.26.0 learns a form that is deprecated in a later release.
- **Two forms exist during the deprecation period.** node-drainer must support the label selector and the rule sets together, with a defined order between them, and the tests must cover both.
- **A more verbose configuration for the simple case.** `scheduler=slurm` becomes a rule set with a CEL expression that must test for the key first.
- **A visible change of direction.** Contributors to #1871 chose a label selector for this field. This ADR replaces that choice soon after it merged.

To limit these costs:

- node-drainer converts `customDrain.nodeSelector` to a rule set at startup, with the name `custom-drain-node-selector`, the outcome `drain: { method: Custom }`, and a priority lower than all configured rule sets. The selection, the record, and the metrics are the same for both forms.
- node-drainer logs a deprecation warning at startup when the field is set. The Helm chart prints a deprecation note.
- If `ruleSets` contains a rule set with `method: Custom` and `customDrain.nodeSelector` is also set, startup fails. One configuration must not use both forms for the same selection.
- The field is removed after at least two minor releases. The release notes give the equivalent rule set.

### fault-remediation

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
      skip: true
  - name: outside-rollout
    priority: 90
    match:
      all:
        - kind: Node
          expression: >-
            !("example.com/remediation-wave" in node.metadata.labels &&
              node.metadata.labels["example.com/remediation-wave"] == "1")
    remediation:
      skip: true
  - name: vm-replace
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

**Evaluation.** At this time, fault-remediation selects the action and then reads the Node for the owner reference of the CR. The change reads the Node first, then evaluates the rule sets, then selects the action. This does not add an API call.

| Result | Action |
|---|---|
| The winner has `action` | fault-remediation uses that entry of `maintenance.actions` instead of the entry for the recommended action |
| The winner has `skip: true` | No CR. The node state is `remediation-skipped`. The attempt does not count against `maxRemediationAttempts` |
| No rule set matches | The action of the event, as at this time |
| A rule set cannot be evaluated | No CR. The node state is `remediation-failed`, with the rule set name in the message |

fault-remediation records the name of the entry that it used, not only the recommended action. It uses that name to find the CR and its `completeConditionType` later. Thus, a rule set change does not lose a CR that is in progress.

### Node state labels

Two new values join the node state labels: `drain-skipped` and `remediation-skipped`. A quarantine session can have more than one event, and each event has its own decision. Thus, the state label can move between a skipped state and the other states:

| From | New expected transitions | Example |
|---|---|---|
| `quarantined` | `drain-skipped` | The first event matches a rule set with `drain: { skip: true }` |
| `drain-skipped` | `draining`, `drain-succeeded` | A later event in the session does not match a skip rule set, and node-drainer drains the node, or marks it `AlreadyDrained` |
| `drain-succeeded` | `remediation-skipped` | The event matches a rule set with `remediation: { skip: true }` |
| `remediation-skipped` | `remediating`, `remediation-succeeded`, `remediation-failed` | A later event in the session is remediated |
| `remediating`, `remediation-succeeded`, `remediation-failed` | `remediation-skipped` | fault-remediation computes the label again from the remaining active failures, and all of them are skipped |

fault-remediation includes `remediation-skipped` when it computes the node label from the active failures. The label shows the state of the node. The record on each event shows the decision for that event.

When the quarantine session ends, the existing removal of the state label removes `drain-skipped` and `remediation-skipped`, as it removes the other values. The removal accepts any state, so it needs no change.

### The record on the event

```proto
message DrainDecision {
  string ruleSet = 1; // rule set that won; empty when no rule set matched
  string method = 2;  // Evict | Custom | Skip
  string scope = 3;   // Node, or the entity type of a partial drain
}

message RemediationDecision {
  string ruleSet = 1; // rule set that won; empty when no rule set matched
  string action = 2;  // entry of maintenance.actions; empty when skipped
  bool skipped = 3;
}

message HealthEventStatus {
  // ... fields 1-7 unchanged ...
  DrainDecision drainDecision = 8;
  RemediationDecision remediationDecision = 9;
}
```

fault-quarantine already records its decision in the cordon-reason label and the quarantine annotation.

The records are new fields of `HealthEventStatus` (fields 1 to 7 are in use at this time). The datastore keeps them with the event. A reader that does not know the fields ignores them, so the change is compatible with earlier versions.

[#1950](https://github.com/NVIDIA/NVSentinel/issues/1950) and ADR-063 ([#2015](https://github.com/NVIDIA/NVSentinel/pull/2015)) propose a `NodeHealth` CR that replaces the datastore. If it is accepted, these two records move to `NodeHealth.status.remediation`. The fields stay the same.

### Phases

Each phase is independent and compatible with earlier versions. Empty configuration gives the current behaviour.

1. The shared rule set schema in `commons/pkg/ruleset`. The CEL rule evaluators move to `commons`. fault-quarantine uses them with no change in behaviour.
2. node-drainer: the drain decision and its record. Then `ruleSets`, `drain-skipped`, and the deprecation of `customDrain.nodeSelector`. This phase requires phase 1.
3. fault-remediation: `ruleSets`, the record, and `remediation-skipped`. This phase requires phase 1.

## Changes for a fault in progress

A fault can take hours, from the event to the end of remediation. Node labels and configuration can change during this time. Each stage decides one time, when it starts, with the current state:

| Stage | Decides | After the decision |
|---|---|---|
| fault-quarantine | When it evaluates the event | Unchanged by a label change. A recovery or a cancel ends it |
| node-drainer | Before the first eviction or custom drain CR | Uses the recorded method until the drain ends |
| fault-remediation | Immediately before it creates the CR | The CR continues. A change does not stop it |

Thus, a label change applies to the stages of a fault that have not started. A change does not undo an action that a stage already did.

A rule set change is a configuration change. As at this time, the component reads it when it starts. The records keep the decisions of events that are in progress, so a restart does not change them.

## Rationale

- **Most of the selection exists.** fault-quarantine and node-drainer can already select by node. A shared profile adds a second way to configure them.
- **Small changes in the right place.** The gap is in fault-remediation. It already reads the Node immediately before it acts, so it can select with current labels at no cost.
- **A simpler configuration for the operator.** node-drainer and fault-remediation use the same rule set fields, variables, winner, and error result. An operator learns one format and one set of rules for priority and errors. A rule set moves from one component to the other with a change to the outcome only. Both components can also select by event, which a label selector cannot do.
- **The outcome key shows the stage.** Each rule set states which stage it configures. A rule set that is in the configuration of the wrong component fails validation at startup, with a clear message.
- **Reusable types.** The schema is in `commons`, so other NVSentinel code can read and validate rule sets with the same types.
- **Tested code.** The CEL rule evaluators already run the quarantine and the validation rule sets. The rules use the same CEL as fault-quarantine.
- **Scale.** No new cache for the full cluster. fault-remediation uses its existing Node read. node-drainer keeps its pruned informer, with the label keys that its rule sets read.

## Consequences

### Positive

- Each component can be changed, released, and reviewed by itself.
- Existing configuration continues to work, including `customDrain.nodeSelector` during its deprecation period. Operators add rule sets only where they need them.
- One rule set schema for node-drainer and fault-remediation, and the same CEL rules as fault-quarantine.
- "Off" is a separate result that the operator can see, for drain and for remediation.
- The drain method no longer changes during a drain.

### Negative

- Each component has its own group definitions. A group that applies to more than one stage is written once in each component.
- fault-quarantine rule sets differ from the shared schema in seven ways (see [Differences from the shared schema](#fault-quarantine)). An operator must know both forms.
- `customDrain.nodeSelector` is deprecated soon after its release in v1.26.0. Operators who adopted it must migrate, and node-drainer supports two forms during the deprecation period. See [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector).
- A simple node selection, such as `scheduler=slurm`, is longer as a CEL rule set than as a label selector.
- A `Node` rule cannot select by a label prefix, by an annotation, or by a field of the spec or the status. The operator must add a label with a known key to the nodes.
- No single field shows the full behaviour for a node. The operator reads the cordon-reason label and the two records on the event.
- A rule set with a high priority hides the rule sets with a lower priority that match the same events.
- The move of the CEL rule evaluators changes fault-quarantine code, although its behaviour does not change.
- A rule set that fails stops the decision in node-drainer and fault-remediation, but does not match in fault-quarantine.

### Mitigations

- A metric counts the wins for each drain and remediation rule set. A rule set that another rule set always hides has a count of zero.
- The existing fault-quarantine evaluator tests move with the CEL rule evaluators and run in `commons`.
- Document one example per case, with the configuration of each component that it needs.
- The startup error for a `Node` rule names the field that is not permitted. The documentation shows how to replace a label prefix or an annotation with a label.

## Alternatives Considered

### Behaviour profiles

One shared profile, selected by ordered routes, that enables or disables each stage. **Rejected** because: fault-quarantine and node-drainer can already select by node. A profile is a shared definition of node groups and their behaviour for all stages. The drain method, the external repair, and the action for each group still need changes in each component.

### platform-connectors override rules

Use the override rules of [ADR-021](021-health-event-property-overrides.md) to change `recommendedAction` by node label. **Rejected** because: the labels come from a cache that can be 10 minutes old. A rule that fails is skipped, so the event keeps the action that the rule excluded. The override changes the event for all stages and the node condition, not only remediation. It also cannot disable remediation, or select a custom action or an external repair.

### An ordered list of CEL expressions in fault-remediation

A new `maintenance.rules` list, with one `expression` for each rule, where the first rule that matches wins. **Rejected** because: it is a third CEL rule format, after the fault-quarantine rule sets and the ADR-021 overrides. The position of a rule in the list decides the result, and fault-quarantine uses `priority` for the same purpose.

### Use the fault-quarantine decision in fault-remediation

Add a remediation field to each quarantine rule set. fault-remediation reads which rule sets matched the event and does not evaluate rules itself. **Rejected** because:

- The decision occurs at quarantine time, which can be hours before remediation. That is the same problem as a profile that is fixed when the event arrives.
- A group that needs a different action must also have its own quarantine rule set.
- Events with `quarantineOverrides.force` skip the rule sets, so they have no record of a match.

### `managed=false` or `STORE_ONLY` to disable remediation

**Rejected** because: these stop node conditions and all later stages. `managed=false` shows that a different system owns the node ([ADR-040](040-external-remediation-request.md)).

### Select the drain method on each pass

The behaviour at this time. **Rejected** because: a label change during a drain can send later passes to a different method, after a custom drain CR already exists.

### Outcome fields next to `match`

Put `skip`, `method`, and `action` next to `match`, as fault-quarantine puts `taint`, `label`, and `cordon`. **Rejected** because: the fields of the common part and the fields of the stage are mixed. A rule set that is copied from one component to the other needs changes in more than one location. A rule set in the configuration of the wrong component is also harder to find, because its fields are not under one key that names the stage.

### Named selectors

Define each node group one time, with a name, and refer to the name from the rule sets of each component. **Deferred** because: it is a shared definition of node groups, which this ADR does not add. The `match` field can accept a reference to a named selector later, without a change to the outcome keys.

### Keep all labels when a rule reads more than named labels

Accept any `Node` rule. If the label keys of a rule cannot be found, the node-drainer informer keeps all labels, as fault-quarantine does at this time. **Rejected** because: one change to one rule can increase the memory of node-drainer from a small cache to all labels of all nodes. Only a log message shows the cause. Also, the node-drainer cache does not keep the spec, the status, or most annotations, so a rule that reads them gets a wrong result and does not fail.

### Keep label selectors in node-drainer

Add `skipDrain.nodeSelector` in the syntax of `customDrain.nodeSelector`, and keep both. **Rejected** because: node-drainer would then use a different syntax from the other two components, and it could not select by event. The label selector is simpler for one label, but a group that applies to drain and to remediation would be written in two syntaxes. This alternative avoids the deprecation of `customDrain.nodeSelector`, which is its main advantage.

## Notes

### Non-goals

- A shared definition of node groups across components, or named selectors.
- Circuit breaker limits for each group. The circuit breaker continues to apply to the full cluster.
- Validation tests and thresholds for each group ([#1922](https://github.com/NVIDIA/NVSentinel/pull/1922)).
- A change to the monitors that run on each node.
- MIG instances in rules.
- New device information in the health event, for example the GPU model or architecture. A different ADR adds it. The rules of this ADR can read it when it is in the event.
- A stop of a maintenance CR when a rule set change disables remediation.
- A change to the rule set format or the behaviour of fault-quarantine.

### Open questions

- **Event overrides and `skip`.** `drainOverrides.skip` on the event marks the drain `AlreadyDrained`, so remediation continues. `drainOverrides.force` evicts all pods immediately; it changes how node-drainer evicts, not whether it drains. Which override has priority over a rule set with `drain: { skip: true }`? Also, the name `skip` has a different effect on remediation in `drainOverrides.skip` (remediation continues) and in the `drain` outcome (no remediation).
- **Duration of the deprecation period.** Proposed answer: two minor releases after the release that adds `ruleSets`.
- **Enum names in rules.** A rule compares `event.recommendedAction` with a number, not with a name such as `COMPONENT_RESET`. Proposed answer: keep the numbers in this ADR, so that existing fault-quarantine rules do not change. A later change to the shared CEL rule evaluators can add the names for all three components.

## References

- [#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904): heterogeneous cluster support (node and device dimensions)
- [#1439](https://github.com/NVIDIA/NVSentinel/issues/1439), [#2021](https://github.com/NVIDIA/NVSentinel/pull/2021): later events on a quarantined node apply taints and cordon (fixed)
- [#1857](https://github.com/NVIDIA/NVSentinel/issues/1857), [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871): custom drain for a subset of nodes
- [#1960](https://github.com/NVIDIA/NVSentinel/pull/1960): validation after a full drain of a `COMPONENT_RESET` event
- [#1950](https://github.com/NVIDIA/NVSentinel/issues/1950), [#2015](https://github.com/NVIDIA/NVSentinel/pull/2015): remediation pipeline without a database (ADR-063)
- [#1758](https://github.com/NVIDIA/NVSentinel/pull/1758): removal of the Node cache from fault-remediation
- [ADR-003](003-rule-based-node-quarantine.md): rule-based node quarantine (CEL rules)
- [ADR-062](062-fault-quarantine-release-at-session-end.md): fault-quarantine release at session end
- [ADR-021](021-health-event-property-overrides.md): health event property overrides
- [ADR-036](036-custom-remediation-actions.md): custom remediation actions
- [ADR-040](040-external-remediation-request.md): external remediation request
- [Node Drainer — Pod label policies](055-pod-drain-policies.md): pod label drain policies
