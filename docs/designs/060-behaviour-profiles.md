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

# ADR-060: Configuration — Behaviour Profiles

Status: Proposed.

## Table of Contents

1. [Context](#context)
2. [Decision](#decision)
3. [Implementation](#implementation)
   - [Configuration](#configuration)
   - [The record on the event](#the-record-on-the-event)
   - [Changes to each component](#changes-to-each-component)
   - [Existing component configuration](#existing-component-configuration)
   - [Delivery](#delivery)
   - [Phases](#phases)
4. [Profile changes for a fault in progress](#profile-changes-for-a-fault-in-progress)
   - [Option A: use the current state at each action](#option-a-use-the-current-state-at-each-action)
   - [Option B: pin the profile when platform-connectors receives the event](#option-b-pin-the-profile-when-platform-connectors-receives-the-event)
5. [Additional configuration for behaviour profiles](#additional-configuration-for-behaviour-profiles)
6. [Rationale](#rationale)
7. [Consequences](#consequences)
8. [Alternatives Considered](#alternatives-considered)
9. [Notes](#notes)
   - [Non-goals](#non-goals)
   - [Open questions](#open-questions)
10. [References](#references)

## Context

Some NVSentinel components can already apply different behaviour to different nodes. fault-quarantine rule sets can
match node labels, and node-drainer can send selected nodes to a drain plugin. But each component uses a different
mechanism, and fault-remediation applies one configuration to the full cluster.

Many clusters contain different types of nodes and devices. Examples are bare-metal nodes and virtual machines, Slurm
nodes and Kubernetes nodes, different GPU models, and accelerators that are not GPUs. Operators must set a different
behaviour for each group ([#1903](https://github.com/NVIDIA/NVSentinel/issues/1903),
[#1904](https://github.com/NVIDIA/NVSentinel/issues/1904)). Typical cases are:

- The operator enables automated remediation for one group of nodes and monitors the results. Then the operator
  enables it for more groups.
- NVSentinel drains a group of nodes, and an external system repairs them
  ([ADR-040](040-external-remediation-request.md)).
- NVSentinel resets bare-metal nodes and replaces virtual machines.
- NVSentinel drains Slurm nodes with a drain plugin and drains all other nodes with the eviction API
  ([#1857](https://github.com/NVIDIA/NVSentinel/issues/1857)).
- NVSentinel reboots the node after a fault on one device type, but not after a fault on a different device type.

At this time, each stage has a different control for each node. Only the fault-quarantine control is complete:

| Stage | Control for each node at this time | Limitation |
|---|---|---|
| platform-connectors | `nvsentinel.dgxc.nvidia.com/managed=false` changes events to `STORE_ONLY` | It stops all actions. It also stops node conditions and recovery events |
| fault-quarantine | CEL `Node` rules in each rule set | Complete for quarantine. A rule set with a catch-all `Node` rule is a fallback. But node-drainer drains each quarantined node, and fault-remediation then acts on it. These rules cannot change the later stages |
| node-drainer | `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) | It selects only the drain plugin or the eviction API. It cannot skip the drain for a node. `drainOverrides.skip` applies to one event, and the publisher sets it |
| fault-remediation | None | One map from action to CR applies to the full cluster. NVSentinel records an action that is not in the map as `remediation-failed` |

If each component gets its own node selector, each component has its own selection logic, its own group definitions,
and its own configuration format. No single record shows which behaviour applied to a fault.

## Decision

Add **behaviour profiles**. A behaviour profile is a named set of switches that enable or disable quarantine, drain,
and remediation. An ordered list of **profile routes** selects the profile.

1. A profile has three sections: `quarantine`, `drain`, and `remediation`. In this ADR, each section has one field,
   `enabled`. The configuration of each component continues to set how an enabled stage operates, for example the
   drain method or the remediation action. Later work can add fields to these sections (see
   [Additional configuration for behaviour profiles](#additional-configuration-for-behaviour-profiles)). The `enabled` field stays the main switch of each section.
   All profiles keep the monitoring. NVSentinel continues to detect faults, store health events, and set node
   conditions for all profiles.
2. A route selects a profile with a CEL expression. The expression can use the health event and the node labels.
   NVSentinel evaluates the routes in sequence, and the first route that matches selects the profile. When no route
   matches, the built-in `default` profile applies.
3. Each component that acts on a fault selects the profile itself, immediately before it acts. These components are
   fault-quarantine, node-drainer, and fault-remediation. Each one evaluates the routes with the stored health event
   and the current node labels. It gets the labels from the node data that it already reads. Thus, a label change or
   a route change applies to faults in progress, at the next action.
4. Each component records the profile and the route that it used on the status of the event.
5. The operator configures profiles with Helm values. The configuration is one typed document with the structure of a
   CRD. A shared package parses and validates it. Helm writes it into the ConfigMap of each component that uses it. It
   is not a served CRD.

This ADR covers three of the cases above:

- A staged rollout of remediation.
- No remediation for one device type.
- Groups of nodes that only get monitoring, or that only get monitoring and a quarantine.

A drain method, an external repair, or a remediation action for each profile is future work. Until then, these
settings apply to the full cluster. `customDrain.nodeSelector` from
[#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) continues to select the drain method.

## Implementation

### Configuration

```yaml
global:
  behaviourProfiles:
    apiVersion: nvsentinel.nvidia.com/v1alpha1
    kind: BehaviourProfiles
    spec:
      profiles:
        # "default" always exists. Its sections are enabled unless set
        # here. Other profiles get the sections they omit from it.
        default:
          remediation: { enabled: false }  # remediation is opt-in here
        remediate:
          remediation: { enabled: true }
        no-remediation:
          remediation: { enabled: false }
        observe-only:
          quarantine: { enabled: false }
          drain: { enabled: false }
          remediation: { enabled: false }
      routes:                              # ordered; the first match wins
        - name: lpu-faults                 # first, so it also wins on wave-1 nodes
          expression: 'event.componentClass == "LPU"'
          profile: no-remediation
        - name: legacy-pools
          expression: 'node.labels["example.com/pool"] in ["legacy-a", "legacy-b"]'
          profile: observe-only
        - name: wave-1
          expression: 'node.labels["example.com/remediation-wave"] == "1"'
          profile: remediate
```

| Section | Result of `enabled: false` | Record |
|---|---|---|
| `quarantine` | NVSentinel does not cordon, taint, or label the node for this fault. No later stage runs | `nodeQuarantined: SkippedByProfile` |
| `drain` | The node stays in quarantine. NVSentinel does not evict more pods | `userPodsEvictionStatus: Skipped` and the node state `drain-skipped` |
| `remediation` | The node stays in quarantine and drained. NVSentinel does not create a maintenance CR | The node state `remediation-skipped` |

When a profile does not set a section, the profile gets that section from `default`. When `default` does not set a
section, that section is enabled. This is the current behaviour. When there are no profiles, all events operate as
they do at this time.

**Routes are CEL expressions.** fault-quarantine rules ([ADR-003](003-rule-based-node-quarantine.md)) and health
event overrides ([ADR-021](021-health-event-property-overrides.md)) also use CEL. Each `expression` must return a
boolean. It can use two variables:

- `event`: the same event fields as the override rules, and also `entitiesImpacted`.
- `node`: the field `node.labels`. These are the current labels of the node in the event.

Thus, one type of route can select node groups, devices, and fault types. A route can combine these conditions with
`&&`, for example `node.labels["example.com/platform"] == "vm" && event.componentClass == "GPU"`. A route can also
select a GPU product, or an accelerator that is not a GPU, with an expression on the impacted entities. This does not
need a new type of route.

When an expression cannot be evaluated, the route does not match. The usual cause is a label that the node does not
have. The result is the same as a label selector that does not match. Each error increments a metric for that route.
Thus, the operator can see a mistake in a label key.

**Validation** occurs in the shared parser at startup, and as a Helm `fail` when Helm writes the configuration:

- Profile names and route names are DNS-1123 labels.
- Each route `expression` compiles and returns a boolean.
- Each route refers to a defined profile, or to `default`.
- Each stage needs the stage before it. `drain` needs `quarantine`, and `remediation` needs `drain`. If NVSentinel
  drains a node without a cordon, the pods start on the node again. If NVSentinel remediates a node that has
  workloads, the workloads stop without a warning. Thus, a profile has one of four structures:
  - All stages.
  - Quarantine and drain.
  - Quarantine only.
  - No stages.

### The record on the event

```proto
message BehaviourProfileRef {
  string name = 1;   // profile that the stage used, or "default"
  string route = 2;  // route that selected it; empty for default
}

message HealthEventStatus {
  // ... fields 1-7 unchanged ...
  // Key: the stage (fault-quarantine, node-drainer, fault-remediation).
  map<string, BehaviourProfileRef> behaviourProfiles = 8;
}
```

Each stage writes its entry when it makes its decision. Different stages can use different profiles for one fault.
The record shows each profile and the stage that used it.

### Changes to each component

- **Shared package** (`commons/pkg/behaviourprofile`): This package contains the schema types. It parses and validates
  the configuration, and applies the sections of `default`. It evaluates the routes, and it finds the label keys that
  the route expressions read. All three components use this package, so they evaluate the routes in the same way.
- **fault-quarantine**: For an unhealthy event, fault-quarantine first does the existing check for a node that is
  already in quarantine. Then it selects the profile with the node labels from its node informer. If the profile
  disables quarantine, it records `nodeQuarantined: SkippedByProfile`. It does not evaluate the rules.
  - An event with `quarantineOverrides.force` does not use this check. fault-quarantine quarantines the node and
    records `nodeQuarantined: Quarantined`, as it does at this time. Thus, the override on the event has priority
    over the profile.
  - It marks this status as final, as it does for other intentional skips. Thus, a cold start does not change the
    decision.
  - node-drainer does not start for this status, so no later stage runs.
  - The node informer keeps only the label keys that the rules read. fault-quarantine adds the keys that the routes
    read (`fault-quarantine/pkg/nodecache`).
  - The circuit breaker, the rule evaluation, the recovery path, and the taints and labels of the rule sets do not
    change.
- **node-drainer**: At each attempt for an event, node-drainer selects the profile with the node labels from its node
  informer. The informer is a local cache, so this read does not add API calls.
  - If the profile disables drain, node-drainer stops. It removes a custom drain CR, as it does for a cancelled
    event. It records `userPodsEvictionStatus: Skipped` and the node state label `drain-skipped`. The status
    `Skipped` is different from `AlreadyDrained`.
  - If the profile enables drain, node-drainer drains the node as it does at this time, with the drain method from its
    own configuration.
  - The node informer keeps only the label keys that node-drainer reads. node-drainer adds the keys that the routes
    read.
- **fault-remediation**: Before it creates a maintenance CR, fault-remediation selects the profile with the node
  labels from its existing Node read. If the profile disables remediation, it records the node state label
  `remediation-skipped` and does not create the CR. Thus, NVSentinel does not record an intentional "off" as
  `remediation-failed`. Remediation needs drain, so an event with a skipped drain does not get to fault-remediation.
  Thus, the trigger filter of fault-remediation does not change.

A profile change does not undo an action that a stage already did. NVSentinel does not return pods that it evicted.
After fault-remediation creates a maintenance CR, a profile change does not stop the CR.

When a component cannot read the node, it tries again, as it does at this time.

### Existing component configuration

Behaviour profiles do not replace the configuration of each component. A profile only enables or disables a stage.
The configuration of each component continues to set how an enabled stage operates. This configuration does not need
names, because a profile in this ADR does not refer to it.

| Component | Configuration that stays | Relation to the profile |
|---|---|---|
| fault-quarantine | `rule-sets` (rules, taint, label, cordon) and `circuitBreaker` | When the profile enables quarantine, the rule sets operate as they do at this time |
| node-drainer | `userNamespaces`, `podDrainPolicies`, `customDrain` (also `customDrain.nodeSelector`), timeouts, and `partialDrainEnabled` | When the profile enables drain, node-drainer drains with these settings |
| fault-remediation | `remediationActions`, the CR templates, `maxRemediationAttempts`, and the log collector | When the profile enables remediation, fault-remediation operates with these settings |

When there are no profiles, the `default` profile applies, and all stages are enabled. Thus, the existing
configuration operates as it does in the current release. An operator can add profiles without a change to the
existing configuration.

The rule sets can already apply different quarantine actions to different nodes. Each rule set has its own taint,
label, and cordon settings, and its `Node` rules can read the node labels. For example, one rule set taints the nodes
that have one label, and a different rule set cordons the nodes that have a different label. A profile can also
select the rule sets that apply. See [Additional configuration for behaviour profiles](#additional-configuration-for-behaviour-profiles).

### Delivery

Helm writes `global.behaviourProfiles` into the ConfigMaps of fault-quarantine, node-drainer, and fault-remediation.
This follows the `ValidationConfiguration` pattern ([ADR-049](049-node-validation.md)). These components restart
their pods when their ConfigMap changes. Thus, a change to a profile or to a route goes through a usual rollout.
platform-connectors does not change.

### Phases

1. Add the shared package, the configuration, and the record on the event status. The three components select the
   profile and record it, but they do not act on it. Operators can examine the record before the profiles change
   the behaviour.
2. fault-quarantine, node-drainer, and fault-remediation use the `enabled` fields.

Each phase is compatible with earlier versions. When there are no profiles, the `default` profile applies to all
events.

## Profile changes for a fault in progress

A fault is in progress from the time platform-connectors receives the event until fault-remediation completes, or
until the operator cancels the quarantine session. This time can be long. For example, a drain that waits for pods to
complete can take hours. During this time, the node labels, the routes, or the settings of a profile can change.

### Option A: use the current state at each action

This is the decision in this ADR. Each stage selects the profile immediately before it acts, with the current labels,
routes, and profile settings.

Advantages:

- A label change or a route change applies to faults in progress at the next action.
- Operators can change the behaviour during an incident without a cancel of the fault.
- This is the usual Kubernetes model, in which a controller acts on the current state.
- The components already read the node for their actions, so this adds little work.

Disadvantages:

- A change can start an action that the profile at the start of the fault disabled.
- A change does not undo an action that a stage already did.

### Option B: pin the profile when platform-connectors receives the event

platform-connectors selects the profile one time and records a reference on the health event. The reference contains
the profile name, a hash of the profile settings, and the route. Later stages use the reference and do not read node
labels to find the profile.

**Rejected** because: a label change or a route change does not apply to a fault in progress. During an incident, the
operator must cancel the fault to change its behaviour. The design also needs rules for the hash during a rollout, and
for copies of events that other components publish again.

## Additional configuration for behaviour profiles

This ADR adds only the `enabled` field. This section shows the fields that later work can add to each section. Each
field is added next to `enabled`, so the profiles in this ADR continue to be valid. Each field refers by name to
configuration that stays in the component. Thus, a profile stays small, and each component keeps the validation of
its own configuration. Each field needs a change in its component. The team selects the fields and their sequence.

A setting can become a profile field only if all of these conditions are true:

- The setting must be different for different node groups, device types, or fault types. Examples are bare metal and
  virtual machines, Slurm nodes and Kubernetes nodes, or the system that repairs the node.
- The setting controls what NVSentinel does to the node. It does not control how the component operates. For
  example, the drain method is a profile field, but a retry count or a diagnostic setting is not.
- The component can apply the setting for each event. A setting that applies to the full process, for example a
  command-line flag, is not a profile field.

Each field keeps its global value in the configuration of the component. A profile only replaces this value. When
the team adds a field, the change must show why the setting is different for different groups.

```yaml
profiles:
  slurm-vm:
    quarantine:
      enabled: true
      ruleSets: ["GPU fatal error ruleset"]   # names of fault-quarantine rule sets
      cordon: true
    drain:
      enabled: true
      method: Custom                          # Evict | Custom
      customDrainTarget: slinky               # named target in node-drainer
      podDrainPolicies: ["batch-jobs"]        # names of node-drainer pod policies
      partialDrain: false
    remediation:
      enabled: true
      mode: Auto                              # Auto | External
      actions:                                # recommended action -> named resource
        COMPONENT_RESET: terminate-node
        RESTART_BM: terminate-node
```

| Field | Result | Refers to | Change in the component |
|---|---|---|---|
| `quarantine.ruleSets` | Only these rule sets apply to the fault | Names of fault-quarantine rule sets | fault-quarantine filters the rule sets before it evaluates them |
| `quarantine.cordon` | Replaces the cordon setting of the rule sets, for example a taint without a cordon | None | fault-quarantine applies the value when it quarantines the node |
| `drain.method` and `drain.customDrainTarget` | Selects the eviction API or a named drain plugin | Named custom drain targets in node-drainer | The single `customDrain` block becomes a map of named targets, and replaces `customDrain.nodeSelector` |
| `drain.podDrainPolicies` | Only these pod policies apply | Names of node-drainer pod policies ([pod label drain policies ADR](055-pod-drain-policies.md)) | node-drainer filters the pod policies |
| `drain.partialDrain` | Replaces `partialDrainEnabled` for this profile. Partial drain needs hardware that isolates each GPU, and this is different for different device types | None | node-drainer reads the value for each event |
| `remediation.mode` | `External` gives the repair to an external system | [ADR-040](040-external-remediation-request.md) | fault-remediation creates an external remediation request for all actions |
| `remediation.actions` | Selects the maintenance resource for each recommended action | Named maintenance resources in fault-remediation | `remediationActions` becomes a map of named resources. fault-remediation finds the CR status by resource, not by action name |
| Device attributes in routes | Routes can select a GPU product or a device type | None | Health events must identify each device |

These fields also need validation rules:

- If `quarantine.cordon` is `false` and drain is enabled, each allowed rule set must apply a `NoSchedule` or
  `NoExecute` taint. Without a cordon or a taint, the pods start on the node again after the drain.
- Each name in `ruleSets`, `customDrainTarget`, `podDrainPolicies`, and `actions` must exist in the configuration of
  the component.
- Because each stage uses the current state, a profile change can change the drain method during a drain. The design
  of `drain.method` must define if node-drainer changes the method or keeps the method that it started with.

The recommended first fields are `drain.method`, `remediation.mode`, and `remediation.actions`. With these fields,
profiles also cover the cases in the [Context](#context) that this ADR does not cover:

- An external repair for one group of nodes.
- A reset for bare metal and a replacement for virtual machines.
- A drain plugin for Slurm nodes.

## Rationale

- **The current state controls each action.** The behaviour of a stage follows the labels and routes at the time that
  the stage acts. Each stage records the profile and the route that it used.
- **A small first step.** Switches are sufficient for the most frequent need. This need is to disable a stage for a
  group of nodes or for a device type. The change is small to review and to implement. Later fields go into the same
  sections, so profiles that operators write now continue to be valid.
- **Scale.** Each component uses the node data that it already reads. NVSentinel does not add a new cache for the full
  cluster. This is important for the target of 100,000 nodes.
- **Profiles that operators can use again.** More than one route can use the same profile. To move a group of nodes to
  a different profile, the operator changes only the route of that group.
- **One selection language.** CEL already controls quarantine rules and event overrides. Of the options, only CEL can
  combine node, device, and fault conditions in one route.

## Consequences

### Positive

- The operator configures the behaviour in one location. The event status records the profile that each stage used.
- An operator can change the behaviour of a fault in progress with a label change.
- "Off" is a separate result that the operator can see. NVSentinel does not record it as a success
  (`AlreadyDrained`) or as a failure (`remediation-failed`).
- Nodes and devices use the same mechanism. Thus, accelerators that are not GPUs do not need a different path.
- platform-connectors and the health event do not change.

### Negative

- The drain method and the remediation action continue to apply to the full cluster. Thus, this ADR does not cover an
  external repair for one group, or a reset for bare metal and a replacement for virtual machines.
- A change can start an action that was disabled when the fault started.
- A profile change does not stop a maintenance CR that fault-remediation already created.
- The sequence of the routes controls the behaviour. A route in the wrong position hides the routes after it, and
  NVSentinel does not show a warning.
- For a simple node group, a CEL expression is longer than a label selector. A CEL expression with a mistake can
  compile and then fail during evaluation.
- The node informers of fault-quarantine and node-drainer keep more label keys, because the routes read them.

### Mitigations

- Log the profile and the route for each decision. Add a metric that counts the matches for each route. A route that
  another route hides has a count of zero.
- Document the point at which a profile change stops each stage, and which actions it cannot undo.
- Keep the route expressions short, and read only the labels that the routes need.

## Alternatives Considered

### A node selector in each component

Use the pattern of [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) in each stage.
**Rejected** because: each component has its own copy of the group definitions and its own configuration format.
No shared record shows the behaviour that applied.

### Pin the profile in platform-connectors

**Rejected.** See [Profile changes for a fault in progress](#profile-changes-for-a-fault-in-progress), option B.

### The full profile schema in one step

Also define drain methods, external repair, and action maps for each profile in this ADR.
**Deferred** because: each of these needs changes in its component, for example named custom drain targets, or a CR
status lookup by resource. Together, they make one large change to review and release. The switches are useful
without these fields, and the sections have space for the fields.
See [Additional configuration for behaviour profiles](#additional-configuration-for-behaviour-profiles).

### Kubernetes label selectors for node groups

Use the label selector syntax for node groups, and use CEL for device and fault conditions.
[#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) and the [pod label drain policies ADR](055-pod-drain-policies.md)
use label selectors for drain scope.
**Rejected** because: routes then need two syntaxes and a rule to combine them. A route that combines node and fault
conditions needs CEL in all cases. CEL can express all label selector operators: `=`, `!=`, `in`, `notin`, and exists.

### A served CRD

**Deferred** because: a CRD needs RBAC, upgrade procedures, and a defined sequence across objects. Helm values already
deliver configuration to these components. The schema already has the structure of a CRD. Thus, a later change to a
CRD changes only the delivery, not the content.

### Use `managed=false` as a profile

**Rejected** because: `managed=false` shows that a different system owns the node (ADR-040). It does not set a
behaviour. It also stops node conditions, but all profiles keep the monitoring and the node conditions.

## Notes

### Non-goals

- Circuit breaker limits for each profile. The circuit breaker continues to apply to the full cluster.
- Dry run for each profile. The global `dryRun` setting applies to all profiles.
- Validation after remediation for each profile.
- A change to the monitors that operate on each node. Monitors already use label selectors for this.
- MIG instances in route expressions.
- A stop of a maintenance CR when a profile change disables remediation.

### Open questions

- **The priority of a profile and the overrides on an event** (`quarantineOverrides`, `drainOverrides`). Proposed
  answer: the overrides on the event have priority. Overrides are explicit and not frequent. This ADR already applies
  this answer to `quarantineOverrides.force`. The team must still decide it for `drainOverrides`.

## References

- [#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904): heterogeneous cluster support (node and device dimensions)
- [#1857](https://github.com/NVIDIA/NVSentinel/issues/1857), [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871): custom drain for a subset of nodes
- [#1670](https://github.com/NVIDIA/NVSentinel/pull/1670): the `managed=false` skip label
- [#1758](https://github.com/NVIDIA/NVSentinel/pull/1758): removal of the cluster-wide node cache from fault-remediation
- [ADR-003](003-rule-based-node-quarantine.md): rule-based node quarantine (CEL rules)
- [ADR-015](015-custom-drain-extensibility.md): custom drain extensibility
- [ADR-021](021-health-event-property-overrides.md): health event property overrides
- [ADR-040](040-external-remediation-request.md): external remediation request
- [ADR-049](049-node-validation.md): node validation (`ValidationConfiguration`)
- [Node Drainer — Pod label policies](055-pod-drain-policies.md): pod label drain policies
