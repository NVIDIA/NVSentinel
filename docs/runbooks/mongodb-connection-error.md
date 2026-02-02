# Runbook: MongoDB Connection Error

## Overview

MongoDB is used to persist health events that are created by health monitors like syslog-health-monitor, gpu-health-monitor etc for 30 days. The fault-handling modules(like fault-quarantine, node-drainer, fault-remediation) connect to MongoDB to process these events and cordon/uncordon/remediate the node. When MongoDB pods crash or fail to start, all event processing in NVSentinel stops.

## Common Causes

MongoDB connection failure can occur due to the following reasons:

### 1. **TLS Certificates Not Installed or Expired**
   - Missing `mongodb-tls-secret` or `mongo-app-client-cert-secret` secrets
   - Expired certificates

### 2. **Initialization Job Not Completed**
   - `create-mongodb-database` job failed or still running
   - Job creates required database, collections, and indexes
   - Without successful completion, MongoDB pod can't initialize properly

### 3. **Storage Class Is Missing**
   - PersistentVolumeClaims (PVCs) use StorageClass to dynamically provision volume. Without a valid StorageClass, PVCs cannot bind to storage and MongoDB pods cannot start without bounded PVCs

## Quick Diagnosis

Run these commands to identify the issue:

```bash
# 1. Check pod status
kubectl -n nvsentinel get pods -l app.kubernetes.io/name=mongodb

# Common statuses and their meanings:
# - Pending → Scheduling issue (resources, node selector, tolerations)
# - ImagePullBackOff → Can't pull the image
# - CrashLoopBackOff → Pod starts but crashes repeatedly
# - Error → Container exited with non-zero code
# - OOMKilled → Ran out of memory
# - Init:Error → Init container failed

# 2. Check pod events for errors
kubectl -n nvsentinel describe pod -l app.kubernetes.io/name=mongodb | grep -A 20 "Events:"

# 3. Check pod logs
kubectl -n nvsentinel logs -l app.kubernetes.io/name=mongodb --tail=100
# Look for these error patterns:
# - TLS/certificate errors: "certificate", "TLS handshake", "x509"
# - Authentication errors: "Authentication failed", "SCRAM", "auth"
# - Connection errors: "connection refused", "network error"
# - Initialization errors: "waiting for initialization", "replica set"
# - Storage errors: "WiredTiger", "disk space", "I/O error"
# - Memory errors: "out of memory", "OOM"

# 4. Check PVC status
kubectl -n nvsentinel get pvc | grep mongodb
# STATUS should be "Bound" (not "Pending" or "Lost")

# 5. Check if initialization job completed
kubectl -n nvsentinel get jobs | grep mongodb
kubectl -n nvsentinel get jobs create-mongodb-database -o yaml | grep -A 5 "status:"

# 6. Check certificates exist
kubectl -n nvsentinel get secrets | grep mongo
# Expected secrets:
# mongo-app-client-cert-secret (client authentication)
# mongo-root-ca-secret (root CA)
# mongo-server-cert-0, mongo-server-cert-1, mongo-server-cert-2 (server certs for each replica)
# mongo-ca-secret (CA secret)
# mongodb (MongoDB credentials)

# 7. Check StatefulSet status
kubectl -n nvsentinel get statefulset mongodb -o wide
# Expected output:
# mongodb   3/3     12d
# READY should show "3/3" meaning all 3 replicas are ready
# If it shows "2/3" or "0/3", some pods are not ready
```

## Detailed Troubleshooting

### Issue 1: Expired Certificates/Secrets

**Diagnosis:**
```bash
# Step 1: Check if secrets exist in nvsentinel namespace
kubectl -n nvsentinel get secret | grep mongo
# Expected secrets:
# - mongo-app-client-cert-secret (for application client authentication)
# - mongo-root-ca-secret (root certificate authority)
# - mongo-server-cert-0, mongo-server-cert-1, mongo-server-cert-2 (server certs for MongoDB replicas)
# - mongo-ca-secret (CA secret)
# - mongodb (MongoDB credentials)

# Step 2: Check Certificate resources
kubectl -n nvsentinel get certificates
# Expected certificates:
# - mongo-app-client-cert
# - mongo-root-ca
# - mongo-server-cert-0, mongo-server-cert-1, mongo-server-cert-2
# All should show READY=True

# Step 3: If certificates don't exist or show READY=False, check cert-manager webhook
kubectl -n cert-manager get secret cert-manager-webhook-ca -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -dates
# Expected output:
# The current date should be BETWEEN notBefore and notAfter
# If current date is AFTER notAfter, the cert is EXPIRED

# Step 4: Check certificate expiration dates (if secrets exist)
kubectl -n nvsentinel get secret mongo-app-client-cert-secret -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -dates
kubectl -n nvsentinel get secret mongo-server-cert-0 -o jsonpath='{.data.tls\.crt}' | base64 -d | openssl x509 -noout -dates

# Step 5: Check MongoDB logs for certificate errors
kubectl -n nvsentinel logs -l app.kubernetes.io/name=mongodb --tail=50 | grep -i "certificate\|tls\|ssl"
```

**Solution:**

**Scenario A: Mongo secrets do NOT exist (cert-manager webhook is the problem)**

If Step 1 showed no mongo secrets, or Step 3 showed cert-manager-webhook-ca is expired:

```bash
# Step 1: Fix cert-manager
# Delete the expired webhook CA secret
kubectl -n cert-manager delete secret cert-manager-webhook-ca

# Step 2: Re-sync cert-manager application from ArgoCD
# This recreates cert-manager with a fresh webhook certificate

# Step 3: Delete the NVSentinel app from ArgoCD
# This cleans up all NVSentinel resources

# Step 4: Delete MongoDB PVCs to ensure fresh connection
kubectl -n nvsentinel delete pvc -l app.kubernetes.io/name=mongodb

# Step 5: Re-sync NVSentinel application from ArgoCD
# This reinstalls NVSentinel with fresh certificates
```

**Scenario B: Mongo secrets exist but are expired (cert-manager is healthy)**

If Step 1 showed mongo secrets exist, and Step 4 showed they are expired, but Step 3 showed cert-manager webhook is healthy:

```bash
# Step 1: Delete the expired mongo secrets
kubectl -n nvsentinel delete secret mongo-app-client-cert-secret mongo-root-ca-secret
kubectl -n nvsentinel delete secret mongo-server-cert-0 mongo-server-cert-1 mongo-server-cert-2

# Step 2: Cert-manager will automatically recreate them
# Verify secrets were recreated
kubectl -n nvsentinel get secret | grep mongo

# Step 3: Delete the NVSentinel app from ArgoCD
# This cleans up all NVSentinel resources

# Step 4: Delete MongoDB PVCs to ensure fresh connection
kubectl -n nvsentinel delete pvc -l app.kubernetes.io/name=mongodb

# Step 5: Re-sync NVSentinel application from ArgoCD
```

### Issue 2: Initialization Job Failed

```bash
# Check job status
kubectl -n nvsentinel get job create-mongodb-database

# Check job logs
kubectl -n nvsentinel logs job/create-mongodb-database

# Check if job completed successfully
kubectl -n nvsentinel get job create-mongodb-database -o jsonpath='{.status.succeeded}'
# Should return "1" if successful

# If job failed, delete the job and mongodb stateful set
kubectl delete  -n nvsentinel statefulset.apps/mongodb
kubectl delete  -n nvsentinel job.batch/create-mongodb-database

# Re-sync the nvsentinel application. ArgoCD will recreate the job
```

### Issue 3: Storage Class Is Missing

```bash
# Check PVC status
kubectl -n nvsentinel get pvc | grep mongodb
# Status should be "Bound", not "Pending"

# Check PVC details and look for errors in Events section
kubectl -n nvsentinel describe pvc -l app.kubernetes.io/name=mongodb

# Look for these issues in the output:
# - Status: Should be "Bound" (not "Pending" or "Lost")
# - Events section: Check for errors like:
#   - "FailedBinding" - no PV available or StorageClass issue
#   - "ProvisioningFailed" - StorageClass provisioner error
# - StorageClass field shows which storage class the PVC is trying to use

# Check what storage class the PVC is requesting
kubectl -n nvsentinel get pvc -l app.kubernetes.io/name=mongodb -o jsonpath='{.items[*].spec.storageClassName}'

# Verify that storage class exists in the cluster
kubectl get storageclass
# If required storage class doesn't exists then upgrade the cluster with correctly configured storage class
```
