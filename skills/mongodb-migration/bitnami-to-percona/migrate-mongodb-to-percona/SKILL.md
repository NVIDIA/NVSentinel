---
name: migrate-mongodb-to-percona
description: >-
  Execute the NVSentinel MongoDB backend migration from Bitnami to the
  Percona Operator after readiness is confirmed: data dump (default
  preserve path), removal of the installation, cleanup of surviving
  objects, values preparation, and the Percona deployment. Destructive:
  on the opt-out clean path all health event data is wiped. Use only
  after check-mongodb-migration-readiness reports READY and the operator
  confirmed the decisions.
maturity: experimental
lifecycle: evergreen
api-version: nvsentinel.skills/v1
allowed-tools: Bash(kubectl *), Bash(helm *), Bash(argocd *), Bash(flux *), Bash(git *), Bash(scripts/mongodb-migration/*), Read, Grep
---

# Migrate MongoDB to Percona

## When to use

Use this skill **after** `check-mongodb-migration-readiness` reports READY.

Hard gate: do NOT run any step below until the operator has confirmed, in
this conversation, (a) the data-handling decision (the default preserve
path, or the clean path with data loss explicitly accepted) and (b) what
happens to each quarantined node. If either is missing, go back to the
readiness skill.

## Inputs

- `NVSENTINEL_NAMESPACE`, `NVSENTINEL_RELEASE` (default `nvsentinel`)
- `DATA_PATH` (`restore` is the default, `clean` is the opt-out): the
  confirmed data-handling decision
- `VALUES_FILES`: the operator's values file(s) for the install
- `CHART_REF`: the chart to deploy in step 5 (path or repo reference plus
  version), captured during readiness so the migration does not silently
  upgrade or downgrade NVSentinel as a side effect
- `ARCHIVE`: dump archive path (restore path only)
- GitOps status from readiness (if managed: reconciliation suspended before
  step 1, git updated before resuming; runbook steps 2, 4a and 6a)

## Setup

```bash
export NVSENTINEL_NAMESPACE="nvsentinel"
export NVSENTINEL_RELEASE="nvsentinel"
```

## Trigger order (required)

0. **GitOps only: confirm reconciliation is stopped** (runbook step 2)
   BEFORE anything below mutates the cluster; with automated sync or
   self-heal active, the controller reverts the step 1 scale-downs and
   re-creates whatever step 2 removes. On ArgoCD, automated sync is
   disabled or a deny sync window covers the migration; on Flux, the
   HelmRelease is suspended. If it is not stopped, stop it now and verify
   before continuing.

1. **Dump (default preserve path; skipped only on the clean path):**

   ```bash
   scripts/mongodb-migration/migrate-data.sh dump <ARCHIVE> --stop-writers
   ```

   `--stop-writers` scales fault-quarantine, node-drainer, and
   fault-remediation to zero and waits for their pods to be GONE, not just
   scaling down (a terminating pod can still write references). This stops
   fault handling until step 8, so it is part of the migration the operator
   confirmed, not an extra decision. Either way the script fails closed: it
   refuses while any writer pod exists, including terminating ones, or
   while their state cannot be determined. Never work around a refusal on
   the preserve path: an archive taken with active writers can contain
   dangling references and must not be used to preserve fault state. The
   script auto-detects the source backend and always excludes
   `ResumeTokens` (change-stream tokens are only valid on the cluster that
   created them). Gate: the script must report a non-empty archive.

2. **Remove the current installation.** How depends on who manages it:
   - GitOps-managed (the common case): reconciliation must already be
     stopped (runbook step 2) BEFORE the step 1 scale-downs, otherwise the
     controller reverts them. There may be no Helm release at all (ArgoCD
     renders charts without creating release records), so `helm uninstall`
     has nothing to act on; remove the rendered resources through the
     controller instead, for example by deleting the NVSentinel application
     with cascading deletion (runbook step 4a). Confirm the workloads are
     gone before continuing. The cleanup script works the same either way.
   - Helm-managed (a release exists, `helm status` succeeds):

     ```bash
     helm uninstall "$NVSENTINEL_RELEASE" -n "$NVSENTINEL_NAMESPACE"
     ```

3. **Cleanup:**

   ```bash
   scripts/mongodb-migration/cleanup.sh --yes
   ```

   Add `--clear-fault-state` ONLY on the clean path. On the restore path,
   fault state (node annotations, remediation resources) must be kept:
   restored documents keep their IDs, so those references become valid
   again. Gate: exit 0. Exit 3 means the script refused (a Helm release
   still exists: finish step 2 first, or the confirmation was declined).
   Exit 1 means an error or leftovers remain; leftover TLS secrets cause
   opaque connection-closed crash loops on the next install, and
   remediation resources stuck on finalizers are reported with the manual
   patch command. Do not continue until cleanup verifies clean.

4. **Values.** Confirm the operator's values contain:
   - `mongodb-store.useBitnami: false` and
     `mongodb-store.usePerconaOperator: true` (both, always together)
   - volume size at or above the provider minimum (OCI block volumes: 50Gi):
     `mongodb-store.psmdb-db.replsets.rs0.volumeSpec.pvc.resources.requests.storage`
   - scheduling for `mongodb-store.job`, `mongodb-store.psmdb-operator`,
     and `mongodb-store.psmdb-db.replsets.rs0` where the cluster uses node
     selectors or taints
   - on single-node test clusters only:
     `mongodb-store.psmdb-db.replsets.rs0.affinity.antiAffinityTopologyKey: "none"`
     (the psmdb default is required anti-affinity across hostnames)

5. **Deploy.** How depends on who manages it:
   - GitOps-managed (the common case): commit the updated values to git
     first, then let the controller deploy (recreate the application, or
     resume the reconciliation stopped in runbook step 2). Git must carry
     the new values BEFORE the controller reconciles, otherwise the first
     sync redeploys the old backend.
   - Helm-managed:

     ```bash
     helm upgrade --install "$NVSENTINEL_RELEASE" <CHART_REF> -n "$NVSENTINEL_NAMESPACE" \
       -f <VALUES_FILES...> --timeout 20m --wait --wait-for-jobs
     ```

     `--wait-for-jobs` matters: `--wait` alone does not wait for the
     database initialization job.

## Failure branches (validated)

| Symptom | Action |
| ------- | ------ |
| Operator log `requested storage (...) is less than actual storage (...)`, psmdb `error`, no primary | Volume below the provider minimum. Fix values, delete the `mongod-data-*` PVCs, re-run step 5. |
| `create-mongodb-database` job `Failed` with `DeadlineExceeded` | The job never retries on its own. `kubectl delete job create-mongodb-database -n "$NVSENTINEL_NAMESPACE"`, then re-run the same `helm upgrade`; Helm recreates it and it completes against the healthy replica set. |
| Consumers crash-loop with TLS/connection-closed errors right after install | Leftover TLS secrets from the old backend. Re-run step 3 fully, then step 5. |
| `helm upgrade` fails with `field is immutable` on the create-mongodb-database Job | An in-place backend switch was attempted on a live release. Follow the runbook troubleshooting row (rollback, manual deletion of everything the failed revision created), then restart from the readiness skill. |

## Next skill to run

- `verify-mongodb-percona-migration` (always; it also performs the restore
  on the restore path)

## References

| Topic | Reference |
|-------|-----------|
| Runbook (source of truth) | `docs/runbooks/mongodb-bitnami-to-percona-migration.md` |
| Scripts | `scripts/mongodb-migration/` |
| Readiness | [check-mongodb-migration-readiness](../check-mongodb-migration-readiness/SKILL.md) |
