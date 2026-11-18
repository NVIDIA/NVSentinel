# ADR-075: Cleanup of stale NVSentinel node conditions

Status: Proposed

## Context

[Issue #1673](https://github.com/NVIDIA/NVSentinel/issues/1673) reports conditions that remain after an upgrade or a check configuration change. The reporter uses conditions to track NVSentinel activity. Disabling `InfiniBandCharDeviceCheck` leaves its existing conditions behind. The reporter also describes stale Ethernet conditions since the v1.15 InfiniBand/Ethernet state-check split. Removing a detected XID from configuration can leave its text in a condition message.

The claim comment proposed an ordinary cleanup Job and asked three scope questions. Maintainer lalitadithya requested an ADR comparing options without Helm hooks. This record proposes a choice for review; it does not authorize implementation.

### Verified current behaviour

The following evidence was checked at `bbfaee0e`:

- The [cleanup template](../../distros/kubernetes/nvsentinel/templates/node-condition-cleanup-hook.yaml) gates resources on `nodeConditionCleanup.enabled` (line 15). It declares Helm `post-upgrade` and Argo CD `PostSync` hooks (lines 25–29). Its [values](../../distros/kubernetes/nvsentinel/values.yaml) default to disabled with an empty `deprecatedConditions` list (lines 655–657).
- The [cleanup script](../../distros/kubernetes/nvsentinel/files/node-condition-cleanup.sh) removes exact listed types without checking Reason or status (lines 52–67). It replaces the complete conditions array without a conditional test.
- The [condition writer](../../platform-connectors/pkg/connectors/kubernetes/process_node_events.go) uses `CheckName` as the condition type (lines 244–258). It writes `True` while fault messages remain and `False` when they clear (lines 263–310). Reasons are `<Check>IsHealthy` or `<Check>IsNotHealthy` (lines 1077–1084). Recovery updates the condition rather than deleting it.
- The [NIC check constants](../../health-monitors/nic-health-monitor/pkg/checks/types.go) define separate `InfiniBandStateCheck` and `EthernetStateCheck` names (lines 28–33). An InfiniBand event cannot update the Ethernet condition through this writer. The v1.15 timing comes from the issue report.
- The [NIC chart](../../distros/kubernetes/nvsentinel/charts/nic-health-monitor/values.yaml) still enables `InfiniBandCharDeviceCheck` by default (lines 41–46). It is a supported check, not a retired type. The [DaemonSet](../../distros/kubernetes/nvsentinel/charts/nic-health-monitor/templates/daemonset.yaml) passes `enabledChecks` to the monitor (lines 107–108).
- The [platform-connector configuration](../../distros/kubernetes/nvsentinel/templates/_platform-connector-config.tpl) contains no complete enabled-check inventory. Policy-defined names and separately configured publishers prevent a universal chart-derived inventory.

[ADR-052](052-deployment-platform-connector.md) supports both node-local DaemonSets and central Deployment replicas. Cleanup must work during their coexistence. [PR #1028](https://github.com/NVIDIA/NVSentinel/pull/1028) changed MongoDB initialization to an ordinary Job because post-hooks could block Helm readiness. The current [external MongoDB Job](../../distros/kubernetes/nvsentinel/templates/job-external-mongodb-setup.yaml) uses that pattern. Its [name helper](../../distros/kubernetes/nvsentinel/templates/_helpers.tpl) hashes inputs to avoid immutable Job updates (lines 66–76). That precedent does not establish identical execution semantics across GitOps tools.

## Decision

Recommend **option C: an ordinary hash-named Job**, without Helm or Argo CD hook annotations, using the safety contract below. Start with explicit retired types; keep chart-derived disabled-type cleanup opt-in and limited to verified inventories.

Maintainers must accept the open decisions before implementation. Keep existing defaults unchanged during the first release.

## Implementation

### Shared safety contract

All execution options use the same input model and deletion rules:

| Input | Source | Removal rule |
|---|---|---|
| Known checks | Reviewed built-in inventory plus explicit operator additions | Required ownership boundary; never use a suffix alone. |
| Retired types | Explicit release-maintained list of checks no longer emitted | Remove matching owned conditions regardless of status, including `True` and `Unknown`. This policy requires maintainer acceptance. |
| Disabled types | Known supported checks minus the effective enabled set for a verified chart inventory | Remove only `False` by default. An explicit operator setting may include `True`; preserve `Unknown`. |

A condition is eligible only when its type is a known check and its Reason equals `<Type>IsHealthy` or `<Type>IsNotHealthy`. Use exact string comparisons. This guard is an ownership convention, not proof against another writer using the same names. Preserve kubelet conditions, NPD source conditions, unknown types and mismatched Reasons.

An active supported check must never enter the retired list. Neither Ethernet state checking nor IB character-device checking is globally retired. Select these checks only when the effective configuration disables them. Review historical names before adding any retired entry. A dropped XID in a still-supported condition message does not retire that condition type.

Compute disabled candidates only where chart settings fully describe the emitted names and node scope. Begin with the NIC `enabledChecks` inventory. Exclude policy-defined, externally managed or uncertain names. Account for monitor enablement, subchart defaults and node selection. If enabled state differs by node, require explicit scope or skip automatic cleanup. Silence and old timestamps do not establish retirement.

Retired and enabled sets must not overlap. Reject ambiguous inputs before any writes. Scope the first run to an explicit node selector, with dry-run enabled. Log the input digest, node UID, type, status, category and outcome. Report counts of candidates, removals, skips and failures without copying condition messages.

### Effect of removing a True condition

`True` means a recorded fault, not verified current failure or recovery. Removing it hides the Kubernetes health signal from dashboards and external automation.

Fault Quarantine consumes stored health events in its [reconciler](../../fault-quarantine/pkg/reconciler/reconciler.go), starting at lines 202–278. Recovery removes tracked faults from annotations before uncordoning (lines 1780–1874). Deleting a condition does not supply a healthy event or release quarantine. A node can therefore remain cordoned after its condition disappears. Stored faults, quarantine annotations, taints and remediation work must remain unchanged.

Cleanup must not synthesize recovery events. Operators must verify repair and use the applicable recovery procedure separately. This distinction follows [ADR-003](003-rule-based-node-quarantine.md) and preserves [ADR-059](059-derived-condition-recovery.md)'s proposed recovery boundary.

### Job lifecycle and permissions

Use an NVSentinel Go cleanup command with context cancellation. Reuse the conditional-patch approach from `conditionsPatch` in the condition writer (lines 175–187). Re-read and recompute after conflicts. Preserve concurrent changes to every retained condition. A timeout, unreadable node or invalid input must never become an empty replacement array.

Hash all effective inputs and the pod specification into the Job name. Include the inventory, lists, node scope, status policy, dry-run setting, command version, image and an explicit rerun generation. Bundle immutable inputs with the Job. Use a dedicated ServiceAccount with `get,list` on Nodes and `patch` on `nodes/status`. Grant no cordon, eviction, Secret or datastore permissions. Dry-run needs only read access.

Use paginated node lists, bounded concurrency, client QPS/burst limits and bounded conflict retries with backoff. Set a run deadline and attempt limit sized for the fleet. Exit nonzero on partial failure and retain its summary. An idempotent rerun checks all guards again. No cleanup failure may block platform-connector readiness or ingestion.

### GitOps and execution timing

Helm creates the ordinary Job on install or an input-changing upgrade. A completed Job with the same name does not run again. Argo CD and Flux reconcile it as an ordinary resource. A changed hash creates a new Job instead of patching an immutable pod template.

Retain completed Jobs by default. TTL deletion can cause Argo CD or Flux to recreate a desired Job repeatedly. Helm alone does not continuously recreate it. Document explicit rerun generation and pruning for each tool; avoid routine Force/Replace annotations. Keep failed Jobs visible until an operator requests a rerun. GitOps Job health may affect sync status, even though ingestion remains independent.

Ordinary resources provide no post-rollout ordering guarantee. Before enabling cleanup, operators must finish retiring or disabling every publisher of a target type. This includes old replicas during rolling upgrades. Run a separate configuration change after that barrier. Never infer this barrier from a delay. Disable and remove any older cleanup Job before changing its policy or rolling back.

### Rollout and upgrade

1. Ship the cleaner disabled, with empty retired entries unless each historical name has been reviewed.
2. Preserve `nodeConditionCleanup.*` and its defaults. Add separate settings with inline documentation; remove no existing key.
3. Require one cleanup mechanism per release. Validate conflicting enablement rather than running both against the same nodes.
4. Complete publisher rollout, then inspect a dry-run on a selected node group.
5. Obtain explicit approval for the retired-type status policy before enabling writes. Expand node scope after reviewing results.
6. Enable disabled-type cleanup separately. Include `True` only after operator review of hidden faults and remaining quarantine state.

Rollback stops future cleanup; it cannot restore deleted conditions. Export candidate conditions before the first write for audit and manual review. Do not automatically restore snapshots over newer health data. An older monitor can recreate a removed condition, but quiet checks might never publish again. Keep retired entries across supported upgrade paths; an upgrade can skip releases.

### Tests and acceptance criteria

- Use table-driven tests for known/unknown types, exact/mismatched Reasons, empty lists and overlapping inputs. Cover every status and both categories.
- Use envtest for status patches. Verify kubelet, NPD and active supported conditions remain unchanged. Concurrent writer updates must survive retries.
- Verify retired types disappear at every status under the accepted policy. Disabled `False` conditions disappear; disabled `True` conditions stay unless explicitly enabled.
- Verify cleanup changes no stored event, quarantine annotation, taint or cordon state. A healthy node must remain schedulable without drain or remediation.
- Test API denial, timeout, throttling, conflict exhaustion, cancellation and partial completion. Each failure must be visible and safe to rerun.
- Render charts with NIC defaults, disabled checks, disabled monitors, custom names and node scopes. Hashes must cover every effective input and pod change.
- Validate Helm, Argo CD and Flux lifecycle cases: install, changed inputs, unchanged completed Job, failed Job, explicit rerun, pruning and rollback. Demonstrate no continuous recreation with retained Jobs.
- Test mixed old/new platform connectors in both ADR-052 topologies. Cleanup must wait for the documented publisher rollout barrier.
- Measure list memory, API request rates and completion time at the target fleet size. Set budgets before enabling cleanup broadly.

## Rationale

- A separate Job keeps fleet cleanup outside ingestion and works with either platform-connector topology.
- Explicit lists make retirement reviewable. A partial inventory avoids treating dynamic names as disabled checks.
- Dedicated RBAC and selected node groups limit operational exposure. Every option still has cluster-wide potential through `nodes/status` access.
- A one-shot operation fits upgrades and configuration changes without continuous API work or new controller leadership.

## Consequences

### Positive

- Operators can review stale-condition removal without relying on hooks.
- Cleanup uses the same contract under Helm, Argo CD and Flux.
- Event storage and module coordination remain unchanged for MongoDB and PostgreSQL.

### Negative

- Maintainers must keep the known and retired inventories accurate.
- Operators must sequence cleanup after publishers stop emitting target types.
- A wrong retired list can hide faults across a large fleet.
- The cleaner command, image packaging, retained Jobs and rerun procedure need maintenance.
- Default-disabled cleanup requires operator action and cannot fix message-level residue.

### Mitigations

- Require reviewed retirement entries, dry-run output and gradual node scope expansion.
- Preserve `True` disabled conditions by default and keep quarantine recovery separate.
- Bound API work and expose partial failure through Job status and summary logs.
- Document pruning, rollback and skipped-release upgrades before enabling the feature.

## Alternatives Considered

All options below inherit the shared inventory, ownership and status rules. Changing execution location cannot discover retirement from silence.

### A. Platform-connectors startup reconciliation

The chart supplies retired and verified disabled lists in platform-connector configuration. DaemonSet instances reconcile only their assigned node. Central replicas need one fleet owner, with coordination during mixed-mode operation. Reuse node status permissions; add node listing for fleet ownership and Lease permissions if leadership requires them.

Helm, Argo CD and Flux need a configuration checksum that causes a rollout. An unchanged running pod does not discover new inputs. Cleanup failures must leave ingestion available and report incomplete work. Restarting retries work; rollback stops future removal but restores nothing.

**Not recommended** because cold starts can multiply API load across replicas and both topologies. Rate limits and ownership coordination add complexity to ingestion. A bad policy reaches every restarted node. It saves a separate image but increases platform-connector lifecycle and testing burden. Nodes added after startup need another trigger.

### B. Periodic sweep in platform-connectors or a small controller

Read the same lists on configuration changes. Use a shared informer with a bounded work queue and cache resync, rather than repeated fleet polling. Platform connectors need topology coordination as in A. A separate controller needs Deployment maintenance and dedicated node read/watch/status-patch RBAC. Leadership adds Lease access.

Helm, Argo CD and Flux manage ordinary resources. Resync handles later nodes and transient failures without a new rollout. The controller reports errors and retries with backoff. Disable it before rollback; prior deletions remain irreversible.

**Deferred** because ongoing reconciliation repeatedly enforces a potentially wrong policy across the fleet. Rate limits bound API work but do not reduce that policy's reach. A new controller adds upgrades, leadership and monitoring. This becomes preferable if cleanup must cover continuously arriving nodes or changing policies.

### C variant. Regular CronJob

A CronJob uses the same explicit retired and chart-derived disabled inputs, ownership guard and status rules. It needs the Job's dedicated RBAC. Helm, Argo CD and Flux manage its schedule without hooks. Use `concurrencyPolicy: Forbid`, deadlines and bounded API rates.

**Deferred** because repeated fleet scans add load when upgrade cleanup is normally finite. Suspend it before policy changes or rollback. Failed runs retain evidence and retry on the next schedule; no run restores removed state. It handles later nodes, but adds schedule and Job-history maintenance. Separate CronJobs or old Jobs can still overlap despite `Forbid`.

### D. Operator-run cleanup command

An operator supplies the same reviewed lists and node scope. The command enforces identical ownership and status rules with temporary dedicated RBAC. Helm, Argo CD and Flux do not execute it. A failed bounded run reports partial progress for manual retry; rollback cannot restore conditions.

**Retained as a diagnostic path**, but rejected as the main delivery mechanism. It limits automatic exposure and avoids a controller. It also requires manual execution after every relevant change, which is the reported maintenance problem.

### Status-quo baseline: cleanup hook

The hook uses only explicit `deprecatedConditions`; it computes no disabled set and enforces neither ownership nor status guards. Its cluster RBAC allows node reads and status writes. The Job has bounded attempts and a deadline, but the script lists the fleet at once.

**Rejected as the primary mechanism** because it depends on Helm/Argo hook execution and manual list maintenance. Flux behaviour depends on Helm hook settings. Failure remains separate from normal reconciliation, and rollback cannot restore deletions. Adding safety guards could reduce its blast radius, but would retain hook lifecycle maintenance.

### Delete by age or synthesize healthy events

**Rejected** because silence does not prove recovery or retirement. Healthy events could release quarantine through the normal event pipeline. Cleanup only removes obsolete display state.

## Notes

Open decisions for maintainers:

- Accept removal of `True` retired conditions regardless of status, or require manual review for those conditions? Should disabled-`True` removal remain an explicit opt-in?
- Replace the hook internally while preserving `nodeConditionCleanup.*`, or coexist for one release with mutually exclusive enablement?
- Confirm message-level cleanup is out of scope, including dropped XIDs inside a still-valid condition. That needs a separate entity/error-aware policy.

This ADR supersedes no existing record. It adds no implementation, dependency, public field or default behaviour change.

## References

- [Issue #1673 and proposal discussion](https://github.com/NVIDIA/NVSentinel/issues/1673)
- [PR #1028: race with Helm hooks](https://github.com/NVIDIA/NVSentinel/pull/1028)
- [ADR-052: Deployment Platform Connector](052-deployment-platform-connector.md)
- [ADR-003: Rule-Based Node Quarantine](003-rule-based-node-quarantine.md)
- [ADR-059: Operator Recovery of Derived Conditions (proposed)](059-derived-condition-recovery.md)
