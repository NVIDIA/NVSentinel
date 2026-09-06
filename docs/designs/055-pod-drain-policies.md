# ADR-055: Node Drainer — Pod label policies

## Context

[Issue #1689](https://github.com/NVIDIA/NVSentinel/issues/1689) describes workloads with different interruption requirements sharing a namespace. Namespace-level drain configuration cannot express those differences. The existing drainer also passes namespace groups through immediate eviction, deadline deletion and completion checks, so filtering only during initial evaluation would still allow an action to affect unrelated pods.

## Decision

Add an optional ordered `podDrainPolicies` list. Each policy matches a standard Kubernetes pod label selector and an optional namespace-name glob, then selects an existing drain mode. The first matching policy wins; unmatched pods fall back to `userNamespaces`.

## Implementation

- Compile and validate selectors at startup. Reject missing or duplicate policy names, empty or invalid selectors, invalid namespace patterns and unsupported modes.
- Preserve the existing namespace precedence for fallback: Immediate, DeleteAfterTimeout, AllowCompletion. Policies take precedence over that fallback.
- Carry a mode-specific predicate through evaluation and execution. Every pod read used for eviction, force deletion or completion applies that predicate in addition to existing eligibility and partial-drain filters.
- Retain only referenced label keys in the compact pod informer cache. Relevant label changes are observed through normal informer updates; configuration changes require a restart.
- Use pod UID and resource-version deletion preconditions so replacement or relabelling after selection causes a retry against a fresh observation.
- Keep the existing event-based timeout, cold-start recovery, cancellation and dry-run behavior. Force overrides change the selected mode without expanding scope. Custom drain configuration and pod policies are mutually exclusive.

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

Document precedence and fallback explicitly, reject malformed configuration before processing events, and test mixed modes, overlapping selectors, restart/deadline behavior, partial drains and stale pod observations. Keep the cache limited to the label keys used by policies.

## Alternatives Considered

### Add a selector only to the initial namespace evaluation

Rejected because later namespace-wide eviction and completion reads would lose the selection and affect other workloads.

### Replace all namespace rules with a general resource matching language

Deferred because it expands the migration and validation surface beyond the requested pod-label behavior. Existing namespace rules remain the fallback.

### Choose drain mode automatically from controller kind

Deferred because a controller kind does not establish that interruption is acceptable. Operators can label the relevant pod templates explicitly.

## References

- [Node drainer configuration](../configuration/node-drainer.md#pod-drain-policies)
- [Kubernetes label selectors](https://kubernetes.io/docs/concepts/overview/working-with-objects/labels/#label-selectors)
