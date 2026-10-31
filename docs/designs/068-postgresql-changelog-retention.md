# ADR-068: Datastore — Bounded retention for the PostgreSQL changelog

Status: Proposed

## Context

With the PostgreSQL datastore, triggers on `health_events` and `maintenance_events` write one row to `datastore_changelog` for every insert, update and delete. The change stream consumers read this table. Nothing deletes from it, so it grows without limit ([issue #2170](https://github.com/NVIDIA/NVSentinel/issues/2170)). The reporter's table, on a cluster of about 350 GPU nodes, grew by about 2.6M rows a day and reached about 42M rows and 90 GB. Change stream polls became slow. The Aurora instance ran out of memory and restarted while index builds ran alongside the changelog load.

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

The store-client PostgreSQL provider runs a periodic, batched cleaner. It deletes a changelog row only when the row is older than the configured retention and before the oldest consumer position in `resume_tokens`, compared as `(changed_at, id)`. Every token counts in that bound, also a token that has not changed for a long time; such a token raises an alert, but it is never ignored. The cleaner is off by default in the release that adds it.

## Implementation

- **Where it runs.** `NewPostgreSQLStore` starts the cleaner after table setup when retention is set, and `Close` stops it. Fault Quarantine, Node Drainer, Fault Remediation and the Health Events Analyzer open the store this way. Platform connectors and the event exporter use the client factory, which does no table setup, so they do not run the cleaner. The event exporter's watcher saves a token like the others, so the bound includes it.
- **One run at a time.** A run reserves a connection and takes `pg_try_advisory_lock` on a key of its own, separate from the setup lock. If another session holds the lock, the run does nothing. The batches run on the reserved connection. The run releases the lock with a new, short context. If the release fails, it closes the session instead of returning the connection to the pool.
- **The bound.** Each run reads all rows of `resume_tokens` and computes:
  - `cutoff = now() - retention`;
  - `oldest`, the smallest `(timestamp, eventID)` among the tokens, compared as a pair.

  A row can be deleted only if `changed_at < cutoff` and `(changed_at, id) < oldest`. Both conditions are part of the delete statement itself, so every deleted row is checked.

  If a token cannot be parsed, the run deletes nothing and reports an error. If there are no tokens, only the age condition applies, because no consumer has a position to protect.
- **Stale tokens stay in the bound.** A token whose `last_updated` is older than the retention still counts in `oldest`. The cleaner still deletes the rows before it, and it reports the token in a metric and a warning that names the client.
  - An old `last_updated` does not prove that a consumer is gone. A consumer that is live but stalled, or that cannot save its token, needs every row after its position.
  - A removed component leaves its token, which holds back the cleanup from that position on. The operator deletes that row from `resume_tokens` only when the component is gone for good. The docs describe this step, and warn not to delete the token of a component that still runs.
- **Legacy tokens.** A token in the old format, with an `eventID` and no `timestamp`, has no pair. The cleaner looks up the `changed_at` of that id. If the row does not exist, the run deletes nothing and reports an error.
- **Batched deletes.** For each table that `createChangeTriggers` watches, the cleaner deletes in batches of 10,000 rows by default, with one transaction per batch:

  ```sql
  DELETE FROM datastore_changelog
  WHERE id IN (
    SELECT id FROM datastore_changelog
    WHERE table_name = $1 AND changed_at < $2 AND (changed_at, id) < ($3, $4)
    ORDER BY changed_at, id
    LIMIT $5)
  ```

  This is a range scan on `idx_changelog_resume_all (table_name, changed_at, id)` from [PR #2233](https://github.com/NVIDIA/NVSentinel/pull/2233). It starts at the oldest row of the table. It stops at the limit, or at the first row that is too new. When there is nothing to delete, it is a short index range read: 8 buffers on a 5M-row changelog in the measurement in the PR.

  A run stops when a batch deletes fewer rows than the limit, when it reaches a limit on batches per run (100 by default), when the context ends, or when a batch fails.

  If `idx_changelog_resume_all` is missing or INVALID, the run deletes nothing and reports it, instead of scanning the table. `ensureChangelogResumeIndex` from PR #2233 builds or repairs the index at the next start of a component.
- **Configuration.** Store-client reads environment variables in the same way as `DATASTORE_MAX_CONNECTIONS`:
  - `DATASTORE_CHANGELOG_RETENTION`: a duration. Unset or `0` turns the cleaner off. Values below `24h` are rejected at start.
  - `DATASTORE_CHANGELOG_CLEANUP_INTERVAL`: default `1h`.
  - `DATASTORE_CHANGELOG_CLEANUP_BATCH_SIZE`: default `10000`.
  - `DATASTORE_CHANGELOG_CLEANUP_MAX_BATCHES`: default `100` per run.

  The chart sets them from new keys under `global.datastore.changelogCleanup`. These are new keys with defaults that keep current behaviour.
- **Metrics.** The cleaner adds new metrics only:
  - `datastore_changelog_cleanup_deleted_rows_total`;
  - `datastore_changelog_cleanup_runs_total` with a `result` label (`deleted`, `nothing_to_delete`, `skipped_locked`, `skipped_no_index`, `error`);
  - `datastore_changelog_cleanup_last_success_timestamp_seconds`;
  - `datastore_changelog_cleanup_oldest_protected_age_seconds`, the age of `oldest`, with a `stale_tokens` gauge. An alert on these shows when one consumer holds back the cleanup.
- **Space.** A delete makes space reusable, but it does not shrink the table file. The cleaner relies on autovacuum and does not run `VACUUM`. `VACUUM` cannot run in a transaction, and only the table owner, the database owner or a superuser can run it. For an existing large table, the docs tell operators to run `pg_repack` once after they turn the cleaner on. `VACUUM FULL` also works, but it needs an exclusive lock and free disk space equal to the new table size, so it belongs in a maintenance window.
- **Tests.**
  - With sqlmock, in the style of the setup-lock and index tests:
    - the bound is the smallest token as a pair, not the smallest `eventID`;
    - a stale token still holds back the cleanup and is reported;
    - nothing older than the bound means no `DELETE` (the healthy case);
    - a held lock, a missing index or an unreadable token means no `DELETE`;
    - the batch loop stops on a short batch, on the batch limit, on `ctx.Done()`, and on an error.
  - Against a real PostgreSQL 16, because sqlmock does not check SQL semantics:
    - a row with a lower id but a newer `changed_at` than the cutoff stays;
    - a row after a consumer's position in `(changed_at, id)` order stays, even when its id is lower than the token's `eventID`;
    - the query plan is an index range scan on a table with millions of rows.

## Rationale

- **Protects every saved position.** Every deleted row is checked against both the age and the oldest saved position, in the same order the poll uses. Stale tokens stay in the bound, because NVSentinel cordons and drains nodes from these events, and a silent gap is worse than a larger table. The limits in the Notes section still apply.
- **Bounded work.** Each batch and each run has a fixed upper limit, and the index range stops at the first row that is too new. A run with nothing to delete costs a short index range read for each watched table.
- **Follows existing patterns.** The setup lock, the CONCURRENTLY index builds of ADR-052 and the `DATASTORE_*` variables already work this way in store-client. No module calls another module.
- **Safe to upgrade.** It is off by default and adds new keys, metrics and variables only. Older components on the same database keep their tokens, so the bound protects them too.

## Consequences

### Positive

- When every consumer saves a recent position, the changelog size is bounded by the retention and the write rate, instead of growing for the life of the cluster.
- With a bounded table, change stream polls, `MarkProcessed` updates and index builds do not slow down as the cluster ages.
- Operators can see the cleanup in metrics, and see which consumer holds it back.

### Negative

- The default does not fix the problem. Operators must turn the cleaner on.
- One consumer that falls behind, or a token left by a removed component, holds back the cleanup for both tables until it catches up or an operator removes the token.
- A consumer whose filter matches no new events does not save a new position, because empty polls do not save one (ADR-054 describes such quiet consumers). Its old position holds back the cleanup even while it is healthy and caught up.
- Each component that opens the store tries the lock once per interval, though only one run does the work.
- The table file does not shrink without an operator step.
- The cleaner depends on the index from PR #2233, and does nothing without it.

### Mitigations

- The docs and the PostgreSQL values file recommend a retention. A later release can change the default after this one has been used in practice.
- The `oldest_protected_age_seconds` and `stale_tokens` metrics make a held-back cleanup visible, and the docs give the steps to find and remove the token of a removed component.
- For quiet consumers, a follow-up can save a position after an empty poll, up to the newest row the poll has read. That changes the checkpoint protocol, so it is an open question below, not part of this decision.
- The lock attempt is one cheap query per component per interval.
- The docs describe the one-time `pg_repack` step.
- The cleaner reports `skipped_no_index`, and the existing setup rebuilds the index at the next start.

## Alternatives Considered

### Helm CronJob

**Rejected** because the job would need its own image and schedule, plus a copy of the components' database connection, Secret and TLS settings in the chart, and those must stay in step for in-cluster and external databases. A job can run the same SQL, so this is a question of operations, not of correctness. The provider already has the connection, and its metrics come from the components that the consumers belong to.

### Call `cleanup_changelog()`

**Rejected** because it deletes by `processed` alone, which can drop rows another consumer has not read. It deletes in one unbounded statement, and the components do not create it. Open PR #1777 moves it into a migration, and nothing calls it there either.

### Delete rows in `MarkProcessed`

**Rejected** because one consumer cannot know which rows the other consumers still need. It would also add delete work and lock waits to the hot path of every consumer.

### Ignore tokens that are not updated within the retention

**Rejected** because an old `last_updated` does not prove a consumer is gone. A live consumer that is stalled, or that fails to save its token, would lose the rows after its position. Failing closed and alerting keeps the decision with an operator.

### Partition the changelog by time

**Rejected for now** because it changes the persisted schema and needs a migration of existing tables, which belongs with the versioned migrations proposed in #1777. Dropping a partition is the cheapest delete, so partitioning can replace the batched delete later. The bound in this ADR still applies to it.

## Notes

- Limits of the bound:
  - **Long write transactions.** `changed_at` is the start time of the writing transaction, not its commit time. A write transaction that stays open for longer than the retention commits a row that is already older than the retention, and possibly before every saved position. The poll does not return such a row today, but the NOTIFY path can still fetch it by id. The cleaner could delete it first. This needs a write transaction open for at least 24 hours, the minimum retention. The docs recommend `idle_in_transaction_session_timeout` and a statement timeout on the NVSentinel database role, which rule it out. This ADR accepts that risk; maintainers can reject that choice.
  - **Saved positions are read positions.** A watcher advances its position in memory when it reads an event, before filtering and delivery. `MarkProcessed` saves the current position, which can be ahead of the event it acknowledges. So a saved position can be ahead of an event that is not yet handled, and a restart can skip or replay events today. The cleaner uses the saved positions as they are. It does not change this behaviour, and it does not make a saved position an acknowledgement.
- Open questions for maintainers:
  - Should the cleaner be on by default, with a 7-day retention to match `cleanup_changelog()`? This ADR proposes off first.
  - Is "an operator removes the token" the right recovery for a removed component? Or should a component remove its own token when it is uninstalled?
  - Should a watcher save a position after an empty poll, so that a quiet consumer stops holding back the cleanup? This changes the checkpoint protocol of every PostgreSQL consumer.
  - Is the long-transaction risk above acceptable with the recommended timeouts, or should the cleaner also require something stronger, such as `track_commit_timestamp`?
- Non-goal: the retention of the documents in `health_events` and `maintenance_events`. Only the change feed is in scope.
- Dependency: the implementation lands after [PR #2233](https://github.com/NVIDIA/NVSentinel/pull/2233).

## References

- [Issue #2170](https://github.com/NVIDIA/NVSentinel/issues/2170): the PostgreSQL changelog is never cleaned up
- [ADR-052](052-deployment-platform-connector.md): precedent for index builds under the setup lock
- [ADR-054](054-changestream-lag-metrics.md): change stream consumer lag metrics
- [PR #1777](https://github.com/NVIDIA/NVSentinel/pull/1777): versioned PostgreSQL schema migrations
- [PR #2233](https://github.com/NVIDIA/NVSentinel/pull/2233): changelog resume index and row comparison in the poll
