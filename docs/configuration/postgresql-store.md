# PostgreSQL Store Configuration

## Overview

NVSentinel can use PostgreSQL instead of MongoDB as the health-events datastore. With `postgresql.enabled: true`, the chart deploys an in-cluster Bitnami PostgreSQL instance.

For architecture, schema, TLS modes, and MongoDB migration, refer to [PostgreSQL Provider](../postgresql-provider.md).

To use an external or cloud-managed PostgreSQL server, set `postgresql.enabled: false` and set `global.datastore.connection` to the address of that server.

## Enable in-cluster PostgreSQL

```yaml
global:
  datastore:
    provider: "postgresql"
    connection:
      host: "nvsentinel-postgresql"
      port: 5432
      database: "nvsentinel"
      username: "postgres"
      sslmode: "verify-full"
      sslcert: "/etc/ssl/client-certs/tls.crt"
      sslkey: "/etc/ssl/client-certs/tls.key"
      sslrootcert: "/etc/ssl/client-certs/ca.crt"

  mongodbStore:
    enabled: false

postgresql:
  enabled: true
```

Full example: `distros/kubernetes/nvsentinel/values-postgresql.yaml`

```bash
helm upgrade --install nvsentinel {chart-path} \
  -f distros/kubernetes/nvsentinel/values-postgresql.yaml \
  --namespace nvsentinel \
  --create-namespace
```

## Configuration keys

| Area | Helm keys |
| ---- | --------- |
| Datastore provider | `global.datastore.*` |
| In-cluster PostgreSQL | `postgresql.*` |

Server TLS and `pg_hba` are under `postgresql.primary.*` (see the example values file). The modules create the schema when they start, so the example sets no init script.

### The `postgresql` subchart is upstream

All keys under `postgresql.`, except `postgresql.enabled`, belong to the vendored Bitnami PostgreSQL chart (chart 15.5.38, PostgreSQL 16.4.0), not to NVSentinel. For its full value reference, refer to the documentation of the upstream chart and to `distros/kubernetes/nvsentinel/charts/postgresql/values.yaml`. This page does not repeat that reference.

The NVSentinel example values file sets these keys:

| Key | Purpose |
|---|---|
| `postgresql.enabled` | Deploys the in-cluster instance. Set `false` for an external or cloud-managed server |
| `postgresql.auth.database` | The database that the modules connect to. Must match `global.datastore.connection.database` |
| `postgresql.auth.postgresPassword` | Password for the `postgres` user. Supply it through `auth.existingSecret`, not in values. Refer to [Secrets](#secrets) below |
| `postgresql.primary.tls.*` | Server certificate, key and CA, and the Secret that holds them |
| `postgresql.primary.pgHbaConfiguration` | Client authentication rules. These rules enforce certificate authentication |

The upstream chart also offers other features, such as replication, backups, resource presets, `ldap`, `audit` and network policy. These features are available and unmodified. But NVSentinel does not test them, and the NVSentinel version does not pin your use of them.

### Pod priority

`global.priorityClassName` and `global.systemPriorityClassName` do not apply to the PostgreSQL pods. The subchart does not read the NVSentinel `global` values. Thus, if you set only those global values, the PostgreSQL StatefulSet has no `priorityClassName`.

Use the upstream key instead:

```yaml
postgresql:
  primary:
    priorityClassName: system-cluster-critical
  # readReplicas:
  #   priorityClassName: system-cluster-critical   # only with architecture: replication
```

The default `architecture` of the chart is `standalone`. Thus, `primary` is the only key that matters, unless you change to `replication`.

Give the datastore at least the priority of the modules that depend on it. A preempted datastore stops fault detection for the whole cluster. The health monitors continue to run at their own higher priority, but they cannot persist what they find.

For the settings that apply to the NVSentinel components, refer to [Pod Priority](./README.md#pod-priority).

### Secrets

Do not put a password in your values file. Create a Secret. Then set the chart to use it:

```yaml
postgresql:
  auth:
    existingSecret: "postgresql-credentials"
    secretKeys:
      adminPasswordKey: "postgres-password"
```

The example values file ships `postgresPassword: "changeme"`, so that a first install works. Replace it before any real deployment.

For the connection of the modules, set `global.datastore.credentialsFromSecret.name` to a Secret that defines `DATASTORE_PASSWORD`. Do not put the password in values. For the full credential model, refer to [External Datastore](../external-datastore.md).

## Health event retention

MongoDB deletes health events after `collectionExpirySeconds`, 30 days by default. PostgreSQL has no TTL, so by default the `health_events` table keeps every event for the life of the cluster. To delete events older than a retention, set `global.datastore.healthEventsCleanup.retention`:

```yaml
global:
  datastore:
    healthEventsCleanup:
      retention: "720h"     # 30 days, the MongoDB default
      # interval: "1h"      # how often a cleanup runs
      # batchSize: 10000    # events deleted per transaction
      # maxBatches: 100     # batches per run
```

The chart passes these to the components as `DATASTORE_HEALTH_EVENTS_RETENTION`, `DATASTORE_HEALTH_EVENTS_CLEANUP_INTERVAL`, `DATASTORE_HEALTH_EVENTS_CLEANUP_BATCH_SIZE` and `DATASTORE_HEALTH_EVENTS_CLEANUP_MAX_BATCHES`. A component reads them when it starts.

- Retention is off by default. The value is a Go duration: `720h` is valid, `30d` is not. The minimum is `24h`, and the interval must be at least `1m`. An invalid value logs a warning and leaves retention off; it never stops a component.
- As with the MongoDB TTL, an event expires by the time it was stored, even if it is still in progress. Choose a retention longer than the longest window of your health-events-analyzer rules, and longer than a node stays quarantined.
- The components that open the store run the cleanup: fault-quarantine, node-drainer, fault-remediation and health-events-analyzer. An advisory lock keeps it to one run at a time. Each run deletes at most `batchSize` × `maxBatches` events, so a large existing backlog drains over several runs.
- The deletes are not kept in `datastore_changelog`. No component acts on a delete, and the changelog has no cleanup yet ([#2170](https://github.com/NVIDIA/NVSentinel/issues/2170)).
- New events reuse the space of deleted ones, but the table file does not shrink. To return the space of a large backlog to the operating system, use `pg_repack`, or `VACUUM FULL health_events` in a maintenance window (it locks the table).

## Verify after install

```bash
kubectl get statefulset -n {namespace} -l app.kubernetes.io/name=postgresql
kubectl get pods -n {namespace} -l app.kubernetes.io/name=postgresql
```

The in-cluster service hostname is usually `{RELEASE_NAME}-postgresql`. For example, it is `nvsentinel-postgresql` when the release name is `nvsentinel`.
