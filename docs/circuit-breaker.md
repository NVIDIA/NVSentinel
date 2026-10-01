# Circuit Breaker

## Overview

The circuit breaker is a safety mechanism that prevents NVSentinel from taking too many nodes offline at once. Similar to an electrical circuit breaker that protects your home from power surges, this feature protects your Kubernetes cluster from widespread service disruptions.

### Why Do You Need This?

When NVSentinel detects issues with GPU nodes, it may cordon them (mark them as unavailable) to prevent workloads from being scheduled on faulty hardware. However, in rare cases such as:

- A misconfigured health check that incorrectly identifies healthy nodes as faulty
- A temporary infrastructure issue that affects many nodes simultaneously
- A software bug in the detection system

The system could potentially cordon too many nodes too quickly, which would reduce cluster capacity and impact your applications.

The circuit breaker acts as a safeguard by automatically preventing new remediation actions when a threshold is reached. Once tripped, it requires manual human intervention to reset, ensuring that someone investigates the root cause before normal operations resume.

## How It Works

The circuit breaker monitors how many nodes are cordoned over a sliding time window. When the number of cordoned nodes exceeds a configured percentage within that time period, the circuit breaker "trips" and enters a protective state.

**When the circuit breaker is active (CLOSED state):**
- NVSentinel operates normally
- Faulty nodes are detected and cordoned as needed
- Your cluster is protected from hardware issues

**When the circuit breaker trips (TRIPPED state):**
- No new node remediation actions will be performed. Any existing remediation operations will continue and will finish to completion
- NVSentinel continues monitoring and detecting issues
- Node events and conditions are still updated for visibility
- No additional nodes will be cordoned until you manually reset the breaker
- **No node is uncordoned either.** Fault quarantine halts every health event while tripped, healthy events included, so a node that recovers stays cordoned

> **⚠️ Important: Manual Intervention Required**
> 
> The circuit breaker will **NOT** automatically reset itself. Once tripped, it remains in the TRIPPED state indefinitely until a human operator investigates the issue and manually resets it. This is by design to prevent the system from repeatedly cordoning nodes when there may be a systematic problem that requires human attention.

Think of it as a "pause button" that activates automatically when something seems wrong, but requires manual action to resume.

### Health status, event processing, and schedulability are separate

A tripped breaker splits three signals that normally move together. Read each one on its own:

| Signal | Written by | Frozen by a tripped breaker? |
|---|---|---|
| Node condition, such as `GpuDcgmConnectivityFailure` | Platform connectors | No. Conditions keep updating, and a recovered node reports healthy |
| Health event processing | Fault quarantine | Yes. All events halt until you reset the breaker and restart the deployment |
| Node schedulability, the cordon | Fault quarantine | Yes. The node stays cordoned after its condition clears |

A healthy node condition is therefore not proof that the node is back in service. Platform connectors clears the condition, but only fault quarantine can uncordon the node, and a tripped breaker stops it from acting. Check the cordon and the breaker status separately:

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

Only nodes labeled `nvidia.com/gpu.present=true` are included when calculating the threshold.

**Example:** With `percentage: 50` and `duration: "5m"`, if 50% or more of your GPU nodes are cordoned within any 5-minute period, the circuit breaker will trip.

`maxNodes` bounds the same window by an absolute count instead. When both are set the lower one binds, so `percentage: 10` with `maxNodes: 20` trips at 20 nodes on a 300 node cluster and at 10% on a 100 node one. Use it when a percentage chosen for one cluster size would become too permissive as the fleet grows. At least one of the two must be positive.

**Recommended Settings:**
- For production clusters with 10+ GPU nodes: Keep enabled with 50% threshold
- For small clusters (< 10 GPU nodes): See [Small clusters](#small-clusters) below
- For a fleet of differently sized clusters: set both, so one config bounds every cluster

### Small clusters

The breaker trips when the nodes cordoned in the window reach `ceil(gpuNodes × percentage / 100)`. That formula rounds up, so the threshold falls to a single node on the smallest clusters. The table below assumes the default `maxNodes: 0`, which leaves the percentage as the only bound:

| GPU nodes | Trips at, `percentage: 50` | Trips at, `percentage: 100` |
|---|---|---|
| 1 | 1st cordon | 1st cordon |
| 2 | 1st cordon | 2nd cordon |
| 3 | 2nd cordon | 3rd cordon |
| 4 | 2nd cordon | 4th cordon |
| 10 | 5th cordon | 10th cordon |

With `maxNodes` set above `0`, the effective threshold is `min(ceil(gpuNodes × percentage / 100), maxNodes)`, capped at the fleet size. The lower bound wins, so `maxNodes: 1` trips on the first cordon at any percentage and every row above collapses to "1st cordon".

On a one-GPU-node cluster, **no percentage avoids the trip**, because one cordoned node is always the whole fleet and the threshold is clamped to the fleet size. The first cordon trips the breaker, which then blocks the healthy event that would uncordon the node.

Disable the breaker on test, demo, and single-node clusters:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: false
```

On clusters of two to nine GPU nodes you can keep the breaker and raise the threshold instead, so that a single fault does not trip it:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true
    percentage: 100    # trip only when every GPU node is cordoned
```

Keep the breaker enabled on production clusters. It is the only automatic limit on how much of your fleet NVSentinel can take out of service.

## Monitoring the Circuit Breaker

### Check Current Status via ConfigMap

The circuit breaker status is stored in a Kubernetes ConfigMap:

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
- `CLOSED`: Normal operation - the circuit breaker is monitoring but not blocking actions
- `TRIPPED`: Protection mode - new node remediation actions are blocked (any in-progress operations will complete)

### Monitor via Prometheus Metrics

NVSentinel exposes metrics for monitoring and alerting:

```
# Current circuit breaker state (1 = TRIPPED, 0 = CLOSED)
fault_quarantine_breaker_state{state="TRIPPED"}

# Percentage of cluster currently cordoned (useful for dashboards)
fault_quarantine_breaker_utilization
```

These metrics can be used to:
- Create Grafana dashboards showing circuit breaker status
- Set up alerts to notify your team when the breaker trips
- Track trends in node cordoning over time

## What To Do When It Trips

If the circuit breaker has tripped, it means a significant percentage of your nodes have been cordoned recently. This section explains what to look for and why. For the operational procedure, with the exact commands in order, work through the [circuit breaker runbook](./runbooks/circuit-breaker.md) instead.

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

Look for:
- **Node Conditions**: Shows which health checks failed (GPU errors, driver issues, etc.)
- **Taints**: Indicates the specific problems detected
- **Events**: Recent activities on the node

### Step 3: Determine the Pattern

Ask yourself:
- Are all nodes showing the same error? (Could indicate a configuration issue)
- Did the problem resolve itself? (Could be a temporary infrastructure issue)
- Are nodes from a specific node group affected? (Could be a deployment issue)

### Step 4: Review Health Check Data

Check your NVSentinel dashboards and data store for health check history to understand:
- When did the failures start?
- Which health checks are failing?
- Are the failures consistent or intermittent?

### Step 5: Take Corrective Action

Based on your findings:
- If it's a legitimate hardware issue: Address the hardware problems before resetting
- If it's a misconfiguration: Fix the configuration (health check thresholds, rules, etc.)
- If it's a false positive: Investigate why the detection was incorrect

## Resetting the Circuit Breaker

**The circuit breaker will NOT automatically reset.** Once tripped, NVSentinel will block all new remediation actions and remain in this protective state until you manually intervene. This ensures that any systematic issues are investigated and resolved before resuming automated remediation.

Once you've investigated and addressed the root cause, reset the breaker by patching the ConfigMap back to `CLOSED` and restarting fault quarantine. The [circuit breaker runbook](./runbooks/circuit-breaker.md) carries the full procedure, including the investigation steps that come first. Follow it rather than improvising the commands.

The one decision the reset asks of you is the **cursor**, which controls what happens to the health events that accumulated while the breaker was tripped:

| Cursor | Effect | Use when |
|---|---|---|
| `CREATE` | Discards the accumulated events and starts from the latest | After a long outage, or when replaying the backlog would trip the breaker again |
| `RESUME` (default) | Processes every event that accumulated while tripped | You are confident the backlog will not re-trip the breaker |

`CREATE` deletes the change stream resume token, then reverts to `RESUME` on its own once consumed, so you never set it back by hand.

Deleting the ConfigMap also resets the breaker. Fault quarantine recreates it as `CLOSED` with the cursor at `RESUME` when it next starts, so deleting is equivalent to choosing `RESUME`. It carries the same re-trip risk without making the choice visible. Prefer the patch.

Remember that resetting the breaker does not uncordon anything. It only lets fault quarantine act again. Uncordon recovered nodes as part of the runbook procedure.

> **⚠️ Warning**
> 
> Only reset after you've:
> 1. Investigated why it tripped
> 2. Addressed any underlying issues
> 3. Verified that the conditions that caused the trip have been resolved
> 
> Resetting without investigation may result in the breaker tripping again immediately or continued cordoning of nodes.

## Best Practices

1. **Enable alerting**: Set up alerts when the circuit breaker trips so you're notified immediately
2. **Review regularly**: Periodically check circuit breaker metrics to identify trends
3. **Test your configuration**: After changes to health checks or rules, monitor closely for false positives
4. **Document incidents**: When the breaker trips, document what caused it and how you resolved it
5. **Right-size thresholds**: Adjust percentage and duration based on your cluster size and risk tolerance

## Troubleshooting

**Q: The circuit breaker keeps tripping even after I reset it**
- This suggests an ongoing issue. Don't repeatedly reset - investigate the root cause first. Remember, the breaker will NOT automatically close, so repeated tripping after manual resets indicates a persistent problem.

**Q: Will the circuit breaker reset itself after some time?**
- No. The circuit breaker requires manual intervention to reset. It will remain in the TRIPPED state indefinitely until you reset it and restart the deployment. See [Resetting the Circuit Breaker](#resetting-the-circuit-breaker).

**Q: The node reports healthy, but it is still cordoned. Why?**
- A tripped breaker halts every health event, including the healthy event that would uncordon the node. Platform connectors still clears the node condition, which is why the node looks recovered while it stays cordoned. Uncordon the recovered nodes first, then reset the breaker, following the [runbook](./runbooks/circuit-breaker.md) in that order: a `RESUME` reset replays the events that accumulated while tripped, so fault quarantine can act on those nodes before you finish the manual cleanup. See [Health status, event processing, and schedulability are separate](#health-status-event-processing-and-schedulability-are-separate).

**Q: My single-node test cluster tripped the breaker on the first fault**
- Expected at any percentage: one cordoned node is the whole fleet. Disable the breaker on single-node clusters. See [Small clusters](#small-clusters).

**Q: Can I disable the circuit breaker?**
- Yes, set `enabled: false` in your Helm values and upgrade the release. However, this removes an important safety mechanism.

**Q: My cluster is healthy but the breaker tripped. What happened?**
- Check if there was a transient issue that self-resolved (network blip, etc.). Review historical health check data in your monitoring system.

**Q: The circuit breaker didn't trip but many nodes are cordoned**
- Check if the cordoning happened gradually over a longer period than your configured duration, or if you have the percentage threshold set too high.

**Q: What happens to already cordoned nodes when the breaker trips?**
- Nodes that were cordoned before the breaker tripped remain cordoned. The circuit breaker only prevents *new* remediation actions. You'll need to manually investigate and potentially uncordon nodes as appropriate. 
