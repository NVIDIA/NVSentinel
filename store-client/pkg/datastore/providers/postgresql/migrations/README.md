# PostgreSQL schema migrations

This directory is the only source of PostgreSQL DDL for NVSentinel. Apply the
SQL files in filename order before starting an application release that
requires them.

The migrations are intentionally not executed by NVSentinel applications,
Helm, or Tilt. A database administrator, Terraform deployment, or database
release pipeline must apply them with a DDL-capable role. Application roles
should have DML permissions only, plus read access to
`nvsentinel_schema_migrations`.

Example:

```bash
for migration in ./*.sql; do
  psql -v ON_ERROR_STOP=1 "$DATABASE_URL" -f "$migration"
done
```

Each file:

- has a monotonically increasing five-digit version prefix;
- executes inside a transaction;
- records its version in `nvsentinel_schema_migrations` only after its DDL
  succeeds;
- is forward-only and must not be edited after release.

To change the schema, add the next migration file and update
`RequiredSchemaVersion` in `../schema_version.go`. Do not add DDL to Go code or
Helm values.

Inspect an existing database with:

```sql
SELECT version, description, applied_at
FROM nvsentinel_schema_migrations
ORDER BY version;
```

Migration versioning does not make destructive rollbacks safe. Prefer
expand/contract changes and use backup recovery or a forward-fix migration when
data has already changed.
