# Runbook: Circuit Breaker Troubleshooting

## Overview

This runbook shows how to investigate and reset a tripped circuit breaker. The circuit breaker automatically trips when NVSentinel cordons too many nodes within a short time window. Then it blocks new remediation actions until someone resets it manually.

This is the canonical reset procedure. The [circuit breaker overview](../circuit-breaker.md) explains the concepts, and refers you to the steps here.

**Key points:**

- The circuit breaker does NOT reset automatically. It needs manual intervention
- Remediation in progress continues, but NVSentinel takes no new actions
- Processing halts for **all** health events, including healthy ones. Thus, while the breaker is tripped, fault quarantine does not uncordon recovered nodes automatically
- Resetting the breaker does not uncordon anything. Uncordon recovered nodes yourself in step 6
- Reset the breaker only after you investigate and fix the root cause

On a cluster with one or two GPU nodes, the default 50% threshold trips on the first cordon. If that is the cause, reset the breaker as described below. Then change the breaker configuration, so that a single cordon does not trip the breaker again. On a cluster with one GPU node, disable the breaker. On a cluster with two GPU nodes, set `percentage` to `100` and `maxNodes` to `0`. See [Small clusters](../circuit-breaker.md#small-clusters).

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

Examine the **Conditions** and **Events** sections for hardware failures.

### 4. Analyze the Pattern

- **Same error on all nodes?** → Configuration issue or infrastructure problem
- **Specific node group affected?** → Likely a deployment issue or a rack level infrastructure problem
- **Issues resolved?** → Temporary infrastructure problem

### 5. Check for Flapping Health Checks

Query kube-state-metrics to see historic node conditions:

```promql
# Count condition transitions over the last hour (flapping = frequent changes)
changes(kube_node_status_condition{condition=~"Gpu.*|SysLogs.*", status="true"}[1h]) > 0

# View current GPU/Syslog conditions across all nodes
kube_node_status_condition{condition=~"Gpu.*|SysLogs.*", status="true"} == 1
```

If health checks flap, this typically shows an infrastructure issue that resolves itself (short network outages, storage latency and other transient issues). Such an issue needs a deeper investigation of the underlying infrastructure.

### 6. Uncordon Affected Nodes

Uncordon the nodes that are healthy again. This step is manual for two reasons:

- A tripped breaker stops fault quarantine from processing the healthy events that normally uncordon the nodes.
- Resetting the breaker in step 7 does not retroactively uncordon anything.

Find the nodes whose condition cleared, but that are still cordoned:

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

Do not uncordon a node while any `Gpu` or `SysLog` condition is still `True`. A match on the condition name alone is not enough, because a present condition can be in either state. If you uncordon a node whose fault is still active, the scheduler immediately puts workloads onto broken hardware again.

Then uncordon each one:

```bash
kubectl uncordon {NODE_NAME}
```

### 7. Reset Circuit Breaker

⚠️ **Reset the breaker only after you fix the root cause**

Choose the appropriate reset mode based on your situation:

#### Option A: Skip accumulated events (recommended after long outages)

To discard the health events that accumulated while the circuit breaker was tripped, use `cursor: CREATE`. This prevents an immediate new trip from stale health events.

```bash
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel
kubectl rollout status deploy fault-quarantine -n nvsentinel
```

#### Option B: Process accumulated events

To process all health events that occurred while the breaker was tripped, use `cursor: RESUME` (or omit cursor). Use this option only if you are confident that the accumulated health events cannot cause an immediate new trip.

```bash
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"RESUME"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel
kubectl rollout status deploy fault-quarantine -n nvsentinel
```

> **Note:** The cursor automatically resets to `RESUME` after startup. You do not need to change it back manually.

### 8. Verify Normal Operation

Monitor for 15-30 minutes:

```bash
# Watch circuit breaker status
kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}'

# Monitor nodes
kubectl get nodes -w
```

> **Note:** If you also watch the `fault_quarantine_event_backlog_count` gauge, its value can be `-1`, which means unknown. After the step-7 restart, it is possible that nothing is left to count (a `CREATE` cursor skipped the backlog). On PostgreSQL, the watcher does not support counting at all, so the gauge stays `-1` there.
