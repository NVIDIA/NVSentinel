# Runbook: Stale Events Troubleshooting

## Overview

NVSentinel uses MongoDB change streams to watch for new health events. These change streams use resume tokens to track their position in the event stream. In some cases, these tokens can become stale, causing the fault handling modules to fail resuming the change stream. This runbook guides you through clearing stale resume tokens.

**Prerequisites:**
- `kubectl` access to the cluster
- `mongosh` (MongoDB Shell) installed locally
- Access to the `nvsentinel` namespace

## Symptoms

You may see errors in the fault handling module logs (`fault-quarantine`, `fault-remediation`, or `node-drainer`) such as:

```
Resume of change stream was not possible, as the resume point may no longer be in the oplog
```

In other cases, modules may have been disabled for a long time and need to start fresh without processing accumulated events:
1. **Disabled modules** - Fault handling modules were disabled for a long time while health events continued accumulating
2. **Circuit breaker tripped** - The circuit breaker was tripped for an extended period, accumulating many unprocessed events

## Procedure

### 1. Connect to MongoDB

Follow the [Datastore Connection Runbook](./datastore-connection.md) to connect to MongoDB:

```bash
cd scripts
./mongodb-shell.sh
```

### 2. Clear All Resume Tokens

Once connected, clear all resume tokens:

```javascript
db.ResumeTokens.deleteMany({})
```

### 3. Restart Fault Handling Deployments

Restart the deployments to start processing events from the current point in the change stream:

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

Check that the modules are processing events without errors:

```bash
kubectl logs -n nvsentinel deployment/fault-quarantine -f
kubectl logs -n nvsentinel deployment/fault-remediation -f
kubectl logs -n nvsentinel deployment/node-drainer -f
```

The modules will now start processing events from the current point in the change stream. They will **not** retroactively process old events that accumulated while the resume tokens were stale.
