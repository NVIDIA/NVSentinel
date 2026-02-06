# Runbook: Cluster Bringup and Node Scale Operations

## Overview

This runbook provides procedures for safely scaling GPU nodes (both during initial cluster bringup and autoscaling operations) while preventing NVSentinel's circuit breaker from tripping due to nodes being in a transitional state.

## Background

### The Problem

When GPU nodes are added to a cluster (whether during initial bringup or scale-up operations), they go through several initialization phases:
1. Node joins the cluster
2. GPU drivers are loaded
3. GPU Operator components are deployed
4. DCGM and other monitoring tools become available
5. Node becomes fully healthy and ready

During phases 1-4, NVSentinel's health checks may fail because:
- DCGM is not yet available
- GPU drivers are still initializing
- GPU Operator pods are still being deployed

### Why the Circuit Breaker Trips

NVSentinel's circuit breaker monitors the **percentage** of nodes that become unhealthy within a time window (default: 50% within 5 minutes). When you scale up nodes:

- **Small clusters** (1-2 nodes): Adding even 1 node represents 50%+ of the cluster
- **Rapid scale-up**: Adding many nodes simultaneously can exceed the threshold
- **Initial bringup**: Starting with CPU-only cluster, then adding GPU nodes creates a large percentage change

When the threshold is exceeded, NVSentinel's circuit breaker trips to protect against what it perceives as a widespread cluster issue.

### Circuit Breaker Behavior

Once tripped:
- No new node remediation actions will be performed
- GPU nodes may remain cordoned even after they become healthy
- Requires manual intervention to reset (see [circuit-breaker.md](circuit-breaker.md))

## Common Scenarios

### Scenario 1: Initial Cluster Bringup (CPU → GPU)

**Use Case:** Creating a CPU-only cluster first, installing runtime components, then adding GPU node pools.

**Why This Happens:** This workflow allows teams to set up cluster infrastructure before GPU capacity arrives, but NVSentinel doesn't distinguish between "new nodes being added" and "existing nodes failing."

**Risk Level:** ⚠️ **HIGH** - Almost guaranteed to trip the circuit breaker on small clusters

### Scenario 2: Autoscaling with Karpenter/Cluster Autoscaler

**Use Case:** Automatic horizontal scaling in response to workload demands.

**Why This Happens:** Large batch jobs or inference workloads can trigger rapid scale-up of many GPU nodes simultaneously.

**Risk Level:** ⚠️ **MEDIUM** - Depends on scale-up speed and cluster size

### Scenario 3: Manual Node Pool Addition

**Use Case:** Adding capacity after receiving additional GPU quota.

**Why This Happens:** Teams manually add node pools or resize existing ones.

**Risk Level:** ⚠️ **MEDIUM** - Depends on the percentage of nodes being added

### Scenario 4: Multi-Zone Expansion

**Use Case:** Adding GPU nodes across multiple availability zones simultaneously.

**Why This Happens:** High-availability requirements or capacity distribution.

**Risk Level:** ⚠️ **HIGH** - Multiple nodes coming online simultaneously

## Recommended Procedures

### Option 1: Disable NVSentinel Management During Scale Operations (Recommended)

This is the **safest and most reliable** approach for planned scale operations.

#### Step 1: Label Nodes to Exclude from NVSentinel Management

**Before adding new nodes**, apply the exclusion label to prevent NVSentinel from managing them during initialization:

```bash
# For existing nodes that will be part of the operation
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false

# For all nodes during initial bringup
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false
```

**Note:** If your infrastructure supports it, configure your node provisioning system to automatically apply this label during node creation.

#### Step 2: Perform Scale-Up Operation

Add your GPU nodes using your standard provisioning workflow:
- Update EKS/GKE/AKS node pool configurations
- Deploy via Terraform/Helm/kubectl
- Trigger Karpenter/Cluster Autoscaler
- Run your infrastructure automation

#### Step 3: Validate GPU Node Health

Wait for all GPU components to become healthy before proceeding:

```bash
# Check that GPU Operator pods are running on new nodes
kubectl get po -n gpu-operator -o wide

# Verify DCGM is accessible on new nodes
kubectl get po -n gpu-operator | grep dcgm

# Check node conditions
kubectl get nodes -o wide

# Validate that nodes are Ready and not cordoned
kubectl get nodes | grep -v SchedulingDisabled
```

**Wait until:**
- All GPU Operator pods are `Running` and `Ready`
- Nodes show `Ready` status
- No nodes are cordoned
- DCGM metrics are being collected

#### Step 4: Re-enable NVSentinel Management

Once nodes are fully healthy, remove the exclusion label:

```bash
# For specific nodes
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-

# For all nodes
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-
```

#### Step 5: Monitor for Issues

After re-enabling management, monitor for 15-30 minutes:

```bash
# Check circuit breaker status
kubectl get cm circuit-breaker -n nvsentinel -o yaml

# Watch for cordoned nodes
kubectl get nodes | grep SchedulingDisabled

# Check NVSentinel metrics
kubectl logs -n nvsentinel -l app=fault-quarantine --tail=50
```

### Option 2: Disable Circuit Breaker for Non-Production Environments

For **development, testing, or staging environments only**, you can disable the circuit breaker entirely.

⚠️ **WARNING:** Do not disable the circuit breaker in production environments. It is a critical safety mechanism.

#### Configuration

In your Helm values:

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: false
```

Then upgrade your NVSentinel installation:

```bash
helm upgrade nvsentinel <chart> -n nvsentinel -f values.yaml
```

**When to use this option:**
- Small test/dev clusters (< 5 nodes)
- Environments where cluster stability is not critical
- Temporary testing scenarios

**Do NOT use this option for:**
- Production clusters
- Clusters running critical workloads
- Shared/multi-tenant environments

### Option 3: Adjust Circuit Breaker Thresholds

For large clusters with gradual scale-up patterns, you can adjust thresholds to reduce false positives.

```yaml
fault-quarantine:
  circuitBreaker:
    enabled: true
    percentage: 70      # Increase from default 50%
    duration: "10m"     # Increase from default 5m
```

**Considerations:**
- Higher percentage = more nodes can be cordoned before tripping
- Longer duration = slower response to legitimate widespread issues
- This is a **band-aid solution** - Option 1 is still recommended

**Recommended settings by cluster size:**
- **1-10 nodes:** Use Option 1 (labeling) or Option 2 (disable for non-prod)
- **10-50 nodes:** `percentage: 60-70%`, `duration: "10m"`
- **50+ nodes:** Default settings usually work, but use Option 1 for large scale-ups

## Advanced: Automating the Label Process

### For Node Provisioning Systems

If your infrastructure supports node initialization hooks, you can automate the labeling process:

#### Example: Kubernetes Node Taints + Labels

During node provisioning, apply a temporary taint and the NVSentinel exclusion label:

```yaml
# Node template configuration
apiVersion: v1
kind: Node
metadata:
  labels:
    k8saas.nvidia.com/ManagedByNVSentinel: "false"
  taints:
  - key: "node.kubernetes.io/not-ready"
    effect: "NoSchedule"
```

Then use a DaemonSet or init controller to:
1. Wait for GPU Operator to be healthy
2. Remove the exclusion label
3. Remove the taint

#### Example: Terraform/Pulumi

```hcl
# Terraform example
resource "kubernetes_node_pool" "gpu_nodes" {
  # ... other configuration ...
  
  node_labels = {
    "k8saas.nvidia.com/ManagedByNVSentinel" = "false"
  }
}

# Use a null_resource with local-exec to remove label after health checks
resource "null_resource" "enable_nvsentinel" {
  depends_on = [kubernetes_node_pool.gpu_nodes]
  
  provisioner "local-exec" {
    command = <<-EOT
      # Wait for nodes to be healthy
      kubectl wait --for=condition=Ready nodes -l your-node-selector --timeout=10m
      
      # Remove exclusion label
      kubectl label nodes -l your-node-selector k8saas.nvidia.com/ManagedByNVSentinel-
    EOT
  }
}
```

### For GitOps/Runtime Controllers

If you use a runtime controller or GitOps system, implement preflight checks similar to the GPU Operator timing fix:

```go
// Pseudocode example
func ScaleUpGPUNodes(desiredCount int) error {
    // 1. Label all existing nodes
    LabelAllNodes("k8saas.nvidia.com/ManagedByNVSentinel", "false")
    
    // 2. Perform scale-up
    ScaleNodePool(desiredCount)
    
    // 3. Wait for GPU Operator to be ready
    WaitForGPUOperatorReady()
    
    // 4. Wait for DCGM to be accessible on all nodes
    WaitForDCGMHealthy()
    
    // 5. Remove labels
    RemoveLabelFromAllNodes("k8saas.nvidia.com/ManagedByNVSentinel")
    
    return nil
}
```

## Comparison with GPU Operator Timing Issues

This issue is analogous to the GPU Operator race condition that was previously addressed:

| Issue | GPU Operator Timing Bug | NVSentinel Circuit Breaker Trip |
|-------|------------------------|--------------------------------|
| **Symptom** | Cluster Policy not ready when checked | Nodes cordoned during scale-up |
| **Root Cause** | Controller checked before GPU resources ready | Too many nodes unhealthy at once |
| **Solution** | Added preflight checks for GPU presence | Label nodes during scale operations |
| **Location** | Runtime controller | Infrastructure/GitOps layer |

Both issues stem from the same problem: **services running inside the cluster cannot distinguish between scale-up and failure scenarios**.

## Troubleshooting

### Circuit Breaker Tripped During Scale-Up

**Symptoms:**
- Circuit breaker status shows `TRIPPED`
- New GPU nodes are cordoned
- Nodes appear healthy but remain unavailable

**Resolution:**

1. Verify nodes are actually healthy:
```bash
kubectl get nodes
kubectl describe node <NODE_NAME>
kubectl get po -n gpu-operator -o wide
```

2. If nodes are healthy, reset the circuit breaker:
```bash
# Option A: Skip accumulated events (recommended for scale-up scenarios)
kubectl patch configmap circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'

# Option B: Process accumulated events
kubectl patch configmap circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"RESUME"}}'

# Restart fault-quarantine
kubectl rollout restart deployment fault-quarantine -n nvsentinel
```

3. Uncordon nodes if necessary:
```bash
kubectl uncordon <NODE_NAME>
```

4. Apply the exclusion label to prevent future issues:
```bash
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false
```

5. Once stable, remove the label:
```bash
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-
```

### Autoscaler Repeatedly Triggers Circuit Breaker

**Problem:** Karpenter or Cluster Autoscaler rapidly scales up, causing repeated circuit breaker trips.

**Solutions:**

1. **Implement gradual scale-up** in your autoscaler configuration:
```yaml
# Karpenter example
apiVersion: karpenter.sh/v1alpha5
kind: Provisioner
metadata:
  name: gpu-provisioner
spec:
  limits:
    resources:
      nvidia.com/gpu: 100
  # Add gradual scale-up constraints
  consolidation:
    enabled: true
  ttlSecondsAfterEmpty: 300
```

2. **Use automation** to apply labels during autoscale events (see "Advanced: Automating the Label Process" above)

3. **Adjust circuit breaker thresholds** for your workload patterns

### Nodes Remain Cordoned After Scale-Up

**Problem:** Even after circuit breaker is reset, nodes stay cordoned.

**Diagnosis:**
```bash
# Check node conditions
kubectl describe node <NODE_NAME> | grep -A 10 Conditions

# Check node taints
kubectl describe node <NODE_NAME> | grep Taints
```

**Resolution:**
```bash
# Manually uncordon if node is healthy
kubectl uncordon <NODE_NAME>

# Check NVSentinel logs for the reason
kubectl logs -n nvsentinel -l app=fault-quarantine | grep <NODE_NAME>
```

## Best Practices

1. **Plan scale operations**: Don't perform unplanned rapid scale-ups in production
2. **Use automation**: Implement automatic labeling in your provisioning system
3. **Monitor circuit breaker**: Set up alerts for circuit breaker status changes
4. **Document procedures**: Ensure your team knows to apply labels during scale operations
5. **Test in staging**: Validate your scale-up procedures in non-production environments first
6. **Size appropriately**: Start with adequate cluster size to minimize the need for large scale-ups
7. **Gradual scaling**: Configure autoscalers for gradual scale-up when possible
8. **Post-scale validation**: Always verify node health before removing exclusion labels

## Related Documentation

- [Circuit Breaker](circuit-breaker.md) - Understanding circuit breaker behavior
- [Driver Upgrades](driver-upgrades.md) - Similar procedure for GPU driver upgrades
- [Cordoned Nodes](cordoned-nodes.md) - General guidance on cordoned nodes
- [ADR-027: Automated Node Labeling for Scale Operations](../designs/027-automated-node-labeling.md) - Proposed automation solutions

## Quick Reference

### Label to exclude from NVSentinel
```bash
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false
```

### Remove exclusion label
```bash
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-
```

### Check circuit breaker status
```bash
kubectl get cm circuit-breaker -n nvsentinel -o yaml
```

### Reset circuit breaker (skip accumulated events)
```bash
kubectl patch configmap circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'
kubectl rollout restart deployment fault-quarantine -n nvsentinel
```

### Check for cordoned nodes
```bash
kubectl get nodes | grep SchedulingDisabled
```

### Uncordon a node
```bash
kubectl uncordon <NODE_NAME>
```

