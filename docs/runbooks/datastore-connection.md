# Runbook: Connecting to the Datastore

## Overview

This runbook guides you through connecting to the NVSentinel MongoDB datastore to query health events and troubleshoot data-related issues.

**Prerequisites:**
- `kubectl` access to the cluster
- `mongosh` (MongoDB Shell) installed locally
- Access to the `nvsentinel` namespace

## Procedure

### 1. Verify MongoDB is Running

Check the MongoDB pods:

```bash
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=mongodb
```

All pods should be in `Running` state. The default deployment has 3 replicas: `mongodb-0`, `mongodb-1`, `mongodb-2`.

### 2. Connect Using the Helper Script

Use the provided script to connect:

```bash
cd scripts
./mongodb-shell.sh
```

The script automatically:
- Sets up port forwarding to MongoDB
- Extracts client certificates from Kubernetes secrets
- Connects with TLS authentication
- Cleans up on exit

### 3. Query Health Events

Once connected, you can query the datastore:

**Count total health events:**
```javascript
db.HealthEvents.countDocuments()
```

**Find unhealthy events:**
```javascript
db.HealthEvents.find({"healthevent.ishealthy": false}).limit(10).pretty()
```

**Find events for a specific node:**
```javascript
db.HealthEvents.find({"healthevent.nodename": "<NODE_NAME>"}).pretty()
```

**Find fatal events:**
```javascript
db.HealthEvents.find({"healthevent.isfatal": true}).limit(10).pretty()
```
