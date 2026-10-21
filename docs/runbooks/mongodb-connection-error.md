# Runbook: MongoDB Connection Error

## Overview

MongoDB is used to persist health events that are created by health monitors like syslog-health-monitor, gpu-health-monitor etc for 30 days. The fault-handling modules(like fault-quarantine, node-drainer, fault-remediation) connect to MongoDB to process these events and cordon/uncordon/remediate the node. When MongoDB pods crash or fail to start, all event processing in NVSentinel stops.

## Common Causes

MongoDB connection failure can occur due to the following reasons:

### 1. **TLS Certificates Not Installed or Expired**
   - Missing `mongodb-tls-secret` or `mongo-app-client-cert-secret` secrets
   - Expired certificates

### 2. **Initialization Job Not Completed**
   - `create-mongodb-database-<seconds>-<scriptHash>` job failed or still running (`-l app.kubernetes.io/name=create-mongodb-database`)
   - Job creates required database, collections, and indexes
   - Without successful completion, MongoDB pod can't initialize properly

### 3. **Storage Class Is Missing**
   - PersistentVolumeClaims (PVCs) use StorageClass to dynamically provision volume. Without a valid StorageClass, PVCs cannot bind to storage and MongoDB pods cannot start without bounded PVCs

### 4. **Open-File Limit Too Low**
   - The node gives containers a soft open-file limit below 64000, for example 1024
   - mongod runs out of file descriptors when the number of client connections grows with the cluster, and crashes

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
# - Open-file limit errors: "Too many open files", "Soft rlimits for open file descriptors too low"

# 4. Check PVC status
kubectl -n nvsentinel get pvc | grep mongodb
# STATUS should be "Bound" (not "Pending" or "Lost")

# 5. Check if initialization job completed
kubectl -n nvsentinel get jobs -l app.kubernetes.io/name=create-mongodb-database
kubectl -n nvsentinel get jobs -l app.kubernetes.io/name=create-mongodb-database -o yaml | grep -A 5 "status:"

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
# Check job status (name is create-mongodb-database-<seconds>-<scriptHash>)
kubectl -n nvsentinel get job -l app.kubernetes.io/name=create-mongodb-database

# Check job logs
kubectl -n nvsentinel logs -l app.kubernetes.io/name=create-mongodb-database

# Check if job completed successfully
kubectl -n nvsentinel get job -l app.kubernetes.io/name=create-mongodb-database -o jsonpath='{.items[-1:].status.succeeded}'
# Should return "1" if successful

# If job failed, delete the job and mongodb stateful set
kubectl delete -n nvsentinel statefulset.apps/mongodb
kubectl delete -n nvsentinel job -l app.kubernetes.io/name=create-mongodb-database

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

### Issue 4: Open-File Limit Too Low

mongod uses one file descriptor for each client connection. Each platform-connectors pod has its own connection pool, so the number of connections grows with the number of nodes. If the node gives containers a low soft open-file limit, mongod runs out of file descriptors and crashes. This usually happens after the cluster grows, for example from 50 to 500 nodes.

**Symptoms:**

- MongoDB pods are in `CrashLoopBackOff`. In [issue #1555](https://github.com/NVIDIA/NVSentinel/issues/1555), mongod exited with code 139.
- MongoDB does not become ready. With Bitnami, the `mongodb` StatefulSet shows fewer than 3 ready replicas. In issue #1555 it showed `0/3`, and because of the `OrderedReady` policy, the last replica did not start. With the Percona Operator, `kubectl -n nvsentinel get perconaservermongodb` does not show `ready`.
- fault-quarantine, node-drainer and fault-remediation crash with `ReplicaSetNoPrimary` or `connection refused`.

**Diagnosis:**

```bash
# Step 1: Look for open-file limit errors in the log of the crashed container
# Bitnami
kubectl -n nvsentinel logs mongodb-0 -c mongodb --previous | grep -i -E "too many open files|soft rlimits"
# Percona Operator
kubectl -n nvsentinel logs mongodb-rs0-0 -c mongod --previous | grep -i -E "too many open files|soft rlimits"
# "Soft rlimits for open file descriptors too low" shows the soft limit in "currentValue"

# Step 2: Check the exit code of the last crash
# Bitnami
kubectl -n nvsentinel get pod mongodb-0 -o jsonpath='{.status.containerStatuses[?(@.name=="mongodb")].lastState.terminated.exitCode}'
# Percona Operator
kubectl -n nvsentinel get pod mongodb-rs0-0 -o jsonpath='{.status.containerStatuses[?(@.name=="mongod")].lastState.terminated.exitCode}'

# Step 3: Find the nodes that run MongoDB
# Bitnami
kubectl -n nvsentinel get pods -l app.kubernetes.io/name=mongodb -o wide
# Percona Operator
kubectl -n nvsentinel get pods -l app.kubernetes.io/component=mongod -o wide

# Step 4: On each of those nodes, check the limit that the container runtime gives to containers
systemctl show containerd --property=LimitNOFILESoft --property=LimitNOFILE
# LimitNOFILESoft below 64000 (for example 1024) confirms this issue
```

If a MongoDB pod is running, you can also read the limit of mongod directly:

```bash
# Bitnami
kubectl -n nvsentinel exec mongodb-0 -c mongodb -- bash -c 'grep "Max open files" /proc/$(pgrep -x mongod)/limits'
# Percona Operator
kubectl -n nvsentinel exec mongodb-rs0-0 -c mongod -- bash -c 'grep "Max open files" /proc/$(pgrep -x mongod)/limits'
```

**Solution:**

```bash
# Step 1: On every node that can run MongoDB, raise the limit of the container runtime
sudo mkdir -p /etc/systemd/system/containerd.service.d
printf '[Service]\nLimitNOFILE=1048576\n' | sudo tee /etc/systemd/system/containerd.service.d/limit-nofile.conf
sudo systemctl daemon-reload
sudo systemctl restart containerd
# For nodes built with kubernetes-sigs/image-builder, also rebuild the image with
# containerd_enable_limit_no_file: true, so that new nodes get the limit

# Step 2: Delete the MongoDB pods, so that they are created again. The new limit applies only to pods that are
# created after the change. A container that restarts in an existing pod keeps the old limit.
# If all MongoDB pods are crashing, there is no primary to keep, so delete them all:
# Bitnami
kubectl -n nvsentinel delete pod -l app.kubernetes.io/name=mongodb
# Percona Operator
kubectl -n nvsentinel delete pod -l app.kubernetes.io/component=mongod

# If some MongoDB pods are still running, keep the majority of the replica set:
#   1. First delete the crashing pods, one at a time, lowest number first. With the OrderedReady policy
#      (the Bitnami default), the StatefulSet does not create a pod again while a pod with a lower number
#      is not ready. If you delete a running pod first, it does not come back, and the replica set can
#      lose its majority.
#   2. Then delete the running pods, one at a time, highest number first.
# For each pod, wait until its replacement exists and is ready before you do the next pod.
# The replacement can take a few seconds to appear, so wait for it to exist first (kubectl 1.31 or later).
# Example for one pod. Bitnami (pods mongodb-0, mongodb-1, mongodb-2):
kubectl -n nvsentinel delete pod mongodb-2
kubectl -n nvsentinel wait --for=create pod/mongodb-2 --timeout=10m
kubectl -n nvsentinel wait --for=condition=Ready pod/mongodb-2 --timeout=10m
# Percona Operator (pods mongodb-rs0-0, mongodb-rs0-1, mongodb-rs0-2):
kubectl -n nvsentinel delete pod mongodb-rs0-2
kubectl -n nvsentinel wait --for=create pod/mongodb-rs0-2 --timeout=10m
kubectl -n nvsentinel wait --for=condition=Ready pod/mongodb-rs0-2 --timeout=10m

# Step 3: Verify the limit on every replica
# Bitnami
for pod in $(kubectl -n nvsentinel get pods -l app.kubernetes.io/name=mongodb -o name); do
  echo "$pod"
  kubectl -n nvsentinel exec "$pod" -c mongodb -- bash -c 'grep "Max open files" /proc/$(pgrep -x mongod)/limits'
done
# Percona Operator
for pod in $(kubectl -n nvsentinel get pods -l app.kubernetes.io/component=mongod -o name); do
  echo "$pod"
  kubectl -n nvsentinel exec "$pod" -c mongod -- bash -c 'grep "Max open files" /proc/$(pgrep -x mongod)/limits'
done
# Each pod must print a line. The soft limit (first number) must be at least 64000.

# Step 4: Verify that MongoDB is ready
# Bitnami: READY must show 3/3
kubectl -n nvsentinel get statefulset mongodb
# Percona Operator: STATUS must show ready
kubectl -n nvsentinel get perconaservermongodb
```

If a MongoDB pod moves to a node that you did not change, it fails again. Change every node that can run MongoDB, or keep MongoDB on nodes that you changed. See [Node Placement](../configuration/mongodb-store.md#node-placement) for Bitnami and [Pod placement (Percona)](../configuration/mongodb-store.md#pod-placement-percona) for the Percona Operator.

fault-quarantine, node-drainer and fault-remediation reconnect when the replica set has a primary. If MongoDB was down for a long time, see [Stale Events](stale-events.md).

To find this problem before mongod fails, monitor the connection capacity of mongod. See [Open-file limit](../configuration/mongodb-store.md#open-file-limit-node-prerequisite).
