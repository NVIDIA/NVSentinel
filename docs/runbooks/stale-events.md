# Runbook: Stale Events Troubleshooting

## Overview

NVSentinel uses MongoDB change streams to watch for new health events. These change streams use resume tokens to track their position in the stream. In some cases, these tokens can become stale. Then the fault handling modules cannot resume the change stream. This runbook tells you how to clear stale resume tokens.

**Prerequisites:**

- `kubectl` access to the cluster
- `mongosh` (MongoDB Shell) installed locally
- Access to the `nvsentinel` namespace

## Symptoms

The logs of the fault handling modules (`fault-quarantine`, `fault-remediation`, or `node-drainer`) can show errors such as this one:

```text
Resume of change stream was not possible, as the resume point may no longer be in the oplog
```

In other cases, the modules must start fresh, without processing the accumulated events. These cases include:

1. **Disabled modules** - Fault handling modules were disabled for a long time while health events continued to accumulate
2. **Circuit breaker tripped** - The circuit breaker stayed tripped for an extended period, and many unprocessed events accumulated

## Procedure

### 1. Connect to MongoDB

Follow the [Datastore Connection Runbook](./datastore-connection.md) to connect to MongoDB:

```bash
cd scripts
./mongodb-shell.sh
```

### 2. Clear All Resume Tokens

Before you clear the tokens, stop the fault handling modules. A running module writes its token again when it acknowledges an event. After the restart, the module then resumes from that token, not from the current position in the change stream.

1. Record the number of replicas of the fault-quarantine, fault-remediation and node-drainer deployments.
2. Scale these deployments to zero replicas.
3. Wait until no pod of these deployments runs.

Then clear all resume tokens:

```javascript
db.ResumeTokens.deleteMany({})
```

### 3. Restart Fault Handling Deployments

Scale each deployment back to the number of replicas that you recorded before the scale-down. The modules then process events from the current position in the change stream. Then restart the deployments:

```bash
kubectl rollout restart deployment/fault-quarantine -n nvsentinel
kubectl rollout restart deployment/fault-remediation -n nvsentinel
kubectl rollout restart deployment/node-drainer -n nvsentinel
```

Wait for the rollout to complete:

```bash
kubectl rollout status deployment/fault-quarantine -n nvsentinel
kubectl rollout status deployment/fault-remediation -n nvsentinel
kubectl rollout status deployment/node-drainer -n nvsentinel
```

### 4. Verify Recovery

Check that the modules process events without errors:

```bash
kubectl logs -n nvsentinel deployment/fault-quarantine -f
kubectl logs -n nvsentinel deployment/fault-remediation -f
kubectl logs -n nvsentinel deployment/node-drainer -f
```

The modules then process events from the current position in the change stream. They do **not** process the old events that accumulated while the resume tokens were stale.

If this problem occurs often, the oplog is too small for the write rate and the length of outages. A change of `mongodb-store.mongodb.persistence.size` through Helm (or of the Percona rs0 PVC) does **not** expand an already-Bound volume. Expand and verify the live PVC with the supported procedure of the backend. Then raise `mongodb-store.oplogSizeMB` (integer megabytes; keep it under about half the live volume). The init Job does not shrink a larger live oplog unless `mongodb-store.oplogAllowShrink` is true. To choose the integer (measure the wrap time, or use the 24h / 100k-node example), refer to [MongoDB Store Configuration](../configuration/mongodb-store.md#oplog-size).
