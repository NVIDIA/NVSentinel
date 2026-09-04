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

`mongodb.persistence.size` (default `8Gi`). Percona: `psmdb-db.replsets.rs0.volumeSpec.pvc.resources.requests.storage`. Sizes are Kubernetes quantities (for example `8Gi`, `32G`). Existing PVCs do not grow on upgrade.

```yaml
mongodb-store:
  mongodb:
    persistence:
      size: "100Gi"
```

### Oplog size

`oplogSizeMB` (default `990`, MongoDB's minimum) is the replica-set oplog size in megabytes. Helm does **not** compute this from the PVC. `990` is Mongo's floor, not a 24-hour window.

Fault Quarantine, Node Drainer, Fault Remediation, and Event Exporter resume from change-stream tokens. If the token ages off the oplog during an outage, they resume from now and **silently skip** events (issue #1594). Size the oplog so tokens survive the longest outage you will tolerate, then keep that size under about **half** the **live** data volume so WiredTiger still has room for collections.

#### What to set (summary)

| Knob | Starting point | Notes |
| ---- | -------------- | ----- |
| Desired window | **24 hours** | Covers rolling updates, evictions, and module restarts. Use **48 hours** if you want extra margin. |
| `oplogSizeMB` | Measure wrap time if Mongo is already running; otherwise use the estimate below | Integer megabytes, minimum `990` |
| PVC | At least **2 × oplog** (Gi), then add TTL data | Bound volumes do not grow from Helm. Do not put a 15 GB oplog on the chart's small **dev** disk. |

You do not need a separate drain-time term for most clusters. If consumers stay caught up, drain time is ~0. The 24-hour window already covers a full day of consumer downtime. Add extra hours only if you know a consumer lags ingest for a long time (for example a large Event Exporter backfill).

#### Measure on a live cluster (preferred)

On any replica, in `mongosh`:

```javascript
const s = db.getSiblingDB("local").oplog.rs.stats()
const first = db.getSiblingDB("local").oplog.rs.find().sort({$natural: 1}).limit(1).next().ts
const last = db.getSiblingDB("local").oplog.rs.find().sort({$natural: -1}).limit(1).next().ts
const spanSec = last.getHighBits() - first.getHighBits()
const currentMB = Math.floor(s.maxSize / (1024 * 1024))
const bytesPerSec = s.size / spanSec
const hoursCapacity = s.maxSize / bytesPerSec / 3600
print("currentMB=" + currentMB + " usedMB=" + Math.floor(s.size / (1024 * 1024)))
print("spanHours=" + (spanSec / 3600).toFixed(1) + " hoursCapacity≈" + hoursCapacity.toFixed(1))
print("for 24h set oplogSizeMB ≈ " + Math.ceil(currentMB * (24 / hoursCapacity)))
```

`hoursCapacity` uses used bytes vs wall-clock span so a disk that is not yet full is not treated as "already holding 24h". If `hoursCapacity` is already ≥ 24, you can leave `oplogSizeMB` as-is (the Job will not shrink unless `oplogAllowShrink` is true). Still keep the value under about half the live PVC. Re-run after large processor changes; resume-token and fault-handling write volume can shift.

At low event rates, resume-token heartbeats can dominate the oplog. Trust wrap time, not `events × 350 B`.

#### Estimate before you have production load

```
oplogSizeMB ≈ event_rate_per_sec × oplog_entry_bytes × window_seconds × extra_writes / 1_000_000
```

| Term | Meaning | How to get it | If you cannot measure yet |
| ---- | ------- | ------------- | ------------------------- |
| `event_rate_per_sec` | HealthEvents inserts per second | `count` in a known window on `HealthEvents`, or scale from a similar cluster | Issue #1594 uses **~500/s at ~100k nodes** (~5/s per 1k nodes). Linear-scale from your node count; this is a noisy-cluster ceiling, not an idle cluster. |
| `oplog_entry_bytes` | Bytes of one health-event insert in the oplog | Issue #1594 measured **~350 B** uncompressed. Live: oplog `size` ÷ inserts in that window | **350** |
| `window_seconds` | How long a resume token must remain in the oplog | 24h = `86400`; 48h = `172800` | **86400** |
| `extra_writes` | Other oplog traffic per health event (resume-token updates, fault-handling writes) | Live: (oplog bytes in a window) ÷ (health-event count × 350). At high ingest this is usually a small multiplier | **1.5** (1.0 if you only count the insert; 1.5 leaves headroom for tokens and extra processor writes) |

Stored collection size is **not** the same as oplog-entry size: WiredTiger compression (and Percona encryption) shrinks data on disk. Do not use uncompressed JSON size for the PVC data term.

#### PVC vs oplog

The data PVC must hold TTL-aged HealthEvents **and** the oplog. WiredTiger needs headroom: keep `oplogSizeMB` under about half the **live** volume.

- **Oplog floor for the disk:** `persistence.size` (Gi) ≳ `2 × oplogSizeMB / 1024`. Example: `15120` MB oplog → at least **32Gi**.
- **TTL data** is often larger than the oplog. After a few days, scale from observed `collStats().storageSize` × (`TTL` / collection age). Uncompressed `rate × 350 B × TTL` overestimates badly.

Raising `persistence.size` does not expand an already-Bound PVC. Expand and verify the live volume first, then raise `oplogSizeMB`. `replSetResizeOplog` does not check free disk.

#### Worked example (production)

On a **~100k-node** cluster writing **~500 health events/s**, with **~350 B** per oplog insert, `extra_writes = 1`, and a **24-hour** window:

`500 × 350 × 86400 / 1_000_000` ≈ **15120** MB.

```yaml
mongodb-store:
  oplogSizeMB: 15120   # 24h at 500/s × 350 B; scale if your rate differs
  oplogAllowShrink: false
  mongodb:
    extraFlags:
      - "--setParameter"
      - "authenticationMechanisms=MONGODB-X509,SCRAM-SHA-256"
      - "--oplogSize=15120"   # must match oplogSizeMB
    persistence:
      size: "32Gi"     # 2× oplog half-volume floor; raise further for 30d TTL data
  # Percona only — must match oplogSizeMB; this replaces the whole configuration block:
  # psmdb-db:
  #   replsets:
  #     rs0:
  #       configuration: |
  #         replication:
  #           oplogSizeMB: 15120
  #         setParameter:
  #           authenticationMechanisms: "MONGODB-X509,SCRAM-SHA-256,SCRAM-SHA-1"
```

That disk is much larger than the chart's **dev** default. A quieter cluster should scale the **500/s** figure with node count, then size the disk at least **2 × oplog** plus TTL data — not by copying the chart's small default volume.

Do not copy `15120` onto a small or idle cluster.

#### Kind, Tilt, and empty clusters

Leave `oplogSizeMB` at **990** (Mongo's minimum). The chart default volume is for development only. Kind/Tilt hostPath often starts with MongoDB's default of 5% of the node disk (several GiB); skip-shrink leaves that larger window in place.

The same integer must also be on mongod startup so a **new** empty member (replaced PVC, added replica) starts at that size instead of Mongo's default. Put it in **our** values, not in vendored chart templates:

- **Bitnami:** `mongodb.extraFlags` must include `--oplogSize=<oplogSizeMB>`. Helm fails if the flag is missing or differs. Do **not** set `mongodb.existingConfigmap` to a snippet `mongodb.conf`: Bitnami then treats the file as fully user-managed, skips `dbPath` / logpath / replSet, and the pod crashloops (`--fork` without `--logpath`).
- **Percona:** `psmdb-db.replsets.rs0.configuration` must contain `replication.oplogSizeMB` equal to `oplogSizeMB`. Helm fails if it is missing or differs. Overriding `configuration` replaces the whole block, so keep `setParameter` as well.

`--set oplogSizeMB=1400` alone is not enough; also set `--oplogSize=1400` (Bitnami) or `replication.oplogSizeMB: 1400` (Percona). Changing flags or the config file does **not** resize a member that already has data — the init Job still runs `replSetResizeOplog` on every reachable member.

The Job **never shrinks** an existing oplog unless you set `mongodb-store.oplogAllowShrink: true`. Shrinking truncates the oldest entries immediately; change-stream watchers then hit `ChangeStreamHistoryLost` and resume from now (silent event loss — issue #1594).

External Mongo is not resized. Resize is skipped when there is no PVC: Bitnami `mongodb.persistence.enabled=false` (emptyDir) or Percona `volumeSpec.hostPath` / `emptyDir`.

A completed Job is immutable. Changing `oplogSizeMB` (or the replica-member list) creates a new Job. One unreachable replica is skipped after 12 tries (~60s) so a TTL update is not blocked; two or more skipped members fail the Job.

### HealthEvents TTL

`collectionExpirySeconds` (default `2592000` / 30d). Same key for external Mongo. The init Job is `create-mongodb-database-<seconds>-<scriptHash>` (`-l app.kubernetes.io/name=create-mongodb-database`).

A completed Job is immutable. TTL, oplog size, and a hash of the mongosh init script are in the Job name so Helm/Argo create a new Job when expiry, oplog, or indexes change. Argo also has `Force=true,Replace=true` so a Failed first run is recreated on sync. For Helm, if you need to rerun the same script, delete the Job by that label and upgrade.

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
