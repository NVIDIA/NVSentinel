# Proposed PostgreSQL changelog retention protocol

Status: Proposed. This supports [ADR-068](designs/068-postgresql-changelog-retention.md); no cleaner implementation is delivered yet. Maintainer acceptance and the ADR acceptance tests are prerequisites for release.

## One run and a previous-snapshot guard

A run reserves a connection and takes `pg_try_advisory_lock` on its own key, separate from setup. Other owners skip a held lock. Batches use that connection. Release the lock with a fresh, short context; discard the underlying session if release fails.

Add an optional `changelog_cleanup_state` table containing one guard record: the full `xid8` horizon and its database observation time. Store setup or operator provisioning creates it. Existing event and token schemas remain unchanged; old components ignore this table.

Under the advisory lock:

1. Read the previous guard. If its observation is less than one configured cleanup interval old, skip without replacing it. Multiple owners must not continually reset the waiting interval.
2. Obtain a new horizon with `pg_snapshot_xmin(pg_current_snapshot())`, and record its observation time after acquisition using the database clock.
3. Save that new guard before deletion. Use only the previous, sufficiently old guard for this run. The first run seeds metadata and deletes nothing.

A failed guard read or write means no deletion. A missing, invalid, future-dated or unusably old guard causes a reported seed-only run. A crash after saving the new guard can delay cleanup, but cannot shorten the required waiting period. Replacement owners use the same persisted state. No long-lived exported snapshot or transaction is kept open between runs.

## Wraparound-safe comparison

Row `xmin` is a 32-bit `xid`; snapshot horizons are full `xid8` values. Do not compare their integer or text casts directly.

Each delete batch starts a new, short `REPEATABLE READ` transaction and first obtains `pg_current_xact_id()`. This fixes its snapshot and assigns the xid that anchors relative ages. The transaction only deletes changelog rows; it must not insert or update them. Validate that the previous full horizon is no later than this id and fewer than `2^31` transactions behind it. Otherwise delete nothing and reseed for a later run. With that checked window, use:

```sql
(age(row_version.xmin) > age($6::xid8::xid)
 OR age(row_version.xmin) < 0)
```

Strict inequality protects the previous horizon. Frozen tuples can retain an original `xmin` more than `2^31` transactions old, whose signed age is negative. They must not permanently block the oldest candidate prefix. For versions visible to this fixed snapshot, negative age identifies old versions: later concurrent versions are invisible. The cleaner creates no newer target versions itself. This reasoning requires repeatable read; read committed can expose a later commit with negative age relative to the assigned batch xid.

An update or delete after snapshot acquisition can cause SQLSTATE `40001`. Roll back the entire batch, report the conflict and end the run. Do not fall back to read committed or reuse an aborted transaction. Count deleted rows only after commit succeeds. A later scheduled run uses a fresh transaction. Validate freezing, signed-age boundaries, concurrent updates and long transactions before release; synthetic xid arithmetic alone does not prove physical wraparound behavior.

## Consumer bound and idle checkpoints

A run computes `cutoff = now() - retention` and the smallest saved `(timestamp, eventID)` pair across all clients. Every saved position counts, including stale ones. An unreadable token prevents deletion. For a legacy token with only `eventID`, obtain its timestamp from the changelog; a missing row prevents deletion. An empty token table defers cleanup. The cleaner has no consumer registry and cannot detect missing consumer rows in a nonempty table. Protection therefore requires the startup invariant and operator rollout gate below. The age and previous-snapshot guards remain unchanged.

When retention is enabled, every error-returning PostgreSQL watcher construction path must initialize its position before returning a watcher. This includes all three paths:

- `PostgreSQLWatcherFactory.CreateChangeStreamWatcher` in `watcher_factory.go`;
- `PostgreSQLDataStore.NewChangeStreamWatcher` in `datastore.go`;
- `PostgreSQLDatabaseClient.NewChangeStreamWatcher` in `database_client.go`.

Use one shared initialization helper so these paths cannot diverge. Constructors return initialization errors. Move resume-position initialization out of `Start` for this mode. `Start(ctx)` keeps its existing signature and has no return value. It uses the prepared position without reloading it. `Start` performs no new checkpoint writes. An uninitialized watcher must report the failure. It must launch no polling or notification readers.

Where supported, run `ResetResumeTokenOnStartIfConfigured` before position initialization. A successful reset removes the old token first. The absent-row initialization then saves a fresh pair before the watcher can poll. A reset failure returns from the constructor without starting readers. `PostgreSQLDatabaseClient.NewChangeStreamWatcher` currently does not invoke this reset helper. Its initialization requirement still applies.

A new watcher means no saved row exists for its client name. Only `sql.ErrNoRows` from the token-row lookup permits an initial write. Reserve a database connection for initialization. Read `clock_timestamp()` on that connection and pair it with `eventID` 0. Persist this exact pair on the same connection before returning the watcher. Use the persisted pair as its read position. Never use the application clock for this initial timestamp. If a concurrent insert wins, load and validate that row instead of overwriting it. Clock-read or persistence failures return through the construction path and prevent reading.

Load, parse or legacy-token conversion failures must return through the construction path when retention is enabled. They must prevent reading and leave existing tokens unchanged. In particular, `convertOldTokenToTimestamp` must propagate lookup failures, including missing changelog rows. It must never replace a legacy token with `(time.Now(), 0)`. The preserved legacy token keeps cleanup blocked under the missing-row rule above.

Legacy conversion failures recur on each restart until an operator repairs or resets the token. Report the failure in a log that names the client. The cleanup warning for an unresolved legacy token must also name the client. The stale-token metric and warning identify missing checkpoint progress after the configured threshold. Cleanup remains blocked while the unresolved token persists.

For recovery, disable cleanup before removing an active client's token. Use the existing reset-on-start configuration where supported, or delete that client's `resume_tokens` row. Restart the consumer through enabled initialization. Accept that it resumes from the new database-clock start pair. This can skip events after its old position and before the new pair. Verify its fresh saved row before re-enabling cleanup. Keep the rollout gate satisfied for every other consumer. The database-client path requires manual deletion unless its caller already provides a reset mechanism. The shared reset helper skips `event-exporter`.

Initial rows with `eventID` 0 use the same `last_updated` stale-token threshold as other rows. Initial persistence does not establish lag evidence; ADR-054 lag stays unknown until a poll records progress or an empty batch.

When retention is enabled, `MarkProcessed` must verify the acknowledged event's `changed_at` before checkpoint persistence. A lookup error or missing row must return an error and write no checkpoint. Never substitute `time.Now()` or derive an acknowledgment position from a missing row. Empty-token acknowledgments must follow the delivery-prefix rules below rather than persist the read head. These are required, backward-compatible runtime changes for the implementation PR. The caller API and token schema remain unchanged. Retention disabled preserves existing startup and acknowledgment fallback behavior.

Track handed-out deliveries in the provider before enqueueing them. Queued events count as in flight. Interpret `MarkProcessed(token)` according to the existing cumulative delivery-prefix contract, without treating an id prefix as timestamp order. Later deliveries remain pending. Empty or unknown tokens cannot certify outstanding work. Acknowledgment persistence must not jump to the read head while later work remains pending. Propagate checkpoint write failures.

An empty filtered snapshot does not prove that its newest pair is safe. An older open transaction can later commit a matching row below that pair. The deletion guard preserves that row but does not make a restarted poll read it.

Use a conservative idle-checkpoint fence on a dedicated connection:

1. Outside the read transaction, clear any cached activity snapshot and record database `clock_timestamp()` as a fence time.
2. Read fresh `pg_stat_activity` for every other session in this database. If any session has an open transaction, including one without an assigned xid, defer idle advancement. Include read-only and aborted transactions. Hidden activity, disabled tracking or a failed inspection also defers it. Full visibility requires a shared role or an operator-managed statistics privilege; never grant privileges automatically.
3. After the activity inspection, read `pg_prepared_xacts`. Any prepared transaction in this database defers advancement. Its `prepared` time is not its transaction start. This order also covers a transaction that prepares during inspection.
4. Only then start a new, short, read-only repeatable-read transaction. Select the newest actual table pair with `changed_at` strictly before the fence time. Verify that the pipeline has no matching rows between the current read position and that candidate. Save no newer boundary and never move backward. A nonempty result follows normal delivery.

The fence time limits the candidate; it is never itself a resume token. Pre-fence transactions must have ended before inspection or block advancement. Transactions starting during inspection remain beyond the fence under the datastore's existing timestamp-ordering assumption. Database clock regressions or untrusted/custom `changed_at` values invalidate that assumption: defer idle advancement and report the condition; deployments that cannot ensure timestamp ordering need a separately accepted commit-ordered cursor design. A clock anomaly cannot be repaired by the `xmin` deletion guard.

This conservative rule can defer idle progress on a busy database or behind one long transaction. Retry at the configured interval without spinning. Missing privileges or tracking prevent idle advancement, not ingestion or ordinary acknowledged delivery. Keep the old token, retain its cleanup bound and report the missing prerequisite. Release tests must cover hidden activity, xid-less transactions, prepare transitions, no-listener commits and restart.

Serialize polling, delivery registration, acknowledgment and checkpoint selection. Persist the verified boundary only after all handed-out deliveries are acknowledged, rechecking under that serialization. A failed or ambiguous check leaves the durable position unchanged. Coalesce idle writes to at most once per cleanup interval per watcher. Refresh `last_updated` even when the pair is unchanged on an idle table.

This changes only provider bookkeeping; the caller API and `{timestamp, eventID}` token shape stay unchanged. Existing consumer lag semantics from ADR-054 stay unchanged. Stale tokens then identify missing checkpoint progress, not normal filtering, though database or checkpoint failures can also cause it. Keep them in the cleanup bound and alert; only an operator can remove a retired consumer's token.

Keep cleanup disabled until all store-owning components run the new version with initial-position persistence and verified, acknowledgment-safe checkpointing. Operators must confirm every active watcher has initialized a saved position before deletion begins. Enabling retention requires restarting watchers through the enabled initialization path, including watchers previously started with retention disabled. Old-version watchers can lack an initial row or persist a read head ahead of work. A nonempty token table cannot prove rollout completeness. If an active watcher's token is removed, operators must disable cleanup until its position is restored safely. Older writers remain supported by the `xmin` guard. Disable cleanup before downgrading a reader. Operators must confirm reader rollout; the unchanged token format cannot identify an old reader automatically. This prerequisite must appear in upgrade documentation and acceptance tests. Default-disabled retention preserves existing operation.

## Bounded deletes

Use `idx_changelog_resume_all (table_name, changed_at, id)` from [PR #2233](https://github.com/NVIDIA/NVSentinel/pull/2233). A missing or invalid index prevents deletion. Bound candidate scanning before applying the transaction guard:

```sql
WITH candidates AS MATERIALIZED (
  SELECT id FROM datastore_changelog
  WHERE table_name = $1 AND changed_at < $2
    AND (changed_at, id) < ($3, $4)
  ORDER BY changed_at, id LIMIT $5
)
DELETE FROM datastore_changelog AS row_version
USING candidates
WHERE row_version.id = candidates.id
  AND row_version.changed_at < $2
  AND (row_version.changed_at, row_version.id) < ($3, $4)
  AND (age(row_version.xmin) > age($6::xid8::xid)
       OR age(row_version.xmin) < 0);
```

Here `$6` is the previous full horizon, validated inside the repeatable-read batch. With no consumer tokens, execute no delete; never omit the pair conditions. Both age branches remain subject to retention and consumer bounds. The guard applies to a snapshot-visible target version; concurrent changes must cause rollback, not a newer-version recheck under read committed. The candidate limit bounds scans even when the guard protects many rows. Stop on a short or blocked batch, the batch-count limit, cancellation or error; do not spin on a protected prefix.

A processed-row update changes `xmin`, which may delay deletion conservatively. Earlier age-and-position benchmarks exclude the guard and do not establish the final query's performance.

## Checkpoint acceptance criteria

The implementation PR must add these tests before retention can be released:

- With retention enabled, a failed `changed_at` lookup returns an error and writes no checkpoint. Cover both database errors and missing rows. Assert the previous durable position remains unchanged.
- Test each of the three named construction paths for initial persistence and failure propagation. Include the database-client path used through `ClientFactory`. Each path saves the exact database-clock start pair before returning the watcher. Cover application-clock skew and `eventID` 0. Restart resumes from the saved pair. `Start` neither resets it nor writes a new checkpoint. No acknowledgment is required for initial persistence.
- Where supported, reset-on-start runs before absent-row initialization. The reset consumer has a fresh saved pair before its first poll. A failed reset returns a construction error. Verify recovery by manual token deletion where reset-on-start is unsupported.
- Failed clock reads or initial-position writes return construction errors and prevent polling and delivery. An uninitialized `Start` launches no readers. Concurrent initial inserts preserve the winning row.
- Only a missing token row permits initial insertion. Load errors, malformed tokens and legacy conversion failures return construction errors without reading or writing checkpoints. Cover missing legacy changelog rows and verify existing tokens remain unchanged. The preserved legacy token blocks cleanup.
- Empty token tables cause no deletion. A nonempty table does not certify rollout completeness; verify the operator prerequisite in upgrade documentation.
- A consumer that starts before its first acknowledgment still protects unread rows with its initial position. Verify the age and previous-snapshot guards remain required.
- Cleanup remains disabled during mixed-version rollout, including old watchers without initial rows. Enable it only after all store-owning components support these rules and active watchers complete enabled initialization. Disable cleanup before a reader downgrade.
- Retention disabled preserves existing behavior. Existing token schemas and caller interfaces remain compatible.

## Space reclamation

A delete makes space reusable, but it does not shrink the table file. The cleaner relies on autovacuum and does not run `VACUUM`. `VACUUM` cannot run in a transaction, and only the table owner, the database owner or a superuser can run it. For an existing large table, the docs tell operators to run `pg_repack` once after they turn the cleaner on. `VACUUM FULL` also works, but it needs an exclusive lock and free disk space equal to the new table size, so it belongs in a maintenance window.

## References

- [PostgreSQL snapshots and transaction ids](https://www.postgresql.org/docs/16/functions-info.html#FUNCTIONS-TXID-SNAPSHOT)
- [System columns](https://www.postgresql.org/docs/16/ddl-system-columns.html)
- [Freezing and wraparound](https://www.postgresql.org/docs/16/routine-vacuuming.html#VACUUM-FOR-WRAPAROUND)
- [Fresh activity statistics and visibility](https://www.postgresql.org/docs/16/monitoring-stats.html#MONITORING-STATS-VIEWS)
- [Prepared transactions](https://www.postgresql.org/docs/16/view-pg-prepared-xacts.html)
- [Repeatable read and serialization failures](https://www.postgresql.org/docs/16/transaction-iso.html#XACT-REPEATABLE-READ)
- [PostgreSQL signed xid-age implementation](https://github.com/postgres/postgres/blob/REL_16_STABLE/src/backend/utils/adt/xid.c)
