# PostgreSQL Provider for NVSentinel

## Overview

NVSentinel supports PostgreSQL as an alternative datastore to MongoDB. The PostgreSQL provider gives similar functionality with a relational database backend. Thus, you have more options when you deploy NVSentinel.

## Features

- **Health event storage and change streams**, as with the MongoDB provider. The metrics `change_stream_resume_token_recoveries_total` and `change_stream_reopens_total` exist only for MongoDB. For the other differences, refer to [Comparison: PostgreSQL and MongoDB](#comparison-postgresql-and-mongodb).
- **TLS/SSL support** with client certificate authentication
- **Change stream emulation** with triggers and polling
- **JSONB storage** for flexible document-like data structures
- **Automatic schema management** with tables, indexes, and triggers
- **Connection pooling and error handling** for production use

## When to Use PostgreSQL

Choose PostgreSQL if you:

- Already have PostgreSQL infrastructure in your organization
- Prefer relational databases for operational reasons
- Need to use existing PostgreSQL expertise
- Want to use PostgreSQL-specific features or tools
- Have compliance requirements that favor PostgreSQL

## Architecture

### Database Schema

The PostgreSQL provider uses these tables:

1. **health_events** - Stores GPU and system health events
2. **maintenance_events** - Stores CSP maintenance events
3. **datastore_changelog** - Tracks changes for change stream emulation
4. **resume_tokens** - Stores resume tokens for change stream clients

Each table stores the document data in JSONB columns. The provider also extracts key fields from the document for indexing.

### Change Stream Emulation

PostgreSQL does not have native change streams like MongoDB. The provider emulates change streams with these items:

- **Triggers** on tables to capture INSERT/UPDATE/DELETE operations
- **Changelog table** to store change events
- **Polling mechanism** to detect new changes
- **Resume tokens** to track client progress and enable resumption

### Durable Watcher Start Position

When a component creates its PostgreSQL watcher, the watcher loads its saved position from the `resume_tokens` table. If no row exists for the component, the watcher creates the row. The watcher completes this step before it reads changes.

For a new row, the watcher saves a `{timestamp, eventID}` pair with eventID `0`. The timestamp is the earlier of these two times:

- The database `clock_timestamp()` minus five seconds
- The oldest transaction start of the other client sessions in the current database

Thus, the watcher reads all changes of the transactions that were open when it saved the position. The five-second margin also covers a transaction that starts while the watcher saves the position. If another client has an idle or long transaction, the start position is earlier, and the watcher can replay more changes.

The watcher then reads the saved row and uses that pair as its read position. If two watchers with the same client name start at the same time, both use the row that the first watcher saved. At start, the watcher does not change a row that exists.

To find the oldest transaction, the watcher examines the `client backend` sessions in the current database, except its own session. Background processes, such as autovacuum and parallel workers, do not change the start position.

If the role of the consumer cannot read the statistics of a session, PostgreSQL hides the `backend_type` of that session. PostgreSQL still shows the user of that session (`usesysid`). A hidden session with a user can be a client, so it causes the full-replay fallback. Background processes show no user, so the watcher ignores them. A client that has not completed its connection has no transaction yet, so the watcher also ignores it.

In these conditions, the transaction start times do not give a reliable bound:

- PostgreSQL hides client activity from the consumer.
- Client activity tracking is disabled.
- The database allows prepared transactions (`max_prepared_transactions` is above `0`).

In each of these conditions, the watcher uses the full-replay fallback. It saves the Unix epoch, `1970-01-01T00:00:00+00:00`, as the timestamp. This time is earlier than every changelog row. A prepared transaction leaves its session, and PostgreSQL shows only its PREPARE time. The transaction can also commit at any moment. Thus, no observation can give a bound for it.

For a new consumer, the full-replay fallback replays the whole available changelog. Only changelog retention can limit this replay, and NVSentinel does not implement changelog retention. When the watcher loads a saved position at the Unix epoch, it logs one warning: `PostgreSQL watcher starts with full changelog replay`. The warning gives the name of the consumer, the possible causes and a link to this section. The fallback does not change existing tokens.

The standard chart uses one PostgreSQL role for all datastore writers and consumers. By default, PostgreSQL sets `max_prepared_transactions` to `0`, which disables prepared transactions. With activity tracking enabled, the standard chart normally does not cause the full-replay fallback. Custom roles, disabled tracking or enabled prepared transactions can cause it.

To prevent the full-replay fallback:

- Use the same role for all client sessions in the database, including all writers and consumers. Alternatively, ask the database administrator to grant `pg_read_all_stats` to the role of the consumer.
- Enable client activity tracking.
- If no client uses two-phase commit, keep `max_prepared_transactions` at `0`.

These steps prevent the fallback only for start positions that watchers create later. They do not replace a start position at the Unix epoch that a watcher saved before. NVSentinel does not grant a privilege automatically. For the statistics views, the `pg_prepared_xacts` view and the `max_prepared_transactions` setting, refer to the PostgreSQL documentation.

If a consumer restarts before its first acknowledgment, it resumes from the saved start position. A consumer that rarely acknowledges events keeps its first start position until an acknowledgment moves it forward. For example, on a healthy cluster, fault-remediation can have no events to acknowledge for a long time. Each restart then reads the changelog again from that position. This replay adds cost, but the consumer does not lose events.

If the watcher cannot load its saved position, the component cannot create the watcher. Thus, the component does not poll the changelog or deliver events.

At start, if the saved token is malformed, the watcher logs an error and does not change the row. The error message is `Resume token is malformed; replaying the changelog from the beginning`. The watcher then replays the changelog from the beginning. Thus, a corrupt row does not make every restart fail.

If a saved token has an eventID but no timestamp, the watcher gets the `changed_at` time of that changelog row. If this lookup gets a database error, the watcher tries again until the lookup succeeds or the component stops. If the changelog row does not exist, the watcher resumes from the earliest `changed_at` of the later rows, with eventID `0`. At start, the watcher does not save the converted position to the row.

A reset is different from a first start. These two operations reset the position of a consumer:

- The `CREATE` mode of the resume-control ConfigMap. Refer to [Change Stream Resume Tokens](./configuration/README.md#change-stream-resume-tokens).
- The circuit breaker `CREATE` cursor. Refer to [Resetting the Circuit Breaker](./circuit-breaker.md#resetting-the-circuit-breaker).

Both operations skip the events that accumulated before the reset. Thus, they replace the token with the current database clock, with no margin, and they do not delete it. The watcher then finds that row and starts exactly at the head of the changelog. If you delete the row by hand, the watcher does a first start, with the five-second margin. Each component does the configured reset before its watcher loads its position. The resume-control ConfigMap does not reset `event-exporter`.

When the watcher starts to read, it uses the saved position. It does not save a new position at that time.

A saved start position does not show that the consumer has read all changes. Thus, the lag of the consumer stays unknown until a poll reads an event or gets an empty batch. Until then, `change_stream_lag_known` is `0` for the consumer.

### Watcher Position Recovery

To reset the position of a consumer, do these steps:

1. If a GitOps controller, such as Argo CD, manages NVSentinel, stop its reconciliation, as in [Stop reconciliation](./runbooks/mongodb-bitnami-to-percona-migration.md#step-2-stop-reconciliation-gitops-managed-installations-only). A running controller can restore the replicas during this procedure.
2. Scale the Deployment of the consumer to zero replicas. Wait until no pod of the consumer exists.
3. Reset the token of the consumer with one of these methods:
   - For fault-quarantine, node-drainer, fault-remediation and health-events-analyzer, set the key of the consumer to `CREATE` in the resume-control ConfigMap. Refer to [Change Stream Resume Tokens](./configuration/README.md#change-stream-resume-tokens).
   - For any consumer, including `event-exporter`, delete the row of the consumer from the `resume_tokens` table.
4. If you used the `CREATE` mode, scale the Deployment to one replica. Wait until the consumer sets its key back to `RESUME`.
5. Scale the Deployment back to its earlier number of replicas.
6. If you stopped the reconciliation in step 1, start it again.
7. Verify the saved start position of the consumer in the `resume_tokens` table.

While the consumer runs, do not delete its row. A running consumer saves its old position again when it acknowledges an event. Some consumers, for example health-events-analyzer, also save their position when they stop. Then the consumer resumes from its old position.

With the `CREATE` mode, the new start position is the database clock. After you delete the row, the new start position can be earlier than the database clock if another client transaction was open. It can also be the Unix epoch if client activity is hidden or prepared transactions are enabled. This recovery can skip the events between the old position and the new start position.

### Delivery Acknowledgments

A consumer acknowledges an event after it processes the event. This acknowledgment also covers all events that the watcher delivered to the consumer before that event. The saved position always stays before the earliest delivered event that the consumer has not acknowledged. Thus, after a restart, the consumer gets every unacknowledged event again, and it can also get some processed events again.

A change can become visible after the watcher reads newer changes, for example when its transaction runs for a long time. If a notification announces such a change behind the saved position, the watcher first moves the saved position back to before the change. Then it delivers the change. If this save fails, the watcher does not deliver the change yet. It logs `Failed to persist checkpoint rewind; delivery will be retried` and tries again. Thus, after a restart, a consumer can receive events that it acknowledged before.

An acknowledgment also updates the changelog. The provider sets `processed` to `TRUE` on the rows of the same table, up to the ID of the event. This column does not show that all consumers acknowledged a row. The provider does not delete rows from the `datastore_changelog` table.

Each watcher tracks at most 4,096 delivered events that wait for acknowledgment. For health-events-analyzer, up to `maxInFlight + 202` events can wait. The Helm value `health-events-analyzer.maxInFlight` sets the maximum number of events that the analyzer has in flight without an acknowledgment, before it applies backpressure. Its default is `1000`. Values up to `3894` fit within the limit. Values of `3895` or higher can exceed it.

When a watcher reaches this limit, it logs `Pending delivery limit reached; retaining evicted positions in checkpoint floor`. The watcher continues to deliver events, and its memory use does not increase. But it stops tracking the oldest events, and the saved position stays before them. The saved position moves forward again when the consumer acknowledges an event that the watcher delivered after all of them. The watcher skips no events, but a restart can replay more events. The watcher logs the message one time for each such period.

If a consumer stops before the watcher delivers an event to it, the consumer receives that event after a restart.

If the watcher cannot save the position when a consumer acknowledges an event, the acknowledgment fails, and the consumer gets the error. Then a restart can replay events that the consumer already processed.

## Configuration

### Helm Chart Deployment

#### Using values-postgresql.yaml

Use the provided values file to deploy NVSentinel with PostgreSQL:

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

PostgreSQL requires TLS for secure connections. The Helm chart configures these items automatically:

1. **cert-manager resources** for certificate generation
2. **Client certificates** for application authentication
3. **Server certificates** for PostgreSQL TLS
4. **InitContainers** to fix certificate permissions

#### SSL Modes

- `disable` - No SSL (not recommended for production)
- `require` - SSL required, but no certificate verification
- `verify-ca` - Verification of the server certificate against the CA
- `verify-full` - Full verification, including the hostname (recommended)

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

The provider also accepts `MONGODB_CLIENT_CERT_MOUNT_PATH` as the certificate directory path.

## Migration Guide

### From MongoDB to PostgreSQL

1. **Export data from MongoDB** (if you want to keep the data):
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

Verify the PostgreSQL deployment:

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

When a component starts, it runs schema setup statements that:

- Create tables if they do not exist
- Create indexes for query performance
- Create triggers for change tracking

### Manual Schema Updates

To change the schema, do these steps:

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

For the complete schema definition, refer to [postgresql-schema.sql](./postgresql-schema.sql).

## Performance Tuning

### Connection Pooling

The provider uses a connection pool with these defaults:

```go
// Defaults. Only MaxOpenConns can be tuned, with DATASTORE_MAX_CONNECTIONS.
MaxOpenConns: 25
MaxIdleConns: 10
ConnMaxLifetime: 1 hour
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

At start, fault-quarantine, node-drainer, fault-remediation and health-events-analyzer create the index if it is missing. They build it with `CREATE INDEX CONCURRENTLY`, so changelog writes continue during the build. Each component holds the setup lock during the build, so the components build the index one at a time.

### Query Optimization

The provider extracts frequently-queried fields from JSONB. This has these effects:

- Reduces JSONB query overhead
- Enables efficient B-tree index usage
- Maintains flexibility for additional JSONB fields

## Troubleshooting

### Connection Issues

**Problem**: "connection refused" or "no route to host"

**Solutions**:

- Verify that the PostgreSQL service runs: `kubectl get svc nvsentinel-postgresql -n nvsentinel`
- Check that no network policy blocks access
- Verify DNS resolution: `nslookup nvsentinel-postgresql.nvsentinel.svc.cluster.local`

### Certificate Issues

**Problem**: "certificate verify failed" or permission denied

**Solutions**:

- Check that cert-manager created the certificates: `kubectl get certificate -n nvsentinel`
- Verify that the certificate secret exists: `kubectl get secret postgresql-client-cert -n nvsentinel`
- Examine the initContainer logs: `kubectl logs -n nvsentinel deployment/fault-quarantine -c fix-cert-permissions`
- Verify that the certificate permissions are correct (key: 600, cert/ca: 644)

### SSL Mode Issues

**Problem**: "SSL required" or "SSL not supported"

**Solutions**:

- Verify that `sslmode` has the correct value in your values file
- Check that the PostgreSQL server has TLS enabled
- Verify that the server certificate is valid

### Change Stream Not Working

**Problem**: Services do not receive updates

**Solutions**:

- Check that the triggers exist. Connect to the database and run `\d health_events` to show the triggers.
- Verify that the changelog table has entries: `SELECT COUNT(*) FROM datastore_changelog;`
- Check that the components update the resume tokens: `SELECT * FROM resume_tokens;`
- Examine the component logs for polling errors

### Performance Issues

**Problem**: Slow queries or high CPU

**Solutions**:

- Check that the indexes exist: run `\d health_events` in psql
- To analyze slow queries, enable the PostgreSQL slow query log
- If necessary, increase the connection pool size
- You can also vacuum the table: `VACUUM ANALYZE health_events;`

## Monitoring

### Metrics

Monitor these PostgreSQL metrics:

- Connection pool utilization
- Query latency (P50, P95, P99)
- Transaction rate
- Table sizes and growth
- Index hit ratio
- Replication lag (if you use replicas)

### Health Checks

Verify the health of the datastore:

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
- If possible, use private networks
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

## Comparison: PostgreSQL and MongoDB

| Feature | PostgreSQL | MongoDB |
|---------|-----------|----------|
| **Data Model** | Relational with JSONB | Document-oriented |
| **Change Streams** | Emulated with triggers | Native |
| **Transactions** | Full ACID | Multi-document ACID |
| **Query Language** | SQL | MQL |
| **Indexing** | B-tree, GiST, GIN | B-tree, Geospatial |
| **Scalability** | Vertical + read replicas | Horizontal sharding |
| **Maturity** | Very mature (30+ years) | Mature (15+ years) |
| **Ops Complexity** | Well-known, standard | Specialized knowledge |
| **Change Stream Latency** | Near real time (LISTEN/NOTIFY, with fallback polling) | &lt;100ms (native) |

## References

- [NVSentinel PostgreSQL Schema](./postgresql-schema.sql)

## Support

For issues or questions, do these steps:

1. Read this page and its troubleshooting section
2. Examine the logs: `kubectl logs -n nvsentinel {pod-name}`
3. File an issue: https://github.com/nvidia/nvsentinel/issues
