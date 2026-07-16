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
helm upgrade --install nvsentinel <chart-path> \
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

## Verify after install

```bash
kubectl get statefulset -n <namespace> -l app.kubernetes.io/name=postgresql
kubectl get pods -n <namespace> -l app.kubernetes.io/name=postgresql
```

In-cluster service hostname is typically `<release-name>-postgresql` (for example `nvsentinel-postgresql` when the release name is `nvsentinel`).
