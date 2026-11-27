# ADR-075: Cleanup of stale NVSentinel node conditions

Status: Proposed

## Context

[Issue #1673](https://github.com/NVIDIA/NVSentinel/issues/1673) reports node conditions left behind after upgrades or check configuration changes. A retired check is a type that NVSentinel no longer emits at all. A disabled check remains supported but is off in the selected nodes' effective configuration. Neither silence nor age proves retirement or repair.

The current [cleanup hook](../../distros/kubernetes/nvsentinel/templates/node-condition-cleanup-hook.yaml) runs after Helm upgrades or Argo CD syncs. Its [settings](../../distros/kubernetes/nvsentinel/values.yaml) default to disabled with an empty `nodeConditionCleanup.deprecatedConditions` list. The [script](../../distros/kubernetes/nvsentinel/files/node-condition-cleanup.sh) deletes listed conditions without checking ownership or fault state. Deleting a condition leaves any tracked fault, quarantine annotation and cordon in place.

## Decision

Use a **Hash-named cleanup Job** with no Helm or Argo CD hook annotations. It resolves retired faults through normal health-event ingestion before removing obsolete conditions.

| Target | Condition | Quarantine annotation and cordon |
|---|---|---|
| Retired check with a tracked fault | Publish matching healthy events; the normal writer clears the fault signal to `False`. Remove the obsolete condition after recovery completes. | Fault-quarantine removes the matching fault. It releases quarantine only when no tracked faults remain, under its normal rules. |
| Retired check without a tracked fault | Remove the owned condition, including `True`, `False` or `Unknown`. | Leave node isolation unchanged. |
| Disabled check | Remove only owned `False` conditions without tracked faults by default. Preserve `True`, `Unknown` and tracked faults. | Keep the tracked fault. An operator can opt in to recovery after verifying the repair; see [Opt-in recovery of disabled checks](#opt-in-recovery-of-disabled-checks). |

The Job never acts on unknown types. It never edits quarantine annotations, taints, cordons, stored faults or maintenance requests directly. Recovery uses existing audit, metrics and downstream rules. It does not certify hardware health or cancel a reset already in progress.

## Implementation

### Owner and trigger

The NVSentinel Helm chart renders the Job, so the Helm release or GitOps application that installs NVSentinel owns it. There is no controller or schedule. The Job runs once when `nodeConditionCleanup.enabled` is `true` and an install, upgrade or sync renders a Job name that does not exist yet. The name changes only when an input changes, as described in [Job naming and GitOps](#job-naming-and-gitops). To run it again with the same inputs, an operator increments `nodeConditionCleanup.rerunGeneration`.

Responsibilities are split:

- NVSentinel maintainers own the known-check inventory and the retired list. They ship them in chart defaults with the release that retires a check.
- Operators own enablement, node scope, dry-run and every recovery opt-in for disabled checks.

The Job reports its results in its logs and exit status. A failed Job stays visible in the release or sync status.

### Check inventory and scope

Require a reviewed known-check inventory and an explicit retired list. Each retired entry must identify the expected publishing agents, component classes and event versions. Require the condition Reason to equal `<Type>IsHealthy` or `<Type>IsNotHealthy`. Use exact comparisons for both type and Reason. A matching suffix alone is insufficient. Preserve kubelet conditions, NPD source conditions and mismatched Reasons.

Retirement withdraws an obsolete fault identity from service. Its last fault otherwise has no publisher that can ever resolve it. Maintainers must verify that retirement also removes the need for that fault's isolation. Retirement is not evidence that damaged hardware was repaired.

No check is renamed today. If a check is renamed, say from `OldCheck` to `NewCheck`, the old type is retired under this policy:

1. Ship the monitor that publishes `NewCheck` first. Do not add `OldCheck` to the retired list in the same release.
2. Add `OldCheck` to the retired list in a later release, after every selected node runs the new monitor. A monitor publishes its current faults when it starts, so a fault that is still present is already tracked under `NewCheck`.
3. Recovering `OldCheck` then removes only the old identity. The node stays quarantined if `NewCheck` still reports the fault.

An active supported type must never enter the retired list. `InfiniBandCharDeviceCheck` remains [enabled by default](../../distros/kubernetes/nvsentinel/charts/nic-health-monitor/values.yaml). `EthernetStateCheck` and `InfiniBandStateCheck` remain [distinct supported types](../../health-monitors/nic-health-monitor/pkg/checks/types.go). Dropping an XID from a supported check does not retire the whole type.

The Job treats a check as disabled on a node only when the chart settings say exactly which checks run on which nodes. Today that is true only for the nic-health-monitor `enabledChecks` list. To decide, the Job reads whether the monitor is enabled, its default checks and its node selector. A check from any other source counts as unknown, and the Job leaves it alone. Examples are a check that a custom monitor or an external publisher sends, or one whose name depends on runtime policy.

Before any write, the Job fails if:

- a check is in both the retired list and the enabled list;
- a configured entry is not in the known-check inventory.

If the Job cannot prove that NVSentinel owns a condition or fault, it reports it for manual review. This includes a tracked fault whose condition is missing.

### Recovery of retired faults

1. List selected Nodes, record each Node's UID, and verify the ownership guard. Read `quarantineHealthEvent` using the existing parser, including its legacy single-event format.
2. For each retired type, select every matching tracked identity. Reject unexpected agents, components, versions or malformed annotation data. Never infer identity from condition text.
3. Build healthy events from those identities. Preserve `nodeName`, `agent`, `componentClass`, `checkName`, `version`, `entitiesImpacted` and `errorCode`. Set `isHealthy=true`, `isFatal=false`, action `NONE` and `processingStrategy=EXECUTE_REMEDIATION`. Clear inherited force/skip overrides. Set `message` to `Healthy event for retired check <checkName>, published by the node condition cleanup Job`, so event history and logs show that the monitor did not send it. The condition writer ignores the message of a healthy event, so this does not change the condition. Assign a new event ID and timestamp. Reuse the batch idempotency key on transport retries. Add audit metadata containing the retirement entry and input digest. Set the event's `metadata` key `nvsentinel.nvidia.com/node-uid` to the Node UID recorded in step 1.
4. Immediately before publishing each recovery event, re-read the Node and require the UID recorded in step 1. Fault-quarantine matches faults by agent, component, check, node name and version, with no Node UID, so a recovery built from a Node that was replaced under the same name could clear a fault on the replacement. Treat a mismatch as a recovery failure and publish nothing for that Node. Then publish through platform-connectors using the monitor API. Keep all normal transformations, persistence and downstream processing. Verify that configured overrides and deduplication permit recovery; a suppressed or altered recovery is a failure. An override alters the recovery only if it changes `isHealthy`, `isFatal`, `recommendedAction` or `processingStrategy` to another value. A rule that sets the values that the Job already set permits it.
5. Observe the condition and quarantine annotation through a Node informer with a bounded deadline. Require matching tracked identities to disappear and the condition writer to finish recovery to `False`. The writer clears matching messages and sets `False` when none remain. Other faults can keep the node quarantined.
   The pre-publication check alone cannot close the race: a Node can be replaced after the check but before the event is processed. So the UID is fenced end to end. Any consumer that mutates Node state for an event carrying `nvsentinel.nvidia.com/node-uid` must make that UID an atomic precondition of each mutation, including retries. A read followed by a separate write is not enough, because the Node can be replaced in between. Use a JSON patch whose first operation is `test` on `/metadata/uid`, or an update that carries the `resourceVersion` of the read that matched the UID. Either way the API server rejects the write if the Node was replaced. This covers fault-quarantine's annotation update and quarantine release, and platform-connectors' condition writer. On a mismatch, the consumer leaves the Node unchanged, logs and counts the stale recovery, and acknowledges the event so it does not retry. Events without the key keep today's behavior, so monitors are unaffected.
6. Re-read the Node before removing the obsolete condition. Require the same UID, ownership guard and no remaining tracked fault for that type. Report partial recovery as failure; never replace it with condition-only deletion.

### Opt-in recovery of disabled checks

Disabling a check stops monitoring. It does not withdraw the fault or prove repair, so the Job keeps the faults of disabled checks by default. An operator can still have a valid reason to clear them. For example, a check was disabled because it was noisy, and its faults are no longer treated as fatal. The condition and quarantine annotation then stay on the node with nothing to clear them.

For that case, `nodeConditionCleanup.disabledRecovery` lists recovery requests. Each entry must name:

- the check;
- the nodes, by name or by a label selector that the Job resolves once and logs;
- who verified the repair and why, which the Job adds to the audit metadata of each recovery event.

The Job validates each entry like a retired entry: the check must be known and disabled on every named node. It then recovers the tracked faults through the steps above, with the same UID fence and completion checks. An empty list, the default, recovers nothing. The Job never removes a disabled `Unknown` condition, even with opt-in.

[Issue #1992](https://github.com/NVIDIA/NVSentinel/issues/1992) explicitly rejects quiet-period XID recovery. [ADR-059](059-derived-condition-recovery.md) provides verified, identity-scoped recovery for configured derived rules. Use that procedure for supported derived checks; do not bypass its verification or history boundary.

### Why recovery uses the event pipeline

The Job sends recovery events through platform-connectors instead of editing Node state itself. The existing consumers then clear the fault with their normal rules:

- [Fault-quarantine](../../fault-quarantine/pkg/reconciler/reconciler.go) removes the matching tracked fault. It keeps the node quarantined while other faults remain, and it releases the quarantine session under [ADR-062](062-fault-quarantine-release-at-session-end.md).
- The [condition writer](../../platform-connectors/pkg/connectors/kubernetes/process_node_events.go) clears the matching messages and sets the condition to `False` when none remain.

Fault-quarantine [matches faults](../../fault-quarantine/pkg/healthEventsAnnotation/health_events_annotation_map.go) by agent, component class, check, node and version, then by entities and error codes. Empty entity or error code lists match more faults, so the recovery keeps the recorded scope. The Job does not call fault-quarantine directly or delete event history, and MongoDB and PostgreSQL use the same ingestion path.

### Condition-only fallback

Delete a condition without publishing only when no tracked fault for its type exists in a freshly read, valid quarantine annotation. An absent annotation counts as no tracked fault. Without a recovery publication in this run, retired types qualify at `False`, `Unknown`, or `True` with no annotation entry. For disabled types it covers only `False` by default. A disabled `True` needs the explicit verification opt-in described above.

A `False` condition with a tracked fault still requires recovery. Malformed annotations, unresolved identities and ingestion failures never qualify for fallback. After publishing recovery, wait for both writers as described above before deleting the resulting `False` condition. The normal writer clears health state; this final deletion removes the unused display entry.

Use conditional JSON patches that test Node UID, the observed annotation state and conditions before replacement. Re-read and recompute after conflicts. The existing [conditionsPatch](../../platform-connectors/pkg/connectors/kubernetes/process_node_events.go) shows the conditions test. Preserve all retained conditions and concurrent updates. A failed read must never become an empty replacement array.

### Permissions and execution

Use a dedicated ServiceAccount with `get,list,watch` on Nodes and `patch` on `nodes/status` for final deletion. Dry-run needs only Node reads. Grant no Node-spec writes, evictions, Secret reads, datastore access or TokenReview access to the Job.

Publishing is a separate authority from Kubernetes RBAC. Project a pod-bound token for the platform-connector audience. Enable the Job's ServiceAccount in the connector's cross-node allowlist only when cleanup is enabled. The [node-binding interceptor](../../platform-connectors/pkg/auth/nodebinding.go) defines that authority. Allow the required network path and mount the public CA bundle for Deployment mode. Socket mode mounts the local UDS and schedules the Job alongside a ready connector. Never weaken authentication to send events under the original agent name.

A gRPC reply has different meanings by topology. The [node-local role](../../platform-connectors/pkg/nodelocal/role.go) acknowledges queue acceptance. The [central role](../../platform-connectors/pkg/central/role.go) processes inside the request and uses idempotency keys. Queue acceptance alone never proves recovery. Completion requires observed downstream state, including annotation removal for every selected fault. Timeouts leave incomplete work visible for rerun. No fallback bypasses a failed publisher.

Use context cancellation, paginated lists, an informer, bounded workers, QPS limits and bounded conflict retries. Set Job deadlines and attempt limits for the fleet size. Exit nonzero on partial failure. Log input digests, Node UIDs, identities and outcome counts without copying diagnostic messages. Recovery may produce existing audit records and metrics; repeat publication must leave node state unchanged.

### Job naming and GitOps

Hash every effective input and the pod specification into the Job name. Include inventories, policies, `disabledRecovery` entries, scope, dry-run, image, command version and an explicit rerun generation. Bundle immutable inputs with that Job. Changed inputs create a new name instead of updating an immutable Job template. An unchanged completed Job stays completed. Recovery itself remains idempotent after retries or manual recreation.

`generateName` supplies uniqueness but no stable desired identity:

| Tool | Why use the hash name |
|---|---|
| Helm | [Upgrade matching](https://github.com/helm/helm/blob/main/pkg/action/upgrade.go) uses resource names. A server-generated name is absent from the rendered release. Helm cannot reliably upgrade or prune that instance. Create-on-upgrade workarounds can leave a new untracked Job each time; some apply paths fail instead. |
| Argo CD | [Generated names](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_hooks/#generate-name) have explicit support for hooks. Ordinary resources need a stable desired name for matching and diffing. Unnamed create-based resources can stay OutOfSync and lack reliable pruning. Hooks would restore the lifecycle this design replaces. |
| Flux | A [HelmRelease](https://fluxcd.io/flux/components/helm/helmreleases/) inherits Helm behavior. A [Kustomization inventory](https://fluxcd.io/flux/components/kustomize/kustomizations/#inventory) tracks named objects for drift correction and pruning. Use stable names in either mode. |

These are identity constraints, not a promise that every tool accepts unnamed ordinary Jobs. Retain the current completed or failed Job without TTL deletion. Argo CD self-heal, Flux Kustomizations, or HelmRelease drift correction can recreate a deleted desired Job. Prune old hash names through Helm upgrades or enabled GitOps pruning after saving summaries. Document manual removal when pruning is disabled. Use rerun generation instead of routine Force/Replace annotations. Job failure can affect sync/release status; ingestion readiness stays independent.

### Faults reclassified by an override

[Issue #2520](https://github.com/NVIDIA/NVSentinel/issues/2520), now tracked here, reports a third kind of stale state. An override rule now classifies a recorded fault as healthy or non-fatal, but the check is active and its monitor still runs. A monitor that publishes on transitions does not send the steady fault again, so the new rule never applies. The condition stays until the monitor restarts. When `global.dryRun` is `false`, the quarantine stays too.

There is no reviewed list for this case, so the candidates must be computed. That needs the stored events, and the Job has no datastore access. So platform-connectors computes the candidates when it starts with an override rule set that it has not checked before:

1. For each tracked fault of an NVSentinel-owned condition that is `True`, read the latest stored unhealthy event with the same identity.
2. Run that event through the current transformers. It is a candidate if the result is healthy or no longer fatal.
3. By default, only log and count the candidates. With `platformConnector.overrideRecovery.enabled`, publish recovery events built as in step 3 of [Recovery of retired faults](#recovery-of-retired-faults), with the message `Healthy event for check <checkName>, published after an override change`. The UID fence and the completion rules apply unchanged.

A candidate whose check is not in the known-check inventory may be a missing retirement entry. It is only reported, never recovered.

### Rollout and hook migration

The Hash-named cleanup Job supersedes the cleanup hook. Keep the legacy hook for one transition release, disabled by default. Warn in chart notes when it is enabled. Reject simultaneous enablement of both mechanisms. Map `nodeConditionCleanup.deprecatedConditions` into the retired candidates during explicit migration, then validate every entry against the retirement inventory. Existing lists may contain merely-disabled checks; reject those entries rather than silently converting them to recovery requests.

After that release, remove hook execution while retaining its value keys as deprecated aliases for the new Job. Require explicit migration approval before any old configuration enables recovery. Keep defaults disabled and lists empty. Preserve retired entries across supported skipped-release upgrades. Document this transition inline in values and release notes before implementation ships.

Before enabling cleanup, observe termination of old publishers and connector pods that can hold their queued batches. This includes both topologies during coexistence under [ADR-052](052-deployment-platform-connector.md). Finish downstream processing of accepted old faults before recovery. Never infer completion from a delay. Run cleanup in a separate configuration change after this barrier.

Recovery does not clear the in-memory state of a monitor that still runs, which matters only for opt-in recovery of a disabled check. A monitor publishes on transitions, so a stale entry in its memory publishes nothing. If the monitor later sends a healthy event for that fault, fault-quarantine finds no tracked fault and skips the event, and the condition is already `False`.

Run dry-run first on an explicit node selector. Export candidate identities for audit. Review retirement coverage and approve recovery effects before enabling writes. Expand scope after checking results. Disable and remove older cleanup Jobs before policy changes or rollback. Rollback stops future cleanup but cannot retract recovery events or restore deleted conditions. Never restore snapshots over newer health state.

### Tests and acceptance criteria

- Table-test known/unknown types, exact Reasons, all statuses, both categories, overlapping inputs and malformed or legacy annotations.
- Use envtest for conditional patches, UID changes and concurrent writers. Preserve kubelet, NPD, active checks and unrelated conditions.
- Queue a recovery event, replace the Node under the same name, then let fault-quarantine and platform-connectors process the stale event. The replacement Node's conditions, quarantine annotation and cordon must be unchanged, and the event must be acknowledged without retrying. Repeat the case with the Node replaced after the consumer reads the UID but before it writes. The write must fail, and the replacement must be unchanged.
- Test retired faults end to end through gRPC, storage, condition clearing and quarantine release on MongoDB and PostgreSQL. Include both connector topologies and mixed versions.
- Verify another fault keeps the node cordoned. Preserve pre-existing isolation and normal validation. A healthy node must receive no cordon, drain, reset or reboot.
- Test every agent/component/version/entity/error identity, multiple faults, overrides, deduplication, repeated publication and interrupted recovery. No partial failure may cause fallback deletion.
- Verify disabled faults remain without explicit opt-in. Reject `disabledRecovery` entries with an unknown check, a check that is enabled on a named node, or no verification record. Test that a valid entry recovers only the named checks on the named nodes. Test no-tracked-fault deletion separately, including retired `True` and `False` with a tracked fault.
- Cover denial, cancellation, timeout, throttling, conflicts and unavailable ingestion. Bound completion observation; prove queue acknowledgment alone cannot mark success.
- Render charts for migration conflicts, aliases, inventories, authentication and node scope. Test Helm, Argo CD and Flux naming, unchanged completion, rerun, pruning, failures and rollback.
- Test the rollout barrier with queued old batches. Measure memory, API rates and completion time before fleet rollout.

## Rationale

- Recovery resolves obsolete faults through existing writers, audit and metrics.
- Explicit retirement removes an identity that cannot report recovery again.
- A separate bounded Job keeps cleanup outside ingestion startup and steady-state processing.
- Stable hash names make execution depend on reviewed inputs across delivery tools.

## Consequences

### Positive

- Retired faults no longer strand nodes in quarantine after their conditions disappear.
- Disabled faults remain visible until an operator verifies recovery.
- Existing event schemas and module coordination stay unchanged.

### Negative

- An incorrect retirement policy can release an unsafe node.
- Publishing requires cross-node authority and available downstream consumers.
- Inventories, rollout ordering, retained Jobs and migration need maintenance.

### Mitigations

- Require reviewed retirement mappings, dry-run and gradual scope expansion.
- Fail closed on uncertain identity, configuration or completion.
- Keep disabled recovery opt-in and preserve session release rules.

## Alternatives Considered

### Platform-connectors startup reconciliation

**Rejected** because restarts multiply fleet work and tie cleanup to ingestion. Both deployment topologies need ownership coordination and rollout ordering.

### Continuous controller or periodic sweep

**Deferred** because upgrade cleanup is finite. A controller adds leadership and monitoring; a CronJob repeats fleet scans. Revisit if later nodes need continuous cleanup.

### Operator-run cleanup command

**Retained for diagnostics**, but rejected as the main delivery mechanism because every configuration change requires manual execution.

### Cleanup hook

**Superseded** because hooks have tool-specific ordering and deletion rules. The migration above preserves configuration while retiring hook execution.

### Delete conditions for tracked faults

**Rejected** because the quarantine annotation and cordon remain. Recovery events are the chosen mechanism for retired faults.

### Delete by age or infer recovery from silence

**Rejected** because neither proves retirement or repair. Supported checks retain their existing recovery procedures.

## Notes

This proposed record changes no implementation or default. Maintainers must accept it before implementation. It supersedes no accepted ADR. Message-level cleanup inside an active condition remains out of scope.

## References

- [Issue #1673](https://github.com/NVIDIA/NVSentinel/issues/1673)
- [ADR-003: Rule-Based Node Quarantine](003-rule-based-node-quarantine.md)
- [ADR-052: Deployment Platform Connector](052-deployment-platform-connector.md)
- [ADR-059: Operator Recovery of Derived Conditions (proposed)](059-derived-condition-recovery.md)
- [ADR-062: Release Quarantine Actions Only at Session End (proposed)](062-fault-quarantine-release-at-session-end.md)
