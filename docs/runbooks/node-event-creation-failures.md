# Runbook: Node Event Creation Failures

## Overview

Node events provide visibility into non-fatal hardware problems. When creation fails, warning signs are hidden from operators.

**Key points:**
- Node events are for non-fatal health issues (warnings)
- Failures typically indicate API server issues

## Symptoms

- Metric `nvsentinel_node_event_operations_total{operation="create", status="failed"}` is increasing
- Health events in MongoDB but not visible in `kubectl describe node`

## Procedure

### 1. Check Platform-Connector Logs

```bash
kubectl logs -n nvsentinel daemonset/platform-connectors
```

Look for error codes:
- **429** → API server throttling
- **403** → RBAC permission denied
- **Connection refused/timeout** → API server unreachable
- **409** → Conflict (should auto-resolve with retries)

### 2. Verify API Server is Reachable

```bash
# Check if API server is accessible
kubectl cluster-info

# Check platform-connector pod status
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=nvsentinel
```

If pods are in `CrashLoopBackOff` or `Error`, API connectivity may be broken.

### 3. Verify RBAC Permissions

```bash
kubectl auth can-i create events --as=system:serviceaccount:nvsentinel:platform-connectors -n default
```

Should return `yes`. If `no`, check the ClusterRole:

```bash
kubectl get clusterrole platform-connectors -o yaml | grep -A 3 "resources: events"
```

Should include `create`, `update`, `list` verbs for `events` resource.

### 4. Verify Resolution

```bash
# Watch for successful event creations
kubectl get events --field-selector involvedObject.kind=Node --watch

# Monitor platform-connector logs
kubectl logs -n nvsentinel daemonset/platform-connectors
```
