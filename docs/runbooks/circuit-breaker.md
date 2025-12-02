# Runbook: Circuit Breaker Troubleshooting

## Overview

This runbook guides you through investigating and resetting a tripped circuit breaker. The circuit breaker automatically trips when too many nodes are cordoned within a short time window, blocking new remediation actions until manually reset.

**Key points:**
- Circuit breaker does NOT auto-reset - requires manual intervention
- In-progress remediation continues, but no new actions are taken
- Only reset after investigating and fixing the root cause

## Procedure

### 1. Verify Status

```bash
kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}'
```
- `TRIPPED` = protection mode active
- `CLOSED` = normal operation

### 2. Identify Cordoned Nodes

```bash
# List cordoned nodes
kubectl get nodes | grep SchedulingDisabled

# Count affected nodes
kubectl get nodes | grep SchedulingDisabled | wc -l
```

### 3. Investigate Root Cause

Check each cordoned node:

```bash
kubectl describe node <NODE_NAME>
```

Look at **Conditions** and **Events** sections for hardware failures.

### 4. Analyze the Pattern

**Same error on all nodes?** → Configuration issue or infrastructure problem
**Specific node group affected?** → Likely a deployment issue or a rack level infrastructure problem
**Issues resolved?** → Temporary infrastructure problem

### 5. Check for Flapping Health Checks

Query kube-state-metrics to see historic node conditions:

```bash
# Check node condition transitions over the last hour
kube_node_status_condition{condition=~"Gpu.*"} [1h]

# Look for frequent status changes (1 = True, 0 = False, -1 = Unknown)
# Flapping shows rapid oscillation between states
```

If health checks are flapping, this typically indicates an infrastructure issue that is auto-resolving (network blips, storage latency, etc.). This requires deeper investigation of the underlying infrastructure.

### 6. Uncordon Affected Nodes

Manually uncordon nodes that are now healthy:

```bash
kubectl uncordon <NODE_NAME>
```

Repeat for each affected node.

### 7. Reset Circuit Breaker

**If the circuit breaker was tripped for a long time** or **if the error affected many more nodes that weren't cordoned due to the breaker**, it is recommended to clear resume tokens to avoid processing accumulated events. Follow the [Stale Events Runbook](./stale-events.md).

⚠️ **Only reset after fixing the root cause**

```bash
kubectl delete cm circuit-breaker -n nvsentinel
kubectl rollout restart deploy fault-quarantine -n nvsentinel
kubectl rollout status deploy fault-quarantine -n nvsentinel
```

### 8. Verify Normal Operation

Monitor for 15-30 minutes:

```bash
# Watch circuit breaker status
kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}'

# Monitor nodes
kubectl get nodes -w
```
