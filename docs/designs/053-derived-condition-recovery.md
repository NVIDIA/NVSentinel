# ADR-053: Health Events Analyzer — Derived-Condition Recovery

## Context

The health-events-analyzer turns a history of source health events into synthetic
derived conditions. A derived unhealthy event uses the rule name as its
`checkName` and can flow through the same quarantine, drain, and remediation
pipeline as an event emitted directly by a health monitor.

Before this decision, those derived conditions had no automatic inverse
transition. A source health monitor could later report that the underlying
condition was healthy, but that event named the source check rather than the
derived rule. The analyzer therefore had no explicit basis for deciding which
derived condition, node, or device identity the healthy event should clear.
Operators had to clear the condition manually or keep analyzer output in
`STORE_ONLY` mode to avoid a permanently latched node condition.

Automatic recovery must account for several constraints:

- A rule may represent either one node-wide condition or separate conditions
  for entities such as GPUs.
- Change-stream events can be replayed after restart, and provider ordering is
  not a sufficient idempotency boundary by itself.
- Delayed records from before a recovery must not immediately recreate the
  recovered condition.
- MongoDB and PostgreSQL must implement the same filter and state semantics.
- Analyzer-produced events must never be consumed as new analyzer inputs.
- A transient store or publish failure must remain replayable, while one
  malformed record must not permanently block the shared stream.

## Decision

Add opt-in, rule-specific recovery mappings to the health-events-analyzer. A
mapping explicitly identifies a trusted healthy source event and selects either
node or entity scope. The analyzer publishes a derived healthy transition only
when persisted state shows that the same rule and recovery identity are
currently unhealthy.

Recovery is not inferred from an aggregation pipeline. Rules without a recovery
mapping retain manual-recovery behavior.

## Implementation

### Recovery configuration

An enabled rule may define a `[rules.recovery]` block with:

- `source_check_name`, which is required;
- optional `source_agent` and `source_error_codes` constraints;
- `scope = "node"` or `scope = "entity"`; and
- `entity_types` for entity scope.

Configuration validation rejects empty or duplicate values, entity types on a
node-scoped mapping, an entity-scoped mapping without entity types, and
`source_agent = "health-events-analyzer"`. The analyzer's own output is excluded
from input, so accepting it as a recovery source would create an unreachable and
misleading configuration.

### Recovery identity and transition

The identity of a derived condition is the rule name, node name, and, for entity
scope, the configured set of entity type/value pairs. Entity-scoped source
events must provide exactly one value for every configured entity type. A
matching source event without entity values acts as a node-wide recovery and
clears each active entity identity for that rule and node. A partially specified
entity identity is rejected rather than broadened.

For every resolved identity, the analyzer reads the latest persisted derived
state. It publishes a transition only when that state is unhealthy. The derived
healthy event:

- uses the rule name as `checkName`;
- sets `isHealthy=true`, `isFatal=false`, and `recommendedAction=NONE`;
- preserves the rule's processing strategy; and
- carries the same configured entity identity as the derived fault.

This state check makes replay converge without repeatedly publishing clears for
an identity that is already healthy. The analyzer leaves uncordon and other
downstream policy decisions to fault-quarantine.

### Ordering, persistence, and replay

For recovery-enabled rules, the analyzer normally advances a source event's
resume token only after the corresponding derived transition is visible in the
event store. This applies to both the unhealthy transition and its later healthy
transition, preventing a recovery from overtaking an unpersisted derived fault.

The persisted recovery source becomes the rule's history boundary. Later rule
evaluation excludes records stored or generated at or before that boundary, so
delayed pre-recovery history cannot immediately recreate the condition.

Transient datastore and publisher errors leave the source unacknowledged and
stop processing so the change stream can replay it. Confirmation is bounded by
a two-minute deadline; expiration exits processing without acknowledging the
source rather than blocking the stream forever. Deterministic configuration or
stored-record failures are logged, counted, and checkpointed after applying the
narrowest safe recovery holdback, so poison data does not halt unrelated rules
and identities.

### Input and datastore behavior

When any enabled rule has a recovery mapping, the shared analyzer watcher also
admits processable healthy source events. Healthy events are considered only by
recovery mappings and are not evaluated as ordinary failure inputs. Both the
watcher filter and every rule pipeline exclude events whose agent is
`health-events-analyzer`, preventing feedback loops.

MongoDB and PostgreSQL implement equivalent recovery filters and deterministic
query-error handling. Provider-specific lookup indexes support state and history
queries. PostgreSQL adds its upgrade index through a non-fatal background task:
startup does not wait for a concurrent index build, shutdown cancels and joins
the task, and a missing or invalid index is retried on a later startup.

## Rationale

- **Explicit semantics:** A rule author, rather than a heuristic, defines which
  healthy signal is authoritative for a derived condition.
- **Scoped safety:** Node and entity identities prevent one device recovery from
  clearing unrelated active faults.
- **Replay convergence:** Persisted state and history boundaries make duplicate
  and delayed events idempotent across restarts.
- **Provider parity:** The decision is expressed in datastore-independent rule
  and identity semantics, with provider-specific query implementations.
- **Pipeline separation:** The analyzer publishes normal health events and does
  not directly mutate Kubernetes node status or remediation state.

## Consequences

### Positive

- Derived conditions can clear automatically when an explicitly trusted source
  reports recovery.
- Existing rules remain unchanged unless they opt into recovery.
- Entity-scoped faults recover independently while node-wide recovery remains
  available when a source cannot identify individual entities.
- Replay, restart, and delayed-history behavior is defined and testable.
- Recovery events continue through the standard storage, quarantine, and
  remediation pipeline instead of introducing a second mutation path.

### Negative

- Rule configuration becomes more complex and an incorrect source mapping can
  prevent a legitimate recovery from matching.
- Enabling recovery for one rule widens the process-wide watcher input for all
  rules, increasing read and dispatch work.
- State confirmation and boundary queries add datastore load and make recovery
  eventually consistent rather than instantaneous.
- Deterministically malformed stored records may conservatively withhold a
  boundary or recovery identity until operators repair or remove the record.
- Persistence of the derived healthy event does not itself guarantee that every
  downstream side effect succeeded. In particular, a terminal Kubernetes API
  write failure can still leave a node condition latched until the connector
  retry work in issue #1743 is deployed.

### Mitigations

- Validate mappings strictly at startup and keep recovery opt-in per rule.
- Use identity-scoped holdbacks so one malformed entity does not block unrelated
  recoveries when its identity can be determined safely.
- Bound persistence confirmation, preserve unacknowledged sources for replay,
  and expose recovery outcome and failure metrics.
- Maintain provider-specific lookup indexes without making performance-index
  creation a startup requirement.
- Address downstream Kubernetes write reliability independently in #1743 so
  the recovery feature and connector bug can be reviewed and released
  separately.

## Alternatives Considered

### Keep all derived conditions on manual recovery

**Rejected** because: it leaves operators responsible for clearing analyzer
conditions and makes `EXECUTE_REMEDIATION` unsafe for conditions that can later
become healthy. It also preserves the fleet workaround of using `STORE_ONLY`
for otherwise actionable derived events.

### Infer recovery automatically from the rule pipeline

**Rejected** because: aggregation pipelines describe how to detect a historical
failure, not which future source is authoritative for clearing it. Inverting an
arbitrary pipeline is ambiguous, especially for count, time-window, and
multi-source rules.

### Have the analyzer patch Kubernetes conditions directly

**Rejected** because: it would bypass storage, fault-quarantine policy, tracing,
and the existing platform-connector path. It would also couple analyzer rules to
Kubernetes and produce different behavior for non-Kubernetes deployments.

### Mutate or delete the stored derived fault

**Rejected** because: health events are an append-only history. Rewriting the
fault would erase audit context and would not produce the healthy transition
consumed by downstream components.

### Periodically scan and reconcile every derived condition

**Rejected** because: polling adds continuous datastore load and still requires
an explicit definition of the healthy source. Change-stream replay plus a
persisted boundary provides recovery without a second scheduler.

## Notes

- This ADR does not enable recovery for every existing analyzer rule. Each rule
  owner must select and validate an authoritative source mapping.
- This ADR does not change fault-quarantine uncordon policy or directly repair
  downstream connector delivery failures.
- The ordered Kubernetes retry fix for #1743 is intentionally maintained in a
  separate pull request from the recovery feature.

## References

- [Issue #1553: Health-events-analyzer recovery](https://github.com/NVIDIA/NVSentinel/issues/1553)
- [Issue #1743: Kubernetes connector drops failed writes](https://github.com/NVIDIA/NVSentinel/issues/1743)
- [ADR-006: Platform Connector Event Buffering](./006-platform-connector-reliability.md)
- [ADR-007: Health Event Correlation](./007-event-correlation-and-analysis.md)
- [ADR-025: Processing Strategy for Health Checks](./025-processing-strategy-for-health-checks.md)
- [ADR-039: Health Event Deduplication](./039-health-event-deduplication.md)
- [Health-events-analyzer configuration](../configuration/health-events-analyzer.md#derived-condition-recovery)
