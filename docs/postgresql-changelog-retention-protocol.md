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

Each delete batch starts a short transaction and first obtains `pg_current_xact_id()`. Validate that the previous full horizon is no later than this id and fewer than `2^31` transactions behind it. Otherwise delete nothing and reseed for a later run. With that checked window, use PostgreSQL's relative transaction ages:

```sql
age(row_version.xmin) > age($6::xid8::xid)
```

The assigned batch transaction id anchors both ages consistently. Strict inequality protects the transaction at the snapshot boundary. Future-looking or ambiguous row ages stay protected. Old frozen rows may be retained conservatively; freezing does not justify a naive numeric comparison. Validate wraparound, freezing, subtransactions and long transactions against real PostgreSQL before release.

## Consumer bound and idle checkpoints

A run computes `cutoff = now() - retention` and the smallest saved `(timestamp, eventID)` pair across all clients. Every saved position counts, including stale ones. An unreadable token prevents deletion. For a legacy token with only `eventID`, obtain its timestamp from the changelog; a missing row prevents deletion. With no tokens, omit only the consumer-position bound; retain the age and previous-snapshot guard.

Track handed-out deliveries in the provider before enqueueing them. Queued events count as in flight. Interpret `MarkProcessed(token)` according to the existing cumulative delivery-prefix contract, without treating an id prefix as timestamp order. Later deliveries remain pending. Empty or unknown tokens cannot certify outstanding work. Acknowledgment persistence must not jump to the read head while later work remains pending. Propagate checkpoint write failures.

When a filtered poll is empty, its existing read position may still be old. In one short, read-only repeatable-read transaction, capture the newest unfiltered pair for that table and verify that the pipeline has no matching rows through that pair. The shared index supports the tail lookup. Never substitute wall-clock time for a read boundary. A nonempty result follows normal delivery, not idle checkpointing.

Serialize polling, delivery registration, acknowledgment and checkpoint selection. Persist the verified boundary only after all handed-out deliveries are acknowledged, rechecking under that serialization. A failed or ambiguous check leaves the durable position unchanged. Coalesce idle writes to at most once per cleanup interval per watcher. Refresh `last_updated` even when the pair is unchanged on an idle table.

This changes only provider bookkeeping; the caller API and `{timestamp, eventID}` token shape stay unchanged. Existing consumer lag semantics from ADR-054 stay unchanged. Stale tokens then identify missing checkpoint progress, not normal filtering, though database or checkpoint failures can also cause it. Keep them in the cleanup bound and alert; only an operator can remove a retired consumer's token.

Enable cleanup only after every consumer uses acknowledgment-safe checkpointing. Older writers remain supported by the `xmin` guard, but older readers can still persist a read head ahead of work. Disable cleanup before downgrading a reader. Operators must confirm reader rollout; the unchanged token format cannot identify an old reader automatically. This prerequisite must appear in upgrade documentation and acceptance tests. Default-disabled retention preserves existing operation.

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
  AND age(row_version.xmin) > age($6::xid8::xid);
```

Here `$6` is the previous full horizon, validated inside the batch transaction. With no consumer tokens, omit the pair conditions. Apply the guard to the current target row version, including after concurrent updates. The candidate limit bounds scans even when the guard protects many rows. Stop on a short or blocked batch, the batch-count limit, cancellation or error; do not spin on a protected prefix.

A processed-row update changes `xmin`, which may delay deletion conservatively. Earlier age-and-position benchmarks exclude the guard and do not establish the final query's performance.

## Space reclamation

A delete makes space reusable, but it does not shrink the table file. The cleaner relies on autovacuum and does not run `VACUUM`. `VACUUM` cannot run in a transaction, and only the table owner, the database owner or a superuser can run it. For an existing large table, the docs tell operators to run `pg_repack` once after they turn the cleaner on. `VACUUM FULL` also works, but it needs an exclusive lock and free disk space equal to the new table size, so it belongs in a maintenance window.

## References

- [PostgreSQL snapshots and transaction ids](https://www.postgresql.org/docs/16/functions-info.html#FUNCTIONS-TXID-SNAPSHOT)
- [System columns](https://www.postgresql.org/docs/16/ddl-system-columns.html)
- [Freezing and wraparound](https://www.postgresql.org/docs/16/routine-vacuuming.html#VACUUM-FOR-WRAPAROUND)
