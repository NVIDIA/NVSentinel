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

> **⚠️ Important: Manual Intervention Required**
> 
> The circuit breaker will **NOT** automatically reset itself. Once tripped, it remains in the TRIPPED state indefinitely until a human operator investigates the issue and manually resets it. This is by design to prevent the system from repeatedly cordoning nodes when there may be a systematic problem that requires human attention.

Think of it as a "pause button" that activates automatically when something seems wrong, but requires manual action to resume.

## Configuration

Configure the circuit breaker through your Helm values:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true      # Enable or disable the protection
    percentage: 50     # Percentage of nodes that can be cordoned
    duration: "5m"     # Time window to monitor
```

**Example:** With `percentage: 50` and `duration: "5m"`, if 50% or more of your cluster nodes are cordoned within any 5-minute period, the circuit breaker will trip.

**Recommended Settings:**
- For production clusters with 10+ nodes: Keep enabled with 50% threshold
- For small clusters (< 10 nodes): Consider disabling or using a higher percentage

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

If the circuit breaker has tripped, it means a significant percentage of your nodes have been cordoned recently. Follow these steps to investigate and resolve:

### Step 1: Identify Affected Nodes

List all cordoned nodes in your cluster:

```bash
kubectl get nodes | grep SchedulingDisabled
```

### Step 2: Investigate the Root Cause

For each cordoned node, check what triggered the cordon:

```bash
kubectl describe node <NODE_NAME>
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

Once you've investigated and addressed the root cause, reset the circuit breaker:

```bash
# Delete the circuit breaker ConfigMap
kubectl delete cm circuit-breaker -n nvsentinel

# Restart the fault quarantine service
kubectl rollout restart deploy fault-quarantine -n nvsentinel
```

> **⚠️ Warning**
> 
> This is the **only** way to reset a tripped circuit breaker. Only perform this reset after you've:
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
- No. The circuit breaker requires manual intervention to reset. It will remain in the TRIPPED state indefinitely until you delete the ConfigMap and restart the deployment.

**Q: Can I disable the circuit breaker?**
- Yes, set `enabled: false` in your Helm values and upgrade the release. However, this removes an important safety mechanism.

**Q: My cluster is healthy but the breaker tripped. What happened?**
- Check if there was a transient issue that self-resolved (network blip, etc.). Review historical health check data in your monitoring system.

**Q: The circuit breaker didn't trip but many nodes are cordoned**
- Check if the cordoning happened gradually over a longer period than your configured duration, or if you have the percentage threshold set too high.

**Q: What happens to already cordoned nodes when the breaker trips?**
- Nodes that were cordoned before the breaker tripped remain cordoned. The circuit breaker only prevents *new* remediation actions. You'll need to manually investigate and potentially uncordon nodes as appropriate. 
