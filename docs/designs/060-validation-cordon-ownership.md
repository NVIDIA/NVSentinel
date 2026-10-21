# ADR-060: Validation — Preserve Cordon Ownership Through Post-Remediation Validation

Status: Proposed

## Context

Fault-quarantine records whether a node was cordoned before a fault in the `quarantineHealthEventCordonPreExisting` annotation ([issue #1424](https://github.com/NVIDIA/NVSentinel/issues/1424)). When a quarantine session ends without validation, it leaves such a cordon in place. When the session ends with a `ValidationRequest` ([ADR-049](049-node-validation.md)), it hands the cordon to lifecycle-manager and drops the record: the request carries only the node name and the tests, and lifecycle-manager's `schedulingGate.cordon.remove` uncordons the node whenever the last request on it completes. A node that an operator cordoned before a fault is therefore uncordoned after automatic repair and validation. The Kubernetes cordon is a single boolean with no owner, so lifecycle-manager cannot tell that cordon from one fault-quarantine applied.

Tracking ownership raises a second problem. A new fault can arrive while a previous fault's validation keeps the node cordoned. The node is already unschedulable at that moment, so the new fault would be recorded as a pre-existing cordon, and the node would never return to service automatically.

Options considered: an additional maintenance taint that operators apply; inferring ownership from the `cordon-by` audit labels; a persisted ownership record that both controllers read and write.

## Decision

Fault-quarantine decides who owns a node's cordon and passes that decision to lifecycle-manager with the `ValidationRequest`. With `schedulingGate.cordon.onlyOwned: true`, lifecycle-manager releases a cordon only when the request proves that NVSentinel acquired it; a pre-existing cordon survives successful validation. Ownership belongs to one continuous cordon, recorded in a claim on the Node, so overlapping faults and requests keep their release eligibility. The default, `onlyOwned: false`, keeps the current release policy.

## Implementation

- **Ownership decision.** `annotationsWithCordonOwnership` in `fault-quarantine/pkg/informer/cordon_ownership.go` decides ownership from the live Node version being patched, including metadata-only updates for later events in a session. A cordon that fault-quarantine applies to a schedulable node is `owned` and receives a claim. A cordon that is already present is `pre-existing`, unless an active owned claim can be inherited. A later fault that only taints the node also inherits the active claim, so its request does not demote the cordon to unknown ownership.
- **Claim.** `commons/pkg/cordonownership` stores `nvsentinel.nvidia.com/cordon-claim` on the Node: a unique ID, the Node UID, and an optional `preserveCordon` decision. A later fault inherits the claim only while a quarantine, a pending handoff, or an owned validation session keeps it active. A detached, malformed, preserved, or different-node claim cannot be inherited.
- **Request.** `CreateValidationRequest` in `fault-quarantine/pkg/validation/validation.go` writes `nvsentinel.nvidia.com/cordon-ownership` (`owned`, `pre-existing`, or `unknown`) and `nvsentinel.nvidia.com/cordon-claim-id` on the request, which also carries a Node owner reference. A retried creation verifies that an existing request of the same name describes the same handoff, and adopts a request that a fault-quarantine without ownership support created for the same node and tests; the fault-quarantine ClusterRole gains `get` on ValidationRequests for that check.
- **Session.** With `onlyOwned`, lifecycle-manager session entries carry the Node UID, the request UID, the claim ID, the ownership, and `preserveCordon`. `releaseOwnedNodeFromSession` in `lifecycle-manager/internal/controller/validationrequest_ownership.go` releases the cordon only when every entry proves ownership, the request's identities match the Node and the session entry, every requested test succeeded on the node, and no quarantine or pending handoff exists. A withheld release is logged with its reason. Session entries without object UIDs come from older controllers and never grant a release. Registration is decided on the live Node when the cached Node refuses it, and a refused request is requeued, because a request outside the node session receives no Node watch events.
- **Taints.** `onlyOwned` gates only the cordon. The configured taints are lifted when the session ends, exactly as the default policy lifts them.
- **Deletion.** Deleting a request removes its association and retires the claim when no association remains. It never releases the cordon under `onlyOwned`; lifecycle-manager logs the retained cordon.
- **Persisted handoff.** With `fault-quarantine.validation.persistHandoff: true`, fault-quarantine keeps `quarantineValidationHealthEvent` on the Node until lifecycle-manager registers the matching request, identified by the request's `nvsentinel.nvidia.com/validation-source-hash`, and consumes it in the same version-protected Node update that stores the session entry. Fault-quarantine builds the request and completes the handoff from the live Node, so informer lag cannot fail the handoff. An unconsumed source blocks the release of an older request.
- **Status before cleanup.** lifecycle-manager persists a successful test group before it deletes the provider resource, so a restart between the two steps cannot lose the result that the release depends on. A deletion that still fails is retried when the request completes, succeeded or failed, or is deleted.
- **Configuration.** `lifecycle-manager.config.schedulingGate.cordon.onlyOwned` and `fault-quarantine.validation.persistHandoff` both default to `false`. The [configuration reference](../configuration/validation.md#enabling-strict-cordon-ownership) lists the checks to run before enabling them and the upgrade order.

## Rationale

- Repair eligibility and permission to resume scheduling are separate decisions, and Kubernetes keeps no owner for a cordon. Only the controller that applied the cordon knows whose it is.
- A claim bound to the Node UID and the request UID survives controller restarts and the deletion or recreation of either object. A name alone does not prove that the original object still exists.
- Gating only the cordon keeps the taint behaviour operators already rely on, and leaves no NVSentinel taint behind on a node that an operator holds.
- Deciding ownership from the Node version being patched, with the UID and `resourceVersion` as preconditions, means a concurrent manual cordon or uncordon conflicts instead of being misread.

## Consequences

### Positive

- Maintenance cordons survive automatic GPU repair and validation.
- Repeated faults during validation still return the node to service automatically.
- The decision to preserve a cordon survives restarts and overlapping requests.

### Negative

- Requests without an ownership grant, such as new-node validation, manual requests, or requests from an older fault-quarantine, do not release the cordon under `onlyOwned`.
- Every ownership update reads the live Node before patching, one extra GET per quarantine action.
- Both controllers share persisted state on the Node, which requires an ordered upgrade and rollback.

### Mitigations

- Both settings are opt-in and default to the current behaviour.
- A withheld release and a deleted last request are logged with their reason, so an operator can review the node and uncordon it explicitly.
- Upgrade lifecycle-manager before fault-quarantine, enable both settings after legacy activity finishes, and drain active claims and handoffs before reverting either controller.

## Alternatives Considered

### Additional maintenance taint

**Rejected** because: it adds a second scheduling restriction that operators must remove separately, and it does not protect a plain `kubectl cordon`.

### Infer ownership from the cordon audit labels

**Rejected** because: the `cordon-by` labels record history rather than the current cordon, and lifecycle-manager already clears them on release.

### Promote ownership when lifecycle-manager registers a request

**Rejected** because: the previous request can disappear before registration, and the request's recorded ownership would then disagree with its session entry.

### Copy the current UID into an old session entry

**Rejected** because: a reused name does not prove that the original request or Node still exists.

## Notes

- Cordoning an already cordoned node changes no Kubernetes state. New maintenance intent during an active repair needs explicit coordination.
- With `onlyOwned`, new-node validation leaves the new-node scheduling gate in place. Releasing that gate under ownership rules is not covered by this record.

## References

- [Issue #1424](https://github.com/NVIDIA/NVSentinel/issues/1424) and [PR #1445](https://github.com/NVIDIA/NVSentinel/pull/1445): preserve pre-existing cordons in fault-quarantine
- [ADR-049: Node Validation](049-node-validation.md)
- [ADR-062: Release Quarantine Actions Only at Session End](062-fault-quarantine-release-at-session-end.md)
