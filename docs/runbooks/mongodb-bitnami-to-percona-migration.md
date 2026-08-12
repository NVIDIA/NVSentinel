# MongoDB Migration: Bitnami to Percona Operator

This runbook describes how to move an existing NVSentinel installation from the Bitnami MongoDB backend (the current default) to the Percona Operator backend.

Common reasons to migrate:

- Your cluster has ARM64 nodes. The Bitnami MongoDB images are only published for amd64, so the default backend cannot run there. The Percona images are multi-arch. See [MongoDB Store Configuration](../configuration/mongodb-store.md) for details.
- You want the operator features described in [ADR-013](../designs/013-mongodb-bitnami-migration.md), such as automated lifecycle management and integrated backups.

The commands below assume the release name `nvsentinel` in the namespace `nvsentinel`. Adjust both if your installation differs.

The runbook is a sequence of steps. Where a step has options, pick the one that matches your setup:

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

- **The default path preserves your health event data.** The dump and restore carry the events into the new backend with their document IDs intact, so node annotations and remediation resources stay valid and in-flight fault handling resumes. In testing, a node quarantined before the migration stayed quarantined through it, node-drainer resolved the event behind the annotation without errors, and fault-remediation recognized its existing maintenance resource on cold start instead of creating a duplicate.
- **The clean path (3b) drops all health event data.** Monitors repopulate the new database as they detect issues, but history is lost, and one-time faults such as GPU XIDs that were already read from the logs are never raised again.
- **Never switch backends in place.** Changing `useBitnami`/`usePerconaOperator` on a live installation fails with an immutable field error on the `create-mongodb-database` Job, and by that point parts of the second backend are already deployed. You end up with two MongoDB clusters running side by side and the service configuration pointing at the broken one. This runbook's remove-and-redeploy flow is the supported path; if you already hit the mixed state, see [Troubleshooting](#troubleshooting).
- **No cloud infrastructure changes are required.** Both backends listen on the same ports (27017 for mongod, 9216 for metrics), every Service is cluster-internal, storage comes from the same StorageClass, and nothing calls cloud APIs. The migration is entirely in-cluster. The three things to check on locked-down environments: the Percona images (`percona/*` and the `lachlanevenson/k8s-kubectl` helper) must be pullable from your registry path, whoever runs the migration needs cluster-level Kubernetes RBAC (the Percona path installs cluster-scoped CRDs), and any custom network policies or exposure you built against the old service name need updating.
- Plan a maintenance window. Between the removal and the completed redeploy, NVSentinel is not monitoring the cluster.

## Helper scripts

The repository ships scripts that implement the mechanical parts of this runbook under `scripts/mongodb-migration/`:

| Script | Step | What it does |
| ------ | ---- | ------------ |
| `preflight.sh` | step 1 | Read-only readiness check: current backend, cert-manager, storage class minimums vs the requested volume size, quarantined nodes, in-flight remediation objects. Prints a verdict table; exits 2 when blocked. |
| `migrate-data.sh` | steps 3a and 8 | `dump` streams a mongodump archive out of the old backend (always excluding `ResumeTokens`) and fails closed while reference-writing components are still running; `--stop-writers` scales them down and waits for their pods to terminate first. `restore` streams the archive into the new backend and fails closed while any deployed datastore consumer is not ready. Document IDs are preserved, so node annotations and remediation resource names stay valid. |
| `cleanup.sh` | step 5 | Deletes everything the removal leaves behind, then verifies nothing remains. Refuses to run while a Helm release is still installed and asks for confirmation. `--dry-run` prints the plan; `--clear-fault-state` also clears node annotations and NVSentinel-owned remediation objects. |
| `verify.sh` | step 7 | Waits on the five post-install gates and prints a verdict table. |

All four respect `NVSENTINEL_NAMESPACE`; `preflight.sh` and `cleanup.sh` also respect `NVSENTINEL_RELEASE` (default `nvsentinel` for both). The scripts automate the steps; the decisions (choosing the data path, reviewing quarantined nodes) stay with you.

## Using the agent skills

For AI-agent-assisted runs, `skills/mongodb-migration/bitnami-to-percona/` contains three agent skills that sequence this runbook with the required confirmation gates:

1. `check-mongodb-migration-readiness` (read-only)
2. `migrate-mongodb-to-percona` (destructive, gated on explicit operator confirmation)
3. `verify-mongodb-percona-migration`

They are plain SKILL.md files and work with any agent that reads that format. Two ways to use them:

- **Point the agent at the files.** Open this repository with your agent (Claude Code, Codex, Cursor, or similar) and ask it to follow `skills/mongodb-migration/bitnami-to-percona/check-mongodb-migration-readiness/SKILL.md`. Each skill ends by naming the next one to run.
- **Install them as named skills.** Copy the three directories into your agent's skills location (for Claude Code, `.claude/skills/` in a project or `~/.claude/skills/`; for Codex, `$CODEX_HOME/skills/`), then invoke them by name.

Whichever way you run them, the skills never take a destructive step without the operator explicitly confirming the data-path decision and the quarantined-node plan in the conversation.

## Step 1: Check readiness

```bash
scripts/mongodb-migration/preflight.sh
```

Resolve any FAIL rows before continuing (the check exits 2 while blocked). Then settle three decisions the later steps depend on:

- **Data path:** preserve health event data (3a, the default) or start clean (3b). Prefer 3a whenever the cluster has active quarantines or in-flight remediations.
- **Management path:** GitOps-managed (steps 2, 4a, 6a) or Helm-managed (4b, 6b).
- **Quarantined nodes:** record the list the preflight reports. On path 3a they carry over automatically. On path 3b you must decide per node, because one-time faults will not be re-detected:

```bash
kubectl get nodes -o custom-columns=NAME:.metadata.name,QUARANTINE:.metadata.annotations.quarantineHealthEvent --no-headers | grep -v "<none>"
```

cert-manager must be installed (both backends use it for TLS certificates).

## Step 2: Stop reconciliation (GitOps-managed installations only)

If NVSentinel is deployed by a GitOps controller such as ArgoCD/FluxCD, stop reconciliation before anything else touches the cluster. Every following step mutates state the controller believes it owns: with automated sync (and especially ArgoCD self-heal) the controller reverts the step 3a scale-downs within seconds and re-creates whatever step 4 removes. These are general guidelines; adapt them to how your applications are structured.

- ArgoCD: disable automated sync (`argocd app set nvsentinel --sync-policy none`, or edit the Application to remove `spec.syncPolicy.automated`), or use a sync window that denies syncs for the duration.
- Flux: `flux suspend helmrelease nvsentinel`.

Reconciliation stays off until step 6a, after the new values are in git. One Flux-specific rule for later: do NOT delete the suspended HelmRelease in step 4a; Flux does not finalize suspended objects, so the deletion hangs. Flux installations are real Helm releases, so step 4a uses `helm uninstall` directly instead. Helm-managed installations skip this step.

## Step 3: Capture the data

### Step 3a: Preserve health event data (default)

Stop the components that write references to events, then dump. An event written after the dump is absent from the archive; if fault-quarantine then annotated a node for it, that annotation would point at a document the restore does not contain. The dump script does both with the `--stop-writers` flag (reconciliation is already off on GitOps-managed clusters, so nothing scales the components back up):

```bash
scripts/mongodb-migration/migrate-data.sh dump /path/to/pre-migration.archive --stop-writers
```

The flag scales down each of fault-quarantine, node-drainer, and fault-remediation that exists in your installation, and waits for their pods to be deleted rather than just accepting the scale-down, because a pod in its termination grace period can still write references. (Events ingested by the platform connectors during this window are lost, which is no worse than path 3b.) To stop them yourself instead, run this and then the dump without the flag:

```bash
for D in fault-quarantine node-drainer fault-remediation; do kubectl get deploy "$D" -n nvsentinel >/dev/null 2>&1 && kubectl scale deploy "$D" -n nvsentinel --replicas=0 && kubectl wait --for=delete pod -l "app.kubernetes.io/name=$D" -n nvsentinel --timeout=120s; done
```

The script fails closed either way: it refuses while those components run or while their state cannot be determined, because an archive taken with active writers must not be used to preserve fault state. It detects the current backend automatically and always excludes the `ResumeTokens` collection (resume tokens are only valid on the cluster that created them; consumers write fresh ones on their next start). Confirm the reported archive size is non-zero, then move straight to step 4.

The dump deliberately includes resolved and already remediated events, and that is safe. Fault-remediation's cold start only enqueues events whose remediation is incomplete, so finished events are never re-processed. Remediation resource names are derived from the event ID, so even a re-processed event finds its existing resource instead of creating a duplicate. The health events collection carries a TTL index on the creation timestamp, so restored history ages out on its original schedule. Keeping the history also preserves the analyzer's context for rules that consider past events.

### Step 3b: Start clean

No dump. All health event data is dropped. Make sure the quarantined-node list from step 1 is recorded somewhere outside the cluster, and plan to review each of those nodes manually after the migration: faults that are still observable (persistent hardware conditions, failing health checks) are re-detected on the next monitoring cycle, but one-time events such as GPU XID errors that were already read from the logs are not raised again.

Note that the step 5 cleanup removes the quarantine annotations, remediation resources, and jobs, but deliberately leaves cordons and NVSentinel-applied taints in place: returning a possibly faulty node to service is your decision, per node. Once you are confident a node is healthy, `kubectl uncordon` it and remove any NVSentinel-applied taints.

## Step 4: Remove the installation

### Step 4a: GitOps-managed (ArgoCD, Flux)

With reconciliation already stopped (step 2), remove the installation. On ArgoCD, delete the application with cascading deletion (`argocd app delete nvsentinel`, or delete the Application resource with the resources finalizer). On Flux, keep the HelmRelease suspended and run `helm uninstall nvsentinel -n nvsentinel` directly (Flux installations are real Helm releases; deleting the HelmRelease while suspended hangs on its finalizer). Confirm the NVSentinel workloads are gone before continuing.

Note that a GitOps-rendered installation is not necessarily a Helm release (ArgoCD renders Helm charts without creating release records), so `helm uninstall` may have nothing to act on; removing the application is the equivalent step. The cleanup script in step 5 works the same either way.

### Step 4b: Helm-managed

```bash
helm uninstall nvsentinel -n nvsentinel
```

## Step 5: Delete the datastore leftovers

Removing the installation intentionally leaves several objects behind:

| Leftover | Why it survives |
| -------- | --------------- |
| `datadir-mongodb-*` PVCs | StatefulSet volume claims are never deleted with the workload |
| `mongodb` secret | Carries a `helm.sh/resource-policy: keep` annotation |
| `mongo-root-ca-secret`, `mongo-app-client-cert-secret`, `mongo-server-cert-*` secrets | Created by cert-manager, which does not remove secrets when Certificates are deleted |
| `mongo-ca-secret` secret | Created by an init job outside of Helm ownership |
| `resume-control` ConfigMap | Created at runtime by health-events-analyzer and other datastore consumers |
| `circuit-breaker` ConfigMap | Created at runtime by fault-quarantine |

Delete all of them:

```bash
scripts/mongodb-migration/cleanup.sh --yes
```

On path 3b, add `--clear-fault-state` to also remove the quarantine node annotations, the NVSentinel-owned remediation resources, and the event-labeled log-collector jobs. On path 3a, do NOT pass that flag: the restored data makes this state valid again.

Do not skip the cleanup. The old TLS secrets belong to the old certificate authority; if they survive, the new installation reuses them, clients present certificates the new database does not trust, and every datastore consumer crash-loops with connection errors that never mention certificates. The script verifies nothing is left and exits non-zero otherwise. It never touches your own secrets (image pull secrets and the like).

If you prefer the manual commands, they are: `kubectl delete pvc -l 'app.kubernetes.io/name in (mongodb, percona-server-mongodb)' -n nvsentinel`, the secret list from the table above (one `mongo-server-cert-<n>` per Bitnami replica, default 3), and `kubectl delete configmap resume-control circuit-breaker -n nvsentinel`. For fault state on path 3b: the six `quarantineHealthEvent*`/`latestFaultRemediationState` node annotations, and the remediation resources by ownership label:

```bash
kubectl delete rebootnodes,terminatenodes,gpuresets,externalremediationrequests -l app.kubernetes.io/managed-by=nvsentinel --ignore-not-found=true
```

These remediation resources are cluster scoped (kubectl ignores any namespace flag for them), so the command selects by the ownership label fault-remediation puts on everything it creates rather than deleting every instance on the cluster. If `kubectl get rebootnodes` still lists `maintenance-*` resources afterwards, they were created without the label (for example by an older release); review them and delete by name if they belong to this installation.

## Step 6: Deploy the Percona backend

Whichever path deploys it, the values must contain:

- `mongodb-store.useBitnami: false` and `mongodb-store.usePerconaOperator: true` (both, always together)
- a volume size at or above your provider's minimum block volume size. The Percona default requests 8Gi; OCI block volumes, for example, are at least 50Gi, the CSI driver rounds the volume up, and the operator then refuses to reconcile, so the replica set never initializes:

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

- scheduling for the Percona components where the cluster uses node selectors or taints (`mongodb-store.job`, `mongodb-store.psmdb-operator`, and `mongodb-store.psmdb-db.replsets.rs0`); values under `mongodb-store.mongodb.*` apply only to the Bitnami backend
- on single-node test clusters only: `mongodb-store.psmdb-db.replsets.rs0.affinity.antiAffinityTopologyKey: "none"` (the psmdb default is required anti-affinity across hostnames)

### Step 6a: GitOps-managed

Commit the updated values to git first, then let the controller deploy: recreate the application (if you deleted it in step 4a) or re-enable the reconciliation you stopped in step 2. Git must carry the new values BEFORE the controller reconciles; if the old values come back first, the sync redeploys the old backend. If the application manages the namespace with pruning enabled, double-check that the objects this runbook preserves (the pull secrets, and on path 3a the node annotations and remediation resources) are not pruned as unmanaged resources. (The dump archive itself is a local file on the machine that ran the dump; no cluster operation can touch it.)

### Step 6b: Helm-managed

```bash
helm upgrade --install nvsentinel <chart> -n nvsentinel -f <your-values.yaml> --timeout 20m --wait --wait-for-jobs
```

The `--wait-for-jobs` flag makes Helm wait for the `create-mongodb-database` job as well; `--wait` alone only waits for the workloads.

Either way, the deploy takes longer than a Bitnami one because the operator starts first, then builds the replica set, and only then can the database initialization job finish. As a reference point from validation runs: the operator was up within a minute, the replica set reached `ready` about 3 minutes in, the initialization job completed about 2 minutes after that, and the NVSentinel services settled shortly after; the whole deploy typically finishes well within 10 minutes. The redeploy creates fresh consumer deployments at their normal replica counts, so the step 3a scale-downs do not carry over.

## Step 7: Verify

```bash
scripts/mongodb-migration/verify.sh
```

Five gates, all of which must pass: the `perconaservermongodb` resource reaches `ready`, the `create-mongodb-database` job completes, the mongod pods are ready, `MONGODB_URI` points at the Percona service, and every deployed datastore consumer (health-events-analyzer, fault-quarantine, node-drainer, fault-remediation) logs a successful ping. It is normal for consumers to restart a few times while the replica set comes up. On path 3a, do not move to step 8 until this gate passes: two of the consumers only see restored events through their live change streams, so all of them must be healthy before the restore.

One connection note: the chart-generated `MONGODB_URI` follows the backend automatically (`mongodb-headless` for Bitnami, `mongodb-rs0` for Percona). Only installations that set `global.datastore.connection.host` explicitly need to update it to `mongodb-rs0.<namespace>.svc.cluster.local`.

## Step 8: Restore and restart (path 3a only)

The consumers stay up during the restore on purpose. This is the opposite of the dump-time rule, and it is not an oversight: the restore is how two of the consumers receive the events (next paragraph), and concurrent live writes are safe because restored documents keep their original IDs while new events get fresh ones. The script enforces this: it refuses to restore (exit 3) while any deployed consumer is not ready, which in practice means step 7 has not passed yet.

```bash
scripts/mongodb-migration/migrate-data.sh restore /path/to/pre-migration.archive
```

How the restored events get processed, so you know what the restart below does and does not do: the restore's inserts flow through MongoDB change streams, so fault-quarantine and health-events-analyzer pick them up live (they have no cold-start replay; this is why step 7 requires every consumer healthy before restoring). node-drainer and fault-remediation additionally re-query for unfinished events on every startup, so for them the restart below is a genuine second net. Restart each consumer that exists in your installation:

```bash
for D in health-events-analyzer fault-quarantine node-drainer fault-remediation; do kubectl get deploy "$D" -n nvsentinel >/dev/null 2>&1 && kubectl rollout restart deploy "$D" -n nvsentinel; done
```

On GitOps-managed clusters these restarts are rollout restarts, not spec changes, so they do not create drift. Continuity checks worth running: quarantined nodes are still cordoned and their annotations reference documents that exist (no repeating `unexpected number of events` lines in the node-drainer logs), and `ResumeTokens` contains only freshly written tokens.

## After the migration

- **Path 3a: the event exporter re-exports everything**, including resolved events. With no resume token in the new datastore, its backfill treats every restored event as new; in testing this was an exact one-to-one re-export of the whole collection. Warn the owners of the downstream sink to expect duplicates.
- **Path 3a: restored quarantine state is latent.** Fault handling is event driven, so a restored quarantine is re-evaluated when the next event or a component cold start touches that node, not spontaneously. This is normal; the state is correct, it just does not generate activity on its own.
- **Path 3a: no CSP maintenance replay.** The restored maintenance events carry the CSP health monitor's progress watermark, so it resumes where it left off.
- **Path 3b: CSP maintenance events replay.** The CSP health monitor tracks its progress through the provider's maintenance feed using the database itself. With the database wiped, it re-ingests every maintenance event still visible in the provider API, which can re-quarantine nodes for maintenance you already handled. Expect this for one polling cycle.
- **Path 3b: quarantine history is gone.** Work through the quarantined-node list you recorded in step 1 and handle those nodes manually; only still-observable faults are re-detected. Nodes stay cordoned (and NVSentinel-applied taints stay in place) until you return each one to service yourself.

## Troubleshooting

| Symptom | Cause | Fix |
| ------- | ----- | --- |
| `mongodb-0` stuck in `Init:ImagePullBackOff`, events show `no match for platform in manifest` | Bitnami MongoDB images have no ARM64 build | Use the Percona backend on ARM64 nodes |
| Scaled-down components come back on their own during steps 3 and 4 | A GitOps controller with automated sync (or self-heal) is still reconciling | Stop reconciliation (step 2) and start the affected step over |
| Operator logs show `requested storage (...) is less than actual storage (...)`, the `perconaservermongodb` resource stays in `error`, services report `ReplicaSetNoPrimary` | Cloud provider minimum volume size is larger than the requested size, so reconciliation stops before the replica set is initialized | Set `volumeSpec` to at least the provider minimum (step 6), delete the `mongod-data-*` PVCs, redeploy |
| All datastore consumers crash-loop with connection errors right after the redeploy, TLS handshakes fail or connections close immediately | Leftover TLS secrets from the previous backend were not deleted, so client certificates belong to the old certificate authority | Redo step 5 completely, then redeploy |
| `create-mongodb-database` job `Failed` with `DeadlineExceeded` after a prolonged wedge | The job never retries after exceeding its deadline | `kubectl delete job create-mongodb-database -n nvsentinel`, then re-apply the deployment (re-run the `helm upgrade`, or re-sync the application); the job is recreated and completes against the healthy replica set |
| `helm upgrade` fails with `cannot patch "create-mongodb-database" ... field is immutable` | Backend flags were changed on a live installation instead of following this runbook | Run `helm rollback <release> <last-good-revision>` (retry once if it errors), then manually delete everything the failed upgrade created: the `perconaservermongodb` resource, the `nvsentinel-psmdb-operator` deployment and its ServiceAccount, Role and RoleBinding, the `mongod-data-*` PVCs, and the `internal-mongodb-users`, `percona-server-mongodb-users` and `mongodb-encryption-key` secrets. Helm no longer tracks these objects after the rollback, so `helm uninstall` will not remove them either. Then follow this runbook from step 1 |
| node-drainer logs repeat `unexpected number of events for node ...` every minute | A node annotation references an event that does not exist in the datastore (path 3b with fault state kept, or a partial restore) | Clear that node's quarantine annotations per step 5 |
| `cleanup.sh --clear-fault-state` reports resources stuck deleting | Some remediation resources carry janitor-managed finalizers, and janitor is uninstalled by this point | Remove the finalizers with the `kubectl patch` command the script prints, then re-run the cleanup |

## Rolling back

Going back to Bitnami is the same procedure in the other direction: stop reconciliation if GitOps-managed, remove the installation, delete the Percona leftovers, and redeploy with `useBitnami: true` and `usePerconaOperator: false`. The dump and restore work the same way (the dump auto-detects the Percona source and takes the same `--stop-writers` flag). The Percona leftovers to delete after removal:

```bash
kubectl delete pvc mongod-data-mongodb-rs0-0 mongod-data-mongodb-rs0-1 mongod-data-mongodb-rs0-2 -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete secret internal-mongodb-users percona-server-mongodb-users mongodb-keyfile mongodb-encryption-key mongodb-ssl mongodb-ssl-internal mongodb-ca-cert mongo-app-client-cert-secret -n nvsentinel --ignore-not-found=true
```

```bash
kubectl delete configmap resume-control circuit-breaker -n nvsentinel --ignore-not-found=true
```

The Percona CRDs (`perconaservermongodbs.psmdb.percona.com` and related) are cluster-scoped and survive removal. That is harmless: they are reused if you install Percona again, and they can stay in place while you run Bitnami. Delete them only if you want a complete teardown and nothing else on the cluster uses them.

Note that a rollback is not possible on ARM64-only clusters, because the Bitnami images do not run there.
