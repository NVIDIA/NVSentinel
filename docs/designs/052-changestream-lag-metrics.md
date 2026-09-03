# ADR-052: `Observability` — change stream consumer lag metrics

## Table of contents

- [Context](#context)
- [Decision](#decision)
- [Implementation](#implementation)
- [Rationale](#rationale)
- [Consequences](#consequences)
- [Alternatives Considered](#alternatives-considered)
- [Testing](#testing)
- [Rollout](#rollout)

## Context

A change stream consumer can fall arbitrarily far behind while presenting as completely
healthy. On a 288 node GB200 fleet, `health-events-analyzer` was **2.4 million events and
roughly 11 days behind** its change stream. Nothing surfaced it: the pod was `Running` with
zero restarts, CPU at 2m of a 1000m limit, no errors in the log. It was found only by manually
decoding a resume token out of MongoDB.

The consequence is not just staleness. A consumer 11 days behind is acting on an 11 day old
view of the fleet, so the "recurring fault patterns" it reported described faults that had
ended over a week earlier. With remediation enabled it would have been quarantining nodes for
resolved faults while ignoring current ones.

Requested in #1709, split out of #1704 because it applies to every change stream consumer
rather than to `health-events-analyzer` alone.

### The capability is half-present already

This is not a missing capability so much as one that exactly one consumer wired up.

`ChangeStreamMetrics.GetUnprocessedEventCount` is defined at
`store-client/pkg/client/interfaces.go:104-106` and implemented for both providers:

| Provider | Implementation | Position representation |
| --- | --- | --- |
| MongoDB | `store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:561` | opaque resume token (`bson.Raw`) |
| PostgreSQL | `store-client/pkg/datastore/providers/postgresql/changestream.go:1390` | monotonic `datastore_changelog.id` (int64) |

Current adoption:

| Consumer | Stream backlog metric | Notes |
| --- | --- | --- |
| `fault-quarantine` | **yes, works** | ticker at `fault-quarantine/pkg/eventwatcher/event_watcher.go:787-798` sets `fault_quarantine_event_backlog_count`; sets `-1` when the watcher lacks the interface |
| `event-exporter` | **declared, never set** | `health_events_exporter_event_backlog_size` is created at `event-exporter/pkg/metrics/metrics.go:73` and assigned nowhere in the module, so it reports `0` forever |
| `health-events-analyzer` | none | no reference to `ChangeStreamMetrics` anywhere in the module. This is the consumer that went 2.4M events behind |
| `fault-remediation` | none | |
| `janitor` | none | |
| `lifecycle-manager` | none | |
| `labeler` | none | |

`node_drainer_queue_depth` exists but measures node-drainer's own in-process work queue, not
stream position, so it does not cover this.

### Three candidate signals, and why the obvious one is wrong

Naming them explicitly, because the distinction drives the decision:

1. **`events_behind`** — count of events after the consumer's position. Correct, but needs a
   `COUNT(*)` per poll against a collection that is ~7 GB on our fleet.
2. **position age** — `now - timestamp(position)`. Nearly free, but **wrong on a quiet
   stream**: a fully caught-up consumer whose last event arrived an hour ago reports an hour of
   "lag" when it is not behind at all. This is the signal that looks obvious and should not be
   used alone.
3. **`lag_seconds`** — `timestamp(head) - timestamp(position)`. Correct and cheap: it compares
   the consumer against the stream rather than against the wall clock, so an idle stream reports
   zero. Needs the timestamp of the newest event, which is one indexed lookup rather than a
   scan.

Signal 2 is what "resume token age" naively means, and it is why this ADR does not simply
export token age.

## Decision

Emit lag metrics from the **shared `store-client` watcher layer**, labelled by the existing
client name, so every consumer gets them without per-module wiring. Export **two** signals:

- `changestream_lag_seconds{client}` — head time minus position time. The primary signal.
- `changestream_events_behind{client}` — count after position. Secondary, and rate-limited.

Do **not** export raw position age as a lag metric, for the reason above.

Fix `event-exporter`'s permanently-zero gauge as part of this, and leave
`fault_quarantine_event_backlog_count` in place so existing dashboards keep working.

## Implementation

### Where it lives

A single poller in the shared watcher, started from `Start(ctx)`, rather than the current
pattern of each consumer running its own ticker. `fault-quarantine`'s loop
(`fault-quarantine/pkg/eventwatcher/event_watcher.go:787-798`) becomes redundant and can be deleted once the
shared metric is in place, or kept temporarily for dashboard continuity.

```
store-client/pkg/client/
  lagreporter.go          # provider-agnostic poller, owns the metrics
  interfaces.go           # ChangeStreamMetrics gains StreamPosition
store-client/pkg/datastore/providers/mongodb/watcher/
  watch_store.go          # implements StreamPosition
store-client/pkg/datastore/providers/postgresql/
  changestream.go         # implements StreamPosition
```

### Interface extension

`GetUnprocessedEventCount` already covers signal 2. Signal 1 needs the two timestamps, which
only the provider can produce:

```go
// StreamPosition reports where the consumer is and where the stream head is, so lag can be
// computed without the caller knowing how a position is represented.
type StreamPosition struct {
    // PositionTime is the timestamp of the last event the consumer marked processed.
    PositionTime time.Time
    // HeadTime is the timestamp of the newest event in the stream.
    HeadTime time.Time
    // HasPosition is false before the consumer has ever marked an event processed, in which
    // case both timestamps are meaningless and no lag is reported.
    HasPosition bool
}

// ChangeStreamMetrics gains:
StreamPosition(ctx context.Context) (StreamPosition, error)
```

Lag is then `HeadTime.Sub(PositionTime)`, clamped at zero, computed once in the shared layer.

### Per-provider derivation

**PostgreSQL** is straightforward, because the changelog already carries a timestamp
(`datastore_changelog.changed_at TIMESTAMPTZ`, `store-client/pkg/datastore/providers/postgresql/datastore.go:382`) and the
position is a row id:

```sql
SELECT
  (SELECT changed_at FROM datastore_changelog WHERE id = $1)              AS position_time,
  (SELECT MAX(changed_at) FROM datastore_changelog WHERE table_name = $2) AS head_time;
```

**MongoDB** needs a position timestamp that the token does not readily give up. Two options,
and the choice matters:

- **Preferred: record the position time when the token is saved.** `MarkProcessed`
  (`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:491`) already holds the change event, whose `clusterTime` is the position
  time. Adding a `positionTime` field to the `ResumeTokens` document makes the timestamp a
  first-class stored value rather than something inferred. Head time is
  `db.<collection>.find().sort({_id: -1}).limit(1)`, which uses the existing `_id` index.
- **Rejected: decode the resume token's `_data`.** A MongoDB resume token's `_data` hex encodes
  the cluster time, and decoding it is how the original 11 day lag was actually measured
  operationally. But that encoding is a **driver implementation detail with no public API**, so
  depending on it in shipped code would break silently on a driver or server change.

The stored-timestamp approach also degrades gracefully: a consumer that has not yet saved a
token reports `HasPosition: false` and no lag series, rather than a misleading zero.

### Cost control

`events_behind` is the expensive one. It is:

- **polled on a separate, longer interval** from `lag_seconds`, configurable, defaulting to
  something like 60s for lag and 300s for the count;
- **skippable entirely** by configuration, for operators who want the cheap signal only;
- never run more than once per interval per consumer, regardless of how many events flow.

`lag_seconds` is two indexed lookups and is safe at the shorter interval.

### Metrics

```
changestream_lag_seconds{client}        gauge   head time minus position time, 0 when caught up
changestream_events_behind{client}      gauge   events after position, -1 when not collected
changestream_position_known{client}     gauge   1 once the consumer has saved a position, else 0
```

`client` is the existing `TokenConfig.ClientName` / `fieldClientName`, so the label space is
the set of consumers and nothing more.

The `-1` convention for `events_behind` follows the precedent `fault-quarantine` already set
for "interface unavailable", which is better than a missing series or a misleading zero.

### The event-exporter bug

`health_events_exporter_event_backlog_size` is registered and never written, so it currently
advertises "no backlog" permanently. That is worse than having no metric, and it is
independent of the rest of this work. It should either be wired to the shared metric or
removed. Recommend removing it in favour of `changestream_events_behind{client="event-exporter"}`
rather than maintaining two names for one thing.

## Rationale

- **One implementation, every consumer.** Seven consumers currently have no lag visibility;
  wiring each one separately is seven chances to omit the eighth.
- **The signal is chosen to be correct rather than convenient.** Comparing the consumer to the
  stream head, rather than to the wall clock, is what makes an idle stream report zero.
- **The expensive query is opt-out, the cheap one is always on.** The failure mode this catches
  is severe enough to warrant a default-on metric, but not at the cost of a `COUNT(*)` over
  millions of documents every few seconds.
- **It reuses what exists.** `GetUnprocessedEventCount` is already implemented on both
  providers; only the timestamp pair is new.

## Consequences

### Positive

- A consumer falling behind becomes alertable, in one dashboard, for every module.
- The failure that took manual token decoding to find becomes a single PromQL query.
- `event-exporter`'s misleading gauge is corrected.

### Negative

- Adds a field to the MongoDB `ResumeTokens` document, so a consumer running new code against
  tokens written by old code has no `positionTime` until it next saves one.
- Adds a periodic query per consumer, small but not free.
- `ChangeStreamMetrics` grows a method, so any out-of-tree implementation must add it.

### Mitigations

- **Missing `positionTime` is handled explicitly**: `HasPosition: false`, no lag series, and
  `changestream_position_known` goes to 1 as soon as the first token is saved under new code.
  No migration step, no backfill.
- Both intervals are configurable and the count is skippable, so cost is bounded by
  configuration rather than by traffic.
- `ChangeStreamMetrics` is already an optional interface, checked with a type assertion at
  `store-client/pkg/client/resume_token.go:79` and `fault-quarantine/pkg/eventwatcher/event_watcher.go:787`, so an implementation lacking the new method
  degrades to "no lag reported" rather than failing to compile.

## Alternatives Considered

### Export raw resume token age (`now - position time`)

**Rejected** because: it reports lag on a caught-up but idle consumer. A quiet cluster would
alert continuously while nothing is wrong, which trains operators to ignore the metric. It is
also the interpretation most likely to be reached for, which is why it is called out here
explicitly rather than left implicit.

### Decode the MongoDB resume token's `_data` field

**Rejected** because: the encoding is an undocumented driver/server implementation detail with
no public API. It works today, and it is how the original incident was diagnosed by hand, but
depending on it in shipped code would break silently on an upgrade. Storing the timestamp at
save time gets the same number with a supported mechanism.

### Wire the existing `GetUnprocessedEventCount` into each consumer separately

**Rejected** because: it is the status quo extended, and the status quo is why seven consumers
have no metric. It also leaves each module free to pick a different metric name, which is how
`fault_quarantine_event_backlog_count` and `health_events_exporter_event_backlog_size` came to
describe the same quantity under two names.

### Count-only, no time-based signal

**Rejected** because: a count is meaningless without knowing the event rate. "50,000 behind" is
an emergency on a quiet cluster and thirty seconds of traffic on a busy one. Seconds of lag is
comparable across clusters and directly alertable.

### Derive lag in each consumer from event timestamps as they arrive

**Rejected** because: it only updates when events arrive, so a fully stalled consumer, which is
the worst case, stops updating its own lag metric and looks frozen rather than behind.

## Testing

- **Unit, per provider**: `StreamPosition` against a seeded changelog / collection, covering
  caught-up (lag 0), behind by a known interval, and no-position-yet.
- **Unit, shared layer**: the poller reports lag from a fake `ChangeStreamMetrics`, clamps
  negative lag to zero, honours the skip-count configuration, and emits `-1` for an uncollected
  count.
- **Interface degradation**: a watcher that does not implement `StreamPosition` produces no lag
  series and no error.
- **Integration**: with `envtest` plus a real MongoDB, stop a consumer, insert events, restart
  it, and assert `changestream_lag_seconds` rises and then returns to zero as it drains. This
  reproduces the #1704 shape directly.
- **Idle-stream regression**: no events for longer than the poll interval, consumer caught up,
  assert lag stays at zero. This is the test that would fail under the rejected position-age
  approach.

## Rollout

1. Extend `ChangeStreamMetrics` and implement `StreamPosition` for both providers, plus the
   shared poller, default on for `lag_seconds` and off for `events_behind`.
2. Delete `event-exporter`'s dead gauge.
3. Leave `fault_quarantine_event_backlog_count` and its ticker in place for one release so
   dashboards can move, then remove the ticker in favour of the shared metric.
4. Document in `docs/METRICS.md`, including the idle-stream caveat and a suggested alert
   (`changestream_lag_seconds > 900` for 10m, tuned per fleet).
