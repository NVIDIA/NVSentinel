# Runbook: Connecting to the Datastore

## Overview

This runbook tells you how to connect to the NVSentinel MongoDB datastore. Then you can query health events and troubleshoot data-related issues.

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

All pods must be in the `Running` state. The default deployment has 3 replicas: `mongodb-0`, `mongodb-1`, `mongodb-2`.

### 2. Connect Using the Helper Script

Use the provided script to connect:

```bash
cd scripts
./mongodb-shell.sh
```

The script automatically does these steps:

- Starts port forwarding to MongoDB
- Extracts client certificates from Kubernetes secrets
- Connects with TLS authentication
- Stops the port forwarding and deletes the extracted certificates on exit

### 3. Query Health Events

After you connect, you can query the datastore:

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
db.HealthEvents.find({"healthevent.nodename": "{NODE_NAME}"}).pretty()
```

**Find fatal events:**

```javascript
db.HealthEvents.find({"healthevent.isfatal": true}).limit(10).pretty()
```
