<!--
Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# ADR-059: Health Events Analyzer — Derived-Condition Recovery

## Context

The health-events-analyzer turns a history of source health events into synthetic derived conditions. A derived unhealthy event uses the rule name as its `checkName` and can flow through the same quarantine, drain, and remediation pipeline as an event emitted directly by a health monitor.

Before this decision, those derived conditions had no automatic inverse transition. A source health monitor could later report that the underlying condition was healthy, but that event named the source check rather than the derived rule. The analyzer therefore had no explicit basis for deciding which derived condition, node, or device identity the healthy event should clear. Operators had to clear the condition manually or keep analyzer output in `STORE_ONLY` mode to avoid a permanently latched node condition.

Automatic recovery must account for several constraints:

- A rule may represent either one node-wide condition or separate conditions for entities such as GPUs.
- Change-stream events can be replayed after restart, and provider ordering is not a sufficient idempotency boundary by itself.
- Delayed records from before a recovery must not immediately recreate the recovered condition.
- MongoDB and PostgreSQL must implement the same filter and state semantics.
- Analyzer-produced events must never be consumed as new analyzer inputs.
- A transient store or publish failure must remain replayable, while one malformed record must not permanently block the shared stream.

## Decision

Add opt-in, rule-specific recovery triggers to the health-events-analyzer. A rule selects a node annotation for operator recovery or a trusted healthy source event for automatic recovery, plus node or entity scope. The analyzer publishes a derived healthy transition only when persisted state shows that the same rule and recovery identity are currently unhealthy.

Recovery is not inferred from an aggregation pipeline. Rules without a recovery mapping retain manual-recovery behavior.

## Implementation

### Recovery configuration

An enabled rule may define a `[rules.recovery]` block with:

- either `annotation_key` for operator requests or `source_check_name` for source-event recovery;
- optional `source_agent` and `source_error_codes` constraints in source-event mode;
- `scope = "node"` or `scope = "entity"`; and
- `entity_types` for entity scope.

Configuration validation rejects empty or duplicate values, entity types on a node-scoped mapping, an entity-scoped mapping without entity types, and `source_agent = "health-events-analyzer"`. The analyzer's own output is excluded from input, so accepting it as a recovery source would create an unreachable and misleading configuration.

### How an operator requests recovery

For Kubernetes deployments, configure `annotation_key` on the affected rule's recovery block, alongside its node or entity scope. This mode does not require `source_check_name`, a monitor, or an operator-provided publisher. It is mutually exclusive with source-event matching on the same rule.

After repairing and verifying the node, the operator sets the configured node annotation to the verification time in RFC 3339 format. The analyzer watches nodes with an informer, reads the active derived state, and directly publishes the corresponding derived healthy event through its existing platform-connector connection. No intermediate source health event is published.

A timestamp value requests recovery for the rule across the node. For an entity-scoped rule, a JSON value with `recoveredAt` and `entities` can select one configured entity identity. Missing, partial, duplicate, or unknown entity fields are rejected rather than broadened. A node-wide request requires verification of every affected device.

The request clears only active derived faults older than its verification time. Future timestamps and requests predating the node's creation are rejected. The analyzer preserves the request time, annotation key, and node UID in the derived healthy event. This stored event supplies the history boundary after restart, even after the request annotation is gone.

Annotation reconciliation and source-event evaluation share a per-node lock. Before evaluating a source event, the analyzer checks pending requests from the synchronized node cache. This prevents fault publication from racing recovery on the same node while retaining concurrency across nodes.

The analyzer removes the annotation only after all applicable transitions are stored. A conditional patch tests the node UID and original annotation value, so cleanup cannot erase a replacement request. Transient failures retain the request for retry. Invalid requests remain visible for the operator to correct. Requests without an active older fault are consumed without publishing a new healthy condition.

This uses existing Kubernetes authorization and audit logging for node annotation writes. Enable the Helm value `nodeRecovery.enabled` when configuring annotation rules to grant the analyzer node get/list/watch/patch permissions. Downstream components retain their normal recovery and uncordon policies; annotation removal confirms storage, not completion of every downstream side effect.

Existing healthy-source mappings remain available for automatic monitor-driven recovery. They are an alternative trigger, not a required step in the operator annotation workflow. See the [operator procedure](../configuration/health-events-analyzer.md#request-recovery-after-verification) for configuration and commands.

### Recovery identity and transition

The identity of a derived condition is the rule name, node name, and, for entity scope, the configured set of entity type/value pairs. Entity-scoped source events must provide exactly one value for every configured entity type. A matching source event without entity values acts as a node-wide recovery and clears each active entity identity for that rule and node. A partially specified entity identity is rejected rather than broadened.

For every resolved identity, the analyzer reads the latest persisted derived state. It publishes a transition only when that state is unhealthy. The derived healthy event:

- uses the rule name as `checkName`;
- sets `isHealthy=true`, `isFatal=false`, and `recommendedAction=NONE`;
- preserves the rule's processing strategy; and
- carries the same configured entity identity as the derived fault.

This state check makes replay converge without repeatedly publishing clears for an identity that is already healthy. The analyzer leaves uncordon and other downstream policy decisions to fault-quarantine.

### Ordering, persistence, and replay

For recovery-enabled rules, the analyzer normally advances a source event's resume token only after the corresponding derived transition is visible in the event store. This applies to both the unhealthy transition and its later healthy transition, preventing a recovery from overtaking an unpersisted derived fault.

The persisted source event or annotation-triggered derived healthy event supplies the rule's history boundary. Later rule evaluation excludes records stored or generated at or before that boundary, so delayed pre-recovery history cannot immediately recreate the condition.

When recovery is enabled, transient datastore and publisher errors leave the source unacknowledged and stop the shared processor for replay. This applies to all inputs on that processor. Without enabled recovery mappings, handler errors retain the default checkpoint-and-continue behavior. With one worker, checkpoint failures stop processing. With multiple workers, the processor retains the last safe checkpoint and retries it on later completions and shutdown. It never advances the checkpoint past an unresolved event. Confirmation is bounded by a two-minute deadline; expiration exits processing without acknowledging the source rather than blocking the stream forever. Deterministic configuration or stored-record failures are logged, counted, and checkpointed after applying the narrowest safe recovery holdback, so poison data does not halt unrelated rules and identities.

### Input and datastore behavior

When any enabled rule has a source-event recovery mapping, the shared analyzer watcher also admits processable healthy source events. Healthy events are considered only by recovery mappings and are not evaluated as ordinary failure inputs. Both the watcher filter and every rule pipeline exclude events whose agent is `health-events-analyzer`, preventing feedback loops.

MongoDB and PostgreSQL implement equivalent recovery filters and deterministic query-error handling. Provider-specific lookup indexes support state and history queries. PostgreSQL adds its upgrade index through a non-fatal background task: startup does not wait for a concurrent index build, shutdown cancels and joins the task, and a missing or invalid index is retried on a later startup.

## Rationale

- **Explicit semantics:** A rule author, rather than a heuristic, defines which healthy signal is authoritative for a derived condition.
- **Scoped safety:** Node and entity identities prevent one device recovery from clearing unrelated active faults.
- **Replay convergence:** Persisted state and history boundaries make duplicate and delayed events idempotent across restarts.
- **Provider parity:** The decision is expressed in datastore-independent rule and identity semantics, with provider-specific query implementations.
- **Pipeline separation:** The analyzer publishes normal health events and does not directly mutate Kubernetes node status or remediation state.

## Consequences

### Positive

- Derived conditions can clear automatically when an explicitly trusted source reports recovery.
- Existing rules remain unchanged unless they opt into recovery.
- Entity-scoped faults recover independently while node-wide recovery remains available when a source cannot identify individual entities.
- Replay, restart, and delayed-history behavior is defined and testable.
- Recovery events continue through the standard storage, quarantine, and remediation pipeline instead of introducing a second mutation path.

### Negative

- Rule configuration becomes more complex and an incorrect source mapping can prevent a legitimate recovery from matching.
- Enabling source-event recovery for one rule widens the process-wide watcher input for all rules, increasing read and dispatch work. A transient processing failure then stops the shared stream until restart, preserving recovery ordering.
- State confirmation and boundary queries add datastore load and make recovery eventually consistent rather than instantaneous.
- Deterministically malformed stored records may conservatively withhold a boundary or recovery identity until operators repair or remove the record.
- Persistence of the derived healthy event does not itself guarantee that every downstream side effect succeeded. In particular, a terminal Kubernetes API write failure can still leave a node condition latched if downstream delivery cannot complete. The ordered retry fix in PR #1747 is now part of `main`, but successful storage alone is not proof of a Kubernetes update.

### Mitigations

- Validate mappings strictly at startup and keep recovery opt-in per rule.
- Use identity-scoped holdbacks so one malformed entity does not block unrelated recoveries when its identity can be determined safely.
- Bound persistence confirmation, preserve unacknowledged sources for replay, and expose recovery outcome and failure metrics.
- Maintain provider-specific lookup indexes without making performance-index creation a startup requirement.
- Use the ordered Kubernetes write retries from PR #1747 and check downstream condition updates separately from storage confirmation.

## Alternatives Considered

### Require operators to publish a source health event

**Not selected for operator recovery:** maintainers requested a node label or annotation contract to avoid custom API publishers. An annotation carries the verification timestamp and optional entity identity without Kubernetes label length restrictions. Monitor-driven source events remain an optional automatic trigger.

### Have another monitor publish the derived healthy event directly

A monitor can publish a healthy event with `agent="health-events-analyzer"` to clear a downstream condition. It must also match the derived rule's `checkName`, node, entity scope, and processing strategy. The analyzer is not required just to deliver that clear event.

This proposal keeps recovery state in the analyzer because clearing the downstream condition does not reset the rule's historical input. Analyzer-produced events are excluded from rule input, including a healthy event published under the analyzer's identity. That event alone cannot establish the source recovery boundary. For example, after three XID events trigger a derived fault, a direct clear leaves those three events in the aggregation window. A later evaluation can match that old history and publish the fault again.

With a configured recovery mapping, the analyzer records the recovery boundary and excludes that history from later evaluation. It also checks persisted state, maps the source to affected rules and entities, rejects stale recovery, and confirms storage before acknowledging the source.

**Not selected for this proposal:** direct publishing is a valid simpler option when only a downstream clear is required. Equivalent automatic recovery would also need a way to reset rule history and coordinate replay. Keeping those responsibilities with the rules avoids duplicating their semantics in each monitor.

### Keep all derived conditions on manual recovery

**Rejected** because: it leaves operators responsible for clearing analyzer conditions and makes `EXECUTE_REMEDIATION` unsafe for conditions that can later become healthy. It also preserves the fleet workaround of using `STORE_ONLY` for otherwise actionable derived events.

### Infer recovery automatically from the rule pipeline

**Rejected** because: aggregation pipelines describe how to detect a historical failure, not which future source is authoritative for clearing it. Inverting an arbitrary pipeline is ambiguous, especially for count, time-window, and multi-source rules.

### Have the analyzer patch Kubernetes conditions directly

**Rejected** because: it would bypass storage, fault-quarantine policy, tracing, and the existing platform-connector path. It would also couple analyzer rules to Kubernetes and produce different behavior for non-Kubernetes deployments.

### Mutate or delete the stored derived fault

**Rejected** because: health events are an append-only history. Rewriting the fault would erase audit context and would not produce the healthy transition consumed by downstream components.

### Periodically scan and reconcile every derived condition

**Rejected** because: polling adds continuous datastore load and still requires an explicit definition of the healthy source. Change-stream replay plus a persisted boundary provides recovery without a second scheduler.

## Notes

- This ADR does not enable recovery for every existing analyzer rule. Each rule owner must select an annotation contract or validate an authoritative source mapping.
- This ADR does not change fault-quarantine uncordon policy or directly repair downstream connector delivery failures.
- PR #1747 supplies the ordered Kubernetes retry fix for #1743 independently of this recovery feature.

## References

- [Issue #1553: Health-events-analyzer recovery](https://github.com/NVIDIA/NVSentinel/issues/1553)
- [Issue #1743: Kubernetes connector drops failed writes](https://github.com/NVIDIA/NVSentinel/issues/1743)
- [ADR-006: Platform Connector Event Buffering](./006-platform-connector-reliability.md)
- [ADR-007: Health Event Correlation](./007-event-correlation-and-analysis.md)
- [ADR-025: Processing Strategy for Health Checks](./025-processing-strategy-for-health-checks.md)
- [ADR-039: Health Event Deduplication](./039-health-event-deduplication.md)
- [Health-events-analyzer configuration](../configuration/health-events-analyzer.md#derived-condition-recovery)
