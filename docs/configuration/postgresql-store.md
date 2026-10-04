# PostgreSQL Store Configuration

## Overview

NVSentinel can use PostgreSQL instead of MongoDB as the health-events datastore. With `postgresql.enabled: true`, the chart deploys an in-cluster Bitnami PostgreSQL instance.

For architecture, schema, TLS modes, and MongoDB migration, see [PostgreSQL Provider](../postgresql-provider.md).

To use a cloud-managed PostgreSQL service (RDS, Azure Database for PostgreSQL, Cloud SQL), see [External Datastore](../external-datastore.md).

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

Server TLS, `pg_hba`, and init scripts are under `postgresql.primary.*` (see the example values file).

### The `postgresql` subchart is upstream

Everything under `postgresql.` other than `postgresql.enabled` belongs to the vendored [Bitnami PostgreSQL chart](https://github.com/bitnami/charts/tree/main/bitnami/postgresql) (chart 15.5.38, PostgreSQL 16.4.0), not to NVSentinel. Its full value reference is the upstream chart's own documentation and `distros/kubernetes/nvsentinel/charts/postgresql/values.yaml`; this page does not repeat it.

The keys NVSentinel's own example sets are these:

| Key | Purpose |
|---|---|
| `postgresql.enabled` | Deploys the in-cluster instance. Set `false` for an external or cloud-managed server |
| `postgresql.auth.database` | Database the modules connect to. Must match `global.datastore.connection.database` |
| `postgresql.auth.postgresPassword` | Password for the `postgres` user. Supply it through `auth.existingSecret` rather than in values — see [Secrets](#secrets) below |
| `postgresql.primary.tls.*` | Server certificate, key and CA, and the Secret holding them |
| `postgresql.primary.pgHbaConfiguration` | Client authentication rules. This is what enforces certificate authentication |
| `postgresql.primary.initdb.scripts` | Schema creation, run once on first start |

Anything else the upstream chart offers — replication, backups, resource presets, `ldap`, `audit`, network policy — is available and unmodified, but NVSentinel does not test it and the NVSentinel version does not pin your usage of it.

### Pod priority

`global.priorityClassName` and `global.systemPriorityClassName` do not apply to the PostgreSQL pods. The subchart does not read NVSentinel's `global` values, so a release rendered with only those globals leaves the PostgreSQL StatefulSet with no `priorityClassName`.

Use the upstream key instead:

```yaml
postgresql:
  primary:
    priorityClassName: system-cluster-critical
  # readReplicas:
  #   priorityClassName: system-cluster-critical   # only with architecture: replication
```

The chart's default `architecture` is `standalone`, so `primary` is the only key that matters unless you switch to `replication`.

Give the datastore at least the priority you give the modules that depend on it. A preempted datastore stops fault detection for the whole cluster, while the health monitors keep running at their own higher priority and cannot persist what they find.

See [Pod Priority](./README.md#pod-priority) for the settings that do apply to NVSentinel's own components.

### Secrets

Do not put a password in your values file. Create a Secret and point the chart at it:

```yaml
postgresql:
  auth:
    existingSecret: "postgresql-credentials"
    secretKeys:
      adminPasswordKey: "postgres-password"
```

The example values file ships `postgresPassword: "changeme"` to keep a first install working. Replace it before any real deployment.

For the modules' own connection, set `global.datastore.credentialsFromSecret.name` to a Secret that defines `DATASTORE_PASSWORD`, rather than putting the password in values. See [External Datastore](../external-datastore.md) for the full credential model.

## Verify after install

```bash
kubectl get statefulset -n {namespace} -l app.kubernetes.io/name=postgresql
kubectl get pods -n {namespace} -l app.kubernetes.io/name=postgresql
```

In-cluster service hostname is typically `{RELEASE_NAME}-postgresql` (for example `nvsentinel-postgresql` when the release name is `nvsentinel`).
