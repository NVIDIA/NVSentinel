# Runbook: Node Event Creation Failures

## Overview

Node events show non-fatal hardware problems. If the creation of a node event fails, operators do not see the warning signs.

**Key points:**

- Node events are for non-fatal health problems (warnings)
- Failures usually indicate API server problems
- platform-connectors names each Event `<node>.<hash of the fault>`. A repeat of the fault refreshes the Event and raises its count. It does not create a new Event.

## Symptoms

- Metric `k8s_platform_connector_node_event_operations_total{operation="create", status="failed"}` increases
- With the deployment platform connector: metric `platform_connector_best_effort_failures_total{connector="kubernetes"}` increases. The deployment platform connector stored the batch, but a node condition or Event write failed (`reason="failed"`) or timed out (`reason="timeout"`).
- Health events are in MongoDB, but do not show in `kubectl describe node`

## Procedure

### 1. Check Platform-Connector Logs

```bash
# node-local platform connector (socket mode)
kubectl logs -n nvsentinel daemonset/platform-connectors
# deployment platform connector (monitors on publishTo: deployment)
kubectl logs -n nvsentinel deployment/platform-connector-deployment
```

Find these error codes in the logs:

- **429** → API server throttling
- **403** → RBAC permission denied
- **Connection refused/timeout** → API server unreachable
- **409** → Conflict (normally, the retries resolve it automatically)

### 2. Verify API Server is Reachable

```bash
# Check if API server is accessible
kubectl cluster-info

# Check platform-connector pod status
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=nvsentinel
# deployment platform connector
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=platform-connector-deployment
```

If the pods are in `CrashLoopBackOff` or `Error`, the API connectivity can be broken.

### 3. Verify RBAC Permissions

```bash
kubectl auth can-i create events --as=system:serviceaccount:nvsentinel:platform-connectors -n default
# deployment platform connector
kubectl auth can-i create events --as=system:serviceaccount:nvsentinel:platform-connector-deployment -n default
```

The command for your platform connector role must return `yes`. If it returns `no`, check the ClusterRole:

```bash
kubectl get clusterrole platform-connectors -o yaml | grep -A 3 "resources: events"
# deployment platform connector
kubectl get clusterrole platform-connector-deployment -o yaml
```

For the node-local platform connector, the ClusterRole must include the `create`, `get` and `update` verbs for the `events` resource. The chart also grants `list`.

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
