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
| Disabled check | Remove only owned `False` conditions without tracked faults by default. Preserve `True`, `Unknown` and tracked faults. | Recovery requires explicit operator opt-in and verified repair. |

The Job never acts on unknown types. It never edits quarantine annotations, taints, cordons, stored faults or maintenance requests directly. Recovery uses existing audit, metrics and downstream rules. It does not certify hardware health or cancel a reset already in progress.

## Implementation

### Ownership and scope

Require a reviewed known-check inventory and an explicit retired list. Each retired entry must identify the expected publishing agents, component classes and event versions. Require the condition Reason to equal `<Type>IsHealthy` or `<Type>IsNotHealthy`. Use exact comparisons for both type and Reason. A matching suffix alone is insufficient. Preserve kubelet conditions, NPD source conditions and mismatched Reasons.

Retirement withdraws an obsolete fault identity from service. Its last fault otherwise has no publisher that can ever resolve it. Maintainers must verify that retirement also removes the need for that fault's isolation. A renamed check needs replacement coverage and an explicit transition policy. Retirement is not evidence that damaged hardware was repaired.

An active supported type must never enter the retired list. `InfiniBandCharDeviceCheck` remains [enabled by default](../../distros/kubernetes/nvsentinel/charts/nic-health-monitor/values.yaml). `EthernetStateCheck` and `InfiniBandStateCheck` remain [distinct supported types](../../health-monitors/nic-health-monitor/pkg/checks/types.go). Dropping an XID from a supported check does not retire the whole type.

Compute disabled candidates only where chart settings fully describe emitted names and node scope. Start with NIC `enabledChecks`. Account for monitor enablement, defaults and node selection. Exclude dynamic policies, external publishers and uncertain inventories. Reject overlapping retired/enabled sets and unknown configured entries before writes. Missing or invalid ownership evidence requires manual review, including tracked faults whose condition is absent.

### Recovery of retired faults

1. List selected Nodes, record each Node's UID, and verify the ownership guard. Read `quarantineHealthEvent` using the existing parser, including its legacy single-event format.
2. For each retired type, select every matching tracked identity. Reject unexpected agents, components, versions or malformed annotation data. Never infer identity from condition text.
3. Build healthy events from those identities. Preserve `nodeName`, `agent`, `componentClass`, `checkName`, `version`, `entitiesImpacted` and `errorCode`. Set `isHealthy=true`, `isFatal=false`, action `NONE` and `processingStrategy=EXECUTE_REMEDIATION`. Clear inherited force/skip overrides. Assign a new event ID and timestamp. Reuse the batch idempotency key on transport retries. Add audit metadata containing the retirement entry, input digest and Node UID.
4. Immediately before publishing each recovery event, re-read the Node and require the UID recorded in step 1. Fault-quarantine matches faults by agent, component, check, node name and version, with no Node UID, so a recovery built from a Node that was replaced under the same name could clear a fault on the replacement. Treat a mismatch as a recovery failure and publish nothing for that Node. Then publish through platform-connectors using the monitor API. Keep all normal transformations, persistence and downstream processing. Verify that configured overrides and deduplication permit recovery; a suppressed or altered recovery is a failure.
5. Observe the condition and quarantine annotation through a Node informer with a bounded deadline. Require matching tracked identities to disappear and the condition writer to finish recovery to `False`. The writer clears matching messages and sets `False` when none remain. Other faults can keep the node quarantined.
6. Re-read the Node before removing the obsolete condition. Require the same UID, ownership guard and no remaining tracked fault for that type. Report partial recovery as failure; never replace it with condition-only deletion.

For merely-disabled checks, removal of monitoring does not withdraw the fault or prove repair. Keep faults by default. Any recovery opt-in must name checks and nodes, record operator verification, and use the same event path. Preserve disabled `Unknown` conditions even with opt-in. [Issue #1992](https://github.com/NVIDIA/NVSentinel/issues/1992) explicitly rejects quiet-period XID recovery. [ADR-059](059-derived-condition-recovery.md) provides verified, identity-scoped recovery for configured derived rules. Use that procedure for supported derived checks; do not bypass its verification or history boundary.

### Verified recovery path

These code references describe the implementation at `a161c861`:

- [HealthEvent schema](../../data-models/protobufs/health_event.proto), lines 78–85 and 118–139: `PlatformConnector.HealthEventOccurredV1(HealthEvents)` and the event fields above.
- [Monitor dial path](../../health-monitors/nic-health-monitor/main.go), lines 175–193: monitors use the shared publisher with the node-local Unix domain socket or the Deployment endpoint. [Publisher transport](../../commons/pkg/healthpub/dial.go), lines 75–122, selects `HEALTH_PUBLISH_TARGET` and requires the direct-mode token.
- [Quarantine identity](../../fault-quarantine/pkg/healthEventsAnnotation/health_events_annotation_map.go), lines 33–45 and 403–425: matching uses agent, component, check, node and version, then entities and error codes. Empty entity/code lists can broaden recovery; preserve the recorded scope.
- [Quarantine recovery](../../fault-quarantine/pkg/reconciler/reconciler.go), lines 1804–1874: healthy events remove matching entities. Remaining faults keep quarantine active. [Annotation update](../../fault-quarantine/pkg/reconciler/reconciler.go), lines 2022–2087, retains the final entry until unquarantine succeeds.
- [Session release](../../fault-quarantine/pkg/reconciler/reconciler.go), lines 2101–2176 and 2288–2334: normal recovery removes session annotations and actions. It preserves pre-existing taints and cordons and can require validation. [ADR-062](062-fault-quarantine-release-at-session-end.md) explains the session boundary.
- [Condition writer](../../platform-connectors/pkg/connectors/kubernetes/process_node_events.go), lines 244–310 and 347–370: `checkName` selects the condition. Healthy events clear matching entity/error messages; an entity-free event clears all messages. An empty message set becomes `False`, not a deleted condition. Reasons follow the ownership convention at lines 1077–1084.

Thus recovery resolves the fault through one event pipeline. The Job does not call fault-quarantine directly. It does not delete event history. MongoDB and PostgreSQL use the same ingestion contract.

### Condition-only fallback

Delete a condition without publishing only when no tracked fault for its type exists in a freshly read, valid quarantine annotation. An absent annotation counts as no tracked fault. Without a recovery publication in this run, retired types qualify at `False`, `Unknown`, or `True` with no annotation entry. For disabled types it covers only `False` by default. A disabled `True` needs the explicit verification opt-in described above.

A `False` condition with a tracked fault still requires recovery. Malformed annotations, unresolved identities and ingestion failures never qualify for fallback. After publishing recovery, wait for both writers as described above before deleting the resulting `False` condition. The normal writer clears health state; this final deletion removes the unused display entry.

Use conditional JSON patches that test Node UID, the observed annotation state and conditions before replacement. Re-read and recompute after conflicts. The existing [conditionsPatch](../../platform-connectors/pkg/connectors/kubernetes/process_node_events.go), lines 175–187, shows the conditions test. Preserve all retained conditions and concurrent updates. A failed read must never become an empty replacement array.

### Permissions and execution

Use a dedicated ServiceAccount with `get,list,watch` on Nodes and `patch` on `nodes/status` for final deletion. Dry-run needs only Node reads. Grant no Node-spec writes, evictions, Secret reads, datastore access or TokenReview access to the Job.

Publishing is a separate authority from Kubernetes RBAC. Project a pod-bound token for the platform-connector audience. Enable the Job's ServiceAccount in the connector's cross-node allowlist only when cleanup is enabled. The [node-binding interceptor](../../platform-connectors/pkg/auth/nodebinding.go), lines 16–75 and 224–228, defines that authority. Allow the required network path and mount the public CA bundle for Deployment mode. Socket mode mounts the local UDS and schedules the Job alongside a ready connector. Never weaken authentication to send events under the original agent name.

A gRPC reply has different meanings by topology. The [node-local role](../../platform-connectors/pkg/nodelocal/role.go), lines 16–21, acknowledges queue acceptance. The [central role](../../platform-connectors/pkg/central/role.go), lines 16–30, processes inside the request and uses idempotency keys. Queue acceptance alone never proves recovery. Completion requires observed downstream state, including annotation removal for every selected fault. Timeouts leave incomplete work visible for rerun. No fallback bypasses a failed publisher.

Use context cancellation, paginated lists, an informer, bounded workers, QPS limits and bounded conflict retries. Set Job deadlines and attempt limits for the fleet size. Exit nonzero on partial failure. Log input digests, Node UIDs, identities and outcome counts without copying diagnostic messages. Recovery may produce existing audit records and metrics; repeat publication must leave node state unchanged.

### Job naming and GitOps

Hash every effective input and the pod specification into the Job name. Include inventories, policies, scope, dry-run, image, command version and an explicit rerun generation. Bundle immutable inputs with that Job. Changed inputs create a new name instead of updating an immutable Job template. An unchanged completed Job stays completed. Recovery itself remains idempotent after retries or manual recreation.

`generateName` supplies uniqueness but no stable desired identity:

| Tool | Why use the hash name |
|---|---|
| Helm | [Upgrade matching](https://github.com/helm/helm/blob/main/pkg/action/upgrade.go) uses resource names. A server-generated name is absent from the rendered release. Helm cannot reliably upgrade or prune that instance. Create-on-upgrade workarounds can leave a new untracked Job each time; some apply paths fail instead. |
| Argo CD | [Generated names](https://argo-cd.readthedocs.io/en/stable/user-guide/resource_hooks/#generate-name) have explicit support for hooks. Ordinary resources need a stable desired name for matching and diffing. Unnamed create-based resources can stay OutOfSync and lack reliable pruning. Hooks would restore the lifecycle this design replaces. |
| Flux | A [HelmRelease](https://fluxcd.io/flux/components/helm/helmreleases/) inherits Helm behavior. A [Kustomization inventory](https://fluxcd.io/flux/components/kustomize/kustomizations/#inventory) tracks named objects for drift correction and pruning. Use stable names in either mode. |

These are identity constraints, not a promise that every tool accepts unnamed ordinary Jobs. Retain the current completed or failed Job without TTL deletion. Argo CD self-heal, Flux Kustomizations, or HelmRelease drift correction can recreate a deleted desired Job. Prune old hash names through Helm upgrades or enabled GitOps pruning after saving summaries. Document manual removal when pruning is disabled. Use rerun generation instead of routine Force/Replace annotations. Job failure can affect sync/release status; ingestion readiness stays independent.

### Rollout and hook migration

The Hash-named cleanup Job supersedes the cleanup hook. Keep the legacy hook for one transition release, disabled by default. Warn in chart notes when it is enabled. Reject simultaneous enablement of both mechanisms. Map `nodeConditionCleanup.deprecatedConditions` into the retired candidates during explicit migration, then validate every entry against the retirement inventory. Existing lists may contain merely-disabled checks; reject those entries rather than silently converting them to recovery requests.

After that release, remove hook execution while retaining its value keys as deprecated aliases for the new Job. Require explicit migration approval before any old configuration enables recovery. Keep defaults disabled and lists empty. Preserve retired entries across supported skipped-release upgrades. Document this transition inline in values and release notes before implementation ships.

Before enabling cleanup, observe termination of old publishers and connector pods that can hold their queued batches. This includes both topologies during coexistence under [ADR-052](052-deployment-platform-connector.md). Finish downstream processing of accepted old faults before recovery. Never infer completion from a delay. Run cleanup in a separate configuration change after this barrier.

Run dry-run first on an explicit node selector. Export candidate identities for audit. Review retirement coverage and approve recovery effects before enabling writes. Expand scope after checking results. Disable and remove older cleanup Jobs before policy changes or rollback. Rollback stops future cleanup but cannot retract recovery events or restore deleted conditions. Never restore snapshots over newer health state.

### Tests and acceptance criteria

- Table-test known/unknown types, exact Reasons, all statuses, both categories, overlapping inputs and malformed or legacy annotations.
- Use envtest for conditional patches, UID changes and concurrent writers. Preserve kubelet, NPD, active checks and unrelated conditions.
- Test retired faults end to end through gRPC, storage, condition clearing and quarantine release on MongoDB and PostgreSQL. Include both connector topologies and mixed versions.
- Verify another fault keeps the node cordoned. Preserve pre-existing isolation and normal validation. A healthy node must receive no cordon, drain, reset or reboot.
- Test every agent/component/version/entity/error identity, multiple faults, overrides, deduplication, repeated publication and interrupted recovery. No partial failure may cause fallback deletion.
- Verify disabled faults remain without explicit opt-in. Test no-tracked-fault deletion separately, including retired `True` and `False` with a tracked fault.
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
