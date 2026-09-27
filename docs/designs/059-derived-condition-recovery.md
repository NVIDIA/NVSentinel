# ADR-059: Health Events Analyzer — Operator Recovery of Derived Conditions

Status: Proposed

## Context

The analyzer publishes derived faults when a rule matches accumulated events. Repairing the hardware does not automatically clear that derived condition. An operator needs a supported way to request recovery without constructing a synthetic health event or editing the event store. Old events also remain in the rule's lookback window and can immediately recreate a condition after it is cleared.

Not every monitor emits healthy transitions. In the [fleet observations on PR #1706](https://github.com/NVIDIA/NVSentinel/pull/1706#issuecomment-5834070368), XID log events had no healthy counterpart, while NVLink monitoring did emit healthy events. The existence of a Prometheus series does not establish that its healthy counter increases. An initial implementation must work for log-derived rules without assuming a healthy source signal.

## Decision

Provide opt-in recovery of configured rules through Kubernetes Node annotations, with MongoDB as the supported store. An operator or their automation verifies repair and writes the verification timestamp to the rule's annotation. The analyzer publishes a matching healthy derived event through the existing platform-connector interface and reports the result as a Kubernetes Event. It retains the annotation.

Healthy-source recovery, PostgreSQL rule support, and changes to shared event-processing/checkpoint behavior are outside this change. Rules without a recovery mapping keep their existing behavior.

## Implementation

Each enabled rule may define a unique annotation key and its identity scope:

```toml
[rules.recovery]
annotation_key = "recovery.nvsentinel.nvidia.com/repeated-xid"
scope = "entity"
entity_types = ["GPU_UUID"]
```

The chart's `health-events-analyzer.nodeRecovery.enabled: true` grants the analyzer `get`, `list`, and `watch` on Nodes, plus `create` on Events in the `default` namespace. It never patches Nodes. Node Events use `default` because their object reference is cluster-scoped. The informer performs an initial list and watches annotation changes; it has a five-minute startup sync deadline, with no global HTTP timeout closing watches. Individual Event writes have a 30-second deadline.

The annotation value is either an RFC3339 verification timestamp or JSON containing `recoveredAt` and `entities`. After verifying the whole node, an operator can request recovery for every active identity belonging to this rule:

```sh
verified_at=$(date -u +%Y-%m-%dT%H:%M:%SZ)
kubectl annotate node worker-1 recovery.nvsentinel.nvidia.com/repeated-xid="$verified_at" --overwrite
```

For a single GPU, the value can be:

```json
{"recoveredAt":"2026-09-27T14:30:00Z","entities":[{"entityType":"GPU_UUID","entityValue":"GPU-123"}]}
```

The operator supplies the time when verification finished, not the time the analyzer processes the request. It must be after the active derived fault and within the current Node's lifetime, and cannot be in the future. A retained request cannot clear faults generated after that time. JSON entity selectors must contain exactly the configured entity types; a timestamp-only value applies to all identities for the rule on that node. Node-scoped rules use `scope = "node"` without `entity_types`.

The annotation controller and source-event processing serialize work per node. Recovery runs on its own retry queue. It reads the latest derived state for each identity and publishes healthy events only for eligible active faults, preserving their agent/check identity, component class, version, and configured entities. Recovery events are nonfatal, use action `NONE`, and follow the rule's processing strategy. Normal fault publication follows the existing path without a database acknowledgment poll.

Direct-mode publication already acknowledges durable storage. Socket-mode recovery checks storage after queue acceptance before reporting success. Each reconciliation is bounded to two minutes. A permanent `ErrPublishRejected` ends the attempt immediately; that request is suppressed until its value changes or the analyzer restarts. Transient store or publication errors retry with backoff. A malformed stored record fails the request and produces a Warning Event. None of these paths changes the source processor's checkpoint-and-continue policy.

Published recovery metadata records the Node UID, annotation key, request digest, and verification time. Subsequent rule queries read this persisted boundary and exclude events generated or stored at or before verification. This survives analyzer restarts and prevents old history from re-firing a recovered condition. A recreated Node has a different UID and does not inherit the old boundary. The event store's normal retention policy also applies to recovery records; deployments must retain them at least as long as rule history can be queried.

The request annotation is retained after success or failure. The operator may replace it after verifying a later repair or remove it manually; removing it does not erase the persisted recovery boundary. `RecoveryCompleted` means the healthy event was stored. Platform-connectors and fault-quarantine subsequently clear conditions and release quarantine through their normal processing; unrelated active faults can keep the node cordoned. `RecoverySkipped` means there was no eligible active fault, `RecoveryInvalid` describes an invalid request, and `RecoveryFailed` points to the analyzer logs. Kubernetes Events expire normally and are not a permanent audit ledger.

## Rationale

- An explicit verification request works for both log-derived faults and monitors that emit healthy transitions.
- Existing event publication preserves the separation between detection, ingestion, and fault management.
- Read-only Node access avoids granting a detection component permission to alter recovery requests or other Node state.
- Persisted verification boundaries make recovery safe across restarts without shared-store processing changes.

## Consequences

### Positive

- Operators have a concrete recovery command and visible outcome.
- Entity selection allows one repaired GPU to recover without clearing another GPU's fault.
- Default configurations and rules without recovery mappings are unchanged.

### Negative

- Operators remain responsible for deciding whether repair is verified.
- Socket mode needs a bounded storage check, and enabled rules add queries for recovery history.
- Annotation recovery is not supported with PostgreSQL in this version.

### Mitigations

- Reject malformed, future, and pre-creation timestamps; never clear a newer fault with an old request.
- Use the analyzer lookup index and scope each query to a node, rule, and derived-event agent.
- Cover recovery, retained requests, invalid data, permanent rejection, restart boundaries, and node recreation in local tests; run the complete fault-to-uncordon flow in the MongoDB E2E matrix.

## Alternatives Considered

### Recover from healthy source events

Deferred because source semantics differ by monitor and some log-derived faults have no healthy transition. A follow-up would need explicit mappings and evidence that the mapped source actually emits healthy events.

### Delete the annotation on success

Rejected because it requires Node write access and removes the operator's verification record. A namespaced Event communicates the outcome instead.

### Extend PostgreSQL and shared event processing in the same PR

Deferred to keep the first version reviewable and avoid coupling recovery to untested rule translation or changed checkpoint behavior. PostgreSQL analyzer support is tracked in [#606](https://github.com/NVIDIA/NVSentinel/issues/606).

## Notes

| Rule family | Recovery in this version |
|---|---|
| Repeated XID and GPC/TPC rules | Annotation after explicit hardware verification; absence of new XID logs is not a healthy transition. |
| Multiple remediations and cross-GPU rules | Node-scoped annotation after whole-node verification. |
| NIC and other configured aggregation rules | Annotation after verification appropriate to the rule; select the exact identity used by its aggregation. |
| Built-in Go XID burst detector | Outside the configurable-rule recovery mapping. |

## References

- [Operator recovery guide](../health-events-analyzer-recovery.md)
- [Health Events Analyzer configuration](../configuration/health-events-analyzer.md)
- [Concurrent node-partitioned event processing](056-concurrent-node-partitioned-event-processing.md)
