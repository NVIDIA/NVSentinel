# MongoDB Store Configuration

## Overview

The MongoDB Store module provides persistent storage for health events collected by NVSentinel monitors. It deploys a MongoDB replica set with TLS encryption and authentication.

Two in-cluster backends are supported: **Bitnami** (default) and **Percona Operator**. This page covers Helm configuration for both.

## Backend selection

| Backend | Helm flags | Configuration keys |
| ------- | ---------- | ------------------ |
| Bitnami (default) | `useBitnami: true`, `usePerconaOperator: false` | `mongodb-store.mongodb.*` |
| Percona Operator | `useBitnami: false`, `usePerconaOperator: true` | `mongodb-store.psmdb-db.*`, `mongodb-store.psmdb-operator.*` |

Both flags must be set explicitly when switching backends. See [ADR-013: MongoDB Migration from Bitnami](../designs/013-mongodb-bitnami-migration.md) for rationale, Bitnami → Percona migration steps, and licensing.

To use a cloud-managed MongoDB (DocumentDB, Atlas, etc.) instead of in-cluster storage, see [External Datastore](../external-datastore.md).

## Percona Operator

Enable Percona on install or upgrade:

```yaml
global:
  mongodbStore:
    enabled: true

mongodb-store:
  useBitnami: false
  usePerconaOperator: true
```

When Percona is enabled, the replica set is configured under `psmdb-db` instead of `mongodb.*` (see defaults in `distros/kubernetes/nvsentinel/charts/mongodb-store/values.yaml`).

- **Service endpoint:** `mongodb-rs0.<namespace>.svc.cluster.local:27017`
- **Metrics:** `percona/mongodb_exporter` sidecar on port `9216` (configured in default `psmdb-db` values)
- **Operator reference:** [Percona Operator for MongoDB](https://docs.percona.com/percona-operator-for-mongodb/)

Verify after install:

```bash
kubectl get perconaservermongodb -n <namespace>
kubectl get pods -n <namespace> -l app.kubernetes.io/name=percona-server-mongodb
```

## Configuration Reference

### Module Enable/Disable

Controls whether the mongodb-store module is deployed in the cluster.

```yaml
global:
  mongodbStore:
    enabled: true
```

### Initialization Job Placement

Configures node placement for initialization jobs (applies to both backends).

```yaml
mongodb-store:
  job:
    nodeSelector: {}
    tolerations: []
```

#### Parameters

##### nodeSelector
Node selector for scheduling MongoDB initialization jobs.

##### tolerations
Tolerations for MongoDB initialization jobs to run on tainted nodes.

### Node Placement

Controls pod scheduling for MongoDB replicas.

```yaml
mongodb-store:
  mongodb:
    nodeSelector: {}
    tolerations: []
```

#### Parameters

##### nodeSelector
Node selector for scheduling MongoDB replica pods.

##### tolerations
Tolerations for MongoDB pods to run on tainted nodes.

### Metrics Exporter

Configures MongoDB metrics exporter for monitoring integration.

```yaml
mongodb-store:
  mongodb:
    metrics:
      enabled: true
      image:
        registry: docker.io
        repository: bitnamilegacy/mongodb-exporter
        tag: 0.41.2-debian-12-r1
```

#### Parameters

##### enabled
Enable MongoDB metrics exporter sidecar container.

##### image.repository
Container image for the MongoDB exporter.

##### image.tag
Image tag for the MongoDB exporter.

The exporter exposes metrics on port 9216 for Prometheus scraping.

### Helper Images

Container images used in init containers and sidecars.

```yaml
mongodb-store:
  mongodb:
    helperImages:
      kubectl:
        repository: docker.io/bitnamilegacy/kubectl
        tag: "1.30.6"
        pullPolicy: IfNotPresent
      mongosh:
        repository: ghcr.io/rtsp/docker-mongosh
        tag: "2.5.2"
        pullPolicy: IfNotPresent
```

#### Parameters

##### kubectl
Image for Kubernetes operations in init containers (secret creation, certificate management).

##### mongosh
Image for MongoDB shell operations and database initialization.
