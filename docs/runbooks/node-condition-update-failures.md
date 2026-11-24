# Runbook: Node Condition Update Failures

## Overview

Node conditions show the hardware health status (GPU, NVSwitch). Update failures prevent accurate health reporting and can affect scheduling decisions.

**Key points:**

- platform-connectors updates node conditions for fatal health events
- Update failures hide the health status

## Symptoms

- Metric `k8s_platform_connector_node_condition_update_total{status="failed"}` increases
- With the deployment platform connector: metric `platform_connector_best_effort_failures_total{connector="kubernetes"}` increases. The deployment platform connector stored the batch, but a node condition or Event write failed (`reason="failed"`) or timed out (`reason="timeout"`).
- Node conditions do not show the current hardware health

## Procedure

### 1. Check Platform Connector Logs

```bash
# node-local platform connector (socket mode)
kubectl logs -n nvsentinel daemonset/platform-connectors
# deployment platform connector (monitors on publishTo: deployment)
kubectl logs -n nvsentinel deployment/platform-connector-deployment
```

Find these error codes in the logs:

- **429** → API server throttling
- **403** → RBAC permission denied
- **404** → The node does not exist
- **409** → Conflict (normally, the retries resolve it automatically)
- **Connection refused/timeout** → API server unreachable

### 2. Verify API Server is Reachable

```bash
# Check API server health
kubectl cluster-info

# Check platform-connector status
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=nvsentinel
# deployment platform connector
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=platform-connector-deployment
```

### 3. Verify RBAC Permissions

```bash
kubectl auth can-i update nodes/status --as=system:serviceaccount:nvsentinel:platform-connectors
# deployment platform connector
kubectl auth can-i update nodes/status --as=system:serviceaccount:nvsentinel:platform-connector-deployment
```

Run the commands again with `patch` in place of `update`, because platform-connectors writes node conditions with a patch.

The commands for your platform connector role must return `yes`. If one returns `no`, check the ClusterRole:

```bash
kubectl get clusterrole platform-connectors -o yaml
# deployment platform connector
kubectl get clusterrole platform-connector-deployment -o yaml
```

For the node-local platform connector, the ClusterRole must include the `update` and `patch` verbs for `nodes/status`.

The `platform-connector-deployment` ClusterRole also needs the `update` and `patch` verbs for `nodes/status`.

### 4. Check Node Exists

```bash
# Verify node from logs exists
kubectl get node {NODE_NAME}
```

If the node was deleted or renamed, the updates fail.

### 5. Verify Resolution

```bash
# Check node conditions reflect current health
kubectl describe node {NODE_NAME}
```
