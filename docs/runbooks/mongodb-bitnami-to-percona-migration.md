# MongoDB Migration: Bitnami to Percona Operator

This runbook tells you how to move an existing NVSentinel installation from the Bitnami MongoDB backend (the default) to the Percona Operator backend.

Common reasons to migrate:

- Your cluster has ARM64 nodes. Bitnami publishes the MongoDB images only for amd64, so the default backend cannot run on ARM64 nodes. The Percona images are multi-arch. For more information, see [MongoDB Store Configuration](../configuration/mongodb-store.md).
- You want features of the Percona Operator, such as automated lifecycle management and integrated backups.

The commands below use the release name `nvsentinel` in the namespace `nvsentinel`. If your installation uses different names, change both.

The runbook is a sequence of steps. If a step has options, select the option that matches your setup:

| Step | Options |
| ---- | ------- |
| 1. Check readiness | common |
| 2. Stop reconciliation | GitOps-managed installations only |
| 3. Capture the data | 3a preserve health event data (default), 3b start clean |
| 4. Remove the installation | 4a GitOps-managed (ArgoCD, Flux), 4b Helm-managed |
| 5. Delete the datastore leftovers | common (one extra flag on path 3b) |
| 6. Deploy the Percona backend | 6a GitOps-managed, 6b Helm-managed |
| 7. Verify | common |
| 8. Restore and restart | path 3a only |

## What to expect

- **The default path preserves your health event data.** The dump and restore move the health events into the new backend and keep their document IDs. Thus, node annotations and maintenance CRs stay valid, and in-flight fault handling resumes.
- **The clean path (3b) drops all health event data.** The monitors fill the new database again when they detect issues, but the history is lost. If the monitors already read a one-time fault from the logs, such as a GPU XID, they never raise it again.
- **Never switch backends in place.** A change to `useBitnami` and `usePerconaOperator` does not migrate the data. Services can connect to a new, empty database, and the existing health events are then not available. Node annotations and maintenance CRs can then refer to health events that are not in the new database. To remove and redeploy the installation, follow this runbook. If you must preserve the health events, use the dump and restore path.
- **The migration requires no cloud infrastructure changes.** Both backends listen on the same ports (27017 for mongod, 9216 for metrics). Every Service is cluster-internal, storage comes from the same StorageClass, and nothing calls cloud APIs. The migration is entirely in-cluster. In locked-down environments, check these three things:
  - Your cluster must be able to pull the Percona images (`percona/*` and the `alpine/kubectl` helper) from your registry path.
  - The person who runs the migration needs cluster-level Kubernetes RBAC, because the Percona path installs cluster-scoped CRDs.
  - If you built custom network policies or exposure for the old service name, update them.
- Plan a maintenance window. Between the removal and the completed redeploy, NVSentinel does not monitor the cluster.

## Helper scripts

The repository has scripts in `scripts/mongodb-migration/` that do the mechanical parts of this runbook:

| Script | Step | What it does |
| ------ | ---- | ------------ |
| `preflight.sh` | step 1 | Read-only readiness check. It checks the current backend, cert-manager, storage class minimums compared to the requested volume size, quarantined nodes, in-flight maintenance CRs and log-collector jobs. Prints a verdict table. Exits 2 when blocked. |
| `migrate-data.sh` | steps 3a and 8 | `dump` streams a mongodump archive out of the old backend, and always excludes `ResumeTokens`. It fails closed while the components that write references still run. `--stop-writers` first scales them down and waits for their pods to terminate. `restore` streams the archive into the new backend. It fails closed while any deployed datastore consumer is not ready. Document IDs stay the same, so node annotations and maintenance CR names stay valid. |
| `cleanup.sh` | step 5 | Deletes everything that the removal leaves, then verifies that nothing remains. Refuses to run while a Helm release is still installed. Asks for confirmation. `--dry-run` prints the plan. `--clear-fault-state` also clears node annotations, NVSentinel-owned maintenance CRs and event-labeled log-collector jobs. |
| `verify.sh` | step 7 | Waits for the five post-install gates and prints a verdict table. |

All four scripts use `NVSENTINEL_NAMESPACE`. `preflight.sh` and `cleanup.sh` also use `NVSENTINEL_RELEASE`. Both variables default to `nvsentinel`. The scripts automate the steps, but the decisions stay with you: you select the data path, and you review the quarantined nodes.

## Using the agent skills

For runs with an AI agent, `skills/mongodb-migration/bitnami-to-percona/` contains three agent skills. The skills take the agent through this runbook in sequence, with the required confirmation gates:

1. `check-mongodb-migration-readiness` (read-only)
2. `migrate-mongodb-to-percona` (destructive, gated on your explicit confirmation)
3. `verify-mongodb-percona-migration`

The skills are plain SKILL.md files. They work with any agent that reads that format. You can use them in two ways:

- **Point the agent at the files.** Open this repository with your agent (Claude Code, Codex, Cursor, or a similar agent). Then tell the agent to follow `skills/mongodb-migration/bitnami-to-percona/check-mongodb-migration-readiness/SKILL.md`. At its end, each skill gives the name of the next skill to run.
- **Install them as named skills.** Copy the three directories into the skills location of your agent. For Claude Code, this location is `.claude/skills/` in a project or `~/.claude/skills/`. For Codex, it is `$CODEX_HOME/skills/`. Then invoke the skills by name.

In both cases, the skills never do a destructive step until you explicitly confirm the data-path decision and the quarantined-node plan in the conversation.

## Step 1: Check readiness

```bash
scripts/mongodb-migration/preflight.sh
```

Before you continue, resolve all FAIL rows. While the check is blocked, it exits 2. Then make the three decisions that the later steps depend on:

- **Data path:** preserve health event data (3a, the default) or start clean (3b). If the cluster has active quarantines or in-flight remediations, prefer 3a. On path 3a, `migrate-data.sh` connects to MongoDB at `svc.cluster.local` hostnames. Thus, if your cluster uses another DNS domain, the dump and the restore of path 3a fail. On such a cluster, use path 3b.
- **Management path:** GitOps-managed (steps 2, 4a, 6a) or Helm-managed (4b, 6b).
- **Quarantined nodes:** record the list that the preflight reports. On path 3a, these nodes keep their quarantine state automatically. On path 3b, you must decide for each node, because the monitors do not detect one-time faults again:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,QUARANTINE:.metadata.annotations.quarantineHealthEvent --no-headers | grep -v "<none>"
```

cert-manager must be installed, because both backends use it for TLS certificates.

## Step 2: Stop reconciliation (GitOps-managed installations only)

If your installation is Helm-managed, skip this step.

If a GitOps controller such as ArgoCD or Flux deploys NVSentinel, stop reconciliation before anything else touches the cluster. Each of the next steps changes a state that the controller thinks it owns. With automated sync (and especially ArgoCD self-heal), the controller reverts the step 3a scale-downs within seconds. It also creates again everything that step 4 removes. These are general guidelines. Adapt them to the structure of your applications.

- ArgoCD: disable automated sync, or use a sync window that denies syncs for the duration. To disable automated sync, run `argocd app set nvsentinel --sync-policy none`, or edit the Application to remove `spec.syncPolicy.automated`.
- Flux: run `flux suspend helmrelease nvsentinel`.

Reconciliation stays off until step 6a, after the new values are in git. One Flux-specific rule for later: in step 4a, do NOT delete the suspended HelmRelease. Flux does not finalize suspended objects, so the deletion hangs. Flux installations are real Helm releases, so step 4a uses `helm uninstall` directly instead.

## Step 3: Capture the data

### Step 3a: Preserve health event data (default)

Stop the components that write references to health events. Then make the dump. If the datastore receives a health event after the dump, the archive does not contain it. If fault-quarantine then annotates a node for that health event, the annotation points at a document that the restore does not contain.

The `--stop-writers` flag makes the dump script do both actions. On GitOps-managed clusters, reconciliation is already off, so nothing scales the components back up. To stop the writers and make the dump, run this command:

```bash
scripts/mongodb-migration/migrate-data.sh dump /path/to/pre-migration.archive --stop-writers
```

With this flag, the script scales down fault-quarantine, node-drainer and fault-remediation, if they exist in your installation. Then the script waits until their pods are deleted. The script does not continue when Kubernetes only accepts the scale-down, because a pod in its termination grace period can still write references. (The health events that the platform connectors ingest during this window are lost. This loss is not worse than path 3b.) To stop the components yourself instead, run this command:

```bash
for D in fault-quarantine node-drainer fault-remediation; do kubectl get deploy "$D" -n nvsentinel >/dev/null 2>&1 && kubectl scale deploy "$D" -n nvsentinel --replicas=0 && kubectl wait --for=delete pod -l "app.kubernetes.io/name=$D" -n nvsentinel --timeout=120s; done
```

Then run the dump without the flag.

In both cases, the script fails closed. It refuses to run while those components run, or while it cannot determine their state. The reason is that you must not use an archive taken with active writers to preserve fault state. The script detects the current backend automatically. It always excludes the `ResumeTokens` collection, because resume tokens are valid only on the cluster that created them. The consumers write new tokens on their next start.

Confirm that the reported archive size is not zero. Then go directly to step 4.

The dump intentionally includes resolved and already remediated health events. This is safe:

- At cold start, fault-remediation queues only the health events whose remediation is incomplete. Thus, it never processes finished health events again.
- The names of maintenance CRs come from the health event ID. Thus, if fault-remediation processes a health event again, it finds the existing maintenance CR and does not create a duplicate.
- The health events collection has a TTL index on the creation timestamp. Thus, the restored history expires on its original schedule.
- The history also keeps the context of health-events-analyzer for rules that use past health events.

### Step 3b: Start clean

This path makes no dump, and it drops all health event data.

Keep a copy of the quarantined-node list from step 1 outside the cluster. Plan to review each of those nodes manually after the migration. The monitors detect the faults that are still observable (persistent hardware conditions, failing health checks) again on the next monitoring cycle. But if the monitors already read a one-time fault from the logs, such as a GPU XID error, they do not raise it again.

The step 5 cleanup removes the quarantine annotations, maintenance CRs, and jobs. It intentionally keeps the cordons and the NVSentinel-applied taints, because the decision to return a possibly faulty node to service is yours, for each node. When you are sure that a node is healthy, run `kubectl uncordon` on it. Then remove the NVSentinel-applied taints from the node.

## Step 4: Remove the installation

### Step 4a: GitOps-managed (ArgoCD, Flux)

Reconciliation is already stopped (step 2). Remove the installation:

- On ArgoCD, delete the application with cascading deletion. Run `argocd app delete nvsentinel`, or delete the Application resource with the resources finalizer.
- On Flux, do not delete the HelmRelease while it is suspended, because the deletion hangs on its finalizer. Flux installations are real Helm releases. Keep the HelmRelease suspended, and run `helm uninstall nvsentinel -n nvsentinel` directly.

Before you continue, confirm that the NVSentinel workloads are gone.

A GitOps-rendered installation is not always a Helm release, because ArgoCD renders Helm charts without creating release records. Thus, `helm uninstall` can have nothing to act on. The removal of the application is the equivalent step. The cleanup script in step 5 works the same way in both cases.

### Step 4b: Helm-managed

```bash
helm uninstall nvsentinel -n nvsentinel
```

## Step 5: Delete the datastore leftovers

The removal of the installation intentionally leaves several objects:

| Leftover | Why it survives |
| -------- | --------------- |
| `datadir-mongodb-*` PVCs | Kubernetes never deletes StatefulSet volume claims with the workload |
| `mongodb` secret | Has a `helm.sh/resource-policy: keep` annotation |
| `mongo-root-ca-secret`, `mongo-app-client-cert-secret`, `mongo-server-cert-*` secrets | Created by cert-manager, which does not remove secrets when Certificates are deleted |
| `mongo-ca-secret` secret | Created by an init job outside of Helm ownership |
| `resume-control` ConfigMap | Created at runtime by health-events-analyzer and other datastore consumers |
| `circuit-breaker` ConfigMap | Created at runtime by fault-quarantine |

Do not skip the cleanup. The old TLS secrets belong to the old certificate authority. If they survive, the new installation uses them again, and clients present certificates that the new database does not trust. Then every datastore consumer crash-loops with connection errors that never mention certificates.

On path 3b, add `--clear-fault-state` to the command below. This flag also removes the quarantine node annotations, the NVSentinel-owned maintenance CRs, and the event-labeled log-collector jobs. On path 3a, do NOT pass that flag, because the restored data makes this state valid again.

Delete all the leftover objects:

```bash
scripts/mongodb-migration/cleanup.sh --yes
```

The script verifies that nothing is left, and otherwise exits non-zero. It never touches your own secrets, for example image pull secrets.

If you prefer to delete the objects manually, do these steps:

- Run `kubectl delete pvc -l 'app.kubernetes.io/name in (mongodb, percona-server-mongodb)' -n nvsentinel`.
- Delete the secrets in the table above (one `mongo-server-cert-<n>` for each Bitnami replica, default 3).
- Run `kubectl delete configmap resume-control circuit-breaker -n nvsentinel`.

For the fault state on path 3b, delete the six `quarantineHealthEvent*`/`latestFaultRemediationState` node annotations. Also delete the maintenance CRs by their ownership label:

```bash
kubectl delete rebootnodes,terminatenodes,gpuresets,externalremediationrequests -l app.kubernetes.io/managed-by=nvsentinel --ignore-not-found=true
```

These maintenance CRs are cluster scoped, and kubectl ignores any namespace flag for them. Thus, the command selects by the ownership label that fault-remediation puts on everything that it creates. It does not delete every instance on the cluster. If `kubectl get rebootnodes` still lists `maintenance-*` CRs after the command, those CRs were created without the label. Review them. If they belong to this installation, delete them by name.

## Step 6: Deploy the Percona backend

For both paths, the values must contain:

- `mongodb-store.useBitnami: false` and `mongodb-store.usePerconaOperator: true` (both, always together)
- a volume size that is equal to or larger than the minimum block volume size of your provider. The Percona default requests 8Gi. For example, OCI block volumes are at least 50Gi. The CSI driver rounds the volume up, then the operator refuses to reconcile, and the replica set never initializes:

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

- scheduling for the Percona components (`mongodb-store.job`, `mongodb-store.psmdb-operator`, and `mongodb-store.psmdb-db.replsets.rs0`), if the cluster uses node selectors or taints. The values under `mongodb-store.mongodb.*` apply only to the Bitnami backend
- on single-node test clusters only: `mongodb-store.psmdb-db.replsets.rs0.affinity.antiAffinityTopologyKey: "none"` (the psmdb default is required anti-affinity across hostnames)
- if your installation sets `global.datastore.connection.host` explicitly: the Percona host `mongodb-rs0.<namespace>.svc.cluster.local`. If your cluster uses another DNS domain, use that domain instead of `cluster.local`. The `MONGODB_URI` that the chart generates follows the backend automatically (`mongodb-headless` for Bitnami, `mongodb-rs0` for Percona). If `global.datastore.credentialsFromSecret` supplies `MONGODB_URI`, change the host in that Secret to the Percona host. Other installations do not need this value

### Step 6a: GitOps-managed

Git must contain the new values BEFORE the controller reconciles. If the old values return first, the sync redeploys the old backend.

If the application manages the namespace with pruning enabled, the controller can prune unmanaged resources. Verify that it does not prune the objects that this runbook preserves. These objects are the pull secrets, and on path 3a, the node annotations and maintenance CRs. (The dump archive is a local file on the machine that ran the dump. No cluster operation can touch it.)

1. Commit the updated values to git.
2. Let the controller deploy. If you deleted the application in step 4a, recreate it. Otherwise, resume the reconciliation that you stopped in step 2.

### Step 6b: Helm-managed

```bash
helm upgrade --install nvsentinel <chart> -n nvsentinel -f <your-values.yaml> --timeout 20m --wait --wait-for-jobs
```

The `--wait-for-jobs` flag makes Helm also wait for the MongoDB init Job (`-l app.kubernetes.io/name=create-mongodb-database`). `--wait` alone waits only for the workloads.

For both paths, the deploy takes longer than a Bitnami deploy. The operator starts first, then it builds the replica set, and only then can the database initialization job finish. Typical times:

- The operator is typically up within a minute.
- The replica set reaches `ready` about 3 minutes after the deploy starts.
- The initialization job completes about 2 minutes after that.
- The NVSentinel services settle shortly after.
- The whole deploy typically finishes well within 10 minutes.

The redeploy creates new consumer deployments at their normal replica counts, so the step 3a scale-downs do not stay in effect.

## Step 7: Verify

```bash
scripts/mongodb-migration/verify.sh
```

There are five gates, and all of them must pass:

- The `perconaservermongodb` resource reaches `ready`.
- The MongoDB init Job (`-l app.kubernetes.io/name=create-mongodb-database`) completes.
- The mongod pods are ready.
- `MONGODB_URI` points at the Percona service.
- Every deployed datastore consumer (health-events-analyzer, fault-quarantine, node-drainer, fault-remediation) logs a successful ping.

It is normal for consumers to restart a few times while the replica set starts. On path 3a, do not go to step 8 until all five gates pass. Two of the consumers see restored health events only through their live change streams. Thus, all consumers must be healthy before the restore.

## Step 8: Restore and restart (path 3a only)

The consumers stay up during the restore on purpose. This is the opposite of the dump-time rule, and it is not a mistake. The restore is how two of the consumers receive the health events (see below). Concurrent live writes are safe, because restored documents keep their original IDs, and new health events get new IDs.

The script enforces this rule. While any deployed consumer is not ready, the script refuses to restore (exit 3). In practice, this means that step 7 has not passed yet.

If you use the event exporter, warn the owners of its downstream sink before the restore. The event exporter exports every restored health event again, including resolved health events. Thus, the sink receives duplicates.

```bash
scripts/mongodb-migration/migrate-data.sh restore /path/to/pre-migration.archive
```

This is how the consumers process the restored health events, and what the restart below does and does not do:

- MongoDB change streams carry the inserts of the restore. Thus, fault-quarantine and health-events-analyzer receive them live. These two consumers have no cold-start replay. This is why step 7 requires every consumer to be healthy before the restore.
- node-drainer and fault-remediation also query again for unfinished health events on every startup. Thus, for these two consumers, the restart below is a real second safety net.

Restart each consumer that exists in your installation:

```bash
for D in health-events-analyzer fault-quarantine node-drainer fault-remediation; do kubectl get deploy "$D" -n nvsentinel >/dev/null 2>&1 && kubectl rollout restart deploy "$D" -n nvsentinel; done
```

On GitOps-managed clusters, these restarts are rollout restarts, not spec changes, so they do not create drift.

Do these continuity checks:

- Quarantined nodes are still cordoned, and their annotations refer to documents that exist. The node-drainer logs do not repeat `unexpected number of events` lines.
- `ResumeTokens` contains only newly written tokens.

## After the migration

- **Path 3a: the event exporter re-exports everything**, including resolved health events. The new datastore has no resume token, so the backfill of the event exporter treats every restored health event as new.
- **Path 3a: restored quarantine state is latent.** Fault handling is event driven. Thus, NVSentinel evaluates a restored quarantine again only when the next health event or a component cold start touches that node. This is normal. The state is correct, but it does not generate activity on its own.
- **Path 3a: no CSP maintenance replay.** The restored maintenance events contain the progress watermark of the CSP health monitor. Thus, the monitor resumes from the point where it stopped.
- **Path 3b: CSP maintenance events replay.** The CSP health monitor uses the database itself to track its progress through the maintenance feed of the provider. After the database is wiped, the monitor ingests again every maintenance event that is still visible in the provider API. This can quarantine nodes again for maintenance that you already handled. Expect this for one polling cycle.
- **Path 3b: quarantine history is gone.** For each node in the quarantined-node list that you recorded in step 1, handle the node manually. NVSentinel detects again only the faults that are still observable. Nodes stay cordoned (and NVSentinel-applied taints stay in place) until you return each node to service yourself.

## Troubleshooting

| Symptom | Cause | Fix |
| ------- | ----- | --- |
| `mongodb-0` stuck in `Init:ImagePullBackOff`, events show `no match for platform in manifest` | Bitnami MongoDB images have no ARM64 build | Use the Percona backend on ARM64 nodes |
| Scaled-down components start again on their own during steps 3 and 4 | A GitOps controller with automated sync (or self-heal) still reconciles | Stop reconciliation (step 2). Then do the affected step again from its start |
| Operator logs show `requested storage (...) is less than actual storage (...)`, the `perconaservermongodb` resource stays in `error`, services report `ReplicaSetNoPrimary` | The minimum volume size of the cloud provider is larger than the requested size, so reconciliation stops before the replica set initializes | Set `volumeSpec` to at least the provider minimum (step 6). Delete the `mongod-data-*` PVCs. Redeploy |
| All datastore consumers crash-loop with connection errors right after the redeploy, TLS handshakes fail or connections close immediately | Leftover TLS secrets from the previous backend still exist, so client certificates belong to the old certificate authority | If the installation is GitOps-managed, stop reconciliation again (step 2). Then do steps 4, 5 and 6 again. Step 5 also deletes the volumes of the new backend. Before step 8, these volumes contain no restored data. The dump archive of path 3a is a local file, so step 5 does not touch it |
| MongoDB init Job (`-l app.kubernetes.io/name=create-mongodb-database`) `Failed` with `DeadlineExceeded` after a long stall | The job never retries after it exceeds its deadline | Run `kubectl delete job -l app.kubernetes.io/name=create-mongodb-database -n nvsentinel`. Then apply the deployment again: run the `helm upgrade` again, or sync the application again. This creates the job again, and the job completes against the healthy replica set |
| `helm upgrade` fails with `cannot patch "create-mongodb-database-..." ... field is immutable` | The backend flags were changed on a live installation, without this runbook | Run `helm rollback <release> <last-good-revision>`. If it gives an error, try one more time. Then manually delete everything that the failed upgrade created. These objects are the `perconaservermongodb` resource, the `nvsentinel-psmdb-operator` deployment and its ServiceAccount, Role and RoleBinding, and the `mongod-data-*` PVCs. They also include the `internal-mongodb-users`, `percona-server-mongodb-users` and `mongodb-encryption-key` secrets. After the rollback, Helm does not track these objects, so `helm uninstall` does not remove them either. Then follow this runbook from step 1 |
| node-drainer logs repeat `unexpected number of events for node ...` every minute | A node annotation refers to a health event that does not exist in the datastore (path 3b with fault state kept, or a partial restore) | Clear the quarantine annotations of that node as in step 5 |
| `cleanup.sh --clear-fault-state` reports resources stuck in deletion | Some maintenance CRs have janitor-managed finalizers, and janitor is already uninstalled at this point | Remove the finalizers with the `kubectl patch` command that the script prints. Then run the cleanup again |

## Rolling back

Do not roll back to Bitnami on an ARM64-only cluster. The Bitnami images do not run on ARM64 nodes.

To return to Bitnami, do the same procedure in the other direction. The restore of `migrate-data.sh` writes only into the Percona backend, so it cannot restore an archive into Bitnami. Thus, a rollback drops all health event data, as on path 3b:

1. If the installation is GitOps-managed, stop reconciliation.
2. Record the quarantined-node list with the command in step 1.
3. Remove the installation.
4. Delete the Percona leftovers. Also clear the fault state, as on path 3b in step 5.
5. Redeploy with `useBitnami: true` and `usePerconaOperator: false`.
6. Handle each node in the quarantined-node list manually, as on path 3b. See [After the migration](#after-the-migration).

Delete these Percona leftovers after the removal:

```bash
kubectl delete pvc mongod-data-mongodb-rs0-0 mongod-data-mongodb-rs0-1 mongod-data-mongodb-rs0-2 -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete secret internal-mongodb-users percona-server-mongodb-users mongodb-keyfile mongodb-encryption-key mongodb-ssl mongodb-ssl-internal mongodb-ca-cert mongo-app-client-cert-secret -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete configmap resume-control circuit-breaker -n nvsentinel --ignore-not-found=true
```

The Percona CRDs (`perconaservermongodbs.psmdb.percona.com` and related CRDs) are cluster-scoped, and they survive the removal. This causes no problem. If you install Percona again, it uses these CRDs again. The CRDs can stay in place while you run Bitnami. If you want a complete teardown and nothing else on the cluster uses the CRDs, you can delete them. Do not delete them in other cases.
