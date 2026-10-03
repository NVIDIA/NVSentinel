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
   - [The reference on the event](#the-reference-on-the-event)
   - [Changes to each component](#changes-to-each-component)
   - [Delivery](#delivery)
   - [Phases](#phases)
4. [Profile changes for a fault in progress](#profile-changes-for-a-fault-in-progress)
   - [Option A: pin the profile when platform-connectors receives the event](#option-a-pin-the-profile-when-platform-connectors-receives-the-event)
   - [Option B: allow profile changes for a fault in progress](#option-b-allow-profile-changes-for-a-fault-in-progress)
   - [Comparison](#comparison)
   - [Proposal](#proposal)
5. [Rationale](#rationale)
6. [Consequences](#consequences)
7. [Alternatives Considered](#alternatives-considered)
8. [Notes](#notes)
   - [Future extensions](#future-extensions)
   - [Non-goals](#non-goals)
   - [Open questions](#open-questions)
9. [References](#references)

## Context

NVSentinel applies the same quarantine, drain, and remediation behaviour to all nodes and all devices in a cluster.
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

At this time, each stage has a different, partial control for each node. Each stage reads the node labels at a
different time:

| Stage | Control for each node at this time | Limitation |
|---|---|---|
| platform-connectors | `nvsentinel.dgxc.nvidia.com/managed=false` changes events to `STORE_ONLY` | It stops all actions. It also stops node conditions and recovery events |
| fault-quarantine | CEL `Node` rules in each rule set | When no rule matches, no later stage runs. There is no fallback group |
| node-drainer | `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) | It selects only the drain plugin or the eviction API. It reads the live labels at each retry, so a label change can change the drain method during a drain |
| fault-remediation | None | One map from action to CR applies to the full cluster. NVSentinel records an action that is not in the map as `remediation-failed` |

If each component gets its own node selector, each stage has its own selection logic and its own group of nodes.
Each stage also reads the labels at a different time. No single record shows which behaviour applied to a fault.

## Decision

Add **behaviour profiles**. A behaviour profile is a named set of switches that enable or disable quarantine, drain,
and remediation. An ordered list of **profile routes** selects the profile for each event.

1. A profile has three sections: `quarantine`, `drain`, and `remediation`. In this ADR, each section has one field,
   `enabled`. The configuration of each component continues to set how an enabled stage operates, for example the
   drain method or the remediation action. Later work can add fields to these sections (see
   [Future extensions](#future-extensions)). The `enabled` field stays the main switch of each section.
2. A route selects a profile with a CEL expression. The expression can use the health event and the node labels.
   NVSentinel evaluates the routes in sequence, and the first route that matches selects the profile. When no route
   matches, the event gets the built-in `default` profile.
3. platform-connectors selects the profile one time, when it receives the event. It records a reference to the
   profile on the health event. All later stages use this reference. They do not read the node labels to find the
   profile. [Profile changes for a fault in progress](#profile-changes-for-a-fault-in-progress) compares this
   decision with other options.
4. The operator configures profiles with Helm values. The configuration is one typed document with the structure of a
   CRD. A shared package parses and validates it. Helm writes it into the ConfigMap of each component that uses it. It
   is not a served CRD.

This ADR covers three of the cases above:

- A staged rollout of remediation.
- No remediation for one device type.
- Groups of nodes that only get monitoring, or that only get a quarantine.

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
| `drain` | The node stays in quarantine. NVSentinel does not evict pods | `userPodsEvictionStatus: Skipped` and the node state `drain-skipped` |
| `remediation` | The node stays in quarantine and drained. NVSentinel does not create a maintenance CR | The node state `remediation-skipped` |

When a profile does not set a section, the profile gets that section from `default`. When `default` does not set a
section, that section is enabled. This is the current behaviour. When there are no profiles, all events operate as
they do at this time.

**Routes are CEL expressions.** fault-quarantine rules ([ADR-003](003-rule-based-node-quarantine.md)) and health
event overrides ([ADR-021](021-health-event-property-overrides.md)) also use CEL. Each `expression` must return a
boolean. It can use two variables:

- `event`: the same event fields as the override rules, and also `entitiesImpacted`.
- `node`: the field `node.labels`. These are the labels of the node in the event. They come from the cached Node
  lookup in platform-connectors.

Thus, one type of route can select node groups, devices, and fault types. A route can combine these conditions with
`&&`, for example `node.labels["example.com/platform"] == "vm" && event.componentClass == "GPU"`. A route can also
select a GPU product, or an accelerator that is not a GPU, with an expression on the impacted entities. This does not
need a new type of route.

When an expression cannot be evaluated, the route does not match. The usual cause is a label that the node does not
have. The result is the same as a label selector that does not match. Each error increments a metric for that route.
Thus, the operator can see a mistake in a label key.

**Validation** occurs in the shared parser at startup, and as a Helm `fail` when Helm writes the configuration:

- Profile names and route names are DNS-1123 labels. The name `unresolved` is reserved.
- Each route `expression` compiles and returns a boolean.
- Each route refers to a defined profile, or to `default`.
- Each stage needs the stage before it. `drain` needs `quarantine`, and `remediation` needs `drain`. If NVSentinel
  drains a node without a cordon, the pods start on the node again. If NVSentinel remediates a node that has
  workloads, the workloads stop without a warning. Thus, a profile has one of four structures:
  - All stages.
  - Quarantine and drain.
  - Quarantine only.
  - No stages.

### The reference on the event

```proto
// Set only by platform-connectors. Any inbound value is overwritten.
message BehaviourProfileRef {
  string name = 1;     // matched profile, "default" or "unresolved"
  string hash = 2;     // hash of the profile's resolved settings
  string route = 3;    // route that selected it; empty for default
}

message HealthEvent {
  // ... fields 1-18 unchanged ...
  BehaviourProfileRef behaviourProfile = 19;
}
```

The hash includes the settings of the profile. It does not include the routes. Thus, a change to the routes, or to
their sequence, does not change the hash. With the hash, a stage can find a difference between its own definition and
the definition that platform-connectors used. This difference occurs during a rollout. In this case, the stage uses
its own definition and increments a metric.

### Changes to each component

- **Shared package** (`commons/pkg/behaviourprofile`): This package contains the schema types. It parses and validates
  the configuration, and applies the sections of `default`. It also calculates the hash and evaluates the routes. All
  components use this package, so they calculate the same hash.
- **platform-connectors**: A new `BehaviourProfileResolver` transformer comes after `OverrideTransformer` in the
  pipeline, because overrides can change fields that an expression uses. It operates in the node-local role and in
  the deployment role.
  - It uses the cached Node lookup of MetadataAugmentor, so it does not add API reads.
  - The cache keeps only the label keys in an allow list. Thus, the resolver finds the label keys that the expressions
    read, and adds them to the cache. If it cannot find a key, it keeps all labels. The node cache of fault-quarantine
    uses the same rule (`fault-quarantine/pkg/nodecache`).
  - It always replaces the reference. health-events-analyzer and lifecycle-manager publish copies of earlier events
    again, and these copies can contain an old reference.
  - It is the only component that gets the `routes`.
- **fault-quarantine**: For an unhealthy event, fault-quarantine first does the existing check for a node that is
  already in quarantine. Then, if the profile disables quarantine, it records `nodeQuarantined: SkippedByProfile`. It
  does not evaluate the rules.
  - It marks this status as final, as it does for other intentional skips. Thus, a cold start does not change the
    decision.
  - node-drainer does not start for this status, so no later stage runs.
  - The circuit breaker, the rule evaluation, the recovery path, and the taints and labels of the rule sets do not
    change.
- **node-drainer**: At the first attempt for an event, node-drainer reads the profile. If the profile disables drain,
  node-drainer records `userPodsEvictionStatus: Skipped` and the node state label `drain-skipped`, and stops. The
  status `Skipped` is different from `AlreadyDrained`. If the profile enables drain, node-drainer drains the node as
  it does at this time, with the drain method from its own configuration.
- **fault-remediation**: If the profile disables remediation, fault-remediation records the node state label
  `remediation-skipped`. It does not create a maintenance CR. Thus, NVSentinel does not record an intentional "off" as
  `remediation-failed`. Remediation needs drain, so an event with a skipped drain does not get to fault-remediation.
  Thus, the trigger filter of fault-remediation does not change.
- **event-exporter**: event-exporter adds the reference to the CloudEvent payload.

Events that NVSentinel stored before the upgrade do not have a reference. The stages use `default` for these events.
This is the behaviour that these events got before the upgrade.

When platform-connectors cannot read the node, it records `unresolved`. The behaviour of `unresolved` is the same as
quarantine only. The quarantine uses the rule sets, as at this time. NVSentinel skips the drain and the remediation,
and records that it skipped them. Thus, a short API failure does not cause the remediation of a node that the
operator excluded from remediation.

### Delivery

Helm writes `global.behaviourProfiles` into the ConfigMaps of platform-connectors, fault-quarantine, node-drainer,
and fault-remediation. This follows the `ValidationConfiguration` pattern ([ADR-049](049-node-validation.md)). Only
platform-connectors gets the `routes`. All four components restart their pods when their ConfigMap changes. Thus, a
change to a profile goes through a usual rollout.

### Phases

1. Add the shared package, the proto field, and the resolver. Events contain the reference, and event-exporter
   exports it. The stages do not use it.
2. fault-quarantine, node-drainer, and fault-remediation use the `enabled` fields.

Each phase is compatible with earlier versions. When there are no profiles, all events get `default`.

## Profile changes for a fault in progress

A fault is in progress from the time platform-connectors receives the event until fault-remediation completes, or
until the operator cancels the quarantine session. This time can be long. For example, a drain that waits for pods to
complete can take hours. During this time, the node labels, the routes, or the settings of a profile can change.

The decision in this ADR pins the profile name, not the profile settings. Each stage reads the settings of the pinned
profile when the stage starts. Thus, a change to the settings of a profile applies to the next stage of a fault in
progress. A change to the labels or to the routes applies only to new faults.

A pinned profile is predictable, but it can be too strict. During an incident, operators can change labels to stop an
action. With a pinned profile, this change does not stop the action for faults in progress. This section compares the
two options.

### Option A: pin the profile when platform-connectors receives the event

This is the decision in this ADR.

Advantages:

- All stages use the same profile for a fault.
- Stages after platform-connectors do not read node labels to find the profile.
- The record shows one profile for each fault.
- A retry cannot change the behaviour of a stage.

Disadvantages:

- A label change or a route change does not apply to a fault in progress, also when the drain takes hours.
- To change the behaviour for a fault in progress, the operator must cancel the fault with a manual uncordon. This
  also stops the drain.

### Option B: allow profile changes for a fault in progress

A stage can use the profile that the current labels and routes select, not only the profile that platform-connectors
recorded.

Advantages:

- A label change or a route change applies to faults in progress, not only to new faults.
- Operators can change the behaviour during an incident without a cancel of the fault.
- This is similar to the usual Kubernetes model, in which a controller acts on the current state.

Disadvantages:

- Stages can use different profiles for one fault. For example, a node gets a quarantine under one profile and a drain
  under a different profile.
- A change can start an action that the profile at the start of the fault disabled.
- The stages after platform-connectors must read node labels. This adds work at a scale of 100,000 nodes.
- The record must show each profile that applied to the fault, and the stage that used it.
- The behaviour is more difficult to predict and to audit.

If the team selects option B, the design must also define when a stage selects the profile again. It must also define
which changes a stage applies to a fault in progress.

### Comparison

| Topic | Option A: pin | Option B: allow changes |
|---|---|---|
| All stages use the same profile | Yes | No |
| A label change applies to a fault in progress | No | Yes |
| Stages read labels after platform-connectors | No | Yes |
| Records for each fault | One profile | One or more profiles |

### Proposal

Phase 1 is the same for both options. platform-connectors records the profile on each event, and no stage uses it.
Thus, the team can make this decision before phase 2.

This ADR proposes option A, because it is simpler to predict, to audit, and to operate at scale. If reviewers require
that label changes apply to faults in progress, the team can select option B before phase 2.

## Rationale

- **One decision for each fault.** All stages use the same profile. The event records the profile and the route that
  selected it.
- **A small first step.** Switches are sufficient for the most frequent need. This need is to disable a stage for a
  group of nodes or for a device type. The change is small to review and to implement. Later fields go into the same sections, so
  profiles that operators write now continue to be valid.
- **Scale.** The resolver uses an existing cache for each node in platform-connectors. NVSentinel does not add a new
  cache for the full cluster. This is important for the target of 100,000 nodes.
- **Profiles that operators can use again.** More than one route can use the same profile. To move a group of nodes to
  a different profile, the operator changes only the route of that group.
- **One selection language.** CEL already controls quarantine rules and event overrides. Of the options, only CEL can
  combine node, device, and fault conditions in one route.

## Consequences

### Positive

- The operator configures the behaviour in one location. Each fault has one record of the behaviour that applied.
- "Off" is a separate result that the operator can see. NVSentinel does not record it as a success
  (`AlreadyDrained`) or as a failure (`remediation-failed`).
- Nodes and devices use the same mechanism. Thus, accelerators that are not GPUs do not need a different path.

### Negative

- The drain method and the remediation action continue to apply to the full cluster. Thus, this ADR does not cover an
  external repair for one group, or a reset for bare metal and a replacement for virtual machines.
- The sequence of the routes controls the behaviour. A route in the wrong position hides the routes after it, and
  NVSentinel does not show a warning.
- For a simple node group, a CEL expression is longer than a label selector. A CEL expression with a mistake can
  compile and then fail during evaluation.
- A label change applies to new faults only after the node metadata cache expires. The cache time is 60 seconds in the
  node-local role and 10 minutes in the deployment role. A fault that NVSentinel processes at that time keeps its
  profile.
- A change to the routes restarts the node-local platform-connector DaemonSet on all nodes.
- A node can match different routes for different faults, and thus get different profiles. This is intentional, but
  it can be unexpected for operators who read the history of a node.

### Mitigations

- Log the profile and the route for each event, and export them. Add a metric that counts the matches for each route.
  A route that another route hides has a count of zero.
- Document how to change the processing of a fault that is in progress. The operator cancels it: a manual uncordon
  cancels the quarantine session. The next fault uses the current labels.
- Keep the routes in a separate file. Then a change to the routes does not restart fault-quarantine, node-drainer, or
  fault-remediation.

## Alternatives Considered

### A node selector in each component

Use the pattern of [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) in each stage.
**Rejected** because: each stage has its own copy of the group definitions, and each stage reads the labels at a
different time. No shared record shows the behaviour that applied.

### The full profile schema in one step

Also define drain methods, external repair, and action maps for each profile in this ADR.
**Deferred** because: each of these needs changes in its component, for example named custom drain targets, or a CR
status lookup by resource. Together, they make one large change to review and release. The switches are useful
without these fields, and the sections have space for the fields.

### Kubernetes label selectors for node groups

Use the label selector syntax for node groups, and use CEL for device and fault conditions.
[#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) and [ADR-055](055-pod-drain-policies.md) use label selectors
for drain scope.
**Rejected** because: routes then need two syntaxes and a rule to combine them. A route that combines node and fault
conditions needs CEL in all cases. CEL can express all label selector operators: `=`, `!=`, `in`, `notin`, and exists.

### Allow profile changes for a fault in progress

**Under discussion.** See [Profile changes for a fault in progress](#profile-changes-for-a-fault-in-progress),
option B.

### A served CRD

**Deferred** because: changes to a live CRD conflict with one profile for each fault, unless the event contains a
copy of the profile. A CRD also needs RBAC, upgrade procedures, and a defined sequence across objects. The schema
already has the structure of a CRD. Thus, a later change to a CRD changes only the delivery, not the content.

### Use `managed=false` as a profile

**Rejected** because: `managed=false` shows that a different system owns the node (ADR-040). It does not set a
behaviour. It also stops node conditions, but all profiles keep the monitoring and the node conditions.

## Notes

### Future extensions

Each extension adds fields next to `enabled` in an existing section. Thus, profiles for this ADR continue to be valid.

- **A drain method for each profile** (`drain.customDrainTarget`): The single `customDrain` block of node-drainer
  becomes a map of named targets, and replaces `customDrain.nodeSelector`. NVSentinel records the selected method on
  the event status. Thus, a retry cannot change the method during a drain. At this time, a label change can cause this.
- **An external repair for each profile** (`remediation.mode: External`): This uses
  [ADR-040](040-external-remediation-request.md).
- **A remediation action for each profile** (`remediation.actions`): This uses a map of named maintenance resources.
  fault-remediation must then find the CR status by resource, not by action name.
- **A list of allowed rule sets** (`quarantine.ruleSets`) and **a maximum number of attempts**
  (`remediation.maxAttempts`) for each profile.
- **Device attributes**, for example the GPU product, in route expressions. This needs health events that identify
  each device.

### Non-goals

- Circuit breaker limits for each profile. The circuit breaker continues to apply to the full cluster.
- Dry run for each profile. The global `dryRun` setting applies to all profiles.
- Validation after remediation for each profile.
- A change to the monitors that operate on each node. Monitors already use label selectors for this.
- MIG instances in route expressions.

### Open questions

- **The priority of a profile and the overrides on an event** (`quarantineOverrides`, `drainOverrides`). Proposed
  answer: the overrides on the event have priority. Overrides are explicit and not frequent.
  `quarantineOverrides.force` already bypasses the rule evaluation.
- **Profile changes for a fault in progress.** The team must select an option before phase 2. See
  [Profile changes for a fault in progress](#profile-changes-for-a-fault-in-progress).
- **The behaviour when platform-connectors cannot read the node.** The `unresolved` profile is a safe choice. An
  alternative is to try the lookup again for a limited time, and then use `unresolved`.

## References

- [#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904): heterogeneous cluster support (node and device dimensions)
- [#1857](https://github.com/NVIDIA/NVSentinel/issues/1857), [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871): custom drain for a subset of nodes
- [#1670](https://github.com/NVIDIA/NVSentinel/pull/1670): the `managed=false` skip label
- [#1758](https://github.com/NVIDIA/NVSentinel/pull/1758): removal of the cluster-wide node cache from fault-remediation
- [ADR-003](003-rule-based-node-quarantine.md): rule-based node quarantine (CEL rules)
- [ADR-015](015-custom-drain-extensibility.md): custom drain extensibility
- [ADR-021](021-health-event-property-overrides.md): health event property overrides
- [ADR-023](023-health-event-transformer-pipeline.md): health event transformer pipeline
- [ADR-040](040-external-remediation-request.md): external remediation request
- [ADR-049](049-node-validation.md): node validation (`ValidationConfiguration`)
- [ADR-055](055-pod-drain-policies.md): pod label drain policies
