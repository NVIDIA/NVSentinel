# ADR-055: Node Drainer — Pod label policies

## Context

[Issue #1689](https://github.com/NVIDIA/NVSentinel/issues/1689) describes workloads with different interruption requirements sharing a namespace. Namespace-level drain configuration cannot express those differences. The existing drainer also passes namespace groups through immediate eviction, deadline deletion and completion checks, so filtering only during initial evaluation would still allow an action to affect unrelated pods.

## Decision

Add an optional ordered `podDrainPolicies` list. Each policy matches a standard Kubernetes pod label selector and an optional namespace-name glob, then selects an existing drain mode. The first matching policy wins. `podDrainPolicies` and `userNamespaces` are mutually exclusive; configurations containing both are rejected. Unmatched pods are outside the drain scope and do not block completion. Existing deployments continue to use namespace rules when no pod policies are configured.

## Implementation

- Compile and validate selectors at startup. Reject missing or duplicate policy names, empty or invalid selectors, invalid namespace patterns and unsupported modes.
- Keep namespace-based draining and pod-policy draining as separate paths. Helm users enabling policies must set `userNamespaces: []` to clear the default namespace rule; both Helm rendering and startup validation reject conflicting configuration.
- Carry a mode-specific predicate through evaluation and execution. Every pod read used for eviction, force deletion or completion applies that predicate in addition to existing eligibility and partial-drain filters.
- Retain only referenced label keys in the compact pod informer cache. Relevant label changes are observed through normal informer updates; configuration changes require a restart.
- Use pod UID and resource-version deletion preconditions so replacement or relabelling after selection causes a retry against a fresh observation.
- Keep the existing event-based timeout, cold-start recovery, cancellation and dry-run behavior. Force overrides change the selected mode without expanding scope. Custom drain configuration and pod policies are mutually exclusive.

### Memory impact

The existing compact informer cache is preserved. Pod policies add no informer, watch or index, and do not retain full API pod objects. System-namespace and DaemonSet pods still retain identity only. With no policies configured, cached pod labels remain nil, as on the namespace-only path.

When policies are enabled, the transform retains only the union of label keys referenced by their selectors. It allocates a label map only if at least one of those keys is present on the pod; unrelated labels are discarded. This applies to all otherwise eligible pods in the cluster-wide cache, including pods outside a policy's namespace or with non-matching label values. Memory growth therefore depends on the number of cached pods carrying referenced keys and the sizes of their retained label values, not just the number of pods selected for a drain. Compiled selectors and the shared key list add configuration-sized overhead; there is no per-pod copy of the policy list.

The allocation benchmark in `node-drainer/pkg/informers/pod_policies_benchmark_test.go` uses the same pod with 50 source labels in every case. Three runs with Go 1.27.0 on macOS ARM64 produced:

| Cached pod | Retained policy labels | Bytes allocated per transform | Allocations per transform |
| --- | --- | --- | --- |
| Eligible, no policies | 0 | 4,064 | 13 |
| Eligible, three referenced keys absent | 0 | 4,064 | 13 |
| Eligible | 1 or 3 | 4,400 | 15 |
| Eligible | 10 | 5,016 | 18 |
| System namespace or DaemonSet, with or without policies | 0 | 1,280 | 1 |

One or three retained labels add 336 bytes and two allocations per cache update in this fixture; ten add 952 bytes and five allocations. These are allocation measurements, not retained heap or component RSS. The input pod is prepared outside the measured loop, so label string allocation and API decoding are excluded; selected label values remain reachable through the cached map. Updates replace cached objects and the old objects become eligible for garbage collection. Total component memory also includes indexes, event queues, database clients, transient API objects and runtime overhead, so these results do not establish a new container memory limit. Keep the set of referenced keys small and measure process memory at the intended pod count and label sizes before changing resource limits.

Reproduce from `node-drainer/`:

```sh
go test ./pkg/informers -run '^$' -bench '^BenchmarkExcludedPodTransform_PolicyLabelRetention$' -benchmem -count=3
```

## Rationale

Workload owners can opt into interruption policies through pod labels without moving applications between namespaces. Standard selector syntax supports equality, set membership and existence using the Kubernetes parser already available in the module. Reusing the current eviction mechanisms keeps timeout and partial-drain behavior consistent.

## Consequences

### Positive

- Workloads sharing a namespace can use different drain modes.
- Existing deployments remain on the namespace-only path until policies are configured.
- Selection is consistent across retries and after a drainer restart.

### Negative

- Rule order becomes part of configuration behavior.
- Relevant label changes can change a workload's policy during a drain.
- Resource-version preconditions may require another reconciliation when a selected pod changes before deletion.

### Mitigations

Document policy order and the unmatched-pod behavior explicitly, reject malformed configuration before processing events, and test mixed modes, overlapping selectors, restart/deadline behavior, partial drains and stale pod observations. Keep the cache limited to the label keys used by policies.

## Alternatives Considered

### Add a selector only to the initial namespace evaluation

Rejected because later namespace-wide eviction and completion reads would lose the selection and affect other workloads.

### Combine pod policies with namespace fallback

Rejected to keep configuration and evaluation simpler. Operators choose either namespace rules or pod policies, without maintaining two overlapping sources of drain modes.

### Replace all namespace rules with a general resource matching language

Deferred because it expands the migration and validation surface beyond the requested pod-label behavior. Existing namespace rules remain available as an alternative to pod policies.

### Choose drain mode automatically from controller kind

Deferred because a controller kind does not establish that interruption is acceptable. Operators can label the relevant pod templates explicitly.

## References

- [Node drainer configuration](../configuration/node-drainer.md#pod-drain-policies)
- [Kubernetes label selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors)
