# Runbook: Node Condition Update Failures

## Overview

Node conditions reflect hardware health status (GPU, NVSwitch). Update failures prevent accurate health reporting and can impact scheduling decisions.

**Key points:**
- Conditions updated for fatal events
- Failures block health status visibility

## Symptoms

- Metric `nvsentinel_node_condition_update_total{status="failed"}` is increasing
- Node conditions don't reflect current hardware health

## Procedure

### 1. Check Platform Connector Logs

```bash
kubectl logs -n nvsentinel daemonset/platform-connectors
```

Look for error codes:
- **429** → API server throttling
- **403** → RBAC permission denied
- **404** → Node doesn't exist
- **409** → Conflict (should auto-resolve with retries)
- **Connection refused/timeout** → API server unreachable

### 2. Verify API Server is Reachable

```bash
# Check API server health
kubectl cluster-info

# Check platform-connector status
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=nvsentinel
```

### 3. Verify RBAC Permissions

```bash
kubectl auth can-i update nodes/status --as=system:serviceaccount:nvsentinel:platform-connectors
```

Should return `yes`. If `no`, check the ClusterRole:

```bash
kubectl get clusterrole platform-connectors -o yaml
```

Should include `update`, `patch` verbs for `nodes/status`.

### 4. Check Node Exists

```bash
# Verify node from logs exists
kubectl get node <NODE_NAME>
```

If node was deleted or renamed, updates will fail.

### 5. Verify Resolution

```bash
# Check node conditions reflect current health
kubectl describe node <NODE_NAME>
```
