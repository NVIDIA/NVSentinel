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
| `fault-quarantine` | **wired, but broken on MongoDB** | ticker at `fault-quarantine/pkg/eventwatcher/event_watcher.go:787-798` sets `fault_quarantine_event_backlog_count`. Works on PostgreSQL; reports `0` forever on MongoDB. See below |
| `event-exporter` | **declared, never set** | `health_events_exporter_event_backlog_size` is created at `event-exporter/pkg/metrics/metrics.go:73` and assigned nowhere in the module, so it reports `0` forever |
| `health-events-analyzer` | none | no reference to `ChangeStreamMetrics` anywhere in the module. This is the consumer that went 2.4M events behind |
| `fault-remediation` | none | |
| `janitor` | none | |
| `lifecycle-manager` | none | |
| `labeler` | none | |

`node_drainer_queue_depth` exists but measures node-drainer's own in-process work queue, not
stream position, so it does not cover this.

### The one working metric does not work on MongoDB

Worth stating separately, because it changes the premise: on MongoDB **no consumer has stream
backlog visibility at all**, including the one that appears to.

The MongoDB watcher's method is
`GetUnprocessedEventCount(ctx, lastProcessedID bson.ObjectID, additionalFilters ...bson.M)`
(`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:561-562`), while the
interface requires `(ctx, lastProcessedID string)`
(`store-client/pkg/client/interfaces.go:104-106`). The signatures differ, so the MongoDB
watcher does not satisfy `ChangeStreamMetrics`. PostgreSQL's does
(`store-client/pkg/datastore/providers/postgresql/changestream.go:1390-1393`).

What makes this silent rather than obvious is the wrapper chain. `fault-quarantine` receives
`resumeControlChangeStreamWatcher` (unwrapped at
`fault-quarantine/pkg/reconciler/reconciler.go:340-346`), and that wrapper *does* declare the
method, so the type assertion at `event_watcher.go:787` **succeeds**. The forwarding assertion
inside it then fails on the MongoDB watcher
(`store-client/pkg/client/resume_token.go:79-82`) and returns an error, so `event_watcher.go:789`
logs at `Debug` and `continue`s without setting the gauge. Two consequences:

- `fault_quarantine_event_backlog_count` is **never written** on MongoDB, so it reports `0`:
  the same false-healthy failure as `event-exporter`'s gauge, but disguised as working code.
- The `-1` "interface unavailable" fallback at `event_watcher.go:798` is **unreachable** for any
  factory-built watcher, because the wrapper always satisfies the assertion.

This is a pre-existing bug rather than something this ADR introduces, and it is filed
separately. It matters here for two reasons: the `-1` convention this ADR reuses has never
actually fired, and a design that adds methods to `ChangeStreamMetrics` inherits exactly this
failure mode.

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

Leave `fault_quarantine_event_backlog_count` in place so existing dashboards keep working. The
two pre-existing gauge bugs are fixed on their own tickets rather than inside this work.

## Implementation

### Where it lives

A single poller in the shared watcher, started from `Start(ctx)`, rather than the current
pattern of each consumer running its own ticker. `fault-quarantine`'s loop
(`fault-quarantine/pkg/eventwatcher/event_watcher.go:787-798`) becomes redundant and can be deleted once the
shared metric is in place, or kept temporarily for dashboard continuity.

```text
store-client/pkg/client/
  lagreporter.go          # provider-agnostic poller, owns the metrics
  interfaces.go           # new ChangeStreamPosition interface, alongside ChangeStreamMetrics
store-client/pkg/datastore/providers/mongodb/watcher/
  watch_store.go          # implements StreamPosition
store-client/pkg/datastore/providers/postgresql/
  changestream.go         # implements StreamPosition
```

### A separate interface, not an extension

The first instinct is to add `StreamPosition` to `ChangeStreamMetrics`. That is wrong, and the
reason is the assertion chain above: `ChangeStreamMetrics` is an **optional interface satisfied
by whole-type assertion**, so adding a method to it means an implementation providing only
`GetUnprocessedEventCount` stops satisfying it and loses the **count** metric it already had.
Capabilities checked this way have to be split per capability, not grouped:

```go
// ChangeStreamPosition is a separate optional interface, so a watcher that implements only
// one of the two capabilities keeps the other. Asserted independently of ChangeStreamMetrics.
type ChangeStreamPosition interface {
    StreamPosition(ctx context.Context) (StreamPosition, error)
}

// StreamPosition reports where the consumer is and where the stream head is, so lag can be
// computed without the caller knowing how a position is represented.
type StreamPosition struct {
    // PositionTime is the timestamp of the last event the consumer marked processed.
    PositionTime time.Time
    // HeadTime is the timestamp of the newest event in the stream.
    HeadTime time.Time
    // PositionID is the provider's opaque identifier for that position, in the form
    // GetUnprocessedEventCount expects. Empty when HasPosition is false.
    PositionID string
    // HasPosition is false before the consumer has ever marked an event processed under
    // code that records a position, in which case the other fields are meaningless.
    HasPosition bool
}
```

`ChangeStreamMetrics` is left exactly as it is.

Lag is then `HeadTime.Sub(PositionTime)`, clamped at zero, computed once in the shared layer.

`PositionID` is what lets the shared poller emit the count signal too. `GetUnprocessedEventCount`
takes a position identifier, and the poller has no other way to obtain one: only
`fault-quarantine` keeps a `LastProcessedObjectIDStore`, which is precisely why no other consumer
could adopt the count metric. Carrying the id alongside the timestamps is what makes the
capability shared rather than per-module. Its shape is provider-defined and opaque to the
poller, so this does not commit the shared layer to a position representation.

### Per-provider derivation

**PostgreSQL** is straightforward, because the changelog already carries a timestamp
(`datastore_changelog.changed_at TIMESTAMPTZ`, `store-client/pkg/datastore/providers/postgresql/datastore.go:382`) and the
position is a row id:

```sql
SELECT
  (SELECT changed_at FROM datastore_changelog WHERE id = $1)              AS position_time,
  (SELECT MAX(changed_at) FROM datastore_changelog WHERE table_name = $2) AS head_time;
```

This needs **a new index**. The head lookup has no `processed` predicate, deliberately, because
the newest event in the stream is usually one the consumer has already processed. Neither
existing index serves it: `idx_changelog_resume` on `(table_name, changed_at, id)` and
`idx_changelog_unprocessed` on `(changed_at)` are both partial on `processed = FALSE`
(`store-client/pkg/datastore/providers/postgresql/datastore.go:428-434`), and
`idx_changelog_table_record` on `(table_name, record_id)` has no timestamp. So:

```sql
CREATE INDEX IF NOT EXISTS idx_changelog_table_changed_at
  ON datastore_changelog(table_name, changed_at);
```

Non-partial, so the `MAX` is an index-only backward scan. Verify with `EXPLAIN` rather than
assuming; without it the head lookup is a sequential scan of the whole changelog on every poll,
which would make the "cheap" signal the expensive one.

**MongoDB** needs a position timestamp that the token does not readily give up. Two options,
and the choice matters:

- **Preferred: record the position time when the token is saved.** Adding `positionTime` and
  `positionId` fields to the `ResumeTokens` document makes both a first-class stored value
  rather than something inferred.

  The timestamp has to be captured where the event is decoded, **not** in `MarkProcessed`.
  `MarkProcessed(ctx, token []byte)`
  (`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:491`) receives only a
  token and never sees the event. `processNextEvent`
  (`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:434-438`) decodes the
  full change event into a `bson.M`, so `event["clusterTime"]` and
  `event["documentKey"]["_id"]` are both available there and must be carried alongside the
  per-event token that already flows to the consumer.
- **Rejected: decode the resume token's `_data`.** A MongoDB resume token's `_data` hex encodes
  the cluster time, and decoding it is how the original 11 day lag was actually measured
  operationally. But that encoding is a **driver implementation detail with no public API**, so
  depending on it in shipped code would break silently on a driver or server change.

The stored-timestamp approach also degrades gracefully: a consumer that has not yet saved a
token reports `HasPosition: false` and no lag series, rather than a misleading zero.

#### MongoDB head time is the subtle half

`db.<collection>.find().sort({_id: -1}).limit(1)` is the obvious choice and is **not
sufficient**, because it returns the newest document rather than the newest change event. This
codebase updates health events in place after insertion, so those are not the same thing:
`UpdateNodeQuarantineStatus`, `UpdatePodEvictionStatus`, `UpdateRemediationStatus` and
`UpdateSpanID` (`store-client/pkg/datastore/providers/mongodb/health_store.go:53-254`) all
modify existing documents, and the watcher opens the stream with
`SetFullDocument(options.UpdateLookup)`
(`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:168`) precisely because
update events are expected. An update to a week-old document advances the stream without
advancing `max(_id)`, so a consumer behind on those events would report near-zero lag.

Take the **later of two head candidates**, both cheap:

1. **`max(_id)` as a timestamp**, one backward scan of the existing `_id` index. Catches insert
   traffic, and keeps working when the cursor is not advancing.
2. **A cursor high-water mark**: the greatest `clusterTime` the watcher has decoded, held in
   memory and updated in `processNextEvent`. Catches update and delete traffic. It is valid as a
   head observation, and distinct from position time, because the cursor reads ahead of the
   consumer rather than in lockstep with it.

Neither alone is sufficient, and the reason is worth stating because it is the same failure this
ADR rejects elsewhere. Candidate 2 alone fails when the watcher's own read loop is blocked,
since `processNextEvent` stops advancing and head freezes at the position, reporting zero lag
for a consumer that is badly behind. That is the "derive lag from arriving events" trap under a
different name. Candidate 1 alone fails on update-only traffic. Their maximum is correct
whenever either mechanism is live, and both are wrong only when the collection is genuinely
idle, which is when zero is the right answer anyway.

Cluster `operationTime` from a `hello` command was considered as a third candidate and
**rejected**: it advances on any activity anywhere in the deployment, so an idle watched
collection would report continuously growing lag. That is the position-age failure mode.

### Cost control

`events_behind` is the expensive one. It is:

- **polled on a separate, longer interval** from `lag_seconds`, configurable, defaulting to
  something like 60s for lag and 300s for the count;
- **skippable entirely** by configuration, for operators who want the cheap signal only;
- never run more than once per interval per consumer, regardless of how many events flow.

`lag_seconds` is one indexed lookup per head candidate plus a stored position read, with no
`COUNT(*)`, and is safe at the shorter interval. On PostgreSQL that depends on the new
`(table_name, changed_at)` index above.

### Metrics

```text
changestream_lag_seconds{client}        gauge   head time minus position time, 0 when caught up
changestream_events_behind{client}      gauge   events after position, -1 when not collected
changestream_position_known{client}     gauge   1 once the consumer has saved a position, else 0
```

`client` is the existing `TokenConfig.ClientName` / `fieldClientName`, so the label space is
the set of consumers and nothing more.

The `-1` convention for `events_behind` marks "not collected" distinctly from "zero behind",
which is better than a missing series or a misleading zero. `fault-quarantine` set this
precedent for "interface unavailable", though as noted above that branch has never actually
fired.

`changestream_position_known` is not a diagnostic afterthought; it is what makes the other two
interpretable. A zero lag reading means "caught up" only when the position is known, so any
alert on `changestream_lag_seconds` must be qualified by it.

## Rationale

- **One implementation, every consumer.** Seven consumers currently have no lag visibility;
  wiring each one separately is seven chances to omit the eighth.
- **The signal is chosen to be correct rather than convenient.** Comparing the consumer to the
  stream head, rather than to the wall clock, is what makes an idle stream report zero.
- **The expensive query is opt-out, the cheap one is always on.** The failure mode this catches
  is severe enough to warrant a default-on metric, but not at the cost of a `COUNT(*)` over
  millions of documents every few seconds.
- **It reuses what exists.** `GetUnprocessedEventCount` is implemented on both providers
  already; the timestamp pair and the position identifier are what is new. On MongoDB it also
  finally gets a caller that works.

## Consequences

### Positive

- A consumer falling behind becomes alertable, in one dashboard, for every module.
- The failure that took manual token decoding to find becomes a single PromQL query.
- Backlog visibility on MongoDB, which today has none on any consumer.

### Negative

- Adds fields to the MongoDB `ResumeTokens` document, so a consumer running new code against a
  token written by old code has no `positionTime` until it next saves one. See the rollout gap
  below, which is the one genuinely unsatisfying part of this design.
- Adds a periodic query per consumer, small but not free.
- Adds a PostgreSQL index, so a migration on an existing changelog table.
- Adds a new optional interface that out-of-tree watchers must implement to get lag metrics.

### The rollout gap for existing tokens, stated plainly

A token written by old code has no `positionTime`, so the consumer reports
`HasPosition: false` and no lag series until it next marks an event processed. For the two
cases that matter this resolves quickly or not at all:

- **A consumer that is behind but still working** marks events processed continuously, by
  definition, so it populates a position within one event and becomes visible almost
  immediately. This is the #1704 shape, and it is covered.
- **A consumer that is fully stalled** never marks anything processed, so it never populates a
  position and emits no lag series at all. It is invisible to `changestream_lag_seconds`
  indefinitely, which is the failure this ADR exists to catch.

There is no backfill that fixes the second case, because the only place the old position's
timestamp exists is inside the opaque resume token, and decoding that is rejected above. So the
mitigation is a metric rather than a migration: **`changestream_position_known == 0` for longer
than a few poll intervals is itself the alert** for a consumer that has started but never
processed an event. That condition is exactly "stalled or brand new", and it must be documented
as alertable alongside the lag threshold rather than treated as a startup detail.

`changestream_events_behind` does not close this gap either, since `PositionID` is stored by the
same write that stores `positionTime`.

### Other mitigations

- Both intervals are configurable and the count is skippable, so cost is bounded by
  configuration rather than by traffic.
- `ChangeStreamPosition` is a separate optional interface asserted independently, so a watcher
  implementing only `ChangeStreamMetrics` keeps its count metric and simply reports no lag. This
  is the whole reason for not extending the existing interface.

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
- **Unit, shared layer**: the poller reports lag from a fake `ChangeStreamPosition`, clamps
  negative lag to zero, honours the skip-count configuration, and emits `-1` for an uncollected
  count.
- **Interface degradation, both directions**: a watcher implementing only `ChangeStreamPosition`
  reports lag and `-1` for the count; a watcher implementing only `ChangeStreamMetrics` keeps
  reporting the count and emits no lag series. The second case is the regression that extending
  `ChangeStreamMetrics` would have caused, so it needs a test rather than a comment.
- **MongoDB head time under updates**: insert events, then update the *oldest* document, and
  assert head time advances. Under `max(_id)` alone it does not, so this test distinguishes the
  two designs. Cover deletes and an explicitly non-monotonic `_id` the same way.
- **Blocked cursor**: with the watcher's read loop blocked, assert head still advances from
  `max(_id)` so lag does not collapse to zero.
- **PostgreSQL query plan**: assert the head lookup uses `idx_changelog_table_changed_at` rather
  than a sequential scan, since the cost argument depends on it.
- **Integration**: with `envtest` plus a real MongoDB, stop a consumer, insert events, restart
  it, and assert `changestream_lag_seconds` rises and then returns to zero as it drains. This
  reproduces the #1704 shape directly.
- **Idle-stream regression**: no events for longer than the poll interval, consumer caught up,
  assert lag stays at zero. This is the test that would fail under the rejected position-age
  approach.

## Rollout

1. Add the PostgreSQL `(table_name, changed_at)` index, ahead of anything that queries it.
2. Add the `ChangeStreamPosition` interface and implement it for both providers, plus the shared
   poller, default on for `lag_seconds` and off for `events_behind`.
3. Leave `fault_quarantine_event_backlog_count` and its ticker in place for one release so
   dashboards can move, then remove the ticker in favour of the shared metric.
4. Document in `docs/METRICS.md`: the idle-stream caveat, a lag alert
   (`changestream_lag_seconds > 900` for 10m, tuned per fleet, qualified by
   `changestream_position_known == 1`), and a separate alert on
   `changestream_position_known == 0` persisting, which is the only signal that covers a
   consumer stalled since before rollout.

Fixing `fault-quarantine`'s MongoDB count and removing `event-exporter`'s dead gauge are
tracked separately, since both are pre-existing bugs that stand on their own.
