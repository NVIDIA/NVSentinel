# Runbook: Stale Events Troubleshooting

## Overview

NVSentinel uses MongoDB change streams to watch for new health events. These change streams use resume tokens to track their position in the stream. In some cases, these tokens can become stale. Then the fault handling modules cannot resume the change stream. This runbook tells you how to make the fault handling modules skip the health events that accumulated.

**Prerequisites:**

- `kubectl` access to the cluster
- Access to the `nvsentinel` namespace

## Symptoms

The logs of the fault handling modules (`fault-quarantine`, `fault-remediation`, or `node-drainer`) can show errors such as this one:

```text
Resume of change stream was not possible, as the resume point may no longer be in the oplog
```

The module recovers from this error automatically. It deletes its stale resume token and continues from the current position in the change stream. The `change_stream_resume_token_recoveries_total` metric counts these recoveries. Use the procedure below only if the modules must also skip the accumulated health events.

The modules must also start fresh, without processing the accumulated events, in these cases:

1. **Disabled modules** - Fault handling modules were disabled for a long time while health events continued to accumulate. For this case, do step 2 before you enable the modules again. In step 2, set only the keys of the modules that you enable again. To enable each module with one replica, set `replicaCount: 1` in the values of its subchart. Then continue at step 4.
2. **Circuit breaker tripped** - The circuit breaker stayed tripped for an extended period, and many unprocessed events accumulated. A restart does not reset the breaker, and fault-quarantine processes no health events while the breaker is tripped. For this case, follow the [circuit breaker runbook](./circuit-breaker.md) instead of the procedure below. It tells you how to investigate the cause before you reset the breaker. To skip the accumulated health events, reset the breaker with the `CREATE` cursor.

## Procedure

On a normal start, each module runs a cold-start recovery. The recovery processes the unfinished health events in the datastore, even after you delete the resume token. Thus, to skip the accumulated health events, use the `CREATE` mode of the resume-control ConfigMap. In this mode, the module deletes only its own resume token, skips the cold-start recovery and records a cutoff time. Later starts run the recovery only for health events that are newer than the cutoff. For more information, see [Change Stream Resume Tokens](../configuration/README.md#change-stream-resume-tokens).

### 1. Stop Fault Handling Deployments

Before you change the resume-control ConfigMap, stop the fault handling modules. A module reads its key only when it starts. Also, a pod that still runs can write its resume token again after the reset. If a GitOps controller manages NVSentinel, stop its reconciliation first, as in [Stop reconciliation](./mongodb-bitnami-to-percona-migration.md#step-2-stop-reconciliation-gitops-managed-installations-only).

1. Record the number of replicas of the fault-quarantine, fault-remediation and node-drainer deployments.
2. Scale these deployments to zero replicas.
3. Wait until no pod of these deployments runs.

### 2. Set the Resume-Control Keys to CREATE

In the resume-control ConfigMap, set the key of each module that starts after this step to `CREATE`. These are the modules that you stopped in step 1, or the modules that you enable again. Do not set the key of a module that continues to run. A module reads its key only when it starts. Thus, the key of a running module stays `CREATE`, and its next restart skips the health events that it did not process.

This command sets the keys of all three modules. Remove the key of each module that does not start after this step:

```bash
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"fault-quarantine":"CREATE","fault-remediation":"CREATE","node-drainer":"CREATE"}}'
```

### 3. Start One Replica of Each Deployment

> **Important**: Start only one replica of each deployment. If two replicas start at the same time, both can run the reset. The second reset can delete the resume token that the first replica saved.

Scale each deployment to one replica. At startup, each module reads its `CREATE` key, deletes its resume token and sets the key back to `RESUME`.

Wait for the rollout to complete:

```bash
kubectl rollout status deployment/fault-quarantine -n nvsentinel
kubectl rollout status deployment/fault-remediation -n nvsentinel
kubectl rollout status deployment/node-drainer -n nvsentinel
```

### 4. Check the Resume-Control Keys

For each module whose key you set in step 2, run its command below. The command waits until the module sets its key back to `RESUME`:

```bash
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.fault-quarantine}'=RESUME --timeout=180s
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.fault-remediation}'=RESUME --timeout=180s
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.node-drainer}'=RESUME --timeout=180s
```

If a command times out, examine the logs of that module before you continue.

### 5. Restore the Replicas

If a deployment needs more than one replica, scale it to that number. The new replicas read `RESUME`, so they do not run the reset. If you set `replicaCount: 1` for this procedure, also set `replicaCount` to the number of replicas that the deployment needs. If you stopped the reconciliation of a GitOps controller, start it again.

### 6. Verify Recovery

Check that the modules process events without errors:

```bash
kubectl logs -n nvsentinel deployment/fault-quarantine -f
kubectl logs -n nvsentinel deployment/fault-remediation -f
kubectl logs -n nvsentinel deployment/node-drainer -f
```

The modules then process events from the current position in the change stream. They do **not** process the old events that accumulated before the reset.

If this problem occurs often, the oplog is too small for the write rate and the length of outages. A change of `mongodb-store.mongodb.persistence.size` through Helm (or of the Percona rs0 PVC) does **not** expand an already-Bound volume. Expand and verify the live PVC with the supported procedure of the backend. Then raise `mongodb-store.oplogSizeMB` (integer megabytes; keep it under about half the live volume). The init Job does not shrink a larger live oplog unless `mongodb-store.oplogAllowShrink` is true. To choose the integer (measure the wrap time, or use the 24h / 100k-node example), refer to [MongoDB Store Configuration](../configuration/mongodb-store.md#oplog-size).
