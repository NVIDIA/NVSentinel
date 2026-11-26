# PostgreSQL changelog cleaner: experimental testing

ADR-068 and its release protocol remain Proposed. The guarded cleaner and its safety tests exist. The remaining release prerequisites are tracked in #2170.

## Temporary enablement gate

Deletion requires both valid cleanup settings and `DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL=true`. The acknowledgment must be exactly `true`. Options cannot bypass this environment gate.

Without acknowledgment, configured retention logs one warning per pool owner and starts no cleaner. Invalid settings also warn and disable cleanup. Startup and ingestion continue.

The temporary gate stays until the idle-checkpoint fence, rollout documentation, and acceptance tests ship (#2170). Until then, use the acknowledgment only for experimental testing. A nonempty token table cannot prove that every active consumer has a saved position.

| Setting | Default | Requirement |
|---|---|---|
| `DATASTORE_CHANGELOG_RETENTION` | `0` | A Go duration of at least `24h`; `0` disables cleanup. |
| `DATASTORE_CHANGELOG_RETENTION_EXPERIMENTAL` | unset | Exactly `true` for experimental deletion. |
| `DATASTORE_CHANGELOG_CLEANUP_INTERVAL` | `1h` | A Go duration of at least `1h`. |
| `DATASTORE_CHANGELOG_CLEANUP_BATCH_SIZE` | `10000` | A positive integer. |
| `DATASTORE_CHANGELOG_CLEANUP_MAX_BATCHES` | `100` | A positive integer. |

The one-hour minimum preserves the protocol's default NOTIFY catch-up window, including when owners use different intervals. The previous snapshot must age one configured interval. Subsecond or minute-scale windows give long writers little catch-up time. Statement and idle-in-transaction timeouts do not bound total transaction lifetime. One hour is a conservative operational floor, not a maximum writer lifetime. The snapshot guard still protects writers active at its observation.

Saved timestamps more than one second beyond the sampled database clock block deletion. The allowance covers small clock adjustments and concurrent saves between clock sampling and token reading. Larger skew cannot establish an acknowledged position. A warning names the client. No application clock supplies the cleaner's bound.

Tokens at the Unix epoch protect full replay. Quiet consumers can retain their initial position indefinitely until they acknowledge work or idle checkpoints ship (#2170). Stale positions remain protected. Warnings name these clients and explain the required progress. Do not remove a live consumer's token to make cleanup progress.

The pool owner starts the cleaner. Constructor context cancellation does not stop it. Owner `Close(ctx)` cancels and waits before closing the pool. Borrowed clients start no cleaner.

## Run the integration tests

Use a separate disposable PostgreSQL 16 database for each concurrent invocation. The cleaner advisory lock applies to the entire database, including runs in different schemas. The database must have no NVSentinel tables in `public`: `changelog_cleanup_state`, `datastore_changelog`, or `resume_tokens`. The suite checks this requirement before schema creation. The fixture keeps `public` in its search path for extension functions used by production table setup.

Set `NVSENTINEL_TEST_POSTGRES_DSN` through the process environment. Keep passwords out of command histories, files, and logs.

```bash
cd store-client
go test -race -tags=integration -count=1 -run '^TestChangelogCleaner_Integration_Safety$' -v ./pkg/datastore/providers/postgresql
```

Without the DSN, the tagged test skips cleanly. Without the tag, ordinary tests and lint exclude it. The test creates a unique schema, calls the production `createTables`, and removes its schema afterward. The database role needs schema and extension creation privileges. Use a disposable database because production setup includes extension creation.

The cases exercise production triggers and BIGSERIAL, tuple ordering, strict consumer boundaries, retention, pending acknowledgments, restart, malformed tokens, missing legacy rows, and empty token tables. An incomplete consumer set keeps cleanup inert through the temporary gate. A long writer remains protected by the previous snapshot. A synchronized concurrent update produces real SQLSTATE `40001` and rolls back the batch. Other cases check batch limits, short and blocked stopping, maintenance progress, and missing-schema deferral.

Tests age only the isolated guard's observation time to avoid a one-hour wait. They use the actual PostgreSQL snapshot horizon and production deletion SQL.

## Remaining release prerequisites

Implement the conservative idle fence and complete reader rollout, downgrade, and acceptance tests before removing the experimental gate. Add Helm settings, metrics, and operator guidance from the protocol.

Document guard-state reset after logical restore or blue/green migration. The stored xid horizon belongs to its original cluster. Document direct or session-pooled connections; session advisory locks require session affinity. Transaction pooling is unsuitable. One reserved connection can stall ingestion with `DATASTORE_MAX_CONNECTIONS=1`.

Document role privileges: `INSERT` and `UPDATE` on `changelog_cleanup_state`, `DELETE` on `datastore_changelog`, and reads for prerequisites and saved tokens. Store setup provisions the optional guard table even with cleanup disabled. Factory owners run no setup DDL.

Complete prepared-writer, subtransaction, physical freezing, signed-age, and million-row query-plan acceptance. Review a batch-size ceiling and remove the watcher's dormant application-clock timestamp fallback. Document conservative scheduling when a protected prefix stops a run. Review transient token-client owners and timed-out `Close` retries. Configuration warnings occur once per owner, so repeated factory creations can warn again. Keep consistent intervals across owners. Operator reclamation guidance must explain autovacuum, reusable space, and `pg_repack`.
