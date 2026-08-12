# ADR-013: MongoDB Migration from Bitnami

## Context

The NVSentinel platform relies on MongoDB as its primary data store for persisting health events. Previously, this was deployed via a heavily customized Bitnami Helm chart, vendored as a subchart within our `mongodb-store` component.

While functional, this approach presented significant operational challenges:
- **Upstream Image Deprecations:** As announced in the [official Bitnami containers issue #83267](https://github.com/bitnami/containers/issues/83267), effective August 28th, 2025, Bitnami moved most versioned container images to a read-only `docker.io/bitnamilegacy` registry, with no further updates or security patches. Our deployment depended on these now-archived images, materially increasing long-term maintenance and security risk. See also: [Appsmith's guidance on Bitnami image deprecation](https://docs.appsmith.com/getting-started/setup/instance-management/bitnami-image-deprecation). 
- **Manual Lifecycle Management:** The Bitnami chart directly created a Kubernetes `StatefulSet`. All "Day 2" operations, such as database version upgrades, scaling, member recovery, and configuration changes, were manual, brittle, and required direct `kubectl` intervention.
- **Lack of Integrated Features:** Critical production features like automated backups, point-in-time recovery (PITR), and advanced monitoring were not part of the chart. Implementing them would require building and maintaining separate, complex solutions.
- **Complex Templating:** To achieve our security requirements (TLS/mTLS with `cert-manager`, X.509 authentication), we had to write complex Go template logic within our `mongodb-store` chart. This included loops to generate per-replica certificates, which was fragile and hard to maintain.

The goal was to modernize our database layer by adopting a Kubernetes Operator, which automates lifecycle management and provides a declarative API for managing the database.

## Decision: Adopt the Percona Operator for MongoDB

We decided to migrate from the Bitnami Helm chart to the **Percona Operator for MongoDB**. This decision was made after evaluating the available open-source MongoDB deployment options for Kubernetes, including the official MongoDB Community Operator and manual StatefulSet approaches. We concluded that Percona provided the best (and effectively the only viable) combination of enterprise-grade features, operational flexibility, and compatibility with our existing infrastructure requirements among fully open-source solutions.

### Options Considered

#### 1. Official MongoDB Community Operator

- **Pros:** It is the official operator from MongoDB.
- **Cons:**
    - **Replica Set Only (no documented sharding):** The Community Operator’s primary CRD is `MongoDBCommunity`, which targets replica set deployments; there is no documented sharded-cluster management in the Community Operator. Sharding orchestration is documented for MongoDB’s Enterprise/Atlas tooling, not the Community Operator. See: MongoDB Community Operator Helm chart and operator repo overviews.
    - **No Integrated Backup/PITR:** The Community Operator does not provide a built-in backup controller or PITR. MongoDB’s integrated backup automation is tied to Ops Manager/Cloud Manager (Enterprise/Atlas); there is no community-equivalent controller comparable to Percona’s PBM integration.
    - **More Basic CRD Surface:** The Community CRD does not document first-class support for pod `sidecars` or a direct `cert-manager` Issuer binding (e.g., no `tls.issuerConf` equivalent). TLS is typically provided via Secrets you create and manage yourself, which is workable but less integrated than Percona’s model for our use case.
    - **Operational Gaps:** Rolling upgrades, observability, and user management can be done, but require more manual integration and/or external systems compared to Percona’s operator.

  References:
  - MongoDB Helm charts (Community Operator): https://github.com/mongodb/helm-charts/tree/main/charts/community-operator
  - MongoDB Kubernetes Operator (Community) repository: https://github.com/mongodb/mongodb-kubernetes-operator
  - MongoDB Ops Manager (backup is an Enterprise feature): https://www.mongodb.com/docs/ops-manager/current/backup/ 

#### 2. Percona Operator for MongoDB

- **Pros:**
    - **Full Topology Support:** Provides native, declarative support for both replica sets and sharded clusters, ensuring a future-proof growth path.
    - **Integrated, Open-Source "Day 2" Features:** Comes with built-in, declarative APIs for **Percona Backup for MongoDB (PBM)** for automated backups with **Point-in-Time Recovery (PITR)**. PITR is achieved through continuous oplog archival to remote storage, allowing restoration to any specific timestamp. The operator's CRD includes native support for **sidecar containers**, which we use to deploy `mongodb_exporter` for Prometheus metrics collection directly alongside each MongoDB pod.
    - **Excellent Flexibility:** The Custom Resource (CRD) API is highly configurable and designed for integration. It has first-class support for adding `sidecars` and an explicit `tls.issuerConf` block for seamless `cert-manager` integration.
    - **Open-Source Operator & Tooling:** The operator and backup tooling (PBM) are Apache 2.0; the database server (Percona Server for MongoDB) is SSPL. See Licensing for details.

  References:
  - Percona Operator docs (features, sharding, automation): https://docs.percona.com/percona-operator-for-mongodb/
  - Backups & PITR with PBM (Operator docs): https://docs.percona.com/percona-operator-for-mongodb/backups.html
  - PITR configuration (oplog archival): https://docs.percona.com/percona-operator-for-mongodb/backups.html#store-operations-logs-for-point-in-time-recovery

- **Cons:**
    - Requires another controller (the operator) running in the cluster. This is an acceptable trade-off for the automation benefits gained.

#### 3. Maintain Our Own Custom Helm Chart

- **Pros:** Maximum control over every configuration detail.
- **Cons:** 
    - **Highest Operational Burden:** We would have to manually implement and maintain:
      - Our own Helm chart templates for `StatefulSet`, `Service`, `PersistentVolumeClaim`, and networking
      - Scripted logic for all "Day 2" operations: version upgrades (requiring manual rolling restart strategies), horizontal scaling (adding/removing replica set members), configuration changes, and pod recovery
      - TLS certificate lifecycle management (generation, rotation, distribution to each pod)
      - MongoDB-specific operational knowledge embedded in scripts rather than leveraged from a battle-tested operator
    - **Container Image Management:** We would need to either:
      - Build and maintain our own MongoDB container images with security patches and updates, including a CI/CD pipeline for image builds, vulnerability scanning, and registry hosting
      - Or depend on upstream images (MongoDB Community, Percona Server for MongoDB, or similar) without operator-level lifecycle automation, requiring manual intervention for breaking changes or version migrations
    - **Ongoing Maintenance Costs:** Every MongoDB version upgrade, security patch, or operational pattern change would require custom chart updates, testing, and rollout procedures. This essentially means re-implementing operator functionality piecemeal, without the benefit of community testing, documentation, or support that comes with established operators like Percona's.

### Licensing and Open Source Model

A key factor in this decision was Percona's commitment to open source, which differs significantly from MongoDB's own licensing strategy.

- **Percona Operator & Tools (Apache 2.0):** The 
`percona-server-mongodb-operator` itself, along with key ecosystem 
tools like `percona-backup-mongodb`, are licensed under the permissive 
Apache 2.0 license. This provides maximum flexibility and avoids vendor 
lock-in for the management layer. References:
  - Operator license: https://github.com/percona/percona-server-mongodb-operator/blob/main/LICENSE
  - PBM license: https://github.com/percona/percona-backup-mongodb/blob/main/LICENSE
- **Percona Server for MongoDB (SSPL):** The underlying database server, `Percona Server for MongoDB`, is distributed under the **Server Side Public License (SSPL)** (Percona describes it as “source-available”). Citation:
  - PSMDB license: https://github.com/percona/percona-server-mongodb/blob/v8.0/LICENSE-Community.txt
- **MongoDB's Licensing:** MongoDB's Community Operator is Apache 2.0, but the database it deploys is SSPL. More advanced operator features (e.g., integrated backup orchestration) live behind the Enterprise/Ops Manager/Atlas ecosystem.

**SSPL Compliance Note:**
Percona Server for MongoDB is licensed under SSPL. This requires that if the software is offered as a service to third parties, the Service Source Code must be made available.

- **Review Status:** This architecture is intended for the NVSentinel platform. Users deploying this stack must verify their usage compliance with SSPL, especially if offering the platform as a managed service to external customers.
- **Risk Assessment:** For internal platform usage, SSPL obligations are generally contained. Multi-tenant or public-facing service deployments should undergo legal review.

Percona's model provides a "best of both worlds" scenario: a permissively licensed, open-source management layer that provides enterprise-grade features (like backups) for free, while still using the SSPL-licensed database core. This avoids the licensing complexities and costs associated with MongoDB's enterprise offerings.

References:
- Operator docs (features, configuration, TLS, backups, sidecars): https://docs.percona.com/percona-operator-for-mongodb/index.html
- Operator release notes (active maintenance cadence): https://docs.percona.com/percona-operator-for-mongodb/RN/index.html

## Architecture & Implementation

The migration was implemented by making our `mongodb-store` chart a dual-backend system, capable of deploying either the old Bitnami chart or the new Percona stack based on a boolean flag. This de-risked the migration and showcased Percona's adaptability.

### 1. Conditional Dependencies

The `mongodb-store/Chart.yaml` was modified to conditionally include either Bitnami or the two Percona charts (`psmdb-operator` and `psmdb-db`):

```yaml
# In distros/kubernetes/nvsentinel/charts/mongodb-store/Chart.yaml
dependencies:
  - name: mongodb # Old Bitnami chart
    condition: useBitnami
  - name: psmdb-operator # Percona Operator
    condition: usePerconaOperator
  - name: psmdb-db # Percona Database CRD
    condition: usePerconaOperator
```

The condition paths are relative to the `mongodb-store` chart's own values, so users set them as `mongodb-store.useBitnami` and `mongodb-store.usePerconaOperator` in the umbrella chart values.

### 2. Declarative Configuration via Custom Resource

Instead of directly templating a `StatefulSet`, we now create a high-level `PerconaServerMongoDB` resource. This resource captures our intent, and the operator handles the low-level implementation.

**Key Integrations from `values.yaml`:**

- **`cert-manager` Integration:** We continue to use `cert-manager` to manage TLS certificates. Client certificates are generated via cert-manager `Certificate` resources that reference our `mongodb-psmdb-issuer`. The Percona Operator is configured to use TLS mode, and certificates are provided to the pods via Kubernetes Secrets created by cert-manager.
  ```yaml
  tls:
    mode: requireTLS
  ```

- **First-Class Sidecar Support for Metrics:** Our `mongodb_exporter` for Prometheus was cleanly integrated using the operator's native `sidecars` API.
  ```yaml
  replsets:
    rs0:
      sidecars:
        - name: mongodb-exporter
          image: percona/mongodb_exporter:0.40.0
          args:
            - --discovering-mode
            - --compatible-mode
            - --collect-all
            - --web.listen-address=:9216
            - --mongodb.direct-connect
          ports:
            - name: metrics
              containerPort: 9216
  ```

### 3. Shift in TLS Management

**Bitnami Approach (Previous):**
- Used cert-manager to generate **per-replica server certificates** via a Go template loop (e.g., `mongo-server-cert-0`, `mongo-server-cert-1`, `mongo-server-cert-2`)
- Also used cert-manager to generate **client certificates** for application connectivity
- Both server and client certificates referenced our custom `mongo-ca-issuer`

**Percona Approach (Current):**
- The Percona Operator **auto-generates server certificates** internally when `tls.mode: requireTLS` is set, eliminating the need for per-replica certificate templates
- We continue using cert-manager to generate a **client certificate** (`mongo-app-client-cert`) that references our custom `mongodb-psmdb-issuer`
- **CA Validation on Reinstall:** A Job init container validates that the client certificate's CA matches the current issuer CA after `helm uninstall`/`helm install` cycles.
  - *Note:* This Job runs asynchronously and does not strictly block MongoDB startup. In case of repeated failures, operators should inspect the Job logs and manually rotate the secret if necessary.
  - **Argo CD Compatibility:** Using a Job for secret management avoids the usage of Helm `lookup` functions, which are often problematic in GitOps/Argo CD environments (as they fail during dry-runs or template generation if the cluster is not accessible or if the resource doesn't exist yet). This approach ensures that secret generation logic is executed reliably on the cluster.
- This hybrid approach simplifies our Helm templates by removing the complex per-replica server certificate loop while maintaining cert-manager integration for client authentication

**Simplification Achieved:**
The removal of the per-replica server certificate generation (the `{{- range $i := until $replicaCount }}` loop in `certmanager.yaml`) significantly reduced template complexity. The operator now handles server certificate provisioning and rotation automatically, while we retain full control over client certificate issuance via our existing cert-manager infrastructure.

### 4. Architectural Shift Summary

| Aspect | Old (Bitnami) | New (Percona Operator) |
| :--- | :--- | :--- |
| **Control Model** | **Imperative:** Manually define a `StatefulSet`. | **Declarative:** Define a `PerconaServerMongoDB` resource; the operator builds the `StatefulSet`.|
| **Lifecycle** | **Manual:** Upgrades, scaling, and recovery are manual `kubectl` tasks. | **Automated:** The operator handles rolling upgrades, scaling, and pod self-healing. |
| **Backups** | **None:** Required a separate, custom-built solution. | **Integrated:** Declarative, scheduled backups and PITR via the `backup` block in the CRD. |
| **Integration** | **Brittle:** Required complex template logic in our chart to inject features. | **Flexible:** Native support for `sidecars` via a purpose-built CRD API. Server TLS certificates are auto-generated by the operator; client certificates continue to use our existing cert-manager infrastructure. |

## Migration Procedure

The full, tested procedure lives in the [MongoDB Bitnami to Percona migration runbook](../runbooks/mongodb-bitnami-to-percona-migration.md). The runbook is the canonical reference; the summary below only records the shape of the procedure and the pitfalls found while validating it on real clusters (amd64 on OCI and arm64 on AWS, August 2026).

The migration is a remove-and-redeploy of the datastore. On the runbook's default path, health event data is preserved with a dump before removal and a restore afterward (document IDs survive, so node annotations and remediation resources stay valid); on the opt-out clean path, existing data is dropped.

1. **Dump** (default path): stop the reference-writing components, then stream a mongodump archive out of the old backend, always excluding `ResumeTokens`.

2. **Remove** the installation (through the GitOps controller, or `helm uninstall`).

3. **Cleanup.** The removal leaves behind the PVCs, the `mongodb` credentials secret (kept by resource policy), the TLS secrets (`mongo-root-ca-secret`, `mongo-app-client-cert-secret`, and one `mongo-server-cert-<n>` per replica from cert-manager, plus the job-created `mongo-ca-secret`), and two runtime-created ConfigMaps (`resume-control` and `circuit-breaker`). All of them must be deleted. Leftover TLS secrets are the sharpest edge: they belong to the old certificate authority, and if they survive, every datastore consumer crash-loops against the new deployment with generic connection errors.

4. **Clear fault-handling state** (clean path only): node annotations (`quarantineHealthEvent*`, `latestFaultRemediationState`) and remediation resources embed database IDs from the old datastore. Stale quarantine annotations in particular cause node-drainer to retry a missing event lookup every minute, indefinitely. On the default path this state is kept, because the restore makes it valid again.

5. **Redeploy** with Percona enabled (`mongodb-store.useBitnami=false`, `mongodb-store.usePerconaOperator=true`, `global.mongodbStore.enabled=true`). Set the psmdb volume size to at least the cloud provider's minimum block volume size (on OCI, 50Gi); otherwise the operator refuses to reconcile and the replica set never initializes.

6. **Verify and restore:** five gates must pass (the `PerconaServerMongoDB` resource reaches `ready`, the `create-mongodb-database` job completes, the mongod pods are ready, `MONGODB_URI` points at the Percona service, and every deployed datastore consumer logs a successful ping), then on the default path the archive is restored and the consumers restarted.

**Do not attempt the switch as an in-place `helm upgrade` on an existing release.** The backend flags change the immutable pod template of the `create-mongodb-database` Job, so the upgrade fails partway through and leaves both backends partially deployed, with the service configuration already pointing at the not-yet-functional Percona endpoint. Recovery requires a Helm rollback plus manual deletion of the resources the failed upgrade created.

## Consequences

### Positive Outcomes

1. **Reduced Operational Overhead:**
   - **Automated Lifecycle Management:** The operator handles rolling upgrades, scaling (both horizontal and vertical), and pod self-healing without manual `kubectl` intervention.
   - **Simplified Helm Templates:** Removed 50+ lines of complex Go template logic for per-replica certificate generation (the `{{- range $i := until $replicaCount }}` loop in `certmanager.yaml`).
   - **Declarative Configuration:** Changed from imperative `StatefulSet` definitions to declarative `PerconaServerMongoDB` custom resources, making intent clearer and reducing configuration drift.

2. **Enhanced Production Readiness:**
   - **Integrated Backup Solution:** Clear path to enabling automated backups and Point-in-Time Recovery (PITR) via Percona Backup for MongoDB (PBM), eliminating the need to build a custom backup solution.
   - **Native Metrics Export:** `mongodb_exporter` runs as a sidecar on each MongoDB pod, providing per-replica metrics via a clean CRD API rather than requiring a separate Deployment.

3. **Future-Proof Architecture:**
   - **Sharding Support:** While currently using a 3-node replica set, the operator provides native sharding capabilities if we need to scale beyond a single replica set's capacity.
   - **Active Maintenance:** Percona Operator receives regular updates (latest: 1.21.1, released October 2025) with MongoDB 8.0 support, ensuring compatibility with current MongoDB versions.
   - **Escape from Deprecated Images:** No longer dependent on `docker.io/bitnamilegacy` images that moved to a read-only registry in August 2025.
   - **ARM64 Support:** The Bitnami MongoDB images are published for amd64 only, so the default backend cannot run on ARM64 nodes (the pod fails at image pull with `no match for platform in manifest`). The Percona operator, server, and exporter images are all multi-arch, which makes the Percona backend the supported way to run NVSentinel on ARM64 clusters (see issue #1497).

### Breaking Changes

- **Bitnami Chart Status:** The Bitnami subchart is controlled by the `useBitnami` flag. We plan to remove the Bitnami path in a future release once the Percona Operator integration is fully validated.
- **Storage:** Switching to the Percona Operator involves new Persistent Volume Claims; the Bitnami volumes themselves are deleted during the migration. The data they held is preserved only via the runbook's default dump-and-restore path; on the opt-out clean path it is lost.
- **Configuration:** `values.yaml` structure for MongoDB configuration has changed. `mongodb.*` values (Bitnami) are ignored when `usePerconaOperator` is enabled, replaced by `psmdb-db.*` and `psmdb-operator.*`.

### Trade-offs and Considerations

1. **MongoDB-Specific Operator Lock-in:**
   - **Consideration:** We're now dependent on Percona's operator for database management. If we want to switch to a different operator in the future, that would require another migration.
   - **Mitigation:** Percona Operator is open source (Apache 2.0), actively maintained, and has a strong community.

2. **Job-Based Secret Management:**
   - **Consideration:** Using Job init containers to create secrets (keyfile, encryption key) and validate CAs means these operations happen at Job execution time rather than during `helm template` rendering. This adds a runtime dependency on the Job's successful execution before MongoDB can start.
   - **Mitigation:** The Job has proper error handling, RBAC permissions, and idempotent logic (reuses existing secrets).

## References

### Percona Operator
- Percona Operator for MongoDB documentation (features, configuration, TLS, backups, sidecars): https://docs.percona.com/percona-operator-for-mongodb/index.html
- Percona Operator for MongoDB release notes (active maintenance cadence): https://docs.percona.com/percona-operator-for-mongodb/RN/index.html
- Percona Server for MongoDB 8.0 release notes (source-available build, compatibility): https://docs.percona.com/percona-server-for-mongodb/8.0/release_notes/8.0.12-4.html
- Percona Operator license (Apache 2.0): https://github.com/percona/percona-server-mongodb-operator/blob/main/LICENSE
- Percona Backup for MongoDB (PBM) license (Apache 2.0): https://github.com/percona/percona-backup-mongodb/blob/main/LICENSE
- Percona Server for MongoDB license (SSPL): https://github.com/percona/percona-server-mongodb/blob/v8.0/LICENSE-Community.txt