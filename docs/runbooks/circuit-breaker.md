# Runbook: Circuit Breaker Troubleshooting

## Overview

This runbook guides you through investigating and resetting a tripped circuit breaker. The circuit breaker automatically trips when too many nodes are cordoned within a short time window, blocking new remediation actions until manually reset.

This is the canonical reset procedure. The [circuit breaker overview](../circuit-breaker.md) explains the concepts and defers to the steps here.

**Key points:**
- Circuit breaker does NOT auto-reset - requires manual intervention
- In-progress remediation continues, but no new actions are taken
- Processing halts for **all** health events, healthy ones included, so recovered nodes are not uncordoned automatically while tripped
- Resetting the breaker does not uncordon anything; uncordon recovered nodes yourself in step 6
- Only reset after investigating and fixing the root cause

On a cluster with one or two GPU nodes the default 50% threshold trips on the first cordon. If that is why you are here, reset as below, then disable the breaker so it does not recur — see [Small clusters](../circuit-breaker.md#small-clusters).

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
kubectl describe node {NODE_NAME}
```

Look at **Conditions** and **Events** sections for hardware failures.

### 4. Analyze the Pattern

**Same error on all nodes?** → Configuration issue or infrastructure problem
**Specific node group affected?** → Likely a deployment issue or a rack level infrastructure problem
**Issues resolved?** → Temporary infrastructure problem

### 5. Check for Flapping Health Checks

Query kube-state-metrics to see historic node conditions:

```promql
# Count condition transitions over the last hour (flapping = frequent changes)
changes(kube_node_status_condition{condition=~"Gpu.*|SysLogs.*", status="true"}[1h]) > 0

# View current GPU/Syslog conditions across all nodes
kube_node_status_condition{condition=~"Gpu.*|SysLogs.*", status="true"} == 1
```

If health checks are flapping, this typically indicates an infrastructure issue that is auto-resolving (network blips, storage latency, etc.). This requires deeper investigation of the underlying infrastructure.

### 6. Uncordon Affected Nodes

Uncordon the nodes that are now healthy. This step is manual because a tripped breaker stops fault quarantine from processing the healthy events that would normally uncordon them, and resetting the breaker in step 7 does not retroactively uncordon anything.

Find nodes whose condition has cleared but which are still cordoned:

```bash
kubectl get nodes | grep SchedulingDisabled

# List the node's Gpu/SysLog conditions that are still True. Every one must clear
# before you uncordon. The lookup has to succeed first: an empty result from a
# failed kubectl call is indistinguishable from a healthy node.
if CONDITIONS=$(kubectl get node {NODE_NAME} \
     -o jsonpath='{range .status.conditions[?(@.status=="True")]}{.type}{"\n"}{end}'); then
  echo "$CONDITIONS" | grep -E "^(Gpu|SysLog)" || echo "no Gpu/SysLog condition is True"
else
  echo "node lookup failed - do not uncordon"
fi
```

Do not uncordon a node while any `Gpu` or `SysLog` condition is still `True`. Matching the condition name alone is not enough — a present condition can be either state, and uncordoning a node whose fault is still active puts workloads straight back onto broken hardware.

Then uncordon each one:

```bash
kubectl uncordon {NODE_NAME}
```

### 7. Reset Circuit Breaker

⚠️ **Only reset after fixing the root cause**

Choose the appropriate reset mode based on your situation:

#### Option A: Skip accumulated events (recommended after long outages)

Use `cursor: CREATE` to discard events that accumulated while the circuit breaker was tripped. This prevents immediate re-tripping from stale events.

```bash
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel
kubectl rollout status deploy fault-quarantine -n nvsentinel
```

#### Option B: Process accumulated events

Use `cursor: RESUME` (or omit cursor) to process all events that occurred while tripped. Only use this if you're confident the accumulated events won't cause immediate re-tripping.

```bash
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"RESUME"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel
kubectl rollout status deploy fault-quarantine -n nvsentinel
```

> **Note:** The cursor automatically resets to `RESUME` after startup, so you don't need to change it back manually.

### 8. Verify Normal Operation

Monitor for 15-30 minutes:

```bash
# Watch circuit breaker status
kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}'

# Monitor nodes
kubectl get nodes -w
```
