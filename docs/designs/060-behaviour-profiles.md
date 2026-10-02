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

## Context

NVSentinel applies one quarantine, drain and remediation behaviour to every node and every device in a cluster. Many
clusters are not uniform: bare-metal nodes next to virtual machines, Slurm-managed nodes next to Kubernetes-only nodes,
different GPU models, and accelerators that are not GPUs. Operators need different behaviour for each group
([#1903](https://github.com/NVIDIA/NVSentinel/issues/1903), [#1904](https://github.com/NVIDIA/NVSentinel/issues/1904)).
Typical cases:

- Turn on automated remediation for one group of nodes, watch the results, then widen it.
- Drain a group with NVSentinel and hand the repair to an external system ([ADR-040](040-external-remediation-request.md)).
- Reset bare-metal nodes but replace virtual machines.
- Drain Slurm nodes through a drain plugin and all other nodes through the eviction API
  ([#1857](https://github.com/NVIDIA/NVSentinel/issues/1857)).
- Reboot after a fault on one device type but not after a fault on another.

Today each stage has its own partial, per-node lever, and each reads node labels at a different time:

| Stage | Per-node lever today | Limitation |
|---|---|---|
| platform-connectors | `nvsentinel.dgxc.nvidia.com/managed=false` turns events into `STORE_ONLY` | All-or-nothing. It also stops node conditions and blocks recovery events |
| fault-quarantine | CEL `Node` rules in each rule set | A non-match means nothing downstream runs. There is no fall-through group |
| node-drainer | `customDrain.nodeSelector` ([#1871](https://github.com/NVIDIA/NVSentinel/pull/1871)) | Binary plugin-or-eviction. Re-evaluated against live labels on every retry, so a relabel can switch the drain method mid-drain |
| fault-remediation | None | One action-to-CR map for the whole cluster. An unmapped action is reported as `remediation-failed` |

Adding a selector to each component in turn would give every stage its own selection logic, its own idea of which
group a node is in, and its own moment at which it reads the labels. There would be no single record of which
behaviour applied to a fault.

## Decision

Introduce **behaviour profiles**: named sets of quarantine, drain and remediation settings, selected by an ordered list
of **profile matchers**.

1. A profile contains mostly switches, plus references by name to configuration that stays in each component.
2. A matcher selects a profile with a CEL expression over the health event and the node's labels. Matchers are
   evaluated in order and the first match wins. With no match, the event gets the built-in `default` profile, which is
   today's behaviour.
3. platform-connectors resolves the profile once, when it receives the event, and records a reference to it on the
   health event. Every later stage acts on that reference instead of reading node labels for policy.
4. Profiles are configured through Helm values, as one typed, CRD-shaped document that a shared package parses and
   validates. It is rendered into each consumer's ConfigMap. It is not a served CRD.

## Implementation

### Configuration

```yaml
global:
  behaviourProfiles:
    apiVersion: nvsentinel.nvidia.com/v1alpha1
    kind: BehaviourProfiles
    spec:
      profiles:
        # "default" always exists. Every profile inherits the sections it
        # does not set from "default", and "default" inherits each
        # component's existing configuration.
        default: {}
        slurm:
          drain:
            mode: Custom
            customDrainTarget: slinky      # defined in node-drainer config
        vendor-repair:
          remediation:
            mode: External                 # ADR-040 hand-off
        replace-vm:
          remediation:
            actions:                       # recommended action -> resource
              COMPONENT_RESET: terminate-node
              RESTART_BM: terminate-node
        observe-only:
          quarantine: { enabled: false }
          drain: { mode: Disabled }
          remediation: { mode: Disabled }
      matchers:                            # ordered; the first match wins
        - name: slurm-pools
          expression: 'node.labels["example.com/scheduler"] == "slurm"'
          profile: slurm
        - name: vm-pools
          expression: 'node.labels["example.com/platform"] in ["vm", "vm-spot"]'
          profile: replace-vm
        - name: lpu-faults
          expression: 'event.componentClass == "LPU"'
          profile: observe-only
```

| Profile section | Fields | Refers to |
|---|---|---|
| `quarantine` | `enabled`, optional `ruleSets` allow-list | fault-quarantine rule-set names |
| `drain` | `mode: Evict \| Custom \| Disabled`, `customDrainTarget` | named custom-drain targets in node-drainer. `Evict` uses node-drainer's existing eviction configuration ([ADR-055](055-pod-drain-policies.md)) |
| `remediation` | `mode: Auto \| External \| Disabled`, `actions`, `maxAttempts`, `allowUndrained` | named maintenance resources in fault-remediation |

**Profiles link to component configuration by name.** Rules, templates and eviction settings stay in each component's
existing configuration. node-drainer's single `customDrain` block becomes a map of named targets, and fault-remediation's
`remediationActions` becomes a map of named maintenance resources plus a default action map. A profile only selects
among them. Existing configuration keeps working: an existing `customDrain` block becomes the target named `default`,
and each existing action entry becomes a resource named after its action.

**Matchers are CEL expressions**, the language fault-quarantine rules ([ADR-003](003-rule-based-node-quarantine.md))
and health-event overrides ([ADR-021](021-health-event-property-overrides.md)) already use. Each `expression` must
return a boolean and sees two variables:

- `event`: the same event map as the override rules, extended with `entitiesImpacted`.
- `node`: `node.labels`, the labels of the node the event names.

Node groups, devices and fault types are therefore all one kind of matcher, and they combine with `&&`, for example
`node.labels["example.com/platform"] == "vm" && event.componentClass == "GPU"`. Matching by GPU product, or by an
accelerator that is not a GPU, is an expression over the impacted entities. It needs no new matcher type.

An expression that fails to evaluate does not match. The most common case is reading a label the node does not
have, which then behaves like a label selector that finds no match. Each failure increments a per-matcher error
metric, so a typo in a key shows up instead of silently never matching.

**Validation** runs in the shared parser at startup and as a Helm `fail` at render time:

- Profile and matcher names are DNS-1123 labels. `unresolved` is reserved.
- Each matcher's `expression` compiles and returns a boolean.
- Each matcher references a defined profile, or `default`.
- Each `customDrainTarget`, `ruleSets` entry and `actions` value names something the owning component defines.
- `quarantine.enabled: false` requires `drain: Disabled` and `remediation: Disabled`. A drain without a cordon
  reschedules pods onto the node.
- `drain: Disabled` with `remediation.mode: Auto` is rejected unless `allowUndrained: true`.

### The reference on the event

```proto
// Set only by platform-connectors. Any inbound value is overwritten.
message BehaviourProfileRef {
  string name = 1;     // matched profile, "default" or "unresolved"
  string hash = 2;     // hash of the profile's resolved settings
  string matcher = 3;  // matcher that selected it; empty for default
}

message HealthEvent {
  // ... fields 1-18 unchanged ...
  BehaviourProfileRef behaviourProfile = 19;
}

message HealthEventStatus {
  // ... fields 1-7 unchanged ...
  string drainMethod = 8;          // set once by node-drainer
  string remediationResource = 9;  // set once by fault-remediation
}
```

The hash covers the profile's settings, not the matchers. Editing or reordering matchers never changes it. It lets a
stage detect that its loaded definition differs from the one that was resolved, which happens during a rollout.

### Per component

- **Shared package** (`commons/pkg/behaviourprofile`): schema types, parsing, inheritance from `default`, validation,
  canonical hashing and matcher evaluation. All consumers use it, so they agree on the hash.
- **platform-connectors**: a `BehaviourProfileResolver` transformer, registered after `OverrideTransformer` because
  overrides can change fields an `expression` tests. It runs in both roles. It reuses MetadataAugmentor's cached Node
  lookup, so it adds no API reads. That cache keeps only allow-listed label keys, so the resolver derives the keys its
  expressions read and adds them to the cache. If a key cannot be derived, it keeps every label, as fault-quarantine's
  node cache does (`fault-quarantine/pkg/nodecache`). It always overwrites the reference, because events re-published
  by health-events-analyzer and lifecycle-manager arrive as copies of earlier events. It is the only consumer of
  `matchers`.
- **fault-quarantine**: after the existing branch for nodes that are already quarantined, and before rule evaluation,
  an unhealthy event whose profile has `quarantine.enabled: false` is recorded as `nodeQuarantined: SkippedByProfile`.
  It is marked terminal, the same way intentional skips already are, so a cold start cannot re-decide it. `ruleSets`
  narrows the rule sets that are evaluated. The circuit breaker, the recovery path and rule-set taints and labels are
  unchanged.
- **node-drainer**: on the first attempt, reads the `drain` section, picks the method and writes `drainMethod`. Retries
  re-fetch the event and reuse `drainMethod` instead of re-reading node labels. `drain: Disabled` records
  `userPodsEvictionStatus: Skipped`, which is distinct from `AlreadyDrained`, plus a `drain-skipped` node state label.
- **fault-remediation**: looks up `(profile, action)` to find a resource and records `remediationResource`. CR status is
  then looked up by resource rather than by action name, because one action can map to different CR kinds under
  different profiles. `remediation: Disabled` records a `remediation-skipped` node state label instead of
  `remediation-failed`. CR templates add the label `nvsentinel.nvidia.com/behaviour-profile`, so janitor and external
  systems can see the profile without a schema change.
- **event-exporter**: adds the reference to the CloudEvent payload.

Events stored before the upgrade have no reference and are treated as `default`, which is what they would have
received. When platform-connectors cannot read the node, it records `unresolved`: quarantine follows the rule sets as
today, while drain and remediation are held and reported. A brief API outage must not remediate a node that the
operator excluded from remediation.

### Delivery

`global.behaviourProfiles` is rendered into the platform-connectors, fault-quarantine, node-drainer and
fault-remediation ConfigMaps, following the `ValidationConfiguration` pattern ([ADR-049](049-node-validation.md)). Only
platform-connectors receives `matchers`. All four already roll their pods when their ConfigMap changes, so a profile
change is gated by a normal rollout.

### Phases

1. Shared package, proto field and resolver. Events carry the reference, and event-exporter exports it. No stage acts
   on it yet.
2. node-drainer and fault-remediation act on the reference.
3. fault-quarantine acts on the reference.
4. Device matching on entity attributes, once health events carry per-device identity.

Each phase is backwards compatible: with no profiles configured, every event resolves to `default`.

## Rationale

- **One decision per fault.** Every stage acts on the same profile, and the event records which one and why. This
  closes the gap where node-drainer can change drain method mid-drain.
- **Scale.** Resolution reuses an existing per-node cache in platform-connectors. node-drainer no longer needs selector
  labels in its node informer. No new cluster-wide cache is added, which matters for the 100,000-node target.
- **Small profiles.** Profiles hold switches and names, while component configuration keeps its existing shape and
  validation. Several matchers can share one profile, and moving a node group to another profile touches only its
  matcher.
- **One selection language.** CEL already drives quarantine rules and event overrides, and it is the only option that
  can express node, device and fault conditions in one matcher.

## Consequences

### Positive

- One place to configure, and one record per fault of the behaviour that applied.
- "Off" becomes a distinct, visible outcome instead of looking like success (`AlreadyDrained`) or failure
  (`remediation-failed`).
- The ADR-040 hand-off becomes `remediation: { mode: External }` instead of a templated configuration block.
- Node and device behaviour use one mechanism, so accelerators that are not GPUs need no separate path.

### Negative

- Matcher order is behaviour: a misplaced matcher silently shadows the ones after it.
- A simple node group is wordier in CEL than in label-selector syntax, and a CEL typo can compile and then fail at
  evaluation.
- A relabel takes effect for new faults only after the node metadata cache expires (60 seconds node-local, 10 minutes
  in the deployment role). A fault already in progress keeps the profile it was resolved with.
- Changing matchers rolls the node-local platform-connector DaemonSet on every node.
- A node that matches different matchers for different faults gets different profiles. That is intended, but it can
  surprise operators reading a node's history.

### Mitigations

- Log the resolved profile and matcher on every event, export them, and add a metric for each matcher's match count, so
  a shadowed matcher shows up as zero.
- Document how to change the handling of a fault already in progress: cancel it (a manual uncordon cancels the
  quarantine session); the next fault resolves against current labels.
- Keep matchers in their own rendered file, so editing them does not restart fault-quarantine, node-drainer or
  fault-remediation.

## Alternatives Considered

### A node selector in each component

Extend the [#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) pattern to every stage.
**Rejected** because: each stage would carry its own copy of the group definitions, read labels at a different time,
and keep no shared record of what applied.

### Kubernetes label selectors for node matching

Use label-selector syntax for node groups and keep CEL for device and fault conditions, as
[#1871](https://github.com/NVIDIA/NVSentinel/pull/1871) and [ADR-055](055-pod-drain-policies.md) do for drain scoping.
**Rejected** because: matchers would need two syntaxes, with a rule for combining them, and anything that mixes node and
fault conditions needs CEL anyway. Everything a label selector expresses (`=`, `!=`, `in`, `notin`, exists) has a
direct CEL form.

### Re-resolve from current labels at each stage

Each stage evaluates the matchers itself, from the node's labels at the moment it starts. This follows the live state of
the cluster more closely.
**Rejected for now** because: the stages could disagree about one fault (for example, quarantined under one profile and
drained under another), every stage would need node label reads at 100,000 nodes, and the audit record would need a
per-stage profile. See the open questions.

### Priority numbers instead of list order

**Rejected** because: priority expresses the same thing as order, but the order is no longer visible in one place, and
equal priorities need a tie-break rule.

### Layered profiles

Resolve one profile per layer (node, device, event) and merge them field by field, so node and device concerns combine
without a combined profile.
**Deferred** because: it adds a merge step and a list-shaped reference for a case that combined profiles cover today.
Revisit if combined profiles multiply.

### A served CRD

**Deferred** because: live edits conflict with resolving once per fault unless the event carries a snapshot, and a
CRD needs RBAC, upgrade handling and a defined order across objects. The schema is already CRD-shaped, so promoting it
later changes delivery, not content.

### Model `managed=false` as a profile

**Rejected** because: `managed=false` expresses ownership by another system (ADR-040), not behaviour. It also stops
node conditions, while every profile keeps monitoring and conditions.

## Notes

### Non-goals

- Per-profile circuit breaker limits. The breaker stays cluster-wide.
- Per-profile dry run. The global `dryRun` setting applies to all profiles.
- Per-profile post-remediation validation.
- Changing which monitors run on which nodes. Monitor scheduling already uses label selectors.
- MIG instances as matcher targets.

### Open questions

- **Precedence between a profile and per-event overrides** (`quarantineOverrides`, `drainOverrides`). Proposed: the
  event's overrides win. They are explicit and rare, and `quarantineOverrides.force` already bypasses rule evaluation.
- **Resolve once versus follow live labels.** This ADR resolves once per fault. If following the live state proves
  important during incidents, one middle ground is to re-resolve at the start of each stage while keeping the
  per-stage recording, so that retries within a stage still do not flip.
- **Behaviour when the node cannot be read.** `unresolved` is conservative. An alternative is to retry the lookup with
  a deadline before falling back.

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
