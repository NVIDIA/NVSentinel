# ADR-068: Datastore — Bounded retention for the PostgreSQL changelog

Status: Proposed

## Context

PostgreSQL triggers append health and maintenance changes to `datastore_changelog`. Nothing deletes its rows. [Issue #2170](https://github.com/NVIDIA/NVSentinel/issues/2170) reports 42M rows and 90 GB on about 350 nodes, growing by 2.6M rows daily. Polls slowed, and index builds alongside this load exhausted Aurora memory.

`docs/postgresql-schema.sql` defines a `cleanup_changelog()` function that deletes processed rows older than 7 days. No component calls it. The components also do not create it, because `createTables` in store-client does not include it.

Three facts about the changelog decide what a cleanup can delete:

- **`processed` is shared by all consumers.** `MarkProcessed` runs `UPDATE ... SET processed = TRUE WHERE id <= $1 AND table_name = $2 AND processed = FALSE`. This only shows that one caller marked the id prefix of that table. It does not show that every consumer read those rows. A cleanup that deletes by `processed`, as `cleanup_changelog()` and the workaround in the issue do, can delete rows that another consumer has not read.
- **A consumer position is a `(changed_at, id)` pair.** Each watcher saves `{"timestamp", "eventID"}` to `resume_tokens`, keyed by `client_name`, with a `last_updated` time. The poll reads rows after the position in `(changed_at, id)` order. A row with a lower id but a later `changed_at` than the position can still be unread, so the `eventID` alone is not a bound.
- **Ids follow allocation order, not commit order or time.** `changed_at` is the start time of the writing transaction, and the id comes from one sequence for both tables. A row can have a lower id than a row that is younger. So a range of ids is not a range of ages.

The options considered are:

1. A periodic cleaner in the store-client PostgreSQL provider, which runs in the components that open the datastore, with one run at a time under an advisory lock.
2. A Helm CronJob that runs the delete in SQL.
3. Calling `cleanup_changelog()` as it is.
4. Deleting rows from `MarkProcessed`.
5. Partitioning `datastore_changelog` by time and dropping old partitions.

## Decision

The PostgreSQL provider runs a periodic, batched cleaner from both datastore and client-factory connections. It deletes a row only when the row is older than the retention and before the oldest `(changed_at, id)` position in `resume_tokens`. Every token counts, including stale tokens, which raise an alert. The cleaner is off by default.

Add a previous-snapshot guard: a row version's `xmin` must precede `pg_snapshot_xmin(pg_current_snapshot())` saved at least one cleanup interval earlier. Persist the full horizon and observation time in a new optional `changelog_cleanup_state` table, under the cleanup lock. The first run records a horizon and deletes nothing. Existing event and token schemas stay unchanged. No commit-timestamp server setting or writer change is required.

Include acknowledgment-safe idle checkpoints in this proposal. Quiet consumers are normal (ADR-054), so an empty poll can advance a verified read position only after all handed-out deliveries are acknowledged. The [guard and checkpoint protocol](../postgresql-changelog-retention-protocol.md) specifies persistence, wraparound, snapshot boundaries and reader-upgrade prerequisites. Implementation still requires maintainer acceptance.

## Implementation

- **Where it runs.** Both `NewPostgreSQLStore` and the PostgreSQL branch of `ClientFactory.CreateDatabaseClient` start the same provider-owned cleaner when retention is set. The store starts it after table setup; the factory starts it after connecting, without table setup or DDL. The object that owns the connection pool owns the cleaner, and its `Close(ctx)` cancels and waits for the cleaner before closing the pool. A client borrowed from a store must not start another cleaner. All queries honour cancellation.

  Fault Quarantine, Node Drainer, Fault Remediation and the Health Events Analyzer use the store constructor. Platform connectors and the event exporter use the factory. Thus a deployment with only platform connectors still has a cleaner owner. Multiple owners coordinate through the advisory lock. The event exporter's saved token remains in the bound.

  The chart passes the retention settings to both paths, including the platform connector DaemonSet and Deployment. Factory connections do not create missing tables or indexes. Missing prerequisites cause a reported, skipped run; operators must provision the schema and index from PR #2233 before enabling retention in a factory-only deployment.
- **One run at a time.** A run reserves a connection and takes `pg_try_advisory_lock` on a key of its own, separate from the setup lock. If another session holds the lock, the run does nothing. The batches run on the reserved connection. The run releases the lock with a new, short context. If the release fails, it closes the session instead of returning the connection to the pool.
- **Guard lifecycle.** Under the advisory lock, read the previous guard. A run before one configured interval has elapsed skips without replacing it. Otherwise sample a new horizon and record its database observation time after sampling, before deletion. Use only the previous horizon for this run. Replacement owners use the same persisted record; a first run seeds it without deletion. Missing, invalid, future-dated or unusably old metadata means a reported seed-only run. A failed read or write means no deletion. Factory connections require separate provisioning of this table.
- **Transaction-id comparison.** Each batch uses repeatable read and assigns its xid before evaluating ages. Validate the previous `xid8` horizon within `2^31` ids. Delete snapshot-visible versions whose age is negative or strictly exceeds the horizon age. Concurrent changes roll back the batch; count only committed deletes.
- **The bound.** Each run reads all rows of `resume_tokens` and computes:
  - `cutoff = now() - retention`;
  - `oldest`, the smallest `(timestamp, eventID)` among the tokens, compared as a pair.

  A row can be deleted only if `changed_at < cutoff` and `(changed_at, id) < oldest`. Both conditions are part of the delete statement itself, so every deleted row is checked.

  Unreadable or absent tokens prevent deletion. The [checkpoint requirements and acceptance criteria](../postgresql-changelog-retention-protocol.md#consumer-bound-and-idle-checkpoints) require verified acknowledgment timestamps and initial positions before reading.

  The age and previous-snapshot guards remain required. A missing or immature guard means no `DELETE`.
- **Stale tokens stay in the bound.** A token whose `last_updated` is older than the retention still counts in `oldest`. The cleaner still deletes the rows before it, and it reports the token in a metric and a warning that names the client.
  - An old `last_updated` does not prove that a consumer is gone. A consumer that is live but stalled, or that cannot save its token, needs every row after its position.
  - A removed component leaves its token, which holds back the cleanup from that position on. The operator deletes that row from `resume_tokens` only when the component is gone for good. The docs describe this step, and warn not to delete the token of a component that still runs.
- **Legacy tokens.** A token in the old format, with an `eventID` and no `timestamp`, has no pair. The cleaner looks up the `changed_at` of that id. If the row does not exist, the run deletes nothing and reports an error.
- **Idle checkpoints.** Track deliveries before enqueueing; queued work counts as in flight. `MarkProcessed` must preserve the acknowledged delivery prefix rather than persist a later read head. An empty or unknown token cannot certify pending work. Serialize polling, delivery registration, acknowledgment and checkpoint writes; propagate persistence failures.

  Empty snapshots cannot exclude later commits below their tail. The protocol requires fresh activity visibility, no other open or prepared transactions, then a new snapshot. Its candidate is an actual pair strictly before the inspection fence. Missing visibility or timestamp-ordering uncertainty defers advancement. Persist only after rechecking acknowledgments. Coalesce writes per cleanup interval; refresh `last_updated` for unchanged idle positions. Caller interfaces, token fields and ADR-054's lag formula stay unchanged.
- **Batched deletes.** Use the [bounded candidate query](../postgresql-changelog-retention-protocol.md#bounded-deletes). Select at most 10,000 candidate rows before checking their current row versions' transaction ages. This bounds work even with a protected prefix. Stop on a short or blocked batch, at 100 batches per run, cancellation or error. Do not spin on a blocked prefix.

  The query uses `idx_changelog_resume_all (table_name, changed_at, id)` from PR #2233. A missing or invalid index prevents deletion. Earlier age-and-position measurements exclude the guard and do not establish the final query's performance.
- **Configuration.** Store-client reads environment variables in the same way as `DATASTORE_MAX_CONNECTIONS`:
  - `DATASTORE_CHANGELOG_RETENTION`: a Go duration. Unset or `0` disables cleanup. Enabled values must be at least `24h`. Recommend `168h` for seven days; `7d` is invalid.
  - `DATASTORE_CHANGELOG_CLEANUP_INTERVAL`: default `1h`.
  - `DATASTORE_CHANGELOG_CLEANUP_BATCH_SIZE`: default `10000`.
  - `DATASTORE_CHANGELOG_CLEANUP_MAX_BATCHES`: default `100` per run.

  Malformed, negative, overflowing or below-minimum retention values log a warning and disable the cleaner. Invalid intervals or non-positive batch settings also disable it. Report once at configuration load; never fail datastore construction, pod startup or ingestion. Explicitly disabled cleanup needs no warning. The interval must be positive.

  The chart sets them from new keys under `global.datastore.changelogCleanup`. These are new keys with defaults that keep current behaviour.
- **Metrics.** The cleaner adds new metrics only:
  - `datastore_changelog_cleanup_deleted_rows_total`;
  - `datastore_changelog_cleanup_runs_total` with a `result` label (`deleted`, `nothing_to_delete`, `skipped_locked`, `skipped_no_index`, `skipped_guard`, `error`);
  - `datastore_changelog_cleanup_last_success_timestamp_seconds`;
  - `datastore_changelog_cleanup_oldest_protected_age_seconds`, the age of `oldest`, with a `stale_tokens` gauge. An alert on these shows when one consumer holds back the cleanup.
- **Space.** Deletes free reusable space without shrinking the table file. Use autovacuum and the [operator reclamation guidance](../postgresql-changelog-retention-protocol.md#space-reclamation); the cleaner runs no `VACUUM`.
- **Tests.**
  - Cover the protocol's [checkpoint acceptance cases](../postgresql-changelog-retention-protocol.md#checkpoint-acceptance-criteria).
  - With sqlmock, in the style of the setup-lock and index tests:
    - the store and factory paths each start a cleaner when enabled and stop it before closing their pool;
    - disabled retention starts no cleaner, and the factory path runs no setup DDL;
    - warm-up, too-early runs, owner replacement and unreadable or unwritable guard state cannot cause premature deletion;
    - invalid retention (`7d`, negative, below `24h`, malformed or overflowing) disables cleanup while startup and ingestion continue;
    - explicit `0`, `24h` and `168h` have the documented behavior;
    - queued work, prefix acknowledgments, delivery/empty-poll races, empty tokens and checkpoint write failure cannot advance past unacknowledged work;
    - the bound is the smallest token as a pair, not the smallest `eventID`;
    - a stale token still holds back the cleanup and is reported;
    - nothing older than the bound means no `DELETE` (the healthy case);
    - a held lock, a missing index or an unreadable token means no `DELETE`;
    - the batch loop stops on a short batch, on the batch limit, on `ctx.Done()`, and on an error.
  - Against a real PostgreSQL 16, because sqlmock does not check SQL semantics:
    - a row with a lower id but a newer `changed_at` than the cutoff stays;
    - a row after a consumer's position in `(changed_at, id)` order stays, even when its id is lower than the token's `eventID`;
    - a transaction active at the previous snapshot commits later: its row stays until a later eligible horizon;
    - subtransactions and prepared transactions preserve the guard; negative-age frozen prefixes advance; concurrent updates roll back the entire batch;
    - open, xid-less, prepared or hidden transactions prevent idle advancement; late commits remain readable after restart without NOTIFY;
    - quiet-consumer checkpoints survive restart without skipping in-flight deliveries;
    - the query plan is an index range scan on a table with millions of rows.
  - Render the chart with the store-owning components disabled. Check retention settings reach platform connectors in both DaemonSet and Deployment modes. Verify a factory-owned cleaner runs with the required schema, skips and reports missing prerequisites, and cancels on close.

## Rationale

- **Protects every saved position.** Every deleted row is checked against both the age and the oldest saved position, in the same order the poll uses. Stale tokens stay in the bound, because NVSentinel cordons and drains nodes from these events, and a silent gap is worse than a larger table. The limits in the Notes section still apply.
- **Bounded work.** Each batch and each run has a fixed upper limit, and the index range stops at the first row that is too new. A run with nothing to delete costs a short index range read for each watched table.
- **Follows existing patterns.** The setup lock, the CONCURRENTLY index builds of ADR-052 and the `DATASTORE_*` variables already work this way in store-client. No module calls another module.
- **Upgrade compatibility.** Cleanup is off by default. The guard covers older writers without changing their events. All store-owning components must support verified initial and acknowledgment checkpoints before enabling cleanup. Disable cleanup before a reader downgrade; the unchanged token shape cannot identify an old reader automatically. Existing tokens remain in the bound and old components ignore the new optional guard table.

## Consequences

### Positive

- When every consumer saves a recent position, the changelog size is bounded by the retention and the write rate, instead of growing for the life of the cluster.
- With a bounded table, change stream polls, `MarkProcessed` updates and index builds do not slow down as the cluster ages.
- Operators can see the cleanup in metrics, and see which consumer holds it back.

### Negative

- The default does not fix the problem. Operators must turn the cleaner on.
- One consumer that falls behind, or a token left by a removed component, holds back the cleanup for both tables until it catches up or an operator removes the token.
- Reader upgrades and an explicit rollout check are required before enabling cleanup. Older readers can persist a read head ahead of acknowledged work.
- Busy databases or hidden activity can delay idle checkpoints.
- Each store or factory owner tries the lock once per interval, though only one run does the work.
- A factory-only deployment must provision the schema and index before enabling cleanup. No constructor creates those prerequisites on the factory path.
- Guard warm-up delays cleanup by at least one interval. Long transactions, stale tokens or conservative frozen-row handling can hold it back.
- The table file does not shrink without an operator step.
- The cleaner depends on the index from PR #2233, and does nothing without it.

### Mitigations

- The docs and the PostgreSQL values file recommend a retention. A later release can change the default after this one has been used in practice.
- The `oldest_protected_age_seconds` and `stale_tokens` metrics make a held-back cleanup visible, and the docs give the steps to find and remove the token of a removed component.
- Acknowledgment-safe idle checkpoints handle healthy quiet streams. Document reader rollout, coalesced writes and checkpoint failures; do not discard stale tokens automatically.
- The lock attempt is one cheap query per component per interval.
- The docs describe the one-time `pg_repack` step.
- The cleaner reports `skipped_no_index`, and the existing setup rebuilds the index at the next start.
- Factory owners report missing prerequisites without running DDL. The docs give the schema provisioning steps for deployments without a store owner.
- Keep cleanup disabled until maintainer acceptance and the guard, idle-checkpoint and upgrade acceptance cases pass.

## Alternatives Considered

### Helm CronJob

**Rejected** because it adds an image and schedule and duplicates connection, Secret and TLS configuration. A job could enforce the same safety conditions; the provider already owns connections and metrics.

### Call `cleanup_changelog()`

**Rejected** because it deletes by `processed` alone, which can drop rows another consumer has not read. It deletes in one unbounded statement, and the components do not create it. Open PR #1777 moves it into a migration, and nothing calls it there either.

### Delete rows in `MarkProcessed`

**Rejected** because one consumer cannot know which rows the other consumers still need. It would also add delete work and lock waits to the hot path of every consumer.

### Ignore tokens that are not updated within the retention

**Rejected** because an old `last_updated` does not prove a consumer is gone. A live consumer that is stalled, or that fails to save its token, would lose the rows after its position. Failing closed and alerting keeps the decision with an operator.

### Partition the changelog by time

**Rejected for now** because it changes the persisted schema and needs a migration of existing tables, which belongs with the versioned migrations proposed in #1777. Dropping a partition is the cheapest delete, so partitioning can replace the batched delete later. The bound in this ADR still applies to it.

## Notes

- **Long transactions.** The previous snapshot horizon protects transactions active at that observation, even when they commit old-looking rows later. The guard needs no server setting. Statement and idle timeouts still do not bound total transaction lifetime.
- **Acknowledgments and upgrades.** This proposal includes the provider checkpoint change. Retention must remain disabled until all readers have it; older writers remain supported. No consumer API or token fields change. Acceptance tests must cover this rollout prerequisite.
- **Existing delivery limits.** Late commits below a saved timestamp remain a polling limitation. The guard preserves NOTIFY's opportunity to deliver them for at least one interval; it does not promise exactly-once delivery across disconnections.
- Open questions for maintainers:
  - Should the cleaner be on by default, with a 7-day retention to match `cleanup_changelog()`? This ADR proposes off first.
  - Is "an operator removes the token" the right recovery for a removed component? Or should a component remove its own token when it is uninstalled?
- Non-goal: the retention of the documents in `health_events` and `maintenance_events`. Only the change feed is in scope.
- Dependency: the implementation lands after [PR #2233](https://github.com/NVIDIA/NVSentinel/pull/2233).

## References

- [Issue #2170](https://github.com/NVIDIA/NVSentinel/issues/2170): the PostgreSQL changelog is never cleaned up
- [ADR-052](052-deployment-platform-connector.md): precedent for index builds under the setup lock
- [ADR-054](054-changestream-lag-metrics.md): change stream consumer lag metrics
- [PR #1777](https://github.com/NVIDIA/NVSentinel/pull/1777): versioned PostgreSQL schema migrations
- [PR #2233](https://github.com/NVIDIA/NVSentinel/pull/2233): changelog resume index and row comparison in the poll
- [PostgreSQL 16 timeouts](https://www.postgresql.org/docs/16/runtime-config-client.html): statement and idle transaction timeouts have different scopes
- [PostgreSQL 16 current date and time](https://www.postgresql.org/docs/16/functions-datetime.html#FUNCTIONS-DATETIME-CURRENT): `now()` returns the transaction start time
- [PostgreSQL 16 NOTIFY](https://www.postgresql.org/docs/16/sql-notify.html): notifications are delivered after commit

- [Guard and idle-checkpoint protocol](../postgresql-changelog-retention-protocol.md): persisted snapshot guard, transaction-id comparisons and bounded SQL
- [PostgreSQL snapshots and transaction ids](https://www.postgresql.org/docs/16/functions-info.html#FUNCTIONS-TXID-SNAPSHOT)
