# ADR-029: Slurm External Drain Health Monitor

## Context

Slurm nodes can be drained by sources other than the slurm-operator (e.g. Slurm's native HealthCheckProgram, admins via `scontrol`, or the native health_check script with `[HC]` reason). The slurm-operator treats any drain reason **not** prefixed with `slurm-operator:` as externally owned and does not modify or clear it. NodeSet pod conditions already reflect the Slurm node state and reason (`SlurmNodeStateDrain` + `Message`).

NVSentinel needs to treat these external Slurm drains as health events so the **full remediation cycle** runs (quarantine → drain → remediate). The DrainRequest is in the **middle** of that pipeline, so we cannot start from a DrainRequest—we must start at the **health event** and publish to the NVSentinel API. The logic is the same as other health monitors; only the **source** (pod conditions) and **publishing endpoint** (NVSentinel API) differ. We also need a **generic** way to parse multi-check drain reasons (e.g. split by a delimiter and regex match) so other consumers can reuse the same contract.

When NVSentinel cordons the node in response to an external drain, it must **not** set the `nodeset.slinky.slurm.net/node-cordon-reason` annotation, so the slurm-operator never overwrites or takes ownership of the external reason.

## Decision

Add a **health monitor** that watches NodeSet pod conditions for external Slurm drains, parses the reason string with a **generic split + regex** contract, and **publishes health events to the NVSentinel API** (same as other monitors). Events enter the normal pipeline (fault-quarantine, node-drainer, fault-remediation). The monitor only publishes events; it does not cordon. When NVSentinel cordons the node downstream (via fault-quarantine / node-drainer) for this event source, it does **not** set the `node-cordon-reason` annotation; cordon node only. This gives the full remediation cycle from a single entry point (health event) and keeps the design reusable via generic parsing.

## Implementation

### 1. Slurm-drain health monitor

**Placement:** `NVSentinel/health-monitors/slurm-drain-monitor/`. Structure follows kubernetes-object-monitor: `main.go`, `pkg/{config,controller,parser,publisher,initializer}`. Go, controller-runtime, same `pb` and gRPC client.

**Detection:**
- **Watch:** NodeSet pods (namespace / label selector). Read `status.conditions`.
- **Detect external drain:** Condition type `SlurmNodeStateDrain` (matches operator constant `PodConditionDrain`), status `True`, and `Message` non-empty and **not** starting with `slurm-operator:` (matches operator constant `nodeReasonPrefix`).

**Parsing (pkg/parser):**
- **Split** `Message` by configurable delimiter (if omitted, the full message is treated as a single segment) → list of segments.
- **Match** each segment against configurable regex rules. Each rule: `regex`, `checkName`, `componentClass`, optional `message`, `recommendedAction`, `isFatal`. If multiple rules match one segment, produce one structured reason per match.

**Health event mapping:**
- Build `pb.HealthEvent` per matched reason: `Agent` = `"slurm-drain-monitor"`, `CheckName`, `Message`, `IsFatal`, `RecommendedAction`, `EntitiesImpacted` = pod (v1/Pod GVK + namespace/name) and node name (from `pod.Spec.NodeName`). Same protos as other monitors.

**Publishing:**
- Call `PlatformConnectorClient.HealthEventOccurredV1` (same API and retry logic as kubernetes-object-monitor). Event flows through fault-quarantine → node-drainer → fault-remediation.

**State transitions:**
- **Unhealthy:** External drain newly detected or reason text changed → publish unhealthy event, store state.
- **Healthy:** External drain clears (condition status `False`, condition removed, `Message` changes to `slurm-operator:`-prefixed, or pod deleted) → publish healthy event, clear state.
- **Deduplication:** Track last-published `message` per pod. Only publish on transition.

**Startup:** Re-scan all matching pods on startup and reconcile against current conditions. Publishing is idempotent so re-publishing existing drains on restart is acceptable.

**Config (TOML):** `namespace`/`labelSelector` for NodeSet pods, `reasonDelimiter`, list of `patterns` (regex + optional fields). Flags: `--platform-connector-socket`, `--processing-strategy`.

### 2. NVSentinel pipeline (downstream)

The monitor only publishes health events; it does not cordon. When fault-quarantine or node-drainer cordons a node for events from the `slurm-drain-monitor` source, the cordon path must **not** set `node-cordon-reason`. This is a configuration / policy constraint in fault-quarantine or node-drainer, not in the monitor.

### 3. Shared parser (future)

The split+regex parser lives in `pkg/parser` within the monitor. If a second consumer appears, extract to a shared NVSentinel package. Until then, keep it colocated.

## Rationale

- **Full cycle:** Starting at the health event gives the full remediation cycle; starting from DrainRequest would be mid-pipeline.
- **Same pattern as other monitors:** Same publishing endpoint and pipeline; only the source (pod conditions) differs. The monitor does not cordon; the "don't set node-cordon-reason" constraint is a downstream pipeline policy.
- **Generic parsing:** Split by delimiter + regex keeps the design reusable for other consumers.
- **No overwrite:** Not setting `node-cordon-reason` when cordoning preserves the slurm-operator's ownership model and avoids losing the audit trail in Slurm.

## Consequences

- **Positive:** Full remediation cycle for external Slurm drains; same NVSentinel API and pipeline; generic parsing usable elsewhere; clear separation between monitor (publish) and pipeline (cordon policy).
- **Negative:** New monitor to deploy and configure.
- **Mitigation:** Config-driven (TOML); follow existing health-monitor patterns (kubernetes-object-monitor).

## Alternatives Considered

- **DrainRequest as entry point:** Rejected because it is mid-pipeline; we need the health event first for the full cycle.
- **Override external reason via node-cordon-reason:** Rejected; would overwrite audit trail and change slurm-operator semantics.
- **CEL-based policy (like kubernetes-object-monitor):** CEL is well-suited for evaluating arbitrary object properties but overkill for string parsing; split+regex is simpler and more explicit for reason parsing.

## References

- kubernetes-object-monitor: `health-monitors/kubernetes-object-monitor` (publisher, controller, config pattern).
- Slurm-operator constants: `SlurmNodeStateDrain` (`PodConditionDrain`), `slurm-operator:` (`nodeReasonPrefix`), `nodeset.slinky.slurm.net/node-cordon-reason` (`AnnotationNodeCordonReason`).
