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

### Current adoption

| Consumer | Stream backlog metric | Notes |
| --- | --- | --- |
| `fault-quarantine` | **yes, works** | ticker at `fault-quarantine/pkg/eventwatcher/event_watcher.go:787-798` sets `fault_quarantine_event_backlog_count` |
| `event-exporter` | **declared, never set** | `health_events_exporter_event_backlog_size` is created at `event-exporter/pkg/metrics/metrics.go:73` and assigned nowhere in the module, so it reports `0` forever. Removal in #1739 |
| `health-events-analyzer` | none | no reference to `ChangeStreamMetrics` anywhere in the module. This is the consumer that went 2.4M events behind |
| `fault-remediation` | none | |
| `janitor` | none | |
| `lifecycle-manager` | none | |
| `labeler` | none | |

`node_drainer_queue_depth` exists but measures node-drainer's own in-process work queue, not
stream position, so it does not cover this.

### Every consumer watches a different, filtered stream

This is the fact that determines the whole design, so it comes before the candidate signals.

Each consumer opens its change stream with **its own aggregation pipeline**, and most of them
are narrow:

| Consumer | Pipeline | Admits |
| --- | --- | --- |
| `fault-quarantine` | `BuildAllHealthEventInsertsPipeline` | every health event insert |
| `node-drainer` | `BuildNodeQuarantineStatusPipeline` (`store-client/pkg/client/mongodb_pipeline_builder.go:44-71`) | only `update` operations that set `nodeQuarantinedStatus` to Quarantined, AlreadyQuarantined, UnQuarantined or Cancelled |
| `fault-remediation` | `BuildQuarantinedAndDrainedNodesPipeline` | only quarantined-and-drained node transitions |

A consumer's position therefore advances only when an event **its own filter admits** is
processed. On a healthy cluster, or any cluster with `global.faultQuarantine.enabled: false`,
no node is ever quarantined, so `node-drainer` and `fault-remediation` match **zero events,
ever**, and their positions never move off the initial token while health events keep arriving
by the thousand.

Any signal that compares a consumer's filtered position against the unfiltered stream will
therefore report enormous lag for those two consumers permanently, while they are perfectly
healthy. That rules out a family of otherwise-appealing designs, including the one this ADR
proposed in its first four revisions.

### Candidate signals, and why the obvious ones are wrong

1. **`events_behind`** — count of events after the consumer's position. Needs a `COUNT(*)` per
   poll against a collection that is ~7 GB on our fleet, and on MongoDB it counts *documents*
   rather than change events, so it misses in-place updates entirely. It is also filtered-stream
   blind, like the next two.
2. **position age** — `now - timestamp(position)`. Nearly free, but **wrong on a quiet stream**:
   a fully caught-up consumer whose last event arrived an hour ago reports an hour of "lag" when
   it is not behind at all.
3. **head minus position** — `timestamp(head) - timestamp(position)`. Fixes the quiet-stream
   case, and is what this ADR proposed through four revisions. **Rejected**, because head is
   global while position is filtered, so it reports "time since deployment" as lag for
   `node-drainer` and `fault-remediation` forever. See [Alternatives](#alternatives-considered).
4. **time since the last evidence of being caught up** — the chosen signal, below.

What all three rejected signals lack is any **affirmative evidence** that a consumer is caught
up. They infer it from position, which is filtered, or from head, which is not the consumer's
business. The stream offers direct evidence instead: **an empty batch means "there is nothing
further for you right now"**, evaluated server-side against that consumer's own pipeline.

## Decision

Emit lag from the **shared `store-client` watcher layer**, labelled by the existing client name,
so every consumer gets it without per-module wiring.

The watcher tracks two timestamps in memory:

- **`lastEmptyBatchAt`** — when the stream last returned no events, meaning the consumer was
  caught up as of that moment, on its own filtered stream.
- **`lastEventReadAt`** — the server-side timestamp of the most recent event the watcher read.

```text
changestream_lag_seconds = now - max(lastEmptyBatchAt, lastEventReadAt)
```

Computed fresh on each scrape, so it keeps growing while a consumer is stuck rather than
freezing at its last update.

Do **not** export raw position age, and do not compare a filtered position against an
unfiltered head. Both are covered under Alternatives.

This design was raised by @KaivalyaMDabhadkar in review on #1738.

## Implementation

### Where it lives

```text
store-client/pkg/client/
  lagreporter.go          # owns the two timestamps and the gauge
store-client/pkg/datastore/providers/mongodb/watcher/
  watch_store.go          # eventLoop switches to TryNext, records both timestamps
store-client/pkg/datastore/providers/postgresql/
  changestream.go         # poller records both timestamps
```

No new interface, no provider method for head time, and no change to `ChangeStreamMetrics`. That
interface is satisfied by whole-type assertion at `store-client/pkg/client/resume_token.go:79`
and `fault-quarantine/pkg/eventwatcher/event_watcher.go:787`, so adding a method to it would
drop any implementation providing only `GetUnprocessedEventCount` and cost it the count metric it
already has. Leaving it untouched avoids that entirely.

### MongoDB: the read loop has to change

Today the loop blocks:

```go
// store-client/pkg/datastore/providers/mongodb/watcher/watch_store.go:411-431
hasNext := w.changeStream.Next(ctx)
```

`Next` does not return until an event arrives or the context is cancelled, so an idle stream is
indistinguishable from a stalled one and there is no moment at which "caught up" can be recorded.
`TryNext` returns immediately when nothing is available, which is precisely the empty-batch
signal:

```go
if w.changeStream.TryNext(ctx) {
    w.recordEventRead(clusterTimeOf(event))   // lastEventReadAt
    w.processNextEvent(ctx)
} else if err := w.changeStream.Err(); err != nil {
    w.handleChangeStreamError(err)
    return
} else {
    w.recordCaughtUp()                        // lastEmptyBatchAt
    // pace the loop; see below
}
```

**`TryNext` needs pacing.** It returns immediately, so a bare loop busy-polls a core whenever the
stream is idle, which on this fleet is most of the time. The empty branch needs a short sleep or
ticker, and that interval sets the resolution of `lastEmptyBatchAt`. Somewhere in the hundreds of
milliseconds to low seconds keeps idle CPU negligible while keeping lag resolution far finer than
any sane alert threshold. **Idle CPU must be measured before and after**, since this is the one
place the design trades cost for signal.

`lastEventReadAt` comes from the event's own server-side timestamp. `processNextEvent`
(`watch_store.go:434-438`) already decodes the full change event into a `bson.M`, so
`event["clusterTime"]` is available where it is needed, with no change to `MarkProcessed` and no
new field on the `ResumeTokens` document.

### PostgreSQL: nearly free

The poller already runs on an interval and already distinguishes a poll that returned rows from
one that did not. A poll returning zero rows **is** the empty batch, so it records
`lastEmptyBatchAt`; a poll returning rows records `lastEventReadAt` from
`datastore_changelog.changed_at` on the newest row read. No schema change and no new index,
because nothing queries for a stream head any more.

### Startup, and why lag must be allowed to be unknown

Both timestamps live in memory, so a restart resets them. Between process start and the first
empty batch or first event read the watcher has **no evidence either way**, and must say so
rather than guess:

- Reporting `0` would claim a caught-up consumer that has not been observed at all, which is the
  false-healthy failure this ADR exists to remove.
- Reporting `now - processStart` would page on every rollout.

So `changestream_lag_seconds` is **not exported at all** until one of the two timestamps is set,
and `changestream_lag_known{client}` reports `0` until then and `1` afterwards. In practice that
gap is one poll interval on PostgreSQL and one `TryNext` on MongoDB, so it closes in
milliseconds on a live stream, but it must be explicit rather than incidental.

A consumer whose watcher never starts, or is wedged before its first read, therefore shows
`changestream_lag_known == 0` persistently, which is itself alertable and is the only signal that
covers that state.

### Metrics

```text
changestream_lag_seconds{client}  gauge  now - max(lastEmptyBatchAt, lastEventReadAt); absent until known
changestream_lag_known{client}    gauge  1 once either timestamp has been set, else 0
```

Two metrics. Every additional candidate considered during review had a flat or zero value with
two possible meanings, which is the defect this ADR removes rather than adds.

`client` is the existing `TokenConfig.ClientName` / `fieldClientName`, so the label space is the
set of consumers and nothing more.

### What this does and does not measure

It measures **the watcher's progress against its own stream**, not the durability of the
consumer's stored position. A consumer that reads and processes an event and then dies before
`MarkProcessed` succeeds looks healthy by this metric, because the read did happen.

That is the right trade for a lag signal, and it should not be oversold: this is not also a
resume-token correctness check. `fault_quarantine_event_backlog_count` stays in place for the one
consumer that has a durable-position count today.

## Rationale

- **Affirmative evidence beats inference.** An empty batch is the stream telling the consumer it
  is caught up, evaluated against that consumer's own filter. Position and head are both proxies
  that break on filtered pipelines.
- **It is correct for every consumer, not just the unfiltered one.** `node-drainer` and
  `fault-remediation` are the consumers most likely to sit idle for weeks, and are exactly the
  ones the rejected designs got wrong.
- **One implementation, every consumer.** Seven consumers have no lag visibility; wiring each
  separately is seven chances to omit the eighth.
- **It deletes machinery rather than adding it.** No head-time query, no `max(_id)` ordering
  assumption, no new PostgreSQL index, no `ResumeTokens` schema change, no interface change, and
  no `COUNT(*)`.

## Consequences

### Positive

- A consumer falling behind becomes alertable, in one dashboard, for every module.
- The failure that took manual token decoding to find becomes a single PromQL query.
- A blocked or wedged read loop is caught by the same metric, because neither timestamp advances
  while the wall clock does.
- Correct on filtered streams, which is what makes it usable on a detection-only deployment.

### Negative

- **The MongoDB read loop changes shape.** `Next` to `TryNext` plus pacing touches the hot path
  of every consumer, and it is the main risk in this ADR.
- Idle CPU rises from "blocked in a syscall" to "wakes on a tick". Expected to be negligible, but
  it must be measured rather than assumed.
- Lag is unknown for a moment after every restart, and needs the companion metric to stay honest
  about it.
- In-memory state means the metric says nothing about history before the current process.

## Alternatives Considered

### Head minus position (`timestamp(head) - timestamp(position)`)

**Rejected**, and this ADR proposed it through four revisions before review caught the flaw.

Head is a property of the whole collection; position is a property of the consumer's **filtered**
stream. Comparing them is only meaningful for a consumer whose pipeline admits everything, which
is `fault-quarantine` alone. For `node-drainer` and `fault-remediation`, whose pipelines match
nothing at all on a cluster where quarantine is disabled, it reports time-since-deployment as
lag, permanently, on two healthy consumers. That is the same false alarm as position age, reached
by a longer route.

It also dragged in incidental machinery the chosen design does not need: a `StreamPosition`
interface, stored `positionTime` and `positionId` fields on the `ResumeTokens` document, a
non-partial `(table_name, changed_at)` PostgreSQL index, and a MongoDB head estimate built from
`max(_id)` plus a cursor high-water mark, which carried its own ObjectID ordering assumption and
its own blind spot for update-only traffic.

### Export raw resume token age (`now - position time`)

**Rejected** because it reports lag on a caught-up but idle consumer, and because position is
filtered, so it is wrong for narrow pipelines for the same reason as above. The chosen signal is
also `now` minus a timestamp, but that timestamp is the last moment the consumer was **observed
to be caught up**, which position age has no way to establish.

### Decode the MongoDB resume token's `_data` field

**Rejected** because the encoding is an undocumented driver and server implementation detail with
no public API. It works today, and it is how the original incident was diagnosed by hand, but
depending on it in shipped code would break silently on an upgrade.

### Count of events behind (`COUNT(*)` after position)

**Rejected.** A count is meaningless without knowing the event rate: "50,000 behind" is an
emergency on a quiet cluster and thirty seconds of traffic on a busy one. On MongoDB the
available implementation counts documents newer than the position
(`CountDocuments({_id: {$gt: lastProcessedID}})`), so it misses in-place updates and deletes
entirely, and health events **are** updated after insertion. It is also filtered-stream blind.

### An event-counter heartbeat for read-loop liveness

**Rejected.** A counter of decoded events is flat on a healthy idle stream and flat on a blocked
read loop, so an alert on its rate hitting zero fires on a quiet cluster. The chosen design needs
no separate liveness signal, because a blocked loop stops advancing both timestamps and the lag
gauge grows on its own.

### Derive lag in each consumer from event timestamps as they arrive

**Rejected** because it only updates when events arrive, so a fully stalled consumer, which is
the worst case, stops updating its own lag metric and looks frozen rather than behind.

## Testing

- **Idle stream, caught up**: no events for many multiples of the pacing interval, assert lag
  stays near zero. This is the test that fails under position age.
- **Narrow pipeline, caught up**: a consumer whose filter admits nothing while the collection
  receives a steady insert load, assert lag stays near zero. **This is the test that fails under
  head minus position**, and it is the reason for the redesign, so it belongs in the suite
  permanently.
- **Genuinely behind**: seed a backlog, start the consumer, assert lag reflects the age of the
  events being read and falls to near zero as it drains.
- **Blocked read loop**: with the loop wedged, assert lag grows monotonically with wall-clock
  time.
- **Slow consumer**: with the consumer not draining the event channel, so the read loop blocks on
  send, assert lag grows. This distinguishes "reading fine, processing slowly" from healthy.
- **Restart**: assert `changestream_lag_seconds` is **absent** and `changestream_lag_known` is
  `0` before the first read or empty batch, and that both become live immediately afterwards.
  Assert lag is never reported as `0` while unknown.
- **Idle CPU**: measure the watcher's CPU on an idle stream before and after the `TryNext`
  change, and assert the pacing interval bounds it. The one performance-sensitive part of this
  design deserves a number rather than a claim.
- **Both providers**: every case above against MongoDB and PostgreSQL, since the empty-batch
  signal is derived differently in each.
- **Integration**: with `envtest` plus a real MongoDB, stop a consumer, insert events, restart
  it, and assert lag rises and then returns to near zero as it drains. This reproduces the #1704
  shape directly.

## Rollout

1. Add the lag reporter and the two metrics behind the PostgreSQL path first, since it needs no
   read-loop change and exercises the shared layer.
2. Change the MongoDB read loop to `TryNext` with pacing, with the idle-CPU measurement from the
   test plan as the gate on merging it.
3. Document in `docs/METRICS.md`, as two alerts:
   - `changestream_lag_seconds > 900` for 10m, tuned per fleet. The primary "behind" alert. It
     needs no `lag_known` qualifier, because the series is absent rather than zero while unknown.
   - `changestream_lag_known == 0` for longer than a startup grace period, suggested at 10
     minutes. Covers a watcher that never started or is wedged before its first read.
4. Leave `fault_quarantine_event_backlog_count` in place. It answers a different question, a
   durable-position count for one consumer, and nothing here replaces it.

Removing `event-exporter`'s dead gauge is tracked separately in #1739, and #1743 covers the
Kubernetes connector dropping events on write failure, which is adjacent but independent.
