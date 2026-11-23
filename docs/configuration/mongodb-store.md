# MongoDB Store Configuration

## Overview

The MongoDB Store module provides persistent storage for the health events that the NVSentinel health monitors collect. It deploys a MongoDB replica set with TLS encryption and authentication.

NVSentinel supports two in-cluster backends: **Bitnami** (default) and **Percona Operator**. This page covers the Helm configuration for both. If your cluster runs on ARM64 nodes, you must use the Percona backend. Refer to [ARM64 support](#arm64-support).

## Backend selection

| Backend | Helm flags | Configuration keys |
| ------- | ---------- | ------------------ |
| Bitnami (default) | `useBitnami: true`, `usePerconaOperator: false` | `mongodb-store.mongodb.*` |
| Percona Operator | `useBitnami: false`, `usePerconaOperator: true` | `mongodb-store.psmdb-db.*`, `mongodb-store.psmdb-operator.*` |

Always set both flags together. For a long-lived installation, set them explicitly in your values instead of relying on the chart defaults. Then your deployment stays on its current backend, even if the chart default changes in a later release.

An upgrade across a backend change does not work in place. It forces a full remove-and-redeploy migration. Thus, guard against an accidental switch.

To use a cloud-managed MongoDB, such as DocumentDB or Atlas, instead of in-cluster storage, refer to [External Datastore](../external-datastore.md).

## ARM64 support

The Bitnami backend does not work on ARM64 nodes. Its images (`bitnamilegacy/mongodb` and the related exporter and TLS images) are available only for amd64. On an ARM64 node, the MongoDB pod never starts. It stays in `ImagePullBackOff` with an event like this one:

```text
Failed to pull image "docker.io/bitnamilegacy/mongodb:8.0.3-debian-12-r0": ... no match for platform in manifest: not found
```

The Percona backend is fully multi-arch. The operator, the MongoDB server and the metrics exporter images all provide arm64 builds. The NVSentinel images are available for amd64 and arm64. To run NVSentinel on ARM64 nodes, enable the Percona backend at install time:

```yaml
global:
  mongodbStore:
    enabled: true

mongodb-store:
  useBitnami: false
  usePerconaOperator: true
```

## Switching backends on an existing installation

**Never switch backends in place.** A change of useBitnami and usePerconaOperator does not migrate the data. Services can then connect to a new, empty database, and the existing health events become unavailable. Node annotations and remediation resources can then refer to events that are not in the new database. Follow this runbook to remove and redeploy the installation. If you must keep the health events, use the dump and restore path.

A backend switch reinstalls the datastore. The default path of the migration runbook moves the health event data to the new datastore with a dump and restore. Only its opt-out clean path drops the data. For the full procedure, follow the [MongoDB Bitnami to Percona migration runbook](../runbooks/mongodb-bitnami-to-percona-migration.md). It includes the cleanup steps and the handling of in-flight quarantines.

## Enabling Bitnami MongoDB on an existing installation

The Bitnami backend generates its root password only during a fresh `helm install`. If you enable the datastore later, you use a `helm upgrade`. An example is when you move from monitoring to cordon and drain. In that case, the chart stops before it deploys anything:

```text
PASSWORDS ERROR: You must provide your current passwords when upgrading the release.
```

The chart reads the `mongodb` Secret during template rendering. Rendering occurs before any Job or init container can create the Secret. Create the Secret yourself first. Then run the same upgrade again. The Percona backend does not have this behavior.

**To prevent this problem, create the Secret at install time.** The Quick Start section of `README.md` in the repository root creates it with the namespace, before the first `helm upgrade --install`. The Secret costs nothing while the datastore is off. It also makes a later enablement of the datastore a single command. The rest of this section is for installations that exist without the Secret.

### Check before you create anything

The Secret must match the credentials that are already in the database volume. A new Secret over a live database locks NVSentinel out of its own data. Run both checks. Then compare their output with the decision table below:

```bash
# 1. A usable credentials Secret. Prints "present" only when the key exists and is
#    non-empty; it never prints the password itself.
kubectl get secret mongodb -n nvsentinel -o jsonpath='{.data.mongodb-root-password}' 2>/dev/null \
  | grep -q . && echo "present" || echo "missing or empty"

# 2. Existing database volumes. If any exist, a database was deployed before.
kubectl get pvc -n nvsentinel -l app.kubernetes.io/name=mongodb
```

It is not sufficient that the Secret exists. The chart fails in the same way if `mongodb-root-password` is absent or empty. Thus, check the key, not the object.

Read the results together:

| Key present | Volumes | What to do |
|---|---|---|
| no | none | Fresh datastore. Create the Secret below. |
| yes | any | Keep the Secret as it is. Go directly to the upgrade. |
| no | one or more | **Stop.** The volume holds credentials that you do not have. A new Secret locks NVSentinel out of that data. |

For the last row, recover the original password from your backup. The data-preserving path in the [migration runbook](../runbooks/mongodb-bitnami-to-percona-migration.md) also needs that password. Its dump step reads the same `mongodb-root-password` key and stops if the key is missing. Without the password, the only remaining option is the clean path of the runbook, which drops the stored health events.

### Create the Secret

The Secret needs one key, `mongodb-root-password`. An empty Secret does not work. The chart treats a missing key the same as a missing Secret. It fails with `The secret "mongodb" does not contain the key "mongodb-root-password"`.

```bash
kubectl create secret generic mongodb -n nvsentinel \
  --from-literal=mongodb-root-password="$(openssl rand -hex 24)"
```

Then run your `helm upgrade` again, without changes. The chart finds the Secret and uses it again on this upgrade and on every later upgrade. The root username stays `root`, as `mongodb-store.mongodb.auth.rootUser` sets it.

Confirm that the datastore started:

```bash
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=mongodb
```

## Percona Operator

Enable Percona at the first install of NVSentinel. On a release that already runs Percona, keep these flag values on every upgrade. To move an existing Bitnami installation to Percona, do not change the flags in place. Follow the [migration runbook](../runbooks/mongodb-bitnami-to-percona-migration.md) instead.

```yaml
global:
  mongodbStore:
    enabled: true

mongodb-store:
  useBitnami: false
  usePerconaOperator: true
```

When Percona is enabled, you configure the replica set under `psmdb-db` instead of `mongodb.*`. For the defaults, refer to `distros/kubernetes/nvsentinel/charts/mongodb-store/values.yaml`.

- **Service endpoint:** `mongodb-rs0.{namespace}.svc.{clusterDomain}:27017`, where `{clusterDomain}` is `global.clusterDomain` (default `cluster.local`; see [Cluster Domain](README.md#cluster-domain))
- **Metrics:** `percona/mongodb_exporter` sidecar on port `9216` (configured in default `psmdb-db` values)

The chart-generated `MONGODB_URI` follows the selected backend automatically (`mongodb-headless` for Bitnami, `mongodb-rs0` for Percona). If you set `global.datastore.connection.host` explicitly in your values, it must match the backend you selected.

### Percona versions

Three values carry the Percona **operator** version and must agree, because the init container runs the operator image:

| Value | What it is |
| --- | --- |
| `psmdb-operator.image.tag` | the operator image |
| `psmdb-db.crVersion` | the schema version of the `PerconaServerMongoDB` resource |
| `psmdb-db.initImage.tag` | the init container, which is the operator image |

Set **`mongodb-store.psmdbVersion`** to the version that you intend to run. **It asserts. It does not set:** Helm resolves subchart values before any template runs, so a parent chart cannot write into them. Set the three values as usual, and set `psmdbVersion` to the same version. The render then fails if any of them disagrees.

This makes the assertion useful. In practice, a common mistake is a partial edit. The edit raises the operator tag and the init image, but forgets `crVersion`. The assertion refuses that edit instead of deploying it. If you leave `psmdbVersion` empty, the assertion is disabled, but the consistency checks below still run.

The **mongod** version (`psmdb-db.image.tag`) is separate. It is a different product on its own version line. Percona publishes the mongod versions that each operator version certifies at `https://check.percona.com/versions/v1/psmdb-operator/<version>`. The chart cannot read that matrix while it renders. Before you change the mongod version, examine that matrix yourself. A pairing outside the matrix renders and runs without a warning.

#### Upgrading

Percona permits an upgrade only to the nearest `major.minor`. The render enforces this rule. It fails in these cases:

- `crVersion` is more than one minor behind the operator.
- The major versions are different.
- `crVersion` is ahead of the operator.

**One minor of skew is allowed on purpose**, because the documented upgrade uses it. On a live cluster, a move from 1.21 to 1.23 has two steps:

1. Raise `psmdb-operator.image.tag` and `psmdb-db.initImage.tag` to `1.22.0`. Keep `crVersion` at `1.21.x`. The operator is then one minor ahead. This skew is permitted, and it does not roll the replica set.
2. Raise `crVersion` to `1.22.0`. **This rolls the replica set.** `crVersion` is a field of the custom resource, so the operator runs SmartUpdate across every member: secondaries first, then a primary step-down.

Then repeat these steps for 1.23. Do not set `psmdbVersion` until you complete all the steps, because it requires that all three values agree.

A non-semver operator tag, for example a local build, disables the skew comparison. The `psmdbVersion` and init image checks still apply.

#### Checking the deployed resource

All the checks above compare values with each other. None of them can see the resource that is deployed. This gap affects an existing cluster. When you adopt a release, the bundled versions move together, so the values stay self-consistent. But the live `PerconaServerMongoDB` still has the `crVersion` that it was installed with. `lookup` returns empty under `helm template`, so a render-time guard cannot close this gap for ArgoCD deployments either.

Set **`mongodb-store.validateDeployedCrVersion: true`** to add an init container to the bootstrap Job. The init container reads the deployed `crVersion` and applies the same rule: the same major, and at most one minor behind the operator. If the pairing is not supported, it fails the Job. It does not let the pairing reconcile silently.

It is **off by default**, because it grants the Job `list` on `perconaservermongodbs.psmdb.percona.com`, in the release namespace. The same value controls the grant and the check. Thus, no installation has the permission without the check that needs it.

Notes:

- The check runs after the users secret that the operator generates exists. Thus, the operator has already reconciled the resource. A missing resource at that point is an error, not a first install. For this reason, the check can fail instead of skipping.
- The check finds the resource by listing the namespace, not by name. Thus, a rename of the resource changes nothing. If the namespace holds more than one resource, the check refuses to guess which one belongs to the release.
- The check runs only when the Job runs, so it cannot see a render. It does not replace `psmdbVersion`. That assertion catches values that disagree with each other before anything is applied.

### Linux kernel 6.19 and newer

On nodes that run Linux kernel 6.19 or newer, the Percona backend can fail to start. `mongodb-rs0-0` exits within a second after it starts, and it stays in `CrashLoopBackOff`. Also, fault-quarantine, node-drainer, fault-remediation and platform-connectors restart in a loop, because they cannot reach the datastore. `kubectl logs --previous` for the `mongod` container is empty.

TCMalloc is the memory allocator of MongoDB. It uses rseq for its per-CPU caches. Kernels 6.19 through 7.0.13 break this use of rseq, so MongoDB 8.0 refuses to start on those kernels (MongoDB issue SERVER-121912). The mongod release that the chart deploys refuses to start on every kernel 6.19 and newer, including kernels that already have the fix.

Ubuntu kernels also hide their upstream release from `uname -r`. This command shows a zero patch level, for example `7.0.0-43-generic` for a kernel built from 7.0.14. Thus, later mongod releases can still refuse to start on these kernels.

The container log stays empty because of the entrypoint script, `/opt/percona/ps-entry.sh`. The init container copies this script from the operator image. The script runs `mongod --version` to read the version. It consumes the refusal message, and then it exits.

To examine the kernel of a node:

```bash
uname -r
# Ubuntu only: the last field is the upstream kernel release
cat /proc/version_signature
```

To see the refusal directly, run the mongod image on a machine with the same kernel:

```bash
docker run --rm --entrypoint mongod percona/percona-server-mongodb:8.0.26-11 --version
```

On an affected kernel, this command prints `MongoDB cannot start: Linux kernel versions 6.19 and newer has a known incompatibility with this version of MongoDB` and exits with status 1.

The Percona image sets `GLIBC_TUNABLES=glibc.pthread.rseq=0`, which leaves rseq for TCMalloc to register. If you set it to `1`, glibc keeps rseq. TCMalloc then uses per-thread caches instead and does not use rseq, and mongod starts:

```yaml
mongodb-store:
  psmdb-db:
    replsets:
      rs0:
        env:
          - name: GLIBC_TUNABLES
            value: "glibc.pthread.rseq=1"
```

With this setting, mongod logs startup warning 8718500. The warning says that the performance impact is critical, and it asks for `GLIBC_TUNABLES=glibc.pthread.rseq=0`. Keep the setting. On these kernels, if you follow that advice, the startup failure occurs again.

Helm replaces lists rather than merging them. If your values already set `env` for `rs0`, add this entry to that list instead of defining a second one.

The cost is some allocator performance under heavy concurrency, which matters little at the load NVSentinel puts on its datastore.

A change to `env` changes the pod spec. On a healthy replica set, the operator restarts mongod on the next upgrade. But the operator restarts pods only when every replica is ready. Thus, a mongod pod that is already in a crash loop never gets the new setting. After the upgrade, wait until the operator writes the setting into the StatefulSet template. Then delete each mongod pod that is in a crash loop.

If you delete a pod before that, it starts again with the old setting:

```bash
kubectl get statefulset mongodb-rs0 -n nvsentinel \
  -o jsonpath='{.spec.template.spec.containers[?(@.name=="mongod")].env}'
kubectl delete pod -n nvsentinel mongodb-rs0-0
```

To confirm that the setting reached the pod:

```bash
kubectl get pod mongodb-rs0-0 -n nvsentinel \
  -o jsonpath='{.spec.containers[?(@.name=="mongod")].env}'
kubectl get pods -n nvsentinel
```

On a fresh install, the `create-mongodb-database` Job runs while mongod is down. If mongod stays down after the deadline of the Job, the Job fails. Helm does not create the Job again, so the database users are missing even after mongod starts. An install whose Job already completed keeps its users. Examine the Job. If it failed, follow [Init Job deadline](#init-job-deadline):

```bash
kubectl get job -n nvsentinel -l app.kubernetes.io/name=create-mongodb-database
```

Before you remove the entry, run the `docker run` command above again with the new image tag. Run it on a machine with each kernel that you use. If it prints the version instead of the refusal, you can remove the entry. The check works only for releases that include the refusal. Older MongoDB 8.0 releases print the version on an affected kernel, and then crash after startup. On kernels 6.19 through 7.0.13, no MongoDB 8.0 release runs without this setting.

### Volume size

The Percona defaults request 8Gi data volumes. Some cloud providers enforce a larger minimum size for block volumes. For example, OCI block volumes are at least 50Gi. If the provisioned volume is larger than the requested size, the operator stops reconciliation with `requested storage is less than actual storage`. The replica set then never initializes. Set the volume size explicitly to at least the minimum of your provider:

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

After the install, verify the deployment:

```bash
kubectl get perconaservermongodb -n {namespace}
kubectl get pods -n {namespace} -l app.kubernetes.io/component=mongod
```

The `perconaservermongodb` resource must reach `ready`, and the init Job (`-l app.kubernetes.io/name=create-mongodb-database`) must complete.

### Init Job deadline

The init Job that creates the database users (`create-mongodb-database`) fails when it runs longer than `activeDeadlineSeconds`. On a cold node, image pulls can wait in a queue long enough to reach this deadline. Helm does not create a failed Job again. Set the deadline next to `nodeSelector` and `tolerations`:

```yaml
mongodb-store:
  job:
    activeDeadlineSeconds: 1800   # default; 0 omits the deadline
```

With an external MongoDB (`mongodbStore.enabled=false`), the setup Job reads its own value instead (default `600`):

```yaml
global:
  datastore:
    setupJob:
      activeDeadlineSeconds: 600
```

If you raise the value, a Job that already failed does not restart. Delete the failed Job. Then run `helm upgrade` again. For the mongodb-store Job:

```bash
kubectl delete job -l app.kubernetes.io/name=create-mongodb-database -n {namespace}
```

For the external MongoDB setup Job:

```bash
kubectl delete job -l app.kubernetes.io/name=external-mongodb-setup -n {namespace}
```

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

### Pod disruption budget (Bitnami)

`mongodb.pdb.minAvailable` and `mongodb.pdb.maxUnavailable` are both empty by default. In that case, the subchart renders `maxUnavailable: 1`. This value limits how many members can be unavailable at the same time. It lets a node drain evict a member and finish. `mongodb.pdb.create: false` removes the budget entirely.

`maxUnavailable` limits voluntary disruptions to one member at a time. This is not the same as quorum protection. The difference is important at small sizes:

- At three members or more, the eviction of one member leaves a majority.
- At two members, the majority is two. Thus, the eviction of one member leaves no majority, and the set loses its primary.
- At one member, the eviction is the outage. The default permits it on purpose, as the trade-off for a drain that completes.

`maxUnavailable: 1` is the right default, because it is the tightest bound that still permits a voluntary disruption. A drain needs such a disruption.

A fixed `minAvailable` is correct only at one specific replica count. For example, a value of 2 can never be satisfied below three members. Then `disruptionsAllowed` is 0, and any drain of a node that hosts a member blocks indefinitely. The pod itself stays `Ready` and reports no problem.

Above three members, the same value is too permissive. At five members, it allows three members to become unavailable. This leaves two members, which is below quorum, and the replica set becomes read-only.

**Set at most one of the two.** The template emits `minAvailable` if it is not empty, and `maxUnavailable` if it is not empty. The API server rejects a PodDisruptionBudget with both fields, because they are mutually exclusive. For this reason, the chart ships both empty, not a literal `maxUnavailable: 1`. Then an explicit `minAvailable` that you set renders alone, as you intended.

If you set `minAvailable`, keep it strictly below `replicaCount`. `minAvailable: 1` on a single-member store does not prevent the deadlock, because `currentHealthy` minus `desiredHealthy` is still 0. It only stops `KubePdbNotEnoughHealthyPods` from reporting the deadlock.

```yaml
mongodb-store:
  mongodb:
    pdb:
      # Leave both empty for maxUnavailable: 1, or set exactly one of them.
      minAvailable: ""
      maxUnavailable: ""
```

Check the result, not the values, because an unsatisfiable budget looks healthy from every other angle:

```bash
kubectl -n nvsentinel get pdb mongodb \
  -o custom-columns=MIN:.spec.minAvailable,MAX:.spec.maxUnavailable,ALLOWED:.status.disruptionsAllowed,HEALTHY:.status.currentHealthy,DESIRED:.status.desiredHealthy
```

`ALLOWED` must be at least 1. If it is 0, a drain hangs.

### Open-file limit (node prerequisite)

The nodes that run MongoDB must give containers a soft open-file limit (`RLIMIT_NOFILE`) of at least 64000. This applies to Bitnami and to the Percona Operator. The chart does not set this limit. The container gets it from the container runtime on the node.

mongod uses one file descriptor for each client connection, and it accepts connections only up to about 80 percent of its soft limit. The number of connections grows with the cluster, because each platform-connectors pod has its own connection pool. If mongod has no free file descriptors, it crashes, and every module that reads the datastore stops. This occurs when the number of connections gets near the limit. That number increases when the cluster grows, when more pods connect, or when connection pools get larger. Thus, a cluster can run correctly for a long time before it fails.

64000 is the MongoDB minimum. Each platform-connectors pod opens about 3 connections, so each node adds about 3 connections. A soft limit of 64000 lets mongod accept about 51000 connections, which is enough for about 17000 nodes. For clusters above about 15000 nodes, use a higher limit, such as the `1048576` in the drop-in below.

If you use the [deployment platform connector](platform-connectors.md#deployment-platform-connector) and also set `platformConnector.daemonset.enabled: false`, the number of connections from platform connectors stays fixed. It does not grow with the number of nodes. While the DaemonSet runs, each of its pods opens its own connections, also when no publisher sends events to it. The node still needs a soft limit of at least 64000, because mongod also uses file descriptors for its data files.

Many node images already set a high limit. Some do not. For example, Ubuntu images built with `kubernetes-sigs/image-builder` with default settings give containers a soft limit of 1024. The image-builder tool installs the containerd `LimitNOFILE` drop-in only on Flatcar and Azure Linux. On other images, build with `containerd_enable_limit_no_file: true`.

On other nodes, set `LimitNOFILE` in a systemd drop-in for the container runtime:

```ini
# /etc/systemd/system/containerd.service.d/limit-nofile.conf
[Service]
LimitNOFILE=1048576
```

```bash
sudo systemctl daemon-reload
sudo systemctl restart containerd
```

The new limit applies only to pods that are created after the change. A container that restarts in an existing pod keeps the old limit. After you change the nodes, delete the MongoDB pods one at a time, so that each pod is created again.

To check the limits that mongod runs with:

```bash
# Bitnami
kubectl exec -n nvsentinel mongodb-0 -c mongodb -- bash -c 'grep "Max open files" /proc/$(pgrep -x mongod)/limits'

# Percona Operator
kubectl exec -n nvsentinel mongodb-rs0-0 -c mongod -- bash -c 'grep "Max open files" /proc/$(pgrep -x mongod)/limits'
```

The first number is the soft limit. mongod also logs `Soft rlimits for open file descriptors too low` at startup when the soft limit is below its recommended minimum.

To find the problem before mongod fails, monitor `mongodb_ss_connections` from the [metrics exporter](#metrics-exporter). `conn_type="available"` is the number of connections that mongod can still accept. With a soft limit of 1024, `available` plus `current` is about 819. This alert fires when less than 20 percent of the connection capacity is free:

```promql
mongodb_ss_connections{conn_type="available"}
  / ignoring(conn_type)
(
  mongodb_ss_connections{conn_type="available"}
  + ignoring(conn_type)
  mongodb_ss_connections{conn_type="current"}
)
< 0.2
```

If mongod already fails, refer to [MongoDB Connection Error](../runbooks/mongodb-connection-error.md#issue-4-open-file-limit-too-low).

### Oplog size

`oplogSizeMB` (default `990`, the MongoDB minimum) is the replica-set oplog size in megabytes. Helm does **not** compute this value from the PVC. `990` is the MongoDB floor, not a 24-hour window.

The fault-quarantine, node-drainer, fault-remediation, health-events-analyzer and event-exporter modules resume from change stream tokens. If the oplog drops a token during an outage, the module resumes from the current position and **silently skips** events. Size the oplog so that tokens survive the longest outage that you accept. Keep that size under about **half** the **live** data volume, so that WiredTiger still has room for collections.

#### What to set (summary)

| Knob | Starting point | Notes |
| ---- | -------------- | ----- |
| Desired window | **24 hours** | Covers rolling updates, evictions, and module restarts. If you want extra margin, use **48 hours**. |
| `oplogSizeMB` | If MongoDB already runs, measure the wrap time. Otherwise, use the estimate below | Integer megabytes, minimum `990` |
| PVC | At least **2 × oplog** (Gi), then add TTL data | Bound volumes do not grow from Helm. Do not put a 15 GB oplog on the small **dev** disk of the chart. |

For most clusters, you do not need a separate drain-time term. If consumers have no backlog, the drain time is ~0. The 24-hour window already covers a full day of consumer downtime. If a consumer stays behind ingest for a long time, for example during a large event-exporter backfill, add extra hours. Otherwise, do not add them.

#### Measure on a live cluster (preferred)

Run this script in `mongosh` on any replica:

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

`hoursCapacity` compares the used bytes with the wall-clock span. Thus, the script does not treat a disk that is not yet full as "already holding 24h". If `hoursCapacity` is already ≥ 24, you can keep `oplogSizeMB` as it is. The Job does not shrink the oplog unless `oplogAllowShrink` is true. Still keep the value under about half the live PVC.

After large processor changes, run the script again. The write volume of resume tokens and fault handling can change.

At low event rates, resume-token heartbeats can dominate the oplog. Trust the wrap time, not `events × 350 B`.

#### Estimate before you have production load

```text
oplogSizeMB ≈ event_rate_per_sec × oplog_entry_bytes × window_seconds × extra_writes / 1_000_000
```

| Term | Meaning | How to get it | If you cannot measure yet |
| ---- | ------- | ------------- | ------------------------- |
| `event_rate_per_sec` | HealthEvents inserts per second | `count` in a known window on `HealthEvents`, or scale from a similar cluster | Use **~500/s at ~100k nodes** (~5/s per 1k nodes). Scale linearly from your node count. This is a ceiling for a noisy cluster, not for an idle cluster. |
| `oplog_entry_bytes` | Bytes of one health-event insert in the oplog | A measured insert is **~350 B** uncompressed. Live: oplog `size` ÷ inserts in that window | **350** |
| `window_seconds` | How long a resume token must remain in the oplog | 24h = `86400`; 48h = `172800` | **86400** |
| `extra_writes` | Other oplog traffic per health event (resume-token updates, fault-handling writes) | Live: (oplog bytes in a window) ÷ (health-event count × 350). At high ingest this is usually a small multiplier | **1.5** (1.0 if you only count the insert; 1.5 leaves headroom for tokens and extra processor writes) |

Stored collection size is **not** the same as oplog-entry size: WiredTiger compression (and Percona encryption) shrinks data on disk. Do not use uncompressed JSON size for the PVC data term.

#### PVC and oplog

The data PVC must hold TTL-aged HealthEvents **and** the oplog. WiredTiger needs headroom: keep `oplogSizeMB` under about half the **live** volume.

- **Oplog floor for the disk:** `persistence.size` (Gi) ≳ `2 × oplogSizeMB / 1024`. Example: `15120` MB oplog → at least **32Gi**.
- **TTL data** is often larger than the oplog. After a few days, scale from observed `collStats().storageSize` × (`TTL` / collection age). Uncompressed `rate × 350 B × TTL` overestimates badly.

A higher `persistence.size` does not expand an already-Bound PVC. First, expand and verify the live volume. Then raise `oplogSizeMB`. `replSetResizeOplog` does not check free disk.

#### Worked example (production)

This example uses a **~100k-node** cluster that writes **~500 health events/s**, with **~350 B** per oplog insert, `extra_writes = 1`, and a **24-hour** window:

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

That disk is much larger than the **dev** default of the chart. For a quieter cluster, scale the **500/s** figure with the node count. Then size the disk to at least **2 × oplog** plus TTL data. Do not copy the small default volume of the chart.

Do not copy `15120` to a small or idle cluster.

#### Kind and empty clusters

Keep `oplogSizeMB` at **990** (the MongoDB minimum). The default volume of the chart is for development only. On a Kind hostPath volume, the oplog often starts with the MongoDB default of 5% of the node disk (several GiB). The Job does not shrink the oplog, so that larger window stays.

Also give the same integer to mongod at startup. Then a **new** empty member (replaced PVC, added replica) starts at that size instead of the MongoDB default. Set it in the NVSentinel values, not in the vendored chart templates:

- **Bitnami:** `mongodb.extraFlags` must include `--oplogSize=<oplogSizeMB>`. Helm fails if the flag is missing or different. Do **not** set `mongodb.existingConfigmap` to a snippet `mongodb.conf`. Bitnami then treats the file as fully user-managed and skips `dbPath` / logpath / replSet. The pod then crashloops (`--fork` without `--logpath`).
- **Percona:** `psmdb-db.replsets.rs0.configuration` must contain `replication.oplogSizeMB` equal to `oplogSizeMB`. Helm fails if it is missing or different. An override of `configuration` replaces the whole block, so also keep `setParameter`.

`--set oplogSizeMB=1400` alone is not sufficient. Also set `--oplogSize=1400` (Bitnami) or `replication.oplogSizeMB: 1400` (Percona). A change of the flags or the config file does **not** resize a member that already has data. The init Job still runs `replSetResizeOplog` on every reachable member.

The Job **never shrinks** an existing oplog unless you set `mongodb-store.oplogAllowShrink: true`. A shrink truncates the oldest entries immediately. Change stream watchers then get `ChangeStreamHistoryLost` and resume from the current position (silent event loss).

NVSentinel does not resize an external MongoDB. The Job also skips the resize when there is no PVC: Bitnami `mongodb.persistence.enabled=false` (emptyDir), or Percona `volumeSpec.hostPath` / `emptyDir`.

A completed Job is immutable. A change of `oplogSizeMB` (or of the replica-member list) creates a new Job. The Job skips one unreachable replica after 12 tries (~60s), so that a TTL update is not blocked. If the Job skips two or more members, the Job fails.

### HealthEvents TTL

`collectionExpirySeconds` (default `2592000` / 30d). External MongoDB uses the same key. The init Job is `create-mongodb-database-<seconds>-<hash>` (`-l app.kubernetes.io/name=create-mongodb-database`).

A completed Job is immutable, so the Job gets a new name when it must change. The name holds the TTL and one hash. The hash covers `oplogSizeMB`, `oplogAllowShrink` and the rendered Job pod template, which includes the mongosh init script. Helm and Argo CD create a new Job when the TTL, an oplog setting or the indexes change.

They also create a new Job when the Job pod changes, for example `mongodb.nodeSelector`, tolerations, images or `global.imagePullSecrets`. Argo CD also has `Force=true,Replace=true`, so that it recreates a Failed first run on sync. With Helm, to run the same script again, delete the Job with the label `app.kubernetes.io/name=create-mongodb-database`. Then run a Helm upgrade.

```yaml
mongodb-store:
  collectionExpirySeconds: 604800  # 7 days
```

### SCRAM Application User

If `mongodb.tls.enabled` is `false`, the initialization Job creates a SCRAM-SHA-256 user with `readWrite` access to the NVSentinel database. If TLS is enabled, MongoDB uses X.509 certificate users instead, and the Job does not create this user.

```yaml
mongodb-store:
  scramAppUser:
    username: nvsentinel
    existingSecret: ""
    passwordKey: "mongodb-root-password"
```

#### Parameters

##### username
Name of the application user created in MongoDB.

##### existingSecret
The Secret that holds the password of this user. If it is empty, the Job uses the MongoDB root password secret. This works without more configuration, but it gives the application the same credential as the administrator. For production, create a dedicated Secret. Then set its name here.

##### passwordKey
The key in that Secret that holds the password.

This setting only creates the application user. To use this account, build `MONGODB_URI` from the username, the resolved password, the host and the database. The creation of the application user does not change the credentials that the modules connect with.

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

### Pod Priority

`global.priorityClassName` and `global.systemPriorityClassName` do not apply to the MongoDB pods. Both backends come from vendored upstream charts that do not read the NVSentinel `global` values. Thus, if you set only those global values, the MongoDB StatefulSet has no `priorityClassName`.

Set it with the key of the backend:

```yaml
# Bitnami MongoDB
mongodb-store:
  mongodb:
    priorityClassName: system-cluster-critical

# Percona (note: priorityClass, not priorityClassName)
mongodb-store:
  psmdb-db:
    replsets:
      rs0:
        priorityClass: system-cluster-critical
```

If you run arbiter or hidden members, the Bitnami chart uses separate keys for them: `mongodb.arbiter.priorityClassName` and `mongodb.hidden.priorityClassName`.

Give the datastore at least the priority of the modules that depend on it. A preempted datastore stops fault detection for the whole cluster. The health monitors continue to run at their own higher priority, but they cannot persist what they find.

The `create-mongodb-database` initialization Job takes no priority class from any key. It runs once and completes. Thus, the scheduler places it on the capacity that is free at that time.

For the settings that apply to the NVSentinel components, refer to [Pod Priority](./README.md#pod-priority).

### Metrics Exporter

Configures the MongoDB metrics exporter for monitoring integration.

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
Enables the MongoDB metrics exporter sidecar container.

##### image.repository
Container image for the MongoDB exporter.

##### image.tag
Image tag for the MongoDB exporter.

The exporter exposes metrics on port 9216 for Prometheus scraping.

### Network Policy

Configures the network policy ingress rules for MongoDB pods. The database port (`27017`) is always restricted to the NVSentinel release namespace. When `usePerconaOperator: true`, the metrics exporter port (`9216`) allows ingress from the release namespace by default. It can also permit additional monitoring namespaces or custom ingress rules. These settings apply only when you use the Percona Operator backend.

```yaml
mongodb-store:
  useBitnami: false
  usePerconaOperator: true
  networkPolicy:
    additionalScrapeNamespaces:
      - monitoring
      - prometheus
    additionalScrapeRules: []
```

#### Parameters

##### additionalScrapeNamespaces
List of namespaces permitted to scrape the metrics exporter on port `9216` when `usePerconaOperator: true`. Defaults to empty (`[]`). The release namespace is always permitted.

##### additionalScrapeRules
Custom ingress rules rendered directly under `from:` for the metrics exporter on port `9216` when `usePerconaOperator: true`. Use this to match specific pod labels or IP blocks.

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
