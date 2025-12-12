# Runbook: Health Monitor UDS Communication Failures

## Overview

Health monitors (GPU, NVSwitch, syslog, CSP) publish events via gRPC over Unix Domain Socket (UDS) to platform-connector. Communication failures block all health event reporting.

**Key points:**
- Platform-connector runs as a DaemonSet (one pod per node)
- Each node has its own UDS socket (`/var/run/nvsentinel.sock`)
- Both platform-connector and health monitors must mount `/var/run/nvsentinel` from hostPath
- Platform-connector requires MongoDB connection during startup

## Symptoms

- Metric `health_events_insertion_to_uds_error` or `trigger_uds_send_errors_total` increasing
- Health monitor logs show gRPC errors (code 14: Unavailable)
- No health events in MongoDB despite monitors running

## Procedure

### 1. Identify Affected Node

```bash
# Find which node has the failing health monitor
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=gpu-health-monitor -o wide

# Check health monitor logs for UDS errors
kubectl logs -n nvsentinel <HEALTH_MONITOR_POD>
```

Look for:
- `"code = Unavailable"` → Socket closed or platform-connector not running
- `"connection refused"` → Socket doesn't exist
- `"broken pipe"` → Socket was closed mid-communication

### 2. Check Platform Connector on That Node

```bash
# Find platform-connector pod on the same node
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=nvsentinel -o wide | grep <NODE_NAME>

# Check platform-connector logs
kubectl logs -n nvsentinel <PLATFORM_CONNECTOR_POD>
```

Look for errors:
- `"failed to initialize database store connector"` → MongoDB connection failed
- `"failed to create database client"` → MongoDB authentication or network issue
- `"failed to listen on unix socket"` → Volume mount issue

### 3. Verify MongoDB Connectivity

Platform-connector requires MongoDB connection during startup. If MongoDB is unavailable, platform-connector will fail to start.

```bash
# Check MongoDB pods are running
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=mongodb
# All pods should be Running and Ready

# Check certificates (mongo-root-ca, mongo-app-client-cert, mongo-server-cert-*)
kubectl get certificates -n nvsentinel
# All should show READY = True

# If certificates not ready, check cert-manager
kubectl get pods -n cert-manager

# Check MongoDB database creation job
kubectl get job -n nvsentinel create-mongodb-database
# Should show COMPLETIONS: 1/1
```

If the MongoDB job needs to be rerun:

```bash
# Save and recreate the job
kubectl get job create-mongodb-database -n nvsentinel -o yaml > create-mongodb-database.yaml
kubectl delete job -n nvsentinel create-mongodb-database
kubectl apply -f create-mongodb-database.yaml
```

Platform-connector connects to MongoDB on port 27017 with TLS. Check network policies:

```bash
kubectl get networkpolicies -n nvsentinel -o yaml
```

### 4. Verify Volume Mounts

```bash
# Check platform-connector mount
kubectl get daemonset platform-connectors -n nvsentinel -o yaml | grep -A 3 "/var/run/nvsentinel"

# Check health monitor mount
kubectl get daemonset gpu-health-monitor -n nvsentinel -o yaml | grep -A 3 "/var/run/nvsentinel"
```

Both should be mounted from hostPath at `/var/run/nvsentinel`.

### 6. Verify Resolution

```bash
# Watch health monitor logs for successful sends
kubectl logs -n nvsentinel <GPU_MONITOR_POD> -f
```
