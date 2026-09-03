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
| `fault-quarantine` | **yes, works** | ticker at `fault-quarantine/pkg/eventwatcher/event_watcher.go:787-798` sets `fault_quarantine_event_backlog_count`; sets `-1` when the watcher lacks the interface. Counts documents rather than change events, see below |
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
- `changestream_documents_behind{client}` — count after position. Secondary, and rate-limited.

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

The first instinct is to add `StreamPosition` to `ChangeStreamMetrics`. That is wrong.
`ChangeStreamMetrics` is an **optional interface satisfied by whole-type assertion**, checked at
`store-client/pkg/client/resume_token.go:79` and
`fault-quarantine/pkg/eventwatcher/event_watcher.go:787`, so adding a method to it means an
implementation providing only `GetUnprocessedEventCount` stops satisfying it and loses the
**count** metric it already had. That would break `fault-quarantine`'s working metric on both
providers until every implementation caught up. Capabilities checked this way have to be split
per capability, not grouped:

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

#### The count is a document backlog, and the name has to say so

The count signal is renamed **`changestream_documents_behind`**, not `events_behind`, because on
MongoDB it cannot be an event count and should not claim to be one.

MongoDB's existing implementation is
`CountDocuments({_id: {$gt: lastProcessedID}})`
(`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:563-574`). That counts
*documents* newer than the position, so it misses every in-place update and every delete, for the
same reason `max(_id)` fails as a head estimate. A consumer 500 status updates behind on old
documents would count zero. Getting the true event backlog would mean scanning the oplog, which
needs privileges the application user should not require, so it is out of scope.

On PostgreSQL the two coincide: `datastore_changelog` holds one row per change, so counting rows
after the position *is* the event count. Rather than give one metric name two meanings, the name
describes the weaker guarantee that holds on both, and the docs state where it is exact.

This is deliberately the lesser signal. `changestream_lag_seconds` is the one that captures
update and delete traffic, which is why it is the primary and default-on metric and the count is
opt-in.

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
  `event["documentKey"]["_id"]` are both available there.

  How that metadata reaches `MarkProcessed` is a design question rather than a detail, so it is
  specified below.
- **Rejected: decode the resume token's `_data`.** A MongoDB resume token's `_data` hex encodes
  the cluster time, and decoding it is how the original 11 day lag was actually measured
  operationally. But that encoding is a **driver implementation detail with no public API**, so
  depending on it in shipped code would break silently on a driver or server change.

The stored-timestamp approach also degrades gracefully: a consumer that has not yet saved a
token reports `HasPosition: false` and no lag series, rather than a misleading zero.

#### Getting the metadata from the event to the stored token

The handoff carries only two fields today:

```go
// store-client/pkg/datastore/types.go:106-109
type EventWithToken struct {
    Event       Event
    ResumeToken []byte
}
```

So the metadata decoded in `processNextEvent` has no route to `MarkProcessed`, and a design that
does not say how it travels will either lose it or, worse, pair a position time with the wrong
token. Both halves need stating:

**Travel.** `EventWithToken` gains the position metadata, additively, so existing readers are
unaffected. `MarkProcessed(ctx, token)` keeps its signature and gains a sibling that takes the
metadata explicitly, letting consumers migrate rather than break. For a consumer that still
calls the old method, the watcher falls back to an in-flight lookup keyed by token, covering
only events between the cursor and the consumer's position.

On a miss it writes the token with **no** position, because a missing lag series is recoverable
and a wrong one is not. "No position" has to mean **actively cleared**, not merely unwritten: the
document may already hold `positionTime` and `positionId` from an earlier token, and a token-only
update would leave that metadata describing a different event than the token beside it. That is
the precise failure this section exists to prevent, so the fallback path must `$unset` both
fields in the same update that writes the token. One write, three fields, no combination of them
that a reader can mistake for a valid position.

**Atomicity.** This comes free, and it is worth recording why. `MarkProcessed` already persists
via a single upsert on the client's own document:

```go
// store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:537-541
w.resumeTokenCol.UpdateOne(
    ctx,
    bson.M{fieldClientName: w.clientName},
    bson.M{"$set": bson.M{"resumeToken": resumeTokenToStore}},
    options.UpdateOne().SetUpsert(true),
)
```

Adding `positionTime` and `positionId` to that same `$set` makes all three fields one
single-document write, which MongoDB guarantees atomically. There is no window in which the
stored token and the stored position describe different events, and no second write to reconcile
after a crash. `TokenDoc` (`watch_store.go:93-95`) gains the two fields to read them back.

This needs a **restart acknowledgement test**: process events, restart mid-stream, and assert the
resumed position time matches the token actually stored rather than the newest event seen.

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
different name. Candidate 1 alone fails on update-only traffic.

**The residual gap, stated rather than claimed away.** Their maximum is not sufficient either.
The two failure conditions intersect: a **blocked read loop** *and* **update-or-delete-only
traffic** leaves candidate 2 frozen and candidate 1 unmoved, so `HeadTime` goes stale and lag
reads near zero for a stalled consumer. That corner is not exotic, because a blocked read loop is
exactly the state this ADR exists to detect. So the MongoDB guarantee is narrowed to what is
actually true:

> On MongoDB, `changestream_lag_seconds` detects a consumer that is behind whenever the
> collection is receiving inserts, or whenever the watcher's read loop is live. It does **not**
> detect a stalled consumer on a collection receiving only updates and deletes.

**A blocked read loop is already covered, for insert traffic, given ordered `_id`s.** No new
metric is needed for it: `max(_id)` is queried against the collection and is therefore
independent of the cursor, so head keeps advancing while the loop is stuck and
`changestream_lag_seconds` rises. Distinguishing "blocked" from "idle" is the head-versus-position
comparison this ADR is built on, and it works whenever inserts are flowing.

The assumption in "given ordered `_id`s" is worth naming, because `max(_id)` advances only for an
insert whose `_id` sorts after the current maximum. Health events carry driver-generated
ObjectIDs, with no explicit `_id` set on insert, and an ObjectID is a 4-byte timestamp followed by
a per-process random value and a counter. Across processes **within the same second** the ordering
is therefore effectively random, not chronological, so an insert can land below the current
maximum and leave head unmoved. Beyond a one-second window the timestamp dominates and ordering
holds, modulo writer clock skew.

The practical effect is bounded by that window and is immaterial against a 60s poll and a
900s alert threshold, but it means the guarantee is "head advances within about a second of an
insert", not "head advances on every insert". The existing count metric already depends on the
same ordering, since it filters `_id > lastProcessedID`
(`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:563`), so this assumption is
inherited rather than introduced here. The blocked-cursor test uses a **non-monotonic `_id`** so
the assumption is exercised rather than assumed.

**A read-loop heartbeat is not available, and should not be faked.** An earlier revision of this
ADR proposed a `changestream_reads_total` counter as a liveness signal, alerting when its rate
hit zero. That is wrong, and wrong in the specific way this ADR spends its Context section
warning about: the counter is flat on a healthy idle stream and flat on a blocked read loop, so
the alert fires on a quiet cluster. It is the position-age mistake wearing a different name, and
it is dropped rather than kept with caveats.

Nor is there an easy fix at a different granularity. The loop blocks inside
`w.changeStream.Next(ctx)`
(`store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:411-431`), which does not
return until an event arrives or the context is cancelled, so a per-iteration heartbeat sits
frozen on an idle stream too and inherits the same ambiguity. A genuine heartbeat needs the loop
restructured around `TryNext` with its own tick, which is a change to the read path's shape and
performance profile, not an observability addition.

So the residual gap stays open and named, with the two ways to close it recorded as future work
rather than smuggled in here:

- **An exact head.** A maintained `updatedAt` on health-event writes would make `MAX(updatedAt)`
  an exact head for inserts and updates, closing the gap without a heartbeat at all. It is a
  change to every writer rather than to the watcher.
- **A real heartbeat.** `TryNext` plus a loop tick would make read-loop liveness observable
  independently of traffic, at the cost of restructuring the read path.

Measure the narrowed guarantee first. Shipping a second metric whose flat value has two meanings
would cost more credibility than the gap does.

Cluster `operationTime` from a `hello` command was considered as a third head candidate and
**rejected**: it advances on any activity anywhere in the deployment, so an idle watched
collection would report continuously growing lag. That is the position-age failure mode.

### Cost control

`documents_behind` is the expensive one. It is:

- **polled on a separate, longer interval** from `lag_seconds`, configurable, defaulting to
  something like 60s for lag and 300s for the count;
- **skippable entirely** by configuration, for operators who want the cheap signal only;
- never run more than once per interval per consumer, regardless of how many events flow.

`lag_seconds` is one indexed lookup per head candidate plus a stored position read, with no
`COUNT(*)`, and is safe at the shorter interval. On PostgreSQL that depends on the new
`(table_name, changed_at)` index above.

### Metrics

```text
changestream_lag_seconds{client}      gauge   head time minus position time, 0 when caught up
changestream_documents_behind{client} gauge   documents after position, -1 when not collected
changestream_position_known{client}   gauge   1 once the consumer has saved a position, else 0
```

Three metrics, deliberately. Every candidate fourth one considered here had a flat or zero value
with two possible meanings, which is the defect this ADR exists to remove rather than add.

`client` is the existing `TokenConfig.ClientName` / `fieldClientName`, so the label space is
the set of consumers and nothing more.

The `-1` convention for `documents_behind` marks "not collected" distinctly from "zero behind",
which is better than a missing series or a misleading zero. `fault-quarantine` set this
precedent for "interface unavailable".

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
- **It reuses what exists.** `GetUnprocessedEventCount` is implemented and working on both
  providers already; the timestamp pair and the position identifier are what is new.

## Consequences

### Positive

- A consumer falling behind becomes alertable, in one dashboard, for every module.
- The failure that took manual token decoding to find becomes a single PromQL query.
- Six consumers that have no backlog or lag visibility today get both, with no per-module wiring.

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

`changestream_documents_behind` does not close this gap either, since `PositionID` is stored by the
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
- **Blocked cursor, insert traffic**: with the watcher's read loop blocked, assert head still
  advances from `max(_id)` so lag does not collapse to zero. Run it twice, once with ascending
  `_id`s and once with a **non-monotonic** `_id` inserted below the current maximum, so the
  documented ordering assumption is exercised rather than trusted.
- **Blocked cursor, update-only traffic**: the documented limitation. Assert that lag does *not*
  detect it, so the gap is pinned by a test rather than left to be rediscovered. A test that
  asserts the known-false thing is what stops someone later "fixing" the metric by widening the
  guarantee in the docs.
- **Idle versus blocked, on the same metrics**: a healthy idle stream and a blocked read loop
  must be distinguishable. With inserts flowing, the blocked case shows rising lag and the idle
  case does not. This is the test that would have caught the rejected heartbeat counter, which
  read identically in both.
- **Restart acknowledgement**: process events, restart mid-stream, and assert the resumed
  position time matches the token actually stored, not the newest event seen. This is what
  catches a position paired with the wrong token.
- **Count semantics**: on MongoDB, update an old document and assert `documents_behind` does not
  move, matching the documented meaning; on PostgreSQL assert the same scenario *does* move it,
  since the changelog has one row per change.
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
   poller, default on for `lag_seconds` and off for `documents_behind`.
3. Leave `fault_quarantine_event_backlog_count` and its ticker in place for one release so
   dashboards can move, then remove the ticker in favour of the shared metric.
4. Document in `docs/METRICS.md`, as two alerts rather than one, because neither covers the
   other's blind spot:
   - `changestream_lag_seconds > 900` for 10m, tuned per fleet, qualified by
     `changestream_position_known == 1`. The primary "behind" alert, and the one that catches a
     blocked read loop whenever inserts are flowing. The threshold must stay well above the
     sub-second `_id` ordering window described above, which 900s comfortably is.
   - `changestream_position_known == 0` for longer than a startup grace period, suggested at
     3 poll intervals or 10 minutes, whichever is longer. This is a **"stalled or uninitialized"**
     alert, not a stall alert: a consumer that has just started, or one that has never yet
     processed an event on a quiet stream, sits legitimately at zero. Without the grace period it
     pages on every rollout. Naming it for both states is deliberate, since "uninitialized for
     ten minutes on a busy cluster" is itself worth looking at.

   Also document the MongoDB caveats explicitly: what `documents_behind` counts, and that lag
   does not detect a stalled consumer on a collection receiving only updates and deletes.

Fixing `fault-quarantine`'s MongoDB count and removing `event-exporter`'s dead gauge are
tracked separately, since both are pre-existing bugs that stand on their own.
