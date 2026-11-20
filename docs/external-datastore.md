# External Datastore Support for NVSentinel

> This page describes the configuration for each CSP to connect NVSentinel to externally managed database services.

---

## Overview

When the datastore is enabled, NVSentinel deploys its own MongoDB database in the Kubernetes cluster that it monitors. This works well for general deployments. But it adds overhead to lean GPU clusters that run only compute workloads.

NVSentinel can connect to an **externally managed database** instead of a database in the monitored cluster. An example is a hosted MongoDB or PostgreSQL service from a cloud provider.

### Common Requirements

To connect to an external database, supply these values:

| Field | Purpose | Example |
|---|---|---|
| Full connection URI | Connect to the external database (the string that your provider shows includes the credentials) | Get it from the provider console. Store it only in a Kubernetes Secret, not in git |
| TLS enabled/disabled | Whether to use TLS | `true` / `false` |
| CA cert secret name | Kubernetes Secret with the CA certificate. Required only if the service uses a private CA, for example AWS DocumentDB. Leave empty for services that use a public CA (MongoDB Atlas, Azure DocumentDB). | `docdb-ca` |

### Expected Behavior

| Scenario | `mongodbStore.enabled` | External MongoDB URI | Result |
|---|---|---|---|
| Internal MongoDB | `true` | N/A | Deploys Bitnami/Percona MongoDB in-cluster |
| External MongoDB | `false` | Secret `MONGODB_URI` + `credentialsFromSecret.name` | Connects to the external MongoDB. Nothing is deployed in-cluster |
| External PostgreSQL | `false` | As set in the PostgreSQL values (for example, connection or the optional `uri`) | Connects to the external PostgreSQL. Nothing is deployed in-cluster |
| Neither | `false` | No | No database is configured. Services have no connection |

---

## `MONGODB_URI` only from a Kubernetes Secret (external MongoDB)

For **external** managed MongoDB (`global.mongodbStore.enabled: false` and `global.datastore.provider: mongodb`), the chart does **not** write the full URI to the datastore ConfigMap. You must create the Secret yourself. Its **data key** must be exactly `MONGODB_URI`. Then set `global.datastore.credentialsFromSecret.name` to the name of that Secret. Helm fails if that name is missing, or if `global.datastore.uri` is set for this mode.

**In-cluster MongoDB** (`global.mongodbStore.enabled: true`): if no `credentialsFromSecret` is set, the chart builds a credential-free `MONGODB_URI` in the ConfigMap. It builds the URI from `global.datastore.connection` (host, port, `extraParams`). You can also set `credentialsFromSecret` to override this URI with a Secret.

1. Create the Secret. Set `MONGODB_URI` in your shell to the full string from your provider, for example from MongoDB Atlas **Connect → Drivers**. Then run this command:

   ```bash
   kubectl create secret generic nvsentinel-datastore-mongodb-uri \
     --from-literal=MONGODB_URI="$MONGODB_URI" \
     --namespace nvsentinel
   ```

   To use a file instead, use `--from-file=MONGODB_URI=./mongodb-uri.txt`. The file must contain one line. Keep the file out of version control.

2. Refer to the Secret in your values:

   ```yaml
   global:
     datastore:
       provider: "mongodb"
       credentialsFromSecret:
         name: nvsentinel-datastore-mongodb-uri
       # ... tls, connection, auth as usual ...
   ```

If `credentialsFromSecret.name` is set, the workloads that use the database read the Secret with `envFrom.secretRef`. Otherwise, they read `MONGODB_URI` from the ConfigMap. In both modes, the embedded `datastore.yaml` does not include a MongoDB `uri` field from the Helm values.

---

## Per-CSP Configuration Guide

### Connection URI format reference (Secret `MONGODB_URI` value)

Set the key `MONGODB_URI` in the Kubernetes Secret to the **exact connection string** from your provider console. Use it as the **literal value**. For MongoDB, never set it as `global.datastore.uri`. Do not paste real credentials into docs or source control.

- **MongoDB Atlas:** The `mongodb+srv` SRV string from **Connect → Drivers**. Include the database path (for example `/HealthEventsDatabase`) and the query options that your deployment needs (for example `retryWrites=false`).
- **AWS DocumentDB:** A `mongodb` URL to the cluster endpoint, with the TLS and replica-set query parameters that the AWS DocumentDB connection documentation gives.
- **Azure DocumentDB vCore:** The primary `mongodb+srv` string from **Settings → Connection Strings**. It includes a host under `mongocluster.cosmos.azure.com` and the TLS and auth query parameters that the portal provides.

### Database and Collection Setup (Automatic)

You do **not** have to create the `HealthEventsDatabase` database or any collections manually, for any of the CSPs below. On `helm install`, and on every upgrade that changes the Job name, NVSentinel automatically runs the `<release>-external-mongodb-setup-<collectionExpirySeconds>-<hash>` Job (`-l app.kubernetes.io/name=external-mongodb-setup`). This Job does these steps:

- Creates the `HealthEventsDatabase` database (MongoDB creates it lazily on first write)
- Creates the `HealthEvents`, `ResumeTokens`, and `MaintenanceEvents` collections if they do not exist
- Creates TTL indexes from `mongodb-store.collectionExpirySeconds` (default 2592000). A TTL change recreates the setup Job. The Job then updates the existing indexes with collMod.
- Creates the query indexes and the unique idempotency index of the deployment platform connector (`healthevent_idempotency_key_unique`). The replicas of the deployment platform connector become ready only when this index exists. Documents that share an idempotency key block this index. The Job then prints an error with a query that lists the shared keys, and runs the rest of the setup. Remove the extra documents. Then run the Job again.

The Job is a plain release resource, not a Helm hook. It runs on install. An upgrade runs it again only when the Job name changes. The name changes with the TTL, the setup script, and the datastore, pull-secret and placement settings. Kubernetes deletes a finished Job after 24 hours, so the next upgrade after that also runs it again.

Helm does not wait for the Job and does not fail when the Job fails. Check the Job yourself:

```bash
kubectl get job -n nvsentinel -l app.kubernetes.io/component=mongodb-init
kubectl logs -n nvsentinel -l app.kubernetes.io/component=mongodb-init
```

The Job completes even if documents share an idempotency key. The log shows that error. To run the Job again, delete it. Then upgrade the release.

You need only a cluster endpoint and a database user with read/write access.

---

### AWS DocumentDB

**Service:** Amazon DocumentDB (MongoDB 5.0 compatible)

---

#### Step 1 — Obtain a DocumentDB Cluster

**Choose one of the two options below.** Use Option A to create a new cluster. Use Option B to connect to a cluster that already exists.

---

##### Option A — Create a New Cluster

In the AWS Console, go to **Amazon DocumentDB → Clusters → Create**. Use these settings:

- **Engine version:** 5.0.0 (minimum; supports change streams)
- **Cluster type:** Instance-based cluster
- **Authentication:** Username/password (SCRAM)
- **VPC:** Same VPC as your EKS cluster (or VPC-peered)
- **Subnet group:** Create a subnet group that covers your private subnets
- **Security group:** Create a new security group. Allow inbound TCP port `27017` from your EKS node CIDR ranges. Refer to [Networking](#networking-and-security-groups) below.

After creation, write down the **Cluster endpoint** (read/write):

```text
{cluster-id}.cluster-{suffix}.{region}.docdb.amazonaws.com
```

---

##### Option B — Use an Existing Cluster

Before you continue, verify these items:

1. **Engine version** — must be `5.0.0` or higher (change streams require this).
2. **Change streams** — must be explicitly enabled (see Step 2 below).
3. **Network access** — the security group of the cluster must allow the EKS node CIDR ranges on TCP port `27017`.
4. **VPC** — the cluster must be in the same VPC as your EKS cluster, or reachable through VPC peering or Transit Gateway.
5. **Credentials** — have a username and password ready for NVSentinel.

---

#### Step 2 — Enable Change Streams

DocumentDB does **not** enable change streams by default. You must enable them explicitly.

**2a. Create or update a cluster parameter group:**

In **Amazon DocumentDB → Parameter groups**, create a custom parameter group or change an existing one. Set this parameter:

```text
change_stream_log_retention_duration = 10800   # seconds (3 hours minimum recommended)
```

Apply the parameter group to your cluster. Then reboot the cluster.

**2b. Enable change streams on the database with `mongosh`:**

Run this command from a pod with network access to DocumentDB, for example a debug pod in your EKS cluster:

```bash
mongosh "$DOCUMENTDB_ADMIN_URI" \
  --eval 'db.adminCommand({ modifyChangeStreams: 1, database: "HealthEventsDatabase", collection: "", enable: true })'
```

Set `DOCUMENTDB_ADMIN_URI` to an admin connection URL from the AWS console for your cluster. Do not store that value in source control.

Without this step, the modules that read the change stream cannot start their change stream watchers: fault-quarantine, node-drainer, fault-remediation, health-events-analyzer and event-exporter.

---

#### Step 3 — Configure DNS Resolution from EKS

DocumentDB cluster endpoints use private DNS names that resolve within the AWS VPC. The names have the form `{cluster-id}.cluster-{suffix}.{region}.docdb.amazonaws.com`. EKS pods must be able to resolve these names.

**Create an AWS Route 53 Private Hosted Zone:**

1. Go to **Route 53 → Hosted zones → Create hosted zone**.
2. Set the **Domain name** to match the DocumentDB endpoint suffix, in the form `cluster-{suffix}.{region}.docdb.amazonaws.com`.
3. Set **Type** to **Private hosted zone**. Associate the zone with your EKS VPC.
4. Create an **A record** that points the cluster endpoint hostname to the private IP of the DocumentDB cluster.

> To find the private IP of the DocumentDB cluster, run `nslookup` from any EC2 instance in the VPC. You can also find it in the AWS Console, in the instance details of the cluster.

**Verify from an EKS pod:**

```bash
kubectl run -it --rm dns-test --image=busybox --restart=Never -- \
  nslookup {cluster-endpoint}
```

Expected output: a valid IP address. `NXDOMAIN` means that the Route 53 hosted zone or the A record is incorrect.

---

#### Step 4 — Create CA Certificate Secret

DocumentDB uses a private Amazon RDS CA. Go TLS does not trust this CA by default. Thus, you must mount it into the NVSentinel pods.

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

The key **must** have the name `ca.crt`. The volume mounts of the Helm chart refer to this key.

---

#### Step 5 — Helm Values Configuration

Create a values override file with your connection details, for example `values-aws-docdb.yaml`. Put the full connection string (with password) in a Kubernetes Secret with the data key `MONGODB_URI`. Then set `credentialsFromSecret.name`. Refer to [`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb).

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

EKS pods can have IPs from **multiple CIDR ranges**, not only from the primary VPC CIDR. This depends on the CNI configuration.

In the security group of the DocumentDB cluster, add an inbound rule for **each** CIDR range that your pods use:

```text
Type:     Custom TCP
Port:     27017
Source:   10.0.0.0/16    # primary VPC CIDR
```

```text
Type:     Custom TCP
Port:     27017
Source:   100.65.0.0/16  # secondary pod CIDR (if using custom networking)
```

> If some pods connect successfully but others get `dial tcp: i/o timeout`, examine the pod IPs. The security group blocks pods with IPs outside the allowed CIDR range.

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

**Choose one of the two options below.** Use Option A to create a new cluster. Use Option B to connect to a cluster that already exists.

---

##### Option A — Create a New Cluster

In the Azure Portal, create a new **"Azure DocumentDB (with MongoDB compatibility)"** resource (vCore tier, MongoDB 8.0, SCRAM auth). Match the region to your AKS cluster.

---

##### Option B — Use an Existing Cluster

Before you continue, verify these items:

1. **Service type** — confirm that the resource is **Azure DocumentDB vCore** (not RU/Serverless). Only vCore supports Change Streams.
2. **MongoDB version** — must be **5.0 or higher** (8.0 recommended).
3. **Network access** — **Settings → Networking** must allow the AKS outbound IPs to reach port `27017`.
4. **Credentials** — have an admin username and password ready for NVSentinel.

---

##### Connection String (both options)

Go to **Settings → Connection Strings**. Copy the **Primary Connection String** into your Kubernetes Secret only. Refer to [`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb).

If you assemble the URI yourself, percent-encode the reserved characters in the password of the database user: `#` → `%23`, `@` → `%40`, `%` → `%25`

> If you do not encode special characters, `platform-connectors` fails at startup with `MongoParseError: Password contains unescaped characters`.

---

#### Step 2 — Helm Values Configuration

Create a values override file with your connection details, for example `values-cosmosdb-test.yaml`. Put the connection string in a Secret (`MONGODB_URI` key). Then set `credentialsFromSecret.name`. Refer to [`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb).

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

AWS offers DocumentDB and Azure offers Cosmos DB. GCP does not offer an equivalent native first-party MongoDB-compatible managed service. The only production-grade options on GCP are:

| Option | Change Streams | VPC Peering | Notes |
|---|---|---|---|
| MongoDB Atlas (M10+) | Yes | Yes | Recommended for production |
| MongoDB Atlas (M0 free) | Yes | No | IP allowlisting only; suitable for testing |
| Self-hosted MongoDB on GCE | Yes | Yes | Not managed; operator burden |
| Cloud SQL | No | Yes | PostgreSQL only; no MongoDB API |

MongoDB Atlas is the recommended solution. Atlas uses the standard MongoDB driver protocol, so all aggregation features work natively. No compatibility workarounds are necessary.

---

#### Step 1 — Obtain a MongoDB Atlas Cluster

**Choose one of the two options below.** Use Option A to create a new cluster. Use Option B to connect to a cluster that already exists.

---

##### Option A — Create a New Cluster

1. Go to cloud.mongodb.com → **Create a deployment**
2. Choose **M0 (Free)** for testing or **M10+** for production
3. Select **GCP** as the provider. Choose the region closest to your GKE cluster (for example, `us-central1`)
4. Name the cluster. Then click **Create**
5. When the Atlas UI prompts you to create a database user, configure SCRAM-SHA-256 auth
6. Copy the full connection string from **Connect → Drivers**. Supply it only through the Kubernetes Secret (`MONGODB_URI`). Do not commit it to git

---

##### Option B — Use an Existing Cluster

Before you continue, verify these items:

1. **Change streams** — Atlas supports change streams on all tiers (M0 and above). No extra configuration is necessary.
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

You do not need a CA certificate secret. Atlas uses the DigiCert public CA, which the Go TLS runtime trusts by default.

Store the Atlas connection string in a Kubernetes Secret (`MONGODB_URI` key). Then set `credentialsFromSecret.name`. Refer to [`MONGODB_URI` only from a Kubernetes Secret](#mongodb_uri-only-from-a-kubernetes-secret-external-mongodb).

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

> If necessary, percent-encode the password characters in the Secret value: `#` → `%23`, `@` → `%40`, `%` → `%25`

---

### OCI

**Status:** This page has no configuration steps for OCI. No viable managed MongoDB service is available on OCI.

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
