# PostgreSQL schema migrations

This directory is the only source of PostgreSQL DDL for NVSentinel. Apply the
SQL files in filename order before starting an application release that
requires them.

The migrations are intentionally not executed by NVSentinel applications or
Helm. A database administrator, Terraform deployment, or database release
pipeline must apply them with a DDL-capable role. The Tilt development
environment applies them with `tilt/apply-postgres-migrations.sh`.
Application roles should have DML permissions only, plus read access to
`nvsentinel_schema_migrations`.

Example:

```bash
for migration in ./*.sql; do
  psql -v ON_ERROR_STOP=1 "$DATABASE_URL" -f "$migration"
done
```

Each file:

- has a monotonically increasing five-digit version prefix;
- executes inside a transaction, unless it carries the
  `-- nvsentinel:no-transaction` marker;
- records its version in `nvsentinel_schema_migrations` only after its DDL
  succeeds;
- is forward-only and must not be edited after release.

A migration carries the `-- nvsentinel:no-transaction` marker only when it
needs a statement that cannot run in a transaction block, such as
`CREATE INDEX CONCURRENTLY`. Each of its statements commits on its own, so
apply it with `ON_ERROR_STOP=1`. Its last statement records the version, and
a check before that statement fails if the DDL did not complete. The file
header explains how to recover from a failed run.

`00003` builds the deployment platform connector's idempotency index
`CONCURRENTLY`, so health event inserts continue during the build. If an
older release is still running when you apply it, that release can abort
the build. In that case `00003` fails on the INVALID index; drop the index as
the file header describes and apply `00003` again.

To change the schema, add the next migration file and update
`RequiredSchemaVersion` in `../schema_version.go`. Do not add DDL to Go code or
Helm values.

Databases created by earlier releases, where the datastore created its tables
at startup, already have the version 1 layout. Apply all migrations from
`00001` onward; `00001` adopts the existing tables in place and keeps their
data. If a pre-existing table has a different layout, `00001` fails before it
changes anything and names the incompatible column.

Inspect an existing database with:

```sql
SELECT version, description, applied_at
FROM nvsentinel_schema_migrations
ORDER BY version;
```

Migration versioning does not make destructive rollbacks safe. Prefer
expand/contract changes and use backup recovery or a forward-fix migration when
data has already changed.
