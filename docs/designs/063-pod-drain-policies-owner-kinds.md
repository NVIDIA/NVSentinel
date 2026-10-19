# ADR-063: Node Drainer — Owner kinds in pod drain policies

Status: Proposed

## Context

[ADR-055](055-pod-drain-policies.md) added `podDrainPolicies`, which select a drain mode by pod labels. [Issue #1689](https://github.com/NVIDIA/NVSentinel/issues/1689) also asked for owner-based selection: operators often want every pod of a given kind of workload, such as all StatefulSet pods, to drain the same way.

For built-in workloads, controller-set labels already get close. The StatefulSet controller adds `statefulset.kubernetes.io/pod-name` and the Job controller adds `batch.kubernetes.io/job-name`, so a policy with a key-existence selector such as `podSelector: "statefulset.kubernetes.io/pod-name"` selects those pods today. Owner kinds add two things that such labels do not:

- **Who decides.** A pod's labels come from its pod template, which the workload owner writes. Any workload can therefore carry `batch.kubernetes.io/job-name` and get the drain mode an operator meant for Jobs. For example, a Deployment pod could get `AllowCompletion`, and since it never completes on its own, the drain would wait on it indefinitely. A pod template cannot set owner references; the workload controller sets the controller reference when it creates the pod. Matching on that reference keeps the choice with the operator who writes the policy. This matters most in shared clusters, where operators do not control tenants' pod templates.
- **One rule for every controller.** Workloads managed by operators, such as training jobs or Ray clusters, are often owned directly by a custom resource. Each operator labels its pods with its own keys, or with none, and some have renamed those keys between versions. The controller kind is recorded the same way for all of them, so one policy can name the kind without per-operator label knowledge.

ADR-055 deferred choosing a drain mode automatically from controller kind, because a kind on its own does not show that interruption is acceptable. This ADR keeps that position: owner matching is something an operator configures explicitly, like a label selector.

## Decision

Add an optional `ownerKinds` list to each pod drain policy. A pod matches when the kind of its controller owner reference is in the list. A policy needs `podSelector`, `ownerKinds` or both; with both, the pod must match both. Everything else from ADR-055 is unchanged: policy order and first-match-wins, the namespace glob, mutual exclusion with `userNamespaces`, force and partial drain behavior, and unmatched pods staying out of scope.

```yaml
podDrainPolicies:
  - name: stateful-workloads
    ownerKinds: ["StatefulSet"]
    mode: AllowCompletion
```

## Implementation

- `PodDrainPolicy` gains `OwnerKinds []string` (`ownerKinds` in TOML and Helm values). Startup validation rejects empty or whitespace-padded kinds, and rejects a policy with neither `podSelector` nor `ownerKinds`, so no policy can select every pod by accident.
- Matching uses the pod's controller reference only, the reference `metav1.GetControllerOf` returns. The comparison is the exact, case-sensitive kind, with no API group, as discussed on the issue.
- Only the direct controller is considered. Deployment pods have a `ReplicaSet` controller and CronJob pods a `Job` controller. Following owners further up would need a lookup or another informer for the intermediate objects.
- Pods without a controller, such as bare pods, never match a policy that sets `ownerKinds`.
- The compact pod informer cache already keeps the kind of each owner reference. It now also keeps the controller flag of each reference.
- The Helm chart renders `ownerKinds` when set, and `podSelector` only when set, so an owner-only policy renders valid TOML.

### Memory impact

The cache change adds one `*bool` per owner reference of a cached pod, and pods usually have a single owner reference. No label keys, informers or indexes are added. Compiled policies hold one small set of kinds each.

## Rationale

- Operators can express "all StatefulSet pods" with a rule that workload owners cannot opt into through pod template labels.
- Kind-only matching on the direct controller is simple to explain and needs no extra API reads.
- Reusing the existing policy list keeps one ordered source of drain modes, consistent with ADR-055.

## Consequences

### Positive

- Owner-based and label-based selection can be combined in one policy or ordered across policies.
- Existing configurations are unchanged: a policy without `ownerKinds` behaves exactly as before.

### Negative

- Deployments and CronJobs are matched through their intermediate controllers (`ReplicaSet`, `Job`), which can surprise operators.
- Kind-only matching cannot tell apart custom resources from different API groups that use the same kind name.

### Mitigations

- The node-drainer configuration docs spell out which controller kinds Deployment and CronJob pods have.
- If group-qualified matching is needed later, `ownerKinds` entries can accept a `group/Kind` form without changing existing kind-only entries.

## Alternatives Considered

### Controller-set labels only

**Not chosen as the only mechanism.** Selectors on labels such as `statefulset.kubernetes.io/pod-name` and `batch.kubernetes.io/job-name` work today and remain supported. However, those labels can also be written into any pod template, so they cannot keep a Jobs-only policy away from other workloads. They also do not exist uniformly for custom-resource controllers. The node-drainer configuration docs mention these labels as the label-based alternative.

### A nested Kyverno-style `match` block

**Deferred** because a flat list matches the shape ADR-055 uses for `podSelector` and is enough for the requested cases. A richer matcher can be added later if needed.

### Following the owner chain to the top-level workload

**Rejected for now** because it needs extra reads (or another informer) for ReplicaSets and Jobs on every evaluation, while the direct controller already identifies Deployment and CronJob pods.

### Choosing a mode automatically from the controller kind

**Rejected**, as in ADR-055: nothing changes unless an operator writes an `ownerKinds` policy.

## References

- [ADR-055: Node Drainer — Pod label policies](055-pod-drain-policies.md)
- [Node drainer configuration](../configuration/node-drainer.md#pod-drain-policies)
- [Kubernetes owners and dependents](https://kubernetes.io/docs/concepts/overview/working-with-objects/owners-dependents/)
