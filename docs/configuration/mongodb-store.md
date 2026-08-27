# MongoDB Store Configuration

## Overview

The MongoDB Store module provides persistent storage for health events collected by NVSentinel monitors. It deploys a MongoDB replica set with TLS encryption and authentication.

Two in-cluster backends are supported: **Bitnami** (default) and **Percona Operator**. This page covers Helm configuration for both. If your cluster runs on ARM64 nodes, you must use the Percona backend; see [ARM64 support](#arm64-support).

## Backend selection

| Backend | Helm flags | Configuration keys |
| ------- | ---------- | ------------------ |
| Bitnami (default) | `useBitnami: true`, `usePerconaOperator: false` | `mongodb-store.mongodb.*` |
| Percona Operator | `useBitnami: false`, `usePerconaOperator: true` | `mongodb-store.psmdb-db.*`, `mongodb-store.psmdb-operator.*` |

Both flags must always be set together. Setting them explicitly in your values, rather than relying on the chart defaults, is recommended for any long-lived installation: it keeps your deployment on its current backend even if the chart default changes in a future release. Upgrading a release across a backend change does not work in place and forces a full remove-and-redeploy migration, so an accidental switch is worth guarding against.

See [ADR-013: MongoDB Migration from Bitnami](../designs/013-mongodb-bitnami-migration.md) for the rationale behind the dual-backend design and for licensing details.

To use a cloud-managed MongoDB (DocumentDB, Atlas, etc.) instead of in-cluster storage, see [External Datastore](../external-datastore.md).

## ARM64 support

The Bitnami backend does not work on ARM64 nodes. Its images (`bitnamilegacy/mongodb` and the related exporter and TLS images) are only published for amd64. On an ARM64 node the MongoDB pod never starts and stays in `ImagePullBackOff` with an event like:

```text
Failed to pull image "docker.io/bitnamilegacy/mongodb:8.0.3-debian-12-r0": ... no match for platform in manifest: not found
```

The Percona backend is fully multi-arch. The operator, the MongoDB server and the metrics exporter images all provide arm64 builds, and NVSentinel's own images are published for both amd64 and arm64. To run NVSentinel on ARM64 nodes, enable the Percona backend at install time:

```yaml
global:
  mongodbStore:
    enabled: true

mongodb-store:
  useBitnami: false
  usePerconaOperator: true
```

## Switching backends on an existing installation

Do **NOT** switch backends by changing the two flags on a live release with `helm upgrade`. The upgrade fails partway through with an immutable field error on the database initialization Job, and by then it has already deployed parts of the other backend. The result is two MongoDB clusters running side by side and services pointed at the wrong one.

Switching backends reinstalls the datastore; the migration runbook's default path carries the health event data over with a dump and restore, and only its opt-out clean path drops it. Follow the [MongoDB Bitnami to Percona migration runbook](../runbooks/mongodb-bitnami-to-percona-migration.md) for the full procedure, including the cleanup steps and the handling of in-flight quarantines.

## Percona Operator

Enable Percona when first installing NVSentinel. On a release that already runs Percona, keep these flags set on every upgrade. To move an existing Bitnami installation to Percona, do not change the flags in place; follow the [migration runbook](../runbooks/mongodb-bitnami-to-percona-migration.md) instead.

```yaml
global:
  mongodbStore:
    enabled: true

mongodb-store:
  useBitnami: false
  usePerconaOperator: true
```

When Percona is enabled, the replica set is configured under `psmdb-db` instead of `mongodb.*` (see defaults in `distros/kubernetes/nvsentinel/charts/mongodb-store/values.yaml`).

- **Service endpoint:** `mongodb-rs0.{namespace}.svc.cluster.local:27017`
- **Metrics:** `percona/mongodb_exporter` sidecar on port `9216` (configured in default `psmdb-db` values)
- **Operator reference:** [Percona Operator for MongoDB](https://docs.percona.com/percona-operator-for-mongodb/)

The chart-generated `MONGODB_URI` follows the selected backend automatically (`mongodb-headless` for Bitnami, `mongodb-rs0` for Percona). If you set `global.datastore.connection.host` explicitly in your values, it must match the backend you selected.

### Volume size

The Percona defaults request 8Gi data volumes. Some cloud providers enforce a larger minimum block volume size (OCI block volumes are at least 50Gi, for example). When the provisioned volume ends up larger than the requested size, the operator stops reconciling with `requested storage is less than actual storage` and the replica set never initializes. Set the volume size explicitly to at least your provider's minimum:

```yaml
mongodb-store:
  psmdb-db:
    replsets:
      rs0:
        volumeSpec:
          pvc:
            resources:
              requests:
                storage: "50Gi"
```

### Pod placement (Percona)

The Percona components use their own scheduling keys. Values under `mongodb-store.mongodb.*` apply only to the Bitnami backend:

```yaml
mongodb-store:
  job:
    nodeSelector: {}
    tolerations: []
  psmdb-operator:
    nodeSelector: {}
    tolerations: []
  psmdb-db:
    replsets:
      rs0:
        nodeSelector: {}
        tolerations: []
```

Verify after install:

```bash
kubectl get perconaservermongodb -n {namespace}
kubectl get pods -n {namespace} -l app.kubernetes.io/component=mongod
```

The `perconaservermongodb` resource must reach `ready`, and the init Job (`-l app.kubernetes.io/name=create-mongodb-database`) must complete.

## Configuration Reference

### Module Enable/Disable

Controls whether the mongodb-store module is deployed in the cluster.

```yaml
global:
  mongodbStore:
    enabled: true
```

### Volume size (Bitnami)

`mongodb.persistence.size` (default `8Gi`). Percona: `psmdb-db.replsets.rs0.volumeSpec.pvc.resources.requests.storage`. Existing PVCs do not grow on upgrade.

```yaml
mongodb-store:
  mongodb:
    persistence:
      size: "100Gi"
```

### HealthEvents TTL

`collectionExpirySeconds` (default `2592000` / 30d). Same key for external Mongo. The init Job is `create-mongodb-database-<seconds>-<scriptHash>` (`-l app.kubernetes.io/name=create-mongodb-database`).

A completed Job is immutable. TTL and a hash of the mongosh init script are in the Job name so Helm/Argo create a new Job when expiry or indexes change. Argo also has `Force=true,Replace=true` so a Failed first run is recreated on sync. For Helm, if you need to rerun the same script, delete the Job by that label and upgrade.

```yaml
mongodb-store:
  collectionExpirySeconds: 604800  # 7 days
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
