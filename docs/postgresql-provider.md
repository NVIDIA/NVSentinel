# PostgreSQL Provider for NVSentinel

## Overview

NVSentinel supports PostgreSQL as an alternative datastore to MongoDB. The PostgreSQL provider offers similar functionality with a relational database backend, providing flexibility in deployment options.

## Features

- **Full feature parity** with MongoDB provider
- **TLS/SSL support** with client certificate authentication
- **Change stream emulation** using triggers and polling
- **JSONB storage** for flexible document-like data structures
- **Automatic schema management** with tables, indexes, and triggers
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

### Durable Watcher Start Position

Every error-returning PostgreSQL watcher constructor loads or creates its saved position before it returns. This rule applies with or without changelog retention. It is a correctness fix and a superset of ADR-068's retention requirement.

For a missing `resume_tokens` row, the constructor saves `{timestamp, eventID}` with eventID `0`. The timestamp is the earlier of database `clock_timestamp()` minus five seconds and the oldest other client session's transaction start in the current database. The same insert inspects activity first and then samples the clock, on the reserved connection. A writer open during the inspection is bounded by its transaction start. A writer that starts between the inspection and the clock sample has a `changed_at` after the inspection, and the five-second margin covers it, because it only has to exceed this one statement's duration. An idle or long transaction moves the start earlier and can increase replay. The watcher reads back the stored pair and uses it as its read position. Concurrent constructors for the same client use the winning row. Existing rows remain unchanged.

The insert inspects `client backend` sessions in the current database, excluding its own session. Background processes, including autovacuum and parallel workers, do not affect the start boundary. PostgreSQL hides `backend_type` for sessions whose statistics the consumer cannot read, but still shows their user (`usesysid`). A hidden session with a user may be a client, so it triggers the full-replay fallback. Background processes show no user and are ignored. A client that is still connecting has no transaction yet and is also ignored.

Hidden client activity or disabled client activity tracking prevents a reliable transaction-start bound. Either condition selects the Unix epoch, `1970-01-01T00:00:00+00:00`, which precedes every changelog row and parses in every session time zone. A database that allows prepared transactions (`max_prepared_transactions` above `0`) also selects this boundary. A prepared transaction leaves its session, PostgreSQL exposes only its PREPARE time, and it can commit at any moment, so no observation can bound it.

For a new consumer, this fallback replays the whole available changelog. Only changelog retention would bound that replay; NVSentinel does not implement changelog retention yet. The watcher logs one warning after loading the saved boundary. The warning names the consumer, lists the possible causes and links to this section. Existing tokens remain unchanged.

The standard chart uses one shared PostgreSQL role for all datastore writers and consumers. PostgreSQL defaults `max_prepared_transactions` to `0`, which disables prepared transactions. With activity tracking enabled, the standard chart normally never triggers this fallback. Custom roles, disabled tracking or enabled prepared transactions can trigger it.

Use the same role for all writers, or ask the database operator to grant the consumer role `pg_read_all_stats`. Enable client activity tracking. If no client uses two-phase commit, keep `max_prepared_transactions` at `0`. These steps prevent future fallback choices; they do not replace an existing saved replay boundary. No privilege is granted automatically. See [PostgreSQL activity visibility](https://www.postgresql.org/docs/16/monitoring-stats.html#MONITORING-STATS-VIEWS) and [prepared transactions](https://www.postgresql.org/docs/16/view-pg-prepared-xacts.html). The [prepared-transaction setting](https://www.postgresql.org/docs/16/runtime-config-resource.html#GUC-MAX-PREPARED-TRANSACTIONS) documents the default.

A restart before the first acknowledgment resumes from that saved pair. A consumer that rarely acknowledges keeps its first start pair until an acknowledgment advances it. For example, fault-remediation can stay quiet on a healthy cluster. Each restart reads the changelog again from that pair. This adds replay cost without losing events.

Load errors prevent construction, polling and delivery. A malformed token is logged and kept, and the watcher replays the changelog from the beginning, so a corrupt row cannot fail every restart. Legacy timestamp lookups retry database errors until success or context cancellation. A missing legacy changelog row resumes from the earliest later `changed_at` with eventID `0`; existing tokens are never replaced.

A reset is different from a first start. Reset-on-start and a circuit-breaker `CREATE` transition ask to skip events that accumulated before the reset, so they replace the token with the current database clock, with no margin, instead of deleting it. Construction then finds that row and starts exactly at the head. Deleting the row by hand still gives the first-start behavior with its five-second margin. The watcher factory and datastore paths run the configured reset before initialization. The database-client path initializes after any reset performed by its caller, including `ClientFactory`. The shared reset helper skips `event-exporter`.

`Start(ctx)` uses the prepared pair and writes no checkpoint. The low-level `NewPostgreSQLChangeStreamWatcher` cannot return initialization errors, so `Start` initializes such a watcher itself. If that fails, `Start` logs the error and launches no readers.

Initial persistence does not establish ADR-054 lag evidence; lag stays unknown until a poll records progress or an empty batch. An initial row with eventID `0` uses the same `last_updated` stale-token threshold as any other checkpoint.

### Recovery and Retention Rollout

Disable changelog cleanup before removing an active client's token. Use reset-on-start where supported, or delete that client's `resume_tokens` row. Restart the consumer and verify its saved start pair. That pair can precede the database clock when another client transaction was open, or be the Unix epoch when activity is hidden or prepared transactions are enabled. This recovery can skip events between the old position and the new start pair. The database-client path requires manual deletion unless its caller provides reset handling.

Keep cleanup disabled until every store-owning component supports initial persistence and verified, acknowledgment-safe checkpoints. Verify that every active watcher has a saved position before enabling cleanup. A nonempty token table cannot prove rollout completeness. Disable cleanup before downgrading a reader. Initial persistence alone does not complete ADR-068; the age and previous-snapshot guards remain required.

### Delivery Acknowledgments

`MarkProcessed(token)` acknowledges the earliest tracked pending delivery with that token and every delivery before it. Resolve that entire delivery prefix before acknowledging. Concurrent consumers must use a contiguous completion watermark, as described in [ADR-056](designs/056-concurrent-node-partitioned-event-processing.md).

The provider tracks matching deliveries before enqueueing them. Buffered events count as pending. It saves the read cursor only when no deliveries remain pending. Otherwise, it saves the smaller of the cursor and the position strictly before every pending delivery in `(changed_at, id)` order. Before handing out a late NOTIFY row behind the persisted checkpoint, the provider saves the rewound position. A failed rewind leaves the row undelivered and its notification ID available for the next catch-up pass. The fetch loop logs the failure and retries the pass. Rewind saves and acknowledgment saves use the same lock. A late NOTIFY row can move the saved position backward. A restart can therefore replay acknowledged events as well as pending events.

Empty, unknown, evicted-only, and already acknowledged tokens certify no outstanding deliveries. Acknowledgments never advance the read cursor past unread rows. Empty tokens retain the existing processed-row update at the read cursor. All tokens retain the cumulative `processed = TRUE WHERE id <= token` update. This flag does not prove that all consumers acknowledged a row.

The provider tracks at most 4,096 individual pending deliveries per watcher. The Helm value `health-events-analyzer.maxInFlight` sets the partitioned processor limit and defaults to `1000`. The watcher and adapter channels each buffer 100 events. The adapter can hold one additional event, and the watcher can register one event before its send blocks. Thus, `maxInFlight + 202` deliveries can await acknowledgment. Values up to `3894` fit within the 4,096-entry window; `3895` or higher can exceed it. Acknowledgments can then fall outside the tracked window. The saved position stays at the eviction floor until a tracked cumulative acknowledgment clears the evicted backlog. No events are skipped; a restart can replay them. At the limit, it evicts the oldest tracked entry and retains a floor. The floor stores the minimum position and maximum delivery sequence across evicted entries. Checkpoints stay strictly before that floor until an acknowledgment matches a tracked delivery after all evicted entries. That cumulative acknowledgment clears the floor. Eviction bounds memory without blocking delivery, including when a consumer drops failed events. The watcher logs once per saturation episode; clearing the floor ends that episode.

Abandoned sends remain pending because the read cursor already includes those rows. A final watermark flush after cancellation or Close must preserve their replay on restart.

Checkpoint write failures return to the caller. The token schema and startup resume behavior stay unchanged. These rules implement acknowledgment safety independently of the proposed [retention protocol](postgresql-changelog-retention-protocol.md#consumer-bound-and-idle-checkpoints). They do not implement retention or idle checkpoints.

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
- Development password (`nvsentinel-dev`)
- Auto-initialized schema with tables and triggers
- Control plane node selector for PostgreSQL pod

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

### Automatic Initialization

When a module starts, it runs schema setup statements that:
- Create tables if they don't exist
- Create indexes for query performance
- Set up triggers for change tracking

### Manual Schema Updates

If you need to modify the schema:

1. Connect to PostgreSQL:
   ```bash
   kubectl exec -it nvsentinel-postgresql-0 -n nvsentinel -- psql -U postgres -d nvsentinel
   ```

2. Run your schema updates:
   ```sql
   -- Example: Add a new index
   CREATE INDEX idx_custom ON health_events ((document->>'customField'));
   ```

### Schema Reference

See [postgresql-schema.sql](./postgresql-schema.sql) for the complete schema definition.

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
- `idx_changelog_resume_all` on `datastore_changelog(table_name, changed_at, id)` - The change stream poll

### Changelog Resume Index

The change stream poll reads the changelog rows of one table after its resume position, in `(changed_at, id)` order. The `idx_changelog_resume_all` index serves this query. Without it, each poll reads and sorts all changelog rows of the table.

Fault Quarantine, Node Drainer, Fault Remediation and the Health Events Analyzer create the index at start if it is missing. They build it with `CREATE INDEX CONCURRENTLY`, so changelog writes continue during the build. The components build it one at a time, under the setup lock.

On an upgrade with a large `datastore_changelog` table, the build can take several minutes. Fault Quarantine, Node Drainer and Fault Remediation do not become ready until it is done, so the rollout takes longer, and `helm upgrade --wait` can reach its timeout. To prevent this, create the index before the upgrade:

```sql
CREATE INDEX CONCURRENTLY IF NOT EXISTS idx_changelog_resume_all ON datastore_changelog (table_name, changed_at, id);
```

The components then find the index and skip the build.

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

Configure PostgreSQL user permissions:

```sql
-- Create limited user for applications
CREATE USER nvsentinel_app WITH PASSWORD 'secure-password';
GRANT CONNECT ON DATABASE nvsentinel TO nvsentinel_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO nvsentinel_app;
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
- [NVSentinel PostgreSQL Schema](./postgresql-schema.sql)

## Support

For issues or questions:
1. Check this documentation and troubleshooting section
2. Review logs: `kubectl logs -n nvsentinel {pod-name}`
3. File an issue: https://github.com/nvidia/nvsentinel/issues
