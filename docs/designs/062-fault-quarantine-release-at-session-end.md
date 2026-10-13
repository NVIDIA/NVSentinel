# ADR-062: Fault Quarantine — Release Quarantine Actions Only at Session End

Status: Proposed

## Context

A quarantine session starts when fault-quarantine quarantines a node, and it ends when the last event that it tracks for the node recovers. A session can track several unhealthy health events at the same time. Fault-quarantine records them in the `quarantineHealthEvent` node annotation. Each event matches its own rule sets, and each rule set can require a taint, a cordon, or a label ([ADR-003](003-rule-based-node-quarantine.md)).

Fault-quarantine applies the taints and the cordon of the event that starts a session. A later event in the same session adds its rule labels, but not its taints or its cordon. Fault-quarantine releases the taints and the cordon of a session in one step, when every event in the session has recovered. Until then, a recovered event's taint and cordon stay on the node, even when no remaining event requires them. [Issue #1439](https://github.com/NVIDIA/NVSentinel/issues/1439) asks for two changes: apply the actions of later events, and release a recovered event's actions on partial recovery. For example, event E1 requires a cordon and taint A, and event E2 requires taint B only. After E1 recovers, the node stays cordoned and keeps taint A until E2 also recovers.

No earlier record states why fault-quarantine releases actions only when the session ends. Node-drainer, fault-remediation, and [ADR-049](049-node-validation.md) depend on this behaviour.

## Decision

Fault-quarantine applies the taints and the cordon that each event in a session requires when the event arrives, including events that arrive after the node is quarantined. It keeps every taint and the cordon that it applied in the session until the last event in the session recovers. It does not release an action when one event recovers and other events remain.

## Implementation

- **Apply the actions of later events.** When an event arrives for a node that is already quarantined, `handleQuarantinedNode` (`fault-quarantine/pkg/reconciler/reconciler.go`) applies the taints and the cordon that its rule sets require. It records them in the session annotations as for the event that starts a session, so the release at session end removes them and keeps pre-existing ones.
- **Release at session end.** This is the current behaviour. When a healthy event leaves other events in the session, `handleQuarantinedNode` removes the recovered entities and keeps the node quarantined. When the last event recovers, it removes the taints, the cordon, and the rule labels, sets the event status to `UnQuarantined`, and creates the post-remediation `ValidationRequest` if validation is enabled.
- Update the fault-quarantine configuration reference (`docs/configuration/fault-quarantine.md`) to state that the taints and the cordon of a session are released only when the session ends.

## Rationale

- **Every active fault gets its isolation.** If a later event requires a cordon or a taint, the node gets it, even when the event that started the session did not require it. Otherwise pods keep landing on a node with an active fault that its rules isolate.

The node must stay unschedulable for the whole session, because node-drainer and fault-remediation assume that no new pods land on it after a drain:

- **Node-drainer trusts an earlier drain.** When a later event arrives, node-drainer marks it `AlreadyDrained` if another event in the session already finished a drain (`IsNodeDrained` in `commons/pkg/drain/drain.go`). If a release lets pods land, the later event evicts nothing, and fault-remediation then reboots or resets the node with those pods on it.
- **Work for a recovered event can still run.** A drain that started for an event continues after the event recovers, and fault-remediation remediates the event when the drain finishes. If the cordon comes off during this, new pods land and are then evicted or interrupted by the reboot.
- **A GPU reset needs a GPU that no process holds.** Pods that land after a release can open the device, so the reset fails and fault-remediation escalates to a reboot.
- **The session end is a safe release point.** When the last event recovers, fault-quarantine sets the `UnQuarantined` status. Node-drainer then cancels every drain in progress for the node, and fault-remediation closes the events that wait behind a maintenance CR. No work remains for the session, so the node can accept pods. A recovery in the middle of a session has no such signal.
- **One release point keeps the session simple.** The node returns to service at one point in time, and ADR-049 validates it once, at that point.

## Consequences

### Positive

- A later fault that requires a cordon or a taint gets it, so the node's isolation covers every active fault.
- Node-drainer and fault-remediation keep their current contract: the node is unschedulable from the first cordon until the session ends.
- The node returns to service at one point, after one validation.

### Negative

- A recovered fault's cordon and taints stay on the node until every other fault in the session recovers. The node stays out of service longer than its active faults require.
- A session that started with taints only can now become cordoned when a later event requires a cordon. More nodes can be cordoned than before, and the circuit breaker counts them.

### Mitigations

- Operators choose which faults require a cordon and which require only taints. A fault that should not take the node out of service can use taints only.
- An operator who decides that the node is fit can uncordon it manually. This ends the session and cancels its events.

## Alternatives Considered

### Release a recovered event's actions on partial recovery

Remove each taint and the cordon that no remaining event requires, behind an opt-in setting.

**Rejected** because: it lets pods land while node-drainer and fault-remediation still act on the node (see Rationale).

### Release on partial recovery and cancel the recovered event

Mark the recovered event `Cancelled`, as a manual uncordon does, and make node-drainer stop trusting an earlier drain after a release.

**Rejected** because: fault-remediation treats `Cancelled` as the end of the session (`handleCancellationEvent` clears all remediation state of the node), so the remaining events would lose theirs. A per-event status would change node-drainer and fault-remediation, and it still cannot stop a reset or reboot that has started.

## Notes

- A session whose rules require only taints does not cordon the node, so pods that tolerate the taints can land during the session. Node-drainer's `AlreadyDrained` check has the same assumption there. That gap exists independently of this record and is tracked separately.
- A later record can revisit partial release if node-drainer and fault-remediation gain per-event cancellation and a drain proof that accounts for pods that land after a drain.

## References

- [Issue #1439](https://github.com/NVIDIA/NVSentinel/issues/1439)
- [ADR-003: Rule-Based Node Quarantine](003-rule-based-node-quarantine.md)
- [ADR-049: Node Validation](049-node-validation.md)
- [PR #2010: review discussion of partial release](https://github.com/NVIDIA/NVSentinel/pull/2010)
