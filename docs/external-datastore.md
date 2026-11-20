# External Datastore Support for NVSentinel

> This document describes the per-CSP configuration for connecting NVSentinel to externally managed database services.

---

## Overview

When the datastore is enabled, NVSentinel deploys its own MongoDB database inside the same
Kubernetes cluster it monitors. This works well for general deployments, but creates an overhead problem for lean GPU
clusters that are focused purely on compute workloads.

NVSentinel can connect to an **externally managed database** —
such as a hosted MongoDB or PostgreSQL service from a cloud provider — instead of running one
inside the monitored cluster.

### Common Requirements

To connect to an external DB, supply these values:

| Field | Purpose | Example |
|---|---|---|
| Full connection URI | Connect to external DB (credentials embedded in the string your provider shows) | Obtain from the provider console; store only in a Kubernetes Secret, not in git |
| TLS enabled/disabled | Whether to use TLS | `true` / `false` |
| CA cert secret name | Kubernetes Secret with CA cert — required only when the service uses a private CA (e.g. AWS DocumentDB). Leave empty for services that use a public CA (MongoDB Atlas, Azure DocumentDB). | `docdb-ca` |

### Expected Behavior

| Scenario | `mongodbStore.enabled` | External MongoDB URI | Result |
|---|---|---|---|
| Internal MongoDB | `true` | N/A | Deploy Bitnami/Percona MongoDB in-cluster |
| External MongoDB | `false` | Secret `MONGODB_URI` + `credentialsFromSecret.name` | Connect to external MongoDB — nothing deployed in-cluster |
| External PostgreSQL | `false` | Per PostgreSQL values (e.g. connection / optional `uri`) | Connect to external PostgreSQL — nothing deployed in-cluster |
| Neither | `false` | No | No DB configured — services will have no connection |

---

## `MONGODB_URI` only from a Kubernetes Secret (external MongoDB)

For **external** managed MongoDB (`global.mongodbStore.enabled: false` and
`global.datastore.provider: mongodb`), the full URI is **not** written to the datastore
ConfigMap. You must create an existing Secret whose **data key** is exactly `MONGODB_URI` and
set `global.datastore.credentialsFromSecret.name`. Helm fails if that name is missing, or if
`global.datastore.uri` is set for this mode.

**In-cluster MongoDB** (for example Tilt / `global.mongodbStore.enabled: true`): when no
`credentialsFromSecret` is set, the chart builds a credential-free `MONGODB_URI` in the
ConfigMap from `global.datastore.connection` (host, port, `extraParams`). You can still set
`credentialsFromSecret` to override with a Secret.

1. Create the Secret. Set `MONGODB_URI` in your shell to the full string from your provider (for example MongoDB Atlas **Connect → Drivers**), or put that single line in a local file that is never committed:

   ```bash
   kubectl create secret generic nvsentinel-datastore-mongodb-uri \
     --from-literal=MONGODB_URI="$MONGODB_URI" \
     --namespace nvsentinel
   ```

   Alternatively: `--from-file=MONGODB_URI=./mongodb-uri.txt` (one line; keep the file out of version control).

2. Reference it in values:

   ```yaml
   global:
     datastore:
       provider: "mongodb"
       credentialsFromSecret:
         name: nvsentinel-datastore-mongodb-uri
       # ... tls, connection, auth as usual ...
   ```

DB-consuming workloads use `envFrom.secretRef` when `credentialsFromSecret.name` is set;
otherwise they read `MONGODB_URI` from the ConfigMap. The embedded `datastore.yaml` does not
include a MongoDB `uri` field from Helm values in either mode.

---

## Per-CSP Configuration Guide

### Connection URI format reference (Secret `MONGODB_URI` value)

Use the **exact connection string** your provider shows in its console as the **literal value** of key `MONGODB_URI` in the Kubernetes Secret (never as `global.datastore.uri` for MongoDB). Do not paste real credentials into docs or source control.

- **MongoDB Atlas:** `mongodb+srv` SRV string from **Connect → Drivers**; include the database path (for example `/HealthEventsDatabase`) and query options your deployment needs (for example `retryWrites=false`).
- **AWS DocumentDB:** `mongodb` URL to the cluster endpoint with TLS and replica-set query parameters per the AWS DocumentDB connection documentation.
- **Azure DocumentDB vCore:** `mongodb+srv` string from **Settings → Connection Strings** (primary); includes host under `mongocluster.cosmos.azure.com` and the TLS/auth query parameters the portal provides.

### Database and Collection Setup (Automatic)

You do **not** need to manually create the `HealthEventsDatabase` database or any collections
in any of the CSPs below. On `helm install`, and on every upgrade that changes the Job name, NVSentinel
automatically runs the `<release>-external-mongodb-setup-<collectionExpirySeconds>-<hash>` Job (`-l app.kubernetes.io/name=external-mongodb-setup`) which:

- Creates the `HealthEventsDatabase` database (MongoDB creates it lazily on first write)
- Creates the `HealthEvents`, `ResumeTokens`, and `MaintenanceEvents` collections if they don't exist
- Creates TTL indexes from `mongodb-store.collectionExpirySeconds` (default 2592000). A TTL change recreates the setup Job and collMod's existing indexes.
- Creates the query indexes and the unique idempotency index of the deployment platform connector (`healthevent_idempotency_key_unique`). The replicas of the deployment platform connector become ready only when this index exists. Documents that share an idempotency key block this index. The Job then prints an error with a query that lists the shared keys, and runs the rest of the setup. Remove the extra documents, then run the Job again.

The Job is a plain release resource, not a Helm hook. It runs on install. An upgrade runs it again only when the Job name changes. The name changes with the TTL, the setup script, and the datastore, pull-secret and placement settings. Kubernetes deletes a finished Job after 24 hours, so the next upgrade after that also runs it again. Helm does not wait for the Job and does not fail when the Job fails. Check the Job yourself:

```bash
kubectl get job -n nvsentinel -l app.kubernetes.io/component=mongodb-init
kubectl logs -n nvsentinel -l app.kubernetes.io/component=mongodb-init
```

The Job completes even when documents share an idempotency key; the log shows that error. To run the Job again, delete it and upgrade.

All you need is a cluster endpoint and a database user with read/write access.

---

### AWS DocumentDB

**Service:** Amazon DocumentDB (MongoDB 5.0 compatible)

---

#### Step 1 — Obtain a DocumentDB Cluster

**Choose one of the two options below** depending on whether you are creating a new cluster
or connecting to one that already exists.

---

##### Option A — Create a New Cluster

In the AWS Console, navigate to **Amazon DocumentDB → Clusters → Create** with these settings:

- **Engine version:** 5.0.0 (minimum; supports change streams)
- **Cluster type:** Instance-based cluster
- **Authentication:** Username/password (SCRAM)
- **VPC:** Same VPC as your EKS cluster (or VPC-peered)
- **Subnet group:** Create a subnet group covering your private subnets
- **Security group:** Create a new security group — allow TCP port `27017` inbound from your EKS node CIDR ranges (see [Networking](#networking-and-security-groups) below)

After creation, note the **Cluster endpoint** (read/write):
```
{cluster-id}.cluster-{suffix}.{region}.docdb.amazonaws.com
```

---

##### Option B — Use an Existing Cluster

Verify the following before proceeding:

1. **Engine version** — must be `5.0.0` or higher (change streams require this).
2. **Change streams** — must be explicitly enabled (see Step 2 below).
3. **Network access** — EKS node CIDR ranges must be allowed on TCP port `27017` in the cluster's security group.
4. **VPC** — cluster must be in the same VPC as your EKS cluster, or reachable via VPC peering / Transit Gateway.
5. **Credentials** — have a username and password ready for NVSentinel.

---

#### Step 2 — Enable Change Streams

DocumentDB does **not** enable change streams by default. You must explicitly enable them.

**2a. Create or update a cluster parameter group:**

In **Amazon DocumentDB → Parameter groups**, create a custom parameter group (or modify an existing one) and set:

```
change_stream_log_retention_duration = 10800   # seconds (3 hours minimum recommended)
```

Apply the parameter group to your cluster and reboot.

**2b. Enable change streams on the database via `mongosh`:**

From a pod with network access to DocumentDB (e.g. a debug pod in your EKS cluster):

```bash
mongosh "$DOCUMENTDB_ADMIN_URI" \
  --eval 'db.adminCommand({ modifyChangeStreams: 1, database: "HealthEventsDatabase", collection: "", enable: true })'
```

Set `DOCUMENTDB_ADMIN_URI` to an admin connection URL from the AWS console for your cluster. Do not store that value in this repository.

Without this, `health-events-analyzer` and `fault-quarantine` will fail to start their change stream watchers.

---

#### Step 3 — Configure DNS Resolution from EKS

DocumentDB cluster endpoints use private DNS names (e.g. `nvsentinel-test-1.cluster-c9g8sagiqhcr.us-east-1.docdb.amazonaws.com`) that resolve within AWS VPC. EKS pods must be able to resolve these names.

**Create an AWS Route 53 Private Hosted Zone:**

1. Go to **Route 53 → Hosted zones → Create hosted zone**.
2. Set the **Domain name** to match the DocumentDB endpoint suffix (e.g. `cluster-c9g8sagiqhcr.us-east-1.docdb.amazonaws.com`).
3. Set **Type** to **Private hosted zone** and associate it with your EKS VPC.
4. Create an **A record** pointing the cluster endpoint hostname to the DocumentDB cluster's private IP address.

> The private IP of the DocumentDB cluster can be found via `nslookup` from any EC2 instance in the VPC, or via the AWS Console under the cluster's instance details.

**Verify from an EKS pod:**

```bash
kubectl run -it --rm dns-test --image=busybox --restart=Never -- \
  nslookup {cluster-endpoint}
```

Expected output: a valid IP address. `NXDOMAIN` means the Route 53 hosted zone or A record is incorrect.

---

#### Step 4 — Create CA Certificate Secret

DocumentDB uses a private Amazon RDS CA that is not trusted by default Go TLS. You must mount it into NVSentinel pods.

**Download the AWS RDS CA bundle:**

```bash
curl -o rds-ca.crt https://truststore.pki.rds.amazonaws.com/us-east-1/us-east-1-bundle.pem
```

**Create a Kubernetes secret:**

```bash
kubectl create secret generic docdb-ca \
  --from-file=ca.crt=rds-ca.crt \
  --namespace nvsentinel
```

The key **must** be named `ca.crt` — this is what the Helm chart volume mounts reference.

---

#### Step 5 — Helm Values Configuration

Create a values override file (e.g. `values-aws-docdb.yaml`) with your connection details.
Put the full connection string (with password) in a Kubernetes Secret with data key
`MONGODB_URI` (see [`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb))
and set `credentialsFromSecret.name`.

Example values:

```yaml
global:
  mongodbStore:
    enabled: false   # disable the in-cluster MongoDB

  datastore:
    provider: "mongodb"
    credentialsFromSecret:
      name: nvsentinel-datastore-mongodb-uri
    connection:
      host: "{cluster-endpoint}"
      port: 27017
      database: "HealthEventsDatabase"
    tls:
      enabled: true
      caSecretName: "docdb-ca"   # name of the secret created in Step 4
```

> Percent-encode any special characters in the password: `#` → `%23`, `@` → `%40`, `%` → `%25`

---

#### Networking and Security Groups

EKS pods can have IPs from **multiple CIDR ranges** depending on the CNI configuration — not just the primary VPC CIDR.

In the DocumentDB cluster's security group, add an inbound rule for **each** CIDR range your pods use:

```
Type:     Custom TCP
Port:     27017
Source:   10.0.0.0/16    # primary VPC CIDR
```

```
Type:     Custom TCP
Port:     27017
Source:   100.65.0.0/16  # secondary pod CIDR (if using custom networking)
```

> If some pods connect successfully but others get `dial tcp: i/o timeout`, check the pod IPs — pods with IPs outside the allowed CIDR range will be blocked by the security group.

To find the pod CIDRs in use:

```bash
kubectl get pods -n nvsentinel -o wide
# Look at the IP column — if some IPs are 100.65.x.x, that CIDR must be added
```

---

### Azure Cosmos DB for MongoDB

**Service:** Azure DocumentDB with MongoDB Compatibility (vCore)

**MongoDB Version:** 8.0

---

#### Important: Choosing the Right Azure Service

Azure has **two different** Cosmos DB MongoDB offerings. They are NOT interchangeable:

| Service | API | Change Streams | Use for NVSentinel? |
|---|---|---|---|
| Azure Cosmos DB for MongoDB (RU/Serverless) | MongoDB 4.2 | **Not supported** | **No** — `NVSentinel` requires Change Streams |
| Azure DocumentDB (with MongoDB compatibility) — vCore | MongoDB 8.0 | **Supported** | **Yes** |

Always create the **"Azure DocumentDB (with MongoDB compatibility)"** resource, NOT "Azure Cosmos DB for MongoDB".

---

#### Step 1 — Obtain a DocumentDB Cluster

**Choose one of the two options below** depending on whether you are creating a new cluster
or connecting to one that already exists.

---

##### Option A — Create a New Cluster

In Azure Portal, create a new **"Azure DocumentDB (with MongoDB compatibility)"** resource
(vCore tier, MongoDB 8.0, SCRAM auth). Match the region to your AKS cluster.

---

##### Option B — Use an Existing Cluster

Verify the following before proceeding:

1. **Service type** — confirm the resource is **Azure DocumentDB vCore** (not RU/Serverless). Change Streams are only supported on vCore.
2. **MongoDB version** — must be **5.0 or higher** (8.0 recommended).
3. **Network access** — AKS outbound IPs must be allowed in **Settings → Networking** to reach port `27017`.
4. **Credentials** — have an admin username and password ready for NVSentinel.

---

##### Connection String (both options)

Go to **Settings → Connection Strings** and copy the **Primary Connection String** into your Kubernetes Secret only (see [`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb)).

If you assemble the URI yourself, percent-encode reserved characters in the database user’s password: `#` → `%23`, `@` → `%40`, `%` → `%25`

> If special characters are not encoded, `platform-connectors` will fail with
> `MongoParseError: Password contains unescaped characters` on startup.

---

#### Step 2 — Helm Values Configuration

Create a values override file (e.g. `values-cosmosdb-test.yaml`) with your connection details.
Put the connection string in a Secret (`MONGODB_URI` key) and set
`credentialsFromSecret.name` — see
[`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb).

```yaml
global:
  mongodbStore:
    enabled: false   # disable the in-cluster MongoDB

  datastore:
    provider: "mongodb"
    credentialsFromSecret:
      name: nvsentinel-datastore-mongodb-uri
    connection:
      host: "{cluster}.mongocluster.cosmos.azure.com"
      port: 27017
      database: "HealthEventsDatabase"
    tls:
      enabled: true
      caSecretName: ""  # Azure uses DigiCert public CA — system CAs are sufficient
```

---

### Google Cloud — MongoDB Atlas

**Service:** MongoDB Atlas (M0 free tier or M10+) hosted on GCP

---

#### Why MongoDB Atlas on GCP?

GCP does not offer a native first-party MongoDB-compatible managed service the way AWS offers DocumentDB or Azure offers Cosmos DB. The only production-grade options on GCP are:

| Option | Change Streams | VPC Peering | Notes |
|---|---|---|---|
| MongoDB Atlas (M10+) | Yes | Yes | Recommended for production |
| MongoDB Atlas (M0 free) | Yes | No | IP allowlisting only; suitable for testing |
| Self-hosted MongoDB on GCE | Yes | Yes | Not managed; operator burden |
| Cloud SQL | No | Yes | PostgreSQL only; no MongoDB API |

MongoDB Atlas is the recommended solution. Atlas uses the standard MongoDB driver protocol so all aggregation features work natively — no compatibility workarounds needed.

---

#### Step 1 — Obtain a MongoDB Atlas Cluster

**Choose one of the two options below** depending on whether you are creating a new cluster
or connecting to one that already exists.

---

##### Option A — Create a New Cluster

1. Go to cloud.mongodb.com → **Create a deployment**
2. Choose **M0 (Free)** for testing or **M10+** for production
3. Select **GCP** as the provider and choose the region closest to your GKE cluster (e.g. `us-central1`)
4. Name the cluster and click **Create**
5. When prompted to create a database user, configure SCRAM-SHA-256 auth in the Atlas UI
6. Copy the full connection string from **Connect → Drivers** and supply it only via the Kubernetes Secret (`MONGODB_URI`); do not commit it to git

---

##### Option B — Use an Existing Cluster

Verify the following before proceeding:

1. **Change streams** — Atlas supports change streams on all tiers (M0 and above). No extra configuration needed.
2. **Network access** — GKE node/pod IPs must be in the Atlas IP Access List (see Step 2).
3. **Credentials** — have a database user with SCRAM-SHA-256 auth credentials ready.

---

#### Step 2 — Configure Network Access

MongoDB Atlas M0 does not support VPC peering. IP allowlisting is the only option.

In the Atlas UI, go to **Security → Network Access → Add IP Address**:

- For **testing**: click **Allow Access from Anywhere** to add `0.0.0.0/0`
- For **production** (M10+): add your GKE node pool CIDR ranges only. Find them with:

```bash
kubectl get nodes -o wide
# Note the EXTERNAL-IP or INTERNAL-IP column for each node
```

> M10+ clusters support VPC peering with GCP.

---

#### Step 3 — Helm Values Configuration

No CA certificate secret is needed — Atlas uses DigiCert public CA which is trusted by the Go TLS runtime by default.

Store the Atlas connection string in a Kubernetes Secret (`MONGODB_URI` key)
and set `credentialsFromSecret.name` — see
[`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb).

Create a values override file `values-atlas-gcp.yaml`:

```yaml
global:
  mongodbStore:
    enabled: false   # disable the in-cluster MongoDB

  datastore:
    provider: "mongodb"
    credentialsFromSecret:
      name: nvsentinel-datastore-mongodb-uri
    connection:
      host: "{cluster}.mongodb.net"
      port: 27017
      database: "HealthEventsDatabase"
    tls:
      enabled: true
      caSecretName: ""   # Atlas uses public DigiCert CA — no custom CA cert required
```

> Percent-encode password characters in the Secret value if needed: `#` → `%23`, `@` → `%40`, `%` → `%25`

---

### OCI

**Status:** This guide has no configuration steps for OCI. No viable managed MongoDB service is available on OCI.

---

#### Options on OCI

| Option | Change Streams | Notes |
|---|---|---|
| Oracle Autonomous Database (MongoDB API) | No | Oracle's own docs explicitly list Change Streams as unsupported. NVSentinel requires Change Streams. |
| MongoDB Atlas on OCI | N/A | MongoDB Atlas officially supports only AWS, GCP, and Azure. OCI is not a supported Atlas provider. |
| OCI Marketplace MongoDB (Apps4Rent, ScaleGrid) | Yes | Only paid third-party options; available in limited regions (UK South London only). |
| Self-managed MongoDB on OCI Compute VM | Yes | Full MongoDB compatibility including Change Streams. Oracle's own reference architecture for MongoDB on OCI recommends this approach. |

The only viable option is a **self-managed MongoDB deployment on OCI Compute VMs**.

---

## References

- [PostgreSQL Provider Documentation](./postgresql-provider.md)
