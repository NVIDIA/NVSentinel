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
   - [fault-quarantine](#fault-quarantine)
   - [node-drainer](#node-drainer)
   - [fault-remediation](#fault-remediation)
   - [Device attributes on health events](#device-attributes-on-health-events)
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

Many clusters contain different types of nodes and devices. Examples are bare-metal nodes and virtual machines, Slurm
nodes and Kubernetes nodes, different GPU models, and accelerators that are not GPUs. Operators must set a different
behaviour for each group ([#1903](https://github.com/NVIDIA/NVSentinel/issues/1903),
[#1904](https://github.com/NVIDIA/NVSentinel/issues/1904)). Typical cases are:

1. The operator enables automated remediation for one group of nodes and monitors the results. Then the operator
   enables it for more groups.
2. NVSentinel drains a group of nodes, and an external system repairs them
   ([ADR-040](040-external-remediation-request.md)).
3. NVSentinel resets bare-metal nodes and replaces virtual machines.
4. NVSentinel drains Slurm nodes with a drain plugin and drains all other nodes with the eviction API
   ([#1857](https://github.com/NVIDIA/NVSentinel/issues/1857)).
5. NVSentinel reboots the node after a fault on one device type or GPU model, but not after a fault on a different one.
6. A group of nodes only gets monitoring, or only gets monitoring and a quarantine.

Each component that acts on a fault already reads the node, and two of them can already select by node:

| Stage | Control for each node at this time | Limitation |
|---|---|---|
| platform-connectors | `managed=false` changes events to `STORE_ONLY`. Override rules ([ADR-021](021-health-event-property-overrides.md)) can change `recommendedAction` | `STORE_ONLY` also stops node conditions. Override rules read node labels from a cache that can be 10 minutes old, skip a rule that fails, and change the event for all later stages |
| fault-quarantine | CEL `Node` and `HealthEvent` rules in each rule set ([ADR-003](003-rule-based-node-quarantine.md)) | None. A later event on a quarantined node also applies its taints and cordon, which are released when the session ends ([ADR-062](062-fault-quarantine-release-at-session-end.md)) |
| node-drainer | `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) and `podDrainPolicies` ([pod label drain policies ADR](055-pod-drain-policies.md)) | Cannot skip the drain for a group. Checks the selector again on each pass, so a label change during a drain can change the drain method. Records the method only in a log |
| fault-remediation | None | One map from action to CR applies to the full cluster. An action that is not in the map is recorded as `remediation-failed` |

Thus, case 4 and case 6 work at this time. The gap is mostly in fault-remediation, which has no selection,
and in the event, which does not identify the device model.

[ADR-060](https://github.com/NVIDIA/NVSentinel/pull/1952) proposed one shared behaviour profile that all three
components read. This ADR replaces it. It adds the missing controls to each component, in the configuration and the
selection syntax that each component already uses.

## Decision

1. **Each component keeps its own configuration.** There is no shared profile and no shared definition of node
   groups. fault-quarantine, node-drainer, and fault-remediation use the same rule set format and the same evaluator.
2. **fault-quarantine:** no change. Its rule sets already select by node and by event. A later event on a quarantined
   node applies the taints and the cordon of the rule sets that it matches
   ([ADR-062](062-fault-quarantine-release-at-session-end.md)).
3. **node-drainer:** add `ruleSets`, in the rule set format of fault-quarantine.
   - Each rule set has a `method` in place of the quarantine fields: `Skip`, `Custom`, or `Evict`. A node with
     `Skip` is not drained. It stays in quarantine and is not remediated.
   - The highest `priority` match wins, as in fault-remediation. No match gives `Evict`.
   - node-drainer selects the method one time, when the drain for an event starts. It records the rule set, the method,
     and the scope on the event status.
   - `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) is deprecated. It continues
     to work as a `Custom` rule set with the lowest priority.
4. **fault-remediation:** add `maintenance.ruleSets`, in the rule set format of fault-quarantine
   ([ADR-003](003-rule-based-node-quarantine.md)).
   - Each rule set has a `name`, a `priority`, and `match.any` or `match.all` of CEL `Node` and `HealthEvent` rules.
     In place of the quarantine fields, it has an `action` from `maintenance.actions`, or `disabled: true`.
   - Of the rule sets that match, the one with the highest `priority` wins. If two have the same priority, the first
     in the configuration wins. This is the conflict rule that fault-quarantine uses for labels.
   - The rule set evaluator moves from fault-quarantine to `commons`, and all three components use it.
   - fault-remediation evaluates the rule sets immediately before it creates the CR, with the Node that it already
     reads.
   - It records the rule set and the action on the event status.
   - A disabled remediation is recorded as `remediation-skipped`, not `remediation-failed`.
5. **Health events identify the device model and architecture.** Each impacted entity can carry these as attributes.
   Rules in all three components can then select a GPU model or a device type.

| Case | Configuration |
|---|---|
| 1. Staged rollout | fault-remediation rule set: `disabled` for nodes that are not in the wave |
| 2. External repair | fault-remediation rule set: an action entry for an `ExternalRemediationRequest` |
| 3. Reset bare metal, replace VMs | fault-remediation rule set: `COMPONENT_RESET` on VM nodes selects a `TerminateNode` entry |
| 4. Slurm drain plugin | node-drainer rule set: `method: Custom` for Slurm nodes. `customDrain.nodeSelector` works at this time |
| 5. Device type or model | A rule set in any component, on `event.componentClass` or entity attributes |
| 6. Monitoring only | fault-quarantine rule sets whose `Node` rule excludes the group, at this time |
| 6. Monitoring and quarantine | node-drainer rule set: `method: Skip` |

## Implementation

### fault-quarantine

The configuration does not change. The rule sets already select by node labels and by event fields. All rule sets
that match apply, and `priority` resolves label conflicts. The cordon-reason label lists the rule sets that matched.
A rule set that cannot be evaluated does not match, and a metric counts the error.

[ADR-062](062-fault-quarantine-release-at-session-end.md) fixed the case of a later event on a quarantined node
([#1439](https://github.com/NVIDIA/NVSentinel/issues/1439), [#2021](https://github.com/NVIDIA/NVSentinel/pull/2021)).
fault-quarantine now applies the taints and the cordon of each rule set that a later event matches. It releases them
when the last event of the session recovers, not when each event recovers.

**Monitoring only.** An operator excludes a group from quarantine with a `Node` rule, for example
`!("example.com/pool" in node.metadata.labels && node.metadata.labels["example.com/pool"] in ["legacy-a", "legacy-b"])`. Health events and node conditions continue for
these nodes. No later stage runs, because node-drainer starts only for a quarantined node.

### node-drainer

```yaml
ruleSets:                                   # the highest priority match wins; no match: Evict
  - name: quarantine-only
    priority: 100
    method: Skip
    match:
      all:
        - kind: Node
          expression: >-
            "example.com/pool" in node.metadata.labels &&
            node.metadata.labels["example.com/pool"] == "quarantine-only"
  - name: lpu-no-drain
    priority: 90
    method: Skip
    match:
      all:
        - kind: HealthEvent
          expression: 'event.componentClass == "LPU"'
  - name: slurm
    priority: 10
    method: Custom
    match:
      all:
        - kind: Node
          expression: >-
            "scheduler" in node.metadata.labels && node.metadata.labels["scheduler"] == "slurm"
customDrain:
  enabled: true
  # Deprecated. Use a rule set with method: Custom.
  # nodeSelector: "scheduler=slurm"
  templateFileName: ...                     # the template and CR settings do not change
```

**Format and variables.** The rule sets use the format, the variables, and the evaluator of fault-remediation. A
`Node` rule reads the Node from the node informer of node-drainer, as fault-quarantine reads its informer. A
`HealthEvent` rule reads the event, so a rule set can also select by fault type. A label selector cannot.

**One decision for each event.** Before node-drainer evicts the first pod or creates the custom drain CR for an event,
it selects the drain method:

1. `drainOverrides` on the event apply first, as they do at this time.
2. Of the rule sets that match, the one with the highest `priority` gives the method. If two have the same priority,
   the first in the configuration wins.
3. If `customDrain.nodeSelector` is set and matches, the method is `Custom`. This is the deprecated form, and it acts
   as a rule set with a lower priority than all others.
4. Otherwise, the method is `Evict`.

A rule set with `method: Custom` is valid only when `customDrain.enabled` is true. Startup validation and a Helm `fail`
check this.

node-drainer records the rule set, the method, and the drain scope (`Node`, or the entity for a partial drain) on the
event status before it acts. Later passes for the same event read the record and do not evaluate the rule sets again.
Thus, a label change during a drain does not change the method. A cold start reads the record.

**A rule set that fails.** node-drainer does not drain and does not record a decision. It sets the node state
`drain-failed`, with the rule set name in the message, and a metric counts the error. The next pass evaluates the rule
sets again, so a label or configuration change can correct the failure. node-drainer does not use `Evict` as a
fallback, because the failed rule set can be the one that selects `Skip` or `Custom` for the node.

**Skip.** node-drainer records `userPodsEvictionStatus: Skipped` and the node state `drain-skipped`. `Skipped` is
different from `AlreadyDrained`. fault-remediation starts only for `Succeeded` and `AlreadyDrained`, so it does not
start for this event. The node stays in quarantine until the event recovers or the operator cancels it.

The recorded scope also replaces the configuration that
[#1960](https://github.com/NVIDIA/NVSentinel/pull/1960) copies into fault-quarantine to find out if a drain was
partial.

**Node cache.** The node informer keeps only the label keys that its selectors read
(`node-drainer/pkg/informers/informers.go`). For the rule sets, node-drainer gets the keys from the CEL expressions
with `commons/pkg/celfields`, as fault-quarantine does for its node cache. If the keys of an expression cannot be
found, the informer keeps all labels. This is correct, but it uses more memory.

**`podDrainPolicies` do not change.** They select pods, not nodes, and they apply inside the `Evict` method.

#### Deprecation of `customDrain.nodeSelector`

`customDrain.nodeSelector` merged in [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) on 2026-09-28 and is in
releases since v1.26.0. This ADR deprecates it soon after its release. The costs are:

- **Operators who adopted it must migrate.** The field continues to work, but the documentation, the Helm values,
  and the examples for #1857 change to rule sets. An operator who reads the current documentation for v1.26.0 learns
  a form that is deprecated in a later release.
- **Two forms exist during the deprecation period.** node-drainer must support the label selector and the rule sets
  together, with a defined order between them, and the tests must cover both.
- **A more verbose configuration for the simple case.** `scheduler=slurm` becomes a rule set with a CEL expression
  that must test for the key first.
- **A visible change of direction.** Contributors to #1871 chose a label selector for this field. This ADR replaces
  that choice soon after it merged.

To limit these costs:

- node-drainer converts `customDrain.nodeSelector` to a rule set at startup, with the name `custom-drain-node-selector`
  and a priority lower than all configured rule sets. The selection, the record, and the metrics are the same for
  both forms.
- node-drainer logs a deprecation warning at startup when the field is set. The Helm chart prints a deprecation note.
- If `ruleSets` contains a `Custom` rule set and `customDrain.nodeSelector` is also set, startup fails. One
  configuration must not use both forms for the same selection.
- The field is removed after at least two minor releases. The release notes give the equivalent rule set.

### fault-remediation

```yaml
maintenance:
  actions:
    COMPONENT_RESET: { kind: RebootNode, ... }       # unchanged
    REPLACE_VM: { kind: TerminateNode, ... }         # unchanged
    EXTERNAL_REPAIR: { kind: ExternalRemediationRequest, ... }
  ruleSets:                                          # the highest priority match wins
    - name: lpu-no-remediation
      priority: 100
      disabled: true
      match:
        all:
          - kind: HealthEvent
            expression: 'event.componentClass == "LPU"'
    - name: outside-rollout
      priority: 90
      disabled: true
      match:
        all:
          - kind: Node
            expression: >-
              !("example.com/remediation-wave" in node.metadata.labels &&
                node.metadata.labels["example.com/remediation-wave"] == "1")
    - name: vm-replace
      priority: 10
      action: REPLACE_VM
      match:
        all:
          - kind: Node
            expression: 'node.metadata.labels["example.com/platform"] == "vm"'
          - kind: HealthEvent
            expression: 'event.recommendedAction == 2'   # COMPONENT_RESET
    - name: partner-repair
      priority: 10
      action: EXTERNAL_REPAIR
      match:
        all:
          - kind: Node
            expression: 'node.metadata.labels["example.com/repair"] == "partner"'
```

**Format.** Each rule set uses the `RuleSetMeta` fields of fault-quarantine: `enabled`, `version`, `name`,
`priority`, and `match`. fault-quarantine already uses this format for a second purpose, `validation.ruleSets`. That
format replaces the quarantine fields with `tests`. The fault-remediation format replaces them with `action` or
`disabled`.

**Variables.** The rules use the same variables as fault-quarantine rules:

- A `HealthEvent` rule reads `event`, which has the fields of the health event, including `entitiesImpacted` with
  the entity attributes. Enum fields, such as `recommendedAction`, are numbers, as they are in fault-quarantine.
- A `Node` rule reads `node`, which is the full Node object. Labels are at `node.metadata.labels`.

**The shared evaluator.** The rule set evaluator is in `fault-quarantine/pkg/evaluator` at this time. It imports the
config and informer packages of fault-quarantine. The change moves it to `commons`, with these changes:

- The `RuleSetMeta`, `Match`, and `Rule` types move with it.
- A `Node` rule gets the Node from an interface, not from the fault-quarantine informer. fault-quarantine gives its
  informer lister, and the direct API read that it uses at cold start. fault-remediation gives the Node that it has
  already read for the event.
- At this time, a `Node` rule fails at startup if there is no informer. After the change, a component that cannot
  give a Node rejects `Node` rules at startup, with a clear message.

The CEL environments, the conversion of the event and the Node, and the `any` and `all` semantics do not change. Thus,
a rule that works in fault-quarantine works in fault-remediation.

**Evaluation.** At this time, fault-remediation selects the action and then reads the Node for the owner reference of
the CR. The change reads the Node first, then evaluates the rule sets, then selects the action. This does not add an
API call. fault-remediation has no Node cache ([#1758](https://github.com/NVIDIA/NVSentinel/pull/1758)), so the
labels are current.

fault-remediation evaluates all enabled rule sets. Of the rule sets that match, the one with the highest `priority`
wins. If two have the same priority, the first in the configuration wins.

| Result | Action |
|---|---|
| The winning rule set has `action` | fault-remediation uses that entry of `maintenance.actions` instead of the entry for the recommended action |
| The winning rule set has `disabled: true` | No CR. The node state is `remediation-skipped`. The attempt does not count against `maxRemediationAttempts` |
| No rule set matches | The action of the event, as at this time |
| A rule set cannot be evaluated | No CR. The node state is `remediation-failed`, with the rule set name in the message. A metric counts the error for each rule set |

A rule set that fails stops the decision, even if a different rule set matches. The failed rule set can be the one
that excludes the node, so fault-remediation does not use the other results. This is different from fault-quarantine,
where a rule set that fails does not match. In fault-quarantine, no match means no action. In fault-remediation, no
match means the default action.

A CEL map lookup of a missing label fails. A rule must test for the key first, as `outside-rollout` does.

fault-remediation records the name of the entry that it used, not only the recommended action. It uses that name to
find the CR and its `completeConditionType` later. Thus, a rule set change does not lose a CR that is in progress.

**Validation** occurs at startup and as a Helm `fail`:

- Rule set names are unique DNS-1123 labels.
- Each `expression` compiles and returns a boolean.
- Each rule set has exactly one of `action` and `disabled`.
- Each `action` is a key of `maintenance.actions`.

A new label value, `remediation-skipped`, joins the node state labels. It has the transition
`drain-succeeded → remediation-skipped`.

### Device attributes on health events

`Entity` has only a type and a value. A rule cannot select a GPU model or architecture.

```proto
message Entity {
  string entityType = 1;
  string entityValue = 2;
  // Optional. Well-known keys: "model" (for example "NVIDIA H100 80GB HBM3"),
  // "architecture" (for example "Hopper").
  map<string, string> attributes = 3;
}
```

- metadata-collector already records the NVML device name of each GPU UUID, in both device plugin and DRA modes. It
  adds the architecture, also from NVML.
- The GPU health monitors add `model` and `architecture` to each `GPU_UUID` entity from this
  metadata.
- The Go and Python publishers carry the field. Monitors that do not set it continue to work.
- A rule reads it as `event.entitiesImpacted.exists(e, e.attributes["architecture"] == "Hopper")`.

### The record on the event

```proto
message DrainDecision {
  string ruleSet = 1; // rule set that won; empty when no rule set matched
  string method = 2;  // Evict | Custom | Skip
  string scope = 3;   // Node, or the entity type of a partial drain
}

message RemediationDecision {
  string ruleSet = 1; // rule set that won; empty when no rule set matched
  string action = 2;  // entry of maintenance.actions; empty when disabled
  bool disabled = 3;
}

message HealthEventStatus {
  // ... fields 1-7 unchanged ...
  DrainDecision drainDecision = 8;
  RemediationDecision remediationDecision = 9;
}
```

fault-quarantine already records its decision in the cordon-reason label and the quarantine annotation.

[#1950](https://github.com/NVIDIA/NVSentinel/issues/1950) and ADR-063 ([#2015](https://github.com/NVIDIA/NVSentinel/pull/2015)) propose a `NodeHealth` CR that replaces the datastore. If
it is accepted, these two records move to `NodeHealth.status.remediation`. The fields stay the same.

### Phases

Each phase is independent and compatible with earlier versions. Empty configuration gives the current behaviour.

1. The rule set evaluator moves to `commons`. fault-quarantine uses it with no change in behaviour.
2. node-drainer: the drain decision and its record. Then `ruleSets`, `drain-skipped`, and the deprecation of
   `customDrain.nodeSelector`. This phase requires phase 1.
3. fault-remediation: `maintenance.ruleSets`, the record, and `remediation-skipped`. This phase requires phase 1.
4. Entity attributes and the monitors that set them.

## Changes for a fault in progress

A fault can take hours, from the event to the end of remediation. Node labels and configuration can change during
this time. Each stage decides one time, when it starts, with the current state:

| Stage | Decides | After the decision |
|---|---|---|
| fault-quarantine | When it evaluates the event | Unchanged by a label change. A recovery or a cancel ends it |
| node-drainer | Before the first eviction or custom drain CR | Uses the recorded method until the drain ends |
| fault-remediation | Immediately before it creates the CR | The CR continues. A change does not stop it |

Thus, a label change applies to the stages of a fault that have not started. A change does not undo an action that a
stage already did.

## Rationale

- **Most of the selection exists.** fault-quarantine and node-drainer can already select by node. A shared profile
  adds a second way to configure them.
- **Small changes in the right place.** The gap is in fault-remediation. It already reads the Node immediately before
  it acts, so it can select with current labels at no cost.
- **One rule syntax in all components that act on a fault.** fault-quarantine, node-drainer, and fault-remediation
  use the same rule set format, variables, and evaluator. An operator can copy a rule from one component to another,
  and `priority` selects the winner in the same way. node-drainer and fault-remediation can also select by event,
  which a label selector cannot do.
- **Tested code.** The evaluator already runs the quarantine and the validation rule sets.
- **Scale.** No new cache for the full cluster. fault-remediation uses its existing Node read. node-drainer keeps
  its pruned informer, with the label keys that its rule sets read.

## Consequences

### Positive

- Each component can be changed, released, and reviewed by itself.
- Existing configuration continues to work, including `customDrain.nodeSelector` during its deprecation period.
  Operators add rule sets only where they need them.
- One rule syntax for all node and event selection.
- "Off" is a separate result that the operator can see, for drain and for remediation.
- The drain method no longer changes during a drain.

### Negative

- Each component has its own group definitions. A group that applies to more than one stage is written once in each
  component, in the same syntax.
- `customDrain.nodeSelector` is deprecated soon after its release in v1.26.0. Operators who adopted it must migrate,
  and node-drainer supports two forms during the deprecation period. See
  [Deprecation of `customDrain.nodeSelector`](#deprecation-of-customdrainnodeselector).
- A simple node selection, such as `scheduler=slurm`, is longer as a CEL rule set than as a label selector.
- No single field shows the full behaviour for a node. The operator reads the cordon-reason label and the two
  records on the event.
- A rule set with a high priority hides the rule sets with a lower priority that match the same events.
- The evaluator move changes fault-quarantine code, although its behaviour does not change.
- A rule set that fails has a different result in each component: no quarantine in fault-quarantine, no drain in
  node-drainer, and no remediation in fault-remediation.

### Mitigations

- A metric counts the wins for each drain and remediation rule set. A rule set that another rule set always hides has a count
  of zero.
- The existing fault-quarantine evaluator tests move with the evaluator and run in `commons`.
- Document one example per case, with the configuration of each component that it needs.

## Alternatives Considered

### Behaviour profiles (ADR-060)

One shared profile, selected by ordered routes, that enables or disables each stage
([#1952](https://github.com/NVIDIA/NVSentinel/pull/1952)).
**Rejected** because: fault-quarantine and node-drainer can already select by node. A profile adds a shared package,
a schema, and changes to three components, but the drain method, the external repair, and the action for each group
still need changes in each component.

### platform-connectors override rules

Use the override rules of [ADR-021](021-health-event-property-overrides.md) to change `recommendedAction` by node
label.
**Rejected** because: the labels come from a cache that can be 10 minutes old. A rule that fails is skipped, so the
event keeps the action that the rule excluded. The override changes the event for all stages and the node condition,
not only remediation. It also cannot disable remediation, or select a custom action or an external repair.

### An ordered list of CEL expressions in fault-remediation

A new `maintenance.rules` list, with one `expression` for each rule, where the first rule that matches wins.
**Rejected** because: it is a third CEL rule format, after the fault-quarantine rule sets and the ADR-021 overrides.
The position of a rule in the list decides the result, and fault-quarantine uses `priority` for the same purpose.

### Use the fault-quarantine decision in fault-remediation

Add a remediation field to each quarantine rule set. fault-remediation reads which rule sets matched the event and
does not evaluate rules itself.
**Rejected** because:

- The decision occurs at quarantine time, which can be hours before remediation. That is the same problem as a profile
  that is fixed when the event arrives.
- A group that needs a different action must also have its own quarantine rule set.
- Events with `quarantineOverrides.force` skip the rule sets, so they have no record of a match.

### `managed=false` or `STORE_ONLY` to disable remediation

**Rejected** because: these stop node conditions and all later stages. `managed=false` shows that a different system
owns the node ([ADR-040](040-external-remediation-request.md)).

### Select the drain method on each pass

The behaviour at this time.
**Rejected** because: a label change during a drain can send later passes to a different method, after a custom
drain CR already exists.

### Keep label selectors in node-drainer

Add `skipDrain.nodeSelector` in the syntax of `customDrain.nodeSelector`, and keep both.
**Rejected** because: node-drainer would then use a different syntax from the other two components, and it could not
select by event. The label selector is simpler for one label, but a group that applies to drain and to remediation
would be written in two syntaxes. This alternative avoids the deprecation of `customDrain.nodeSelector`, which is its
main advantage.

## Notes

### Non-goals

- A shared definition of node groups across components.
- Circuit breaker limits for each group. The circuit breaker continues to apply to the full cluster.
- Validation tests and thresholds for each group ([#1922](https://github.com/NVIDIA/NVSentinel/pull/1922)).
- A change to the monitors that run on each node.
- MIG instances in rules.
- A stop of a maintenance CR when a rule set change disables remediation.
- A change to the rule set format or the behaviour of fault-quarantine.

### Open questions

- **Event overrides and `Skip`.** Proposed answer: `drainOverrides.force` on the event has priority over a rule set
  with `method: Skip`, as `quarantineOverrides.force` has priority over the quarantine rule sets.
- **Duration of the deprecation period.** Proposed answer: two minor releases after the release that adds `ruleSets`.
- **A `remediation-skipped` node after the event recovers.** Proposed answer: the label is removed when the
  quarantine ends.
- **Enum names in rules.** A rule compares `event.recommendedAction` with a number, not with a name such as
  `COMPONENT_RESET`. Proposed answer: keep the numbers in this ADR, so that existing fault-quarantine rules do not
  change. A later change to the shared evaluator can add the names for both components.
- **Attribute sources outside GPUs.** NVSwitch and NIC entities do not have a model source at this time.

## References

- [#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904): heterogeneous cluster support (node and device dimensions)
- [#1952](https://github.com/NVIDIA/NVSentinel/pull/1952): ADR-060 behaviour profiles
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
