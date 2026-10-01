# Runbook: Node Event Creation Failures

## Overview

Node events provide visibility into non-fatal hardware problems. When creation fails, warning signs are hidden from operators.

**Key points:**
- Node events are for non-fatal health issues (warnings)
- Failures typically indicate API server issues
- An Event is named `<node>.<hash of the fault>`. A repeat of the fault refreshes the Event and raises its count. It does not create a new Event.

## Symptoms

- Metric `k8s_platform_connector_node_event_operations_total{operation="create", status="failed"}` is increasing
- With the deployment platform connector: metric `platform_connector_best_effort_failures_total{connector="kubernetes"}` is increasing. The batch is stored, but a node condition or Event write failed (`reason="failed"`) or timed out (`reason="timeout"`).
- Health events in MongoDB but not visible in `kubectl describe node`

## Procedure

### 1. Check Platform-Connector Logs

```bash
# node-local platform connector (socket mode)
kubectl logs -n nvsentinel daemonset/platform-connectors
# deployment platform connector (monitors on publishTo: deployment)
kubectl logs -n nvsentinel deployment/platform-connector-deployment
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
# deployment platform connector
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=platform-connector-deployment
```

If pods are in `CrashLoopBackOff` or `Error`, API connectivity may be broken.

### 3. Verify RBAC Permissions

```bash
kubectl auth can-i create events --as=system:serviceaccount:nvsentinel:platform-connectors -n default
# deployment platform connector
kubectl auth can-i create events --as=system:serviceaccount:nvsentinel:platform-connector-deployment -n default
```

Should return `yes`. If `no`, check the ClusterRole:

```bash
kubectl get clusterrole platform-connectors -o yaml | grep -A 3 "resources: events"
# deployment platform connector
kubectl get clusterrole platform-connector-deployment -o yaml
```

Should include `create`, `update`, `list` verbs for `events` resource.

The `platform-connector-deployment` ClusterRole needs only `create`, `get` and `update` for `events`.

### 4. Verify Resolution

```bash
# Watch for successful event creations
kubectl get events --field-selector involvedObject.kind=Node --watch

# Monitor platform-connector logs
# node-local platform connector (socket mode)
kubectl logs -n nvsentinel daemonset/platform-connectors
# deployment platform connector (monitors on publishTo: deployment)
kubectl logs -n nvsentinel deployment/platform-connector-deployment
```
