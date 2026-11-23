# Circuit Breaker

## Overview

The circuit breaker is a safety mechanism. It prevents NVSentinel from taking too many nodes offline at the same time. An electrical circuit breaker protects your home from power surges. In the same way, this feature protects your Kubernetes cluster from widespread service disruptions.

### Why Do You Need This?

When NVSentinel detects issues with GPU nodes, it can cordon them (mark them as unschedulable). This prevents the scheduler from placing workloads on faulty hardware. However, in rare cases, NVSentinel can cordon too many nodes too quickly. Examples of these cases are:

- A misconfigured health check that incorrectly identifies healthy nodes as faulty
- A temporary infrastructure issue that affects many nodes at the same time
- A software bug in the detection system

Too many cordons reduce cluster capacity and affect your applications.

The circuit breaker is a safeguard. When the number of cordons reaches a threshold, it automatically prevents new remediation actions. After it trips, a person must reset it manually. Thus, someone investigates the root cause before normal operations resume.

## How It Works

The circuit breaker monitors the number of cordoned nodes over a sliding time window. When the number of cordoned nodes reaches a configured percentage within that time period, the circuit breaker "trips". Then it enters a protective state.

**When the circuit breaker is active (CLOSED state):**

- NVSentinel operates normally
- NVSentinel detects and cordons faulty nodes as necessary
- NVSentinel protects your cluster from hardware issues

**When the circuit breaker trips (TRIPPED state):**

- NVSentinel does not start new node remediation actions. Existing remediation operations continue until they finish
- NVSentinel continues to monitor and detect issues
- NVSentinel still updates node events and conditions for visibility
- NVSentinel cordons no more nodes until you manually reset the breaker
- **Fault quarantine does not uncordon any node either.** While the breaker is tripped, fault quarantine halts every health event, including healthy events. Thus, a node that recovers stays cordoned

> **⚠️ Important: Manual Intervention Required**
> 
> The circuit breaker does **NOT** reset itself automatically. After it trips, it stays in the TRIPPED state indefinitely, until a human operator investigates the issue and resets it manually. This behavior is by design. A trip can have a systematic cause that needs human attention. The manual reset prevents a loop in which NVSentinel cordons nodes again and again.

The breaker acts as a "pause button". It activates automatically when something seems wrong, but it needs a manual action to resume.

### Health status, event processing, and schedulability are separate

A tripped breaker splits three signals that normally move together. Read each one separately:

| Signal | Written by | Frozen by a tripped breaker? |
|---|---|---|
| Node condition, such as `GpuDcgmConnectivityFailure` | Platform connectors | No. Conditions continue to update, and a recovered node reports healthy |
| Health event processing | Fault quarantine | Yes. All health events halt until you reset the breaker and restart the deployment |
| Node schedulability, the cordon | Fault quarantine | Yes. The node stays cordoned after its condition clears |

Thus, a healthy node condition does not prove that the node is in service again. Platform connectors clears the condition. But only fault quarantine uncordons the node automatically, and a tripped breaker stops fault quarantine from acting. Check the cordon and the breaker status separately:

```bash
kubectl get node {NODE_NAME}                                        # look for SchedulingDisabled
kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}'
```

This gap is most visible on small clusters, where the default 50% threshold can trip on the first cordon. See [Small clusters](#small-clusters).

## Configuration

Configure the circuit breaker through your Helm values:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true      # Enable or disable the protection
    percentage: 50     # Percentage of GPU nodes that can be cordoned
    maxNodes: 0        # Absolute node ceiling; 0 disables this bound
    duration: "5m"     # Time window to monitor
```

When the breaker calculates the threshold, it includes only nodes labeled `nvidia.com/gpu.present=true`.

**Example:** With `percentage: 50` and `duration: "5m"`, the circuit breaker trips if NVSentinel cordons 50% or more of your GPU nodes within any 5-minute period.

`maxNodes` bounds the same window by an absolute count instead. If you set both, the lower one binds. For example, `percentage: 10` with `maxNodes: 20` trips at 20 nodes on a 300 node cluster, and at 10% on a 100 node cluster. If a percentage for one cluster size can become too permissive as the fleet grows, use this bound. At least one of `percentage`, `maxNodes`, `maxCordonedNodes` and `maxCordonedPercentage` must be positive.

**Recommended Settings:**

- For production clusters with 10+ GPU nodes: Keep the breaker enabled with a 50% threshold
- For small clusters (< 10 GPU nodes): See [Small clusters](#small-clusters) below
- For a fleet of differently sized clusters: Set both bounds, so that one configuration bounds every cluster

### Small clusters

The breaker trips when the nodes cordoned in the window reach `ceil(gpuNodes × percentage / 100)`. That formula rounds up, so the threshold falls to a single node on the smallest clusters.

A deleted node continues to count until it leaves the window. The last GPU node is the exception. If its deletion leaves no GPU nodes, the breaker stops counting the cordons of every node that left the cluster. Thus, the GPU nodes that return start with a full budget. The breaker continues to count the cordons of nodes that are still in the cluster.

The table below assumes the default `maxNodes: 0`, which leaves the percentage as the only bound:

| GPU nodes | Trips at, `percentage: 50` | Trips at, `percentage: 100` |
|---|---|---|
| 1 | 1st cordon | 1st cordon |
| 2 | 1st cordon | 2nd cordon |
| 3 | 2nd cordon | 3rd cordon |
| 4 | 2nd cordon | 4th cordon |
| 10 | 5th cordon | 10th cordon |

If `percentage` and `maxNodes` are both above `0`, the effective threshold is `min(ceil(gpuNodes × percentage / 100), maxNodes)`, capped at the fleet size. The lower bound wins. If `percentage` is `0`, the effective threshold is `maxNodes`, capped at the fleet size. Thus, `maxNodes: 1` trips on the first cordon at any percentage, and every row above becomes "1st cordon".

On a one-GPU-node cluster, **no percentage avoids the trip**. One cordoned node is always the whole fleet, and the breaker clamps the threshold to the fleet size. The first cordon trips the breaker. Then the breaker blocks the healthy event, which otherwise uncordons the node.

Disable the breaker on test, demo, and single-node clusters:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: false
```

On clusters of two to nine GPU nodes, you can keep the breaker and raise the threshold instead. Set `percentage` to `100` and `maxNodes` to `0`. If you also set `maxCordonedNodes` or `maxCordonedPercentage`, the bound must be at least two nodes. With these values, a single fault does not trip the breaker:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true
    percentage: 100    # trip only when every GPU node is cordoned
    maxNodes: 0        # 0 disables this bound
```

Keep the breaker enabled on production clusters. It is the only automatic limit on how much of your fleet NVSentinel can remove from service.

## Monitoring the Circuit Breaker

### Check Current Status through ConfigMap

Fault quarantine stores the circuit breaker status in a Kubernetes ConfigMap:

```bash
kubectl get cm circuit-breaker -n nvsentinel -o yaml
```

Example output:

```yaml
apiVersion: v1
data:
  status: CLOSED    # Can be either CLOSED or TRIPPED
kind: ConfigMap
metadata:
  name: circuit-breaker
  namespace: nvsentinel
```

**Status meanings:**

- `CLOSED`: Normal operation. The circuit breaker monitors, but does not block actions
- `TRIPPED`: Protection mode. The breaker blocks new node remediation actions. Operations that are in progress complete

### Monitor through Prometheus Metrics

NVSentinel exposes metrics for monitoring and alerting:

```text
# Current circuit breaker state: one series, labelled with the current state, at 1
fault_quarantine_breaker_state{state="TRIPPED"}

# Percentage of cluster currently cordoned (useful for dashboards)
fault_quarantine_breaker_utilization
```

`fault_quarantine_breaker_state` exports only the current state. Thus, while the breaker is `CLOSED`, the `state="TRIPPED"` series is absent, and not 0. Alert on `fault_quarantine_breaker_state{state="TRIPPED"} == 1`. In dry run, fault quarantine does not create the breaker. Thus, it exports no `fault_quarantine_breaker_state` series.

Use these metrics to:

- Create Grafana dashboards that show the circuit breaker status
- Configure alerts that notify your team when the breaker trips
- Track trends in node cordoning over time

## What To Do When It Trips

If the circuit breaker tripped, NVSentinel recently cordoned a significant percentage of your nodes. This section explains what to examine and why. For the operational procedure, with the exact commands in order, use the [circuit breaker runbook](./runbooks/circuit-breaker.md) instead.

### Step 1: Identify Affected Nodes

List all cordoned nodes in your cluster:

```bash
kubectl get nodes | grep SchedulingDisabled
```

### Step 2: Investigate the Root Cause

For each cordoned node, check what triggered the cordon:

```bash
kubectl describe node {NODE_NAME}
```

Examine these sections:

- **Node Conditions**: Shows which health checks failed (GPU errors, driver issues and other failures)
- **Taints**: Shows the specific problems that NVSentinel detected
- **Events**: Recent activities on the node

### Step 3: Determine the Pattern

Ask yourself:

- Do all nodes show the same error? (This can show a configuration issue)
- Did the problem resolve itself? (This can be a temporary infrastructure issue)
- Does the problem affect nodes from a specific node group? (This can be a deployment issue)

### Step 4: Review Health Check Data

Examine the health check history in your NVSentinel dashboards and datastore. Use it to answer these questions:

- When did the failures start?
- Which health checks fail?
- Are the failures consistent or intermittent?

### Step 5: Take Corrective Action

Based on your findings:

- If it is a legitimate hardware issue: Correct the hardware problems before you reset the breaker
- If it is a misconfiguration: Fix the configuration (health check thresholds, rules and other settings)
- If it is a false positive: Investigate why the detection was incorrect

## Resetting the Circuit Breaker

**The circuit breaker does NOT reset automatically.** After the breaker trips, NVSentinel blocks all new remediation actions. It stays in this protective state until you manually intervene. Thus, someone investigates and resolves any systematic issues before automated remediation resumes.

After you investigate and correct the root cause, reset the breaker. To reset it, patch the ConfigMap back to `CLOSED`. Then restart fault quarantine. The [circuit breaker runbook](./runbooks/circuit-breaker.md) has the full procedure, including the investigation steps that come first. Follow the runbook. Do not improvise the commands.

The reset asks you for one decision: the **cursor**. The cursor controls what occurs to the health events that accumulated while the breaker was tripped:

| Cursor | Effect | Use when |
|---|---|---|
| `CREATE` | Discards the accumulated health events and starts from the latest | After a long outage, or when a replay of the backlog can trip the breaker again |
| `RESUME` (default) | Processes every health event that accumulated while the breaker was tripped | You are confident that the backlog cannot trip the breaker again |

`CREATE` deletes the change stream resume token. After fault quarantine consumes the request, the cursor reverts to `RESUME` automatically. Thus, you never set it back manually.

Deleting the ConfigMap also resets the breaker. When fault quarantine next starts, it recreates the ConfigMap as `CLOSED`, with the cursor at `RESUME`. Thus, deleting the ConfigMap is equivalent to choosing `RESUME`. It has the same risk of a new trip, but it does not make the choice visible. Prefer the patch.

Resetting the breaker does not uncordon anything. It only lets fault quarantine act again. Uncordon recovered nodes as part of the runbook procedure.

> **⚠️ Warning**
> 
> Reset the breaker only after you:
> 1. Investigate why it tripped
> 2. Correct any underlying issues
> 3. Verify that the conditions that caused the trip are resolved
> 
> If you reset without investigation, the breaker can trip again immediately, or NVSentinel can continue to cordon nodes.

## Best Practices

1. **Enable alerting**: Configure alerts for circuit breaker trips, so that you get a notification immediately
2. **Review regularly**: Periodically check circuit breaker metrics to identify trends
3. **Test your configuration**: After changes to health checks or rules, monitor closely for false positives
4. **Document incidents**: When the breaker trips, document what caused it and how you resolved it
5. **Adjust thresholds**: Set the percentage and duration based on your cluster size and risk tolerance

## Troubleshooting

**Q: The circuit breaker keeps tripping even after I reset it**

- This suggests an ongoing issue. Do not reset the breaker again and again. First, investigate the root cause. The breaker does NOT close automatically. Thus, repeated trips after manual resets show a persistent problem.

**Q: Does the circuit breaker reset itself after some time?**

- No. The circuit breaker needs manual intervention to reset. It stays in the TRIPPED state indefinitely, until you reset it and restart the deployment. See [Resetting the Circuit Breaker](#resetting-the-circuit-breaker).

**Q: The node reports healthy, but it is still cordoned. Why?**

- A tripped breaker halts every health event, including the healthy event that otherwise uncordons the node. Platform connectors still clears the node condition. Thus, the node looks recovered, but it stays cordoned. See [Health status, event processing, and schedulability are separate](#health-status-event-processing-and-schedulability-are-separate).
- Follow the [runbook](./runbooks/circuit-breaker.md) in this order:
  1. Uncordon the recovered nodes.
  2. Reset the breaker.
- Do not reset the breaker first. A `RESUME` reset replays the health events that accumulated during the trip. If you reset first, fault quarantine can act on those nodes before you finish the manual cleanup.

**Q: My single-node test cluster tripped the breaker on the first fault**

- This is expected at any percentage, because one cordoned node is the whole fleet. Disable the breaker on single-node clusters. See [Small clusters](#small-clusters).

**Q: What happens on a cluster with no GPU nodes?**

- The breaker stays `CLOSED` and fault quarantine continues to run, because there is nothing to cordon. The breaker trips in one case: the GPU node count is zero, and a node cordoned in the window is still in the cluster. This case means that `gpuNodeLabel` does not match the nodes that fault quarantine quarantines. A cordoned node that was deleted does not trip the breaker. An example is the last GPU node, when you replace it.

**Q: Can I disable the circuit breaker?**

- Yes. Set `enabled: false` in your Helm values. Then upgrade the release. However, this removes an important safety mechanism.

**Q: My cluster is healthy but the breaker tripped. What happened?**

- Check if a transient issue occurred and resolved itself (for example, a short network outage). Review historical health check data in your monitoring system.

**Q: The circuit breaker did not trip but many nodes are cordoned**

- Check if the cordons occurred gradually, over a period longer than your configured duration. Also check if your percentage threshold is too high.

**Q: What happens to already cordoned nodes when the breaker trips?**

- Nodes that were cordoned before the breaker tripped stay cordoned. The circuit breaker only prevents *new* remediation actions. Investigate these nodes manually. If a node recovered, uncordon it.
