# Runbook: Circuit Breaker Troubleshooting

## Overview

This runbook guides you through investigating and resetting a tripped circuit breaker. The circuit breaker automatically trips when too many nodes are cordoned within a short time window, blocking new remediation actions until manually reset.

**Key points:**
- Circuit breaker does NOT auto-reset - requires manual intervention
- In-progress remediation continues, but no new actions are taken
- Only reset after investigating and fixing the root cause

## Common Scenarios

### Cluster Scale Operations

Circuit breakers often trip during **legitimate cluster operations** such as:

- **Initial cluster bringup**: Creating a CPU-only cluster, then adding GPU node pools
- **Autoscaling events**: Karpenter/Cluster Autoscaler rapidly provisioning nodes
- **Capacity expansion**: Adding new node pools after receiving GPU quota
- **Multi-zone expansion**: Adding nodes across multiple availability zones

**Why**: New GPU nodes appear "unhealthy" during initialization (GPU driver, GPU Operator, DCGM not ready). On small clusters, even adding 1 node can exceed the threshold.

**Prevention**: Label nodes before scale operations:

```bash
# Before adding nodes
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false

# After nodes are healthy
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-
```

See [Cluster Scale Operations](cluster-scale-operations.md) for detailed procedures.

### GPU Driver Upgrades

During driver/GPU Operator upgrades, DCGM becomes temporarily unavailable. Parallel upgrades can trip the breaker.

See [Driver Upgrades](driver-upgrades.md) for procedures.

### Node Maintenance

OS maintenance (OSMO), node reboots, or infrastructure work affecting multiple nodes simultaneously.

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

```promql
# Count condition transitions over the last hour (flapping = frequent changes)
changes(kube_node_status_condition{condition=~"Gpu.*|SysLogs.*", status="true"}[1h]) > 0

# View current GPU/Syslog conditions across all nodes
kube_node_status_condition{condition=~"Gpu.*|SysLogs.*", status="true"} == 1
```

If health checks are flapping, this typically indicates an infrastructure issue that is auto-resolving (network blips, storage latency, etc.). This requires deeper investigation of the underlying infrastructure.

### 6. Uncordon Affected Nodes

Manually uncordon nodes that are now healthy:

```bash
kubectl uncordon <NODE_NAME>
```

Repeat for each affected node.

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

## Related Documentation

- [Cluster Scale Operations](cluster-scale-operations.md) - Preventing circuit breaker trips during node scale-up
- [Driver Upgrades](driver-upgrades.md) - Managing circuit breaker during GPU driver upgrades  
- [ADR-022: Circuit Breaker Reset Mechanism](../designs/022-circuit-breaker-reset-mechanism.md) - Design details on cursor modes
- [ADR-027: Automated Node Labeling](../designs/027-automated-node-labeling.md) - Future automation for scale operations

