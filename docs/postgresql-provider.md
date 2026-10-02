# PostgreSQL Provider for NVSentinel

## Overview

NVSentinel supports PostgreSQL as an alternative datastore to MongoDB. The PostgreSQL provider offers similar functionality with a relational database backend, providing flexibility in deployment options.

## Features

- **Full feature parity** with MongoDB provider
- **TLS/SSL support** with client certificate authentication
- **Change stream emulation** using triggers and polling
- **JSONB storage** for flexible document-like data structures
- **Versioned SQL migrations** managed separately from application runtime
- **Production-ready** with connection pooling and error handling

## When to Use PostgreSQL

Choose PostgreSQL if you:
- Already have PostgreSQL infrastructure in your organization
- Prefer relational databases for operational reasons
- Need to leverage existing PostgreSQL expertise
- Want to use PostgreSQL-specific features or tools
- Have compliance requirements that favor PostgreSQL

## Architecture

### Database Schema

The PostgreSQL provider uses the following tables:

1. **health_events** - Stores GPU and system health events
2. **maintenance_events** - Stores CSP maintenance events
3. **datastore_changelog** - Tracks changes for change stream emulation
4. **resume_tokens** - Stores resume tokens for change stream clients

Each table uses JSONB columns to store document data while extracting key fields for indexing.

### Change Stream Emulation

PostgreSQL doesn't have native change streams like MongoDB. The provider emulates this using:

- **Triggers** on tables to capture INSERT/UPDATE/DELETE operations
- **Changelog table** to store change events
- **Polling mechanism** to detect new changes
- **Resume tokens** to track client progress and enable resumption

## Configuration

### Helm Chart Deployment

#### Using values-postgresql.yaml

Deploy NVSentinel with PostgreSQL using the provided values file:

```bash
helm install nvsentinel ./distros/kubernetes/nvsentinel \
  -f distros/kubernetes/nvsentinel/values-postgresql.yaml \
  --namespace nvsentinel \
  --create-namespace
```

#### Custom Configuration

Create your own values file with PostgreSQL settings:

```yaml
global:
  datastore:
    provider: "postgresql"
    connection:
      host: "your-postgresql-host"
      port: 5432
      database: "nvsentinel"
      username: "postgres"
      sslmode: "verify-full"
      sslcert: "/etc/ssl/client-certs/tls.crt"
      sslkey: "/etc/ssl/client-certs/tls.key"
      sslrootcert: "/etc/ssl/client-certs/ca.crt"

  # Disable MongoDB when using PostgreSQL
  mongodbStore:
    enabled: false

# Enable PostgreSQL subchart
postgresql:
  enabled: true
  auth:
    postgresPassword: "your-secure-password"
    database: "nvsentinel"
```

### TLS/SSL Configuration

PostgreSQL requires TLS for secure connections. The Helm chart automatically configures:

1. **cert-manager resources** for certificate generation
2. **Client certificates** for application authentication
3. **Server certificates** for PostgreSQL TLS
4. **InitContainers** to fix certificate permissions

#### SSL Modes

- `disable` - No SSL (not recommended for production)
- `require` - SSL required but no certificate verification
- `verify-ca` - Verify server certificate against CA
- `verify-full` - Full verification including hostname (recommended)

### Environment Variables

The provider recognizes these environment variables:

- `DATASTORE_PROVIDER` - Set to "postgresql"
- `DATASTORE_HOST` - PostgreSQL hostname
- `DATASTORE_PORT` - PostgreSQL port (default: 5432)
- `DATASTORE_DATABASE` - Database name
- `DATASTORE_USERNAME` - Database username
- `DATASTORE_SSLMODE` - SSL mode
- `DATASTORE_SSLCERT` - Client certificate path
- `DATASTORE_SSLKEY` - Client key path
- `DATASTORE_SSLROOTCERT` - CA certificate path
- `POSTGRESQL_CLIENT_CERT_MOUNT_PATH` - Certificate directory path

Alternatively, use `MONGODB_CLIENT_CERT_MOUNT_PATH` for backward compatibility - the SDK handles both.

## Development with Tilt

### Using PostgreSQL in Tilt

Set the environment variable before running Tilt:

```bash
export USE_POSTGRESQL=1
tilt up
```

This will:
- Load `values-tilt-postgresql.yaml` automatically
- Deploy PostgreSQL instead of MongoDB
- Generate PostgreSQL certificates via cert-manager
- Run the chart's PostgreSQL setup Job, which applies the versioned SQL migrations
- Configure all services to use PostgreSQL

### Switching Back to MongoDB

Simply unset the variable and restart:

```bash
unset USE_POSTGRESQL
tilt down
tilt up
```

### Development Values

The `values-tilt-postgresql.yaml` file includes:
- Single-replica PostgreSQL for faster startup
- Certificate-based authentication
- Control plane node selector for PostgreSQL pod

Tilt starts the services after the setup Job applies the migrations. Helm deployments use the same Job; see [Schema Management](#schema-management).

## Migration Guide

### From MongoDB to PostgreSQL

1. **Export data from MongoDB** (if preserving data):
   ```bash
   # Export collections to JSON
   mongoexport --uri="mongodb://..." --db=nvsentinel --collection=health_events --out=health_events.json
   mongoexport --uri="mongodb://..." --db=nvsentinel --collection=maintenance_events --out=maintenance_events.json
   ```

2. **Deploy PostgreSQL**:
   ```bash
   helm upgrade nvsentinel ./distros/kubernetes/nvsentinel \
     -f values-postgresql.yaml \
     --namespace nvsentinel
   ```

3. **Import data to PostgreSQL** (if needed):
   ```bash
   # Connect to PostgreSQL and import JSONB data
   # (Custom import script required based on your data)
   ```

### Testing the Migration

Verify PostgreSQL deployment:

```bash
# Check PostgreSQL is running
kubectl get statefulset nvsentinel-postgresql -n nvsentinel

# Check pods are ready
kubectl get pods -n nvsentinel | grep postgresql

# Verify services are using PostgreSQL
kubectl logs -n nvsentinel deployment/fault-quarantine | grep -i postgres
```

## Schema Management

### Versioned Migrations

The canonical schema is the ordered SQL migration set in [`distros/kubernetes/nvsentinel/files/postgresql-migrations`](../distros/kubernetes/nvsentinel/files/postgresql-migrations/). Each migration records its version in `nvsentinel_schema_migrations`.

NVSentinel applications never apply DDL. At startup, each application reads `nvsentinel_schema_migrations`. If the schema is older than the version it requires, the application waits up to five minutes and then exits.

### Setup Job (Default)

When `global.datastore.provider` is `postgresql`, the chart runs a setup Job named `<release>-postgresql-migrations-<hash>`. The Job applies the pending migrations in filename order and skips the versions that `nvsentinel_schema_migrations` already records. It runs on a fresh install and again on each upgrade that changes the migrations. The applications start at the same time and wait for the schema version that the Job records.

The Job uses the same connection settings as the applications. To run the migrations as a separate DDL role, put that role's credentials in a Secret with the keys `username` and `password`, and set `global.datastore.setupJob.adminSecret` to its name. Other settings are `global.datastore.setupJob.image` (any image with `psql`) and `global.datastore.setupJob.activeDeadlineSeconds` (default `3600`).

Check the Job:

```bash
kubectl get jobs -n nvsentinel -l app.kubernetes.io/name=postgresql-migrations
kubectl logs -n nvsentinel -l app.kubernetes.io/name=postgresql-migrations
```

### Applying Migrations Yourself

When a database team or a release pipeline owns the schema, disable the Job:

```yaml
global:
  datastore:
    setupJob:
      enabled: false
```

Then apply the files in filename order with a DDL-capable role, before you deploy an NVSentinel version that requires them. Use `psql` with `ON_ERROR_STOP`. Do not wrap the files in one transaction: `00003_health_event_idempotency_index.sql` builds an index `CONCURRENTLY`, which cannot run inside a transaction block.

`ON_ERROR_STOP` stops only the failed `psql` call, so the loop must stop too. Otherwise a later migration records a higher version over an incomplete schema.

```bash
for migration in distros/kubernetes/nvsentinel/files/postgresql-migrations/*.sql; do
  psql -X -v ON_ERROR_STOP=1 "host=<host> dbname=<database> user=<ddl-role>" -f "$migration" ||
    { echo "Migration failed: $migration" >&2; break; }
done
```

Every migration is idempotent, so you can apply the full set again. Query the applied version with:

```sql
SELECT version, description, applied_at
FROM nvsentinel_schema_migrations
ORDER BY version;
```

Migrations are forward-only. Use expand/contract changes for compatibility;
recover destructive changes from backup or with a forward-fix migration.

### Application Role Grants

The application role needs only these grants. Run them as the owner of the tables after the migrations:

```sql
GRANT CONNECT ON DATABASE nvsentinel TO nvsentinel_app;
GRANT USAGE ON SCHEMA public TO nvsentinel_app;
GRANT SELECT, INSERT, UPDATE ON health_events, maintenance_events TO nvsentinel_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON resume_tokens TO nvsentinel_app;
GRANT SELECT, INSERT, UPDATE ON datastore_changelog TO nvsentinel_app;
GRANT USAGE ON SEQUENCE datastore_changelog_id_seq TO nvsentinel_app;
GRANT SELECT ON nvsentinel_schema_migrations TO nvsentinel_app;
```

Replace `nvsentinel` and `nvsentinel_app` with your database and application role. The change-capture triggers run as the role that writes the event. Thus the application role needs `INSERT` on `datastore_changelog` and `USAGE` on its sequence. Without them, every health event and maintenance event write fails. In the bundled PostgreSQL, the application role owns the tables, so these grants are not necessary.

### Upgrading from a Release That Created the Schema at Startup

> **Breaking change.** Earlier releases created and changed the PostgreSQL schema when a component started. This release does not. Components exit if the schema version is older than the version they require.

- **Bundled PostgreSQL, or external PostgreSQL with the setup Job enabled (the default):** no action is necessary. The Job applies migrations `00001` to `00003` to the existing tables during the upgrade. The migrations keep existing data.
- **Setup Job disabled:** apply all migrations, starting with `00001`, before you upgrade. `00001` keeps the tables and data that earlier releases created, creates only the objects that are missing, and records version 1. It stops with an error if an existing table has an incompatible layout.
- **Restricted application role:** give the role the [grants above](#application-role-grants). Earlier releases needed DDL rights; this release does not.

Migration `00003` builds a unique index on the health event idempotency key. If duplicate keys exist, the build fails and leaves an `INVALID` index. The header of the migration file describes how to find the duplicates and apply the migration again.

## Performance Tuning

### Connection Pooling

The provider uses Go's `database/sql` package with connection pooling:

```go
// Default settings (can be tuned via environment variables)
MaxOpenConns: 25
MaxIdleConns: 25
ConnMaxLifetime: 5 minutes
```

### Indexing Strategy

Key indexes for performance:
- `node_name` - Queries by node
- `status` - Status filtering
- `created_at` / `event_timestamp` - Time-based queries
- `agent` / `csp` - Event source filtering
- `is_fatal` - Fatal event queries

### Query Optimization

The provider extracts frequently-queried fields from JSONB:
- Reduces JSONB query overhead
- Enables efficient B-tree index usage
- Maintains flexibility for additional JSONB fields

## Troubleshooting

### Connection Issues

**Problem**: "connection refused" or "no route to host"

**Solutions**:
- Verify PostgreSQL service is running: `kubectl get svc nvsentinel-postgresql -n nvsentinel`
- Check network policies aren't blocking access
- Verify DNS resolution: `nslookup nvsentinel-postgresql.nvsentinel.svc.cluster.local`

### Certificate Issues

**Problem**: "certificate verify failed" or permission denied

**Solutions**:
- Check cert-manager created certificates: `kubectl get certificate -n nvsentinel`
- Verify certificate secret exists: `kubectl get secret postgresql-client-cert -n nvsentinel`
- Check initContainer logs: `kubectl logs -n nvsentinel deployment/fault-quarantine -c fix-cert-permissions`
- Ensure certificate permissions are correct (key: 600, cert/ca: 644)

### SSL Mode Issues

**Problem**: "SSL required" or "SSL not supported"

**Solutions**:
- Verify `sslmode` is set correctly in values
- Check PostgreSQL server has TLS enabled
- Verify server certificate is valid

### Change Stream Not Working

**Problem**: Services not receiving updates

**Solutions**:
- Check triggers are created: Connect to DB and run `\d health_events` to see triggers
- Verify changelog table has entries: `SELECT COUNT(*) FROM datastore_changelog;`
- Check resume tokens are being updated: `SELECT * FROM resume_tokens;`
- Review component logs for polling errors

### Performance Issues

**Problem**: Slow queries or high CPU

**Solutions**:
- Check indexes exist: `\d health_events` in psql
- Analyze slow queries: Enable PostgreSQL slow query log
- Increase connection pool size if needed
- Consider vacuuming: `VACUUM ANALYZE health_events;`

## Monitoring

### Metrics

Monitor these PostgreSQL metrics:
- Connection pool utilization
- Query latency (P50, P95, P99)
- Transaction rate
- Table sizes and growth
- Index hit ratio
- Replication lag (if using replicas)

### Health Checks

Verify datastore health:

```bash
# Check PostgreSQL pod health
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=postgresql

# Check service connectivity from a pod
kubectl exec -it deployment/fault-quarantine -n nvsentinel -- \
  psql "postgresql://postgres:password@nvsentinel-postgresql:5432/nvsentinel?sslmode=require"

# Check for errors in logs
kubectl logs -n nvsentinel deployment/fault-quarantine | grep -i error
```

## Security Considerations

### Authentication

- **Client certificates** - Mutual TLS authentication required
- **Password authentication** - Not recommended for production
- **SCRAM-SHA-256** - Supported for password-based auth

### Authorization

Create a limited application role, then give it only the grants in [Application Role Grants](#application-role-grants):

```sql
CREATE USER nvsentinel_app WITH PASSWORD 'secure-password';
```

### Network Security

- Use NetworkPolicies to restrict access
- Enable TLS for all connections
- Use private networks when possible
- Rotate certificates regularly

## Backup and Recovery

### Backup Strategy

The PostgreSQL subchart supports automated backups:

```yaml
postgresql:
  backup:
    enabled: true
    cronjob:
      schedule: "0 2 * * *"  # Daily at 2 AM
      storage:
        size: 10Gi
```

### Manual Backup

```bash
# Backup all data
kubectl exec nvsentinel-postgresql-0 -n nvsentinel -- \
  pg_dump -U postgres nvsentinel > backup.sql

# Backup specific table
kubectl exec nvsentinel-postgresql-0 -n nvsentinel -- \
  pg_dump -U postgres -t health_events nvsentinel > health_events_backup.sql
```

### Recovery

```bash
# Restore from backup
kubectl exec -i nvsentinel-postgresql-0 -n nvsentinel -- \
  psql -U postgres nvsentinel < backup.sql
```

## Comparison: PostgreSQL vs MongoDB

| Feature | PostgreSQL | MongoDB |
|---------|-----------|----------|
| **Data Model** | Relational with JSONB | Document-oriented |
| **Change Streams** | Emulated via triggers | Native |
| **Transactions** | Full ACID | Multi-document ACID |
| **Query Language** | SQL | MQL |
| **Indexing** | B-tree, GiST, GIN | B-tree, Geospatial |
| **Scalability** | Vertical + read replicas | Horizontal sharding |
| **Maturity** | Very mature (30+ years) | Mature (15+ years) |
| **Ops Complexity** | Well-known, standard | Specialized knowledge |
| **Change Stream Latency** | ~1s (polling) | &lt;100ms (native) |

## References

- [PostgreSQL Official Documentation](https://www.postgresql.org/docs/)
- [PostgreSQL JSONB Documentation](https://www.postgresql.org/docs/current/datatype-json.html)
- [Bitnami PostgreSQL Helm Chart](https://github.com/bitnami/charts/tree/main/bitnami/postgresql)
- [NVSentinel PostgreSQL Migrations](../distros/kubernetes/nvsentinel/files/postgresql-migrations/)

## Support

For issues or questions:
1. Check this documentation and troubleshooting section
2. Review logs: `kubectl logs -n nvsentinel {pod-name}`
3. File an issue: https://github.com/nvidia/nvsentinel/issues
