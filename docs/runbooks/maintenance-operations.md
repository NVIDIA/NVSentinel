# NVSentinel Maintenance Operations Guide

## Overview

This guide provides comprehensive procedures for common maintenance operations that require temporarily disabling or adjusting NVSentinel's node management. These operations share a common pattern: they involve changes that make nodes temporarily appear unhealthy to NVSentinel, potentially triggering the circuit breaker.

## Common Pattern: The `ManagedByNVSentinel` Label

Most maintenance operations follow this pattern:

```bash
# 1. Disable NVSentinel management
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false

# 2. Perform your maintenance operation
# ...

# 3. Verify node health
kubectl get nodes
kubectl get po -n gpu-operator

# 4. Re-enable NVSentinel management
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-
```

**Why this works**: The label tells NVSentinel to ignore the node, preventing it from:
- Cordoning the node during transitional states
- Contributing to circuit breaker thresholds
- Triggering alerts for expected maintenance events

## Operation-Specific Guides

### 1. Cluster Scale Operations

**Applies to:**
- Initial cluster bringup (CPU → GPU)
- Autoscaling events (Karpenter, Cluster Autoscaler)
- Manual capacity expansion
- Multi-zone node additions

**Why needed**: New nodes go through initialization where GPU components aren't ready yet, appearing unhealthy to NVSentinel.

**Detailed guide**: See [Cluster Scale Operations](cluster-scale-operations.md)

**Quick reference**:
```bash
# Label ALL nodes before adding new GPU nodes
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false

# Perform scale-up
# Wait for GPU Operator and DCGM healthy

# Remove labels
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🔴 **HIGH** - Almost certain to trip circuit breaker on small clusters without labeling

---

### 2. GPU Driver Upgrades

**Applies to:**
- GPU driver version updates
- GPU Operator upgrades
- NVIDIA driver module changes

**Why needed**: During upgrades, DCGM becomes temporarily unavailable, triggering health check failures.

**Detailed guide**: See [Driver Upgrades](driver-upgrades.md)

**Quick reference**:
```bash
# Label nodes to be upgraded
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false

# Perform GPU driver/operator upgrade
# Verify all gpu-operator pods are Running and Ready

# Remove labels
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🟡 **MEDIUM** - Will trip if upgrading many nodes in parallel

---

### 3. OSMO (OS Maintenance Operations)

**Applies to:**
- Operating system upgrades
- Kernel updates
- Security patches requiring node reboots
- Node drain and replace operations

**Why needed**: Node reboots cause all GPU services to restart, creating a period where nodes appear unhealthy.

**Procedure**:
```bash
# Before starting OSMO maintenance
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false

# Follow your OSMO maintenance procedure:
# - Drain node
# - Apply updates
# - Reboot
# - Wait for node to rejoin cluster

# Verify node health
kubectl get nodes
kubectl get po -n gpu-operator -o wide | grep <NODE_NAME>

# After node is fully healthy, remove label
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🟡 **MEDIUM** - Depends on how many nodes are maintained in parallel

---

### 4. Node Hardware Maintenance

**Applies to:**
- GPU hardware replacement
- Memory upgrades
- Network card replacement
- Power supply maintenance
- Physical server relocation

**Procedure**:
```bash
# Label and drain the node
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false
kubectl drain <NODE_NAME> --ignore-daemonsets --delete-emptydir-data

# Cordon to prevent scheduling during maintenance
kubectl cordon <NODE_NAME>

# Perform physical hardware maintenance
# ...

# Uncordon and verify health
kubectl uncordon <NODE_NAME>
kubectl get nodes <NODE_NAME>
kubectl get po -n gpu-operator -o wide | grep <NODE_NAME>

# Wait for all GPU services to be healthy (DCGM, device plugin, etc.)

# Remove label
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🟢 **LOW** - Usually affects single nodes, unlikely to trip circuit breaker

---

### 5. Infrastructure Maintenance (Network, Power, Cooling)

**Applies to:**
- Rack-level maintenance affecting multiple nodes
- Network switch upgrades
- Data center maintenance windows
- Power infrastructure changes

**Procedure**:
```bash
# Before maintenance window starts
# Label ALL nodes that will be affected
kubectl label node -l <rack-label>=<value> k8saas.nvidia.com/ManagedByNVSentinel=false

# Optionally silence alerts for the maintenance window
# (if you have alerting configured)

# Perform infrastructure maintenance
# ...

# After maintenance, verify all nodes are healthy
kubectl get nodes

# Remove labels from all affected nodes
kubectl label node -l <rack-label>=<value> k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🔴 **HIGH** - Infrastructure maintenance often affects many nodes simultaneously

---

### 6. GPU Operator Component Updates

**Applies to:**
- Device plugin updates
- DCGM exporter updates
- GPU feature discovery updates
- MIG manager updates

**Procedure**:
```bash
# Label all GPU nodes
kubectl label node -l nvidia.com/gpu.present=true k8saas.nvidia.com/ManagedByNVSentinel=false

# Update GPU Operator components via Helm
helm upgrade gpu-operator nvidia/gpu-operator -n gpu-operator -f values.yaml

# Wait for rollout to complete
kubectl rollout status daemonset -n gpu-operator nvidia-dcgm-exporter
kubectl rollout status daemonset -n gpu-operator nvidia-device-plugin-daemonset

# Verify all pods are healthy
kubectl get po -n gpu-operator

# Remove labels
kubectl label node -l nvidia.com/gpu.present=true k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🟡 **MEDIUM** - Depends on rollout strategy and cluster size

---

### 7. Troubleshooting and Testing

**Applies to:**
- Testing NVSentinel rules
- Debugging health check issues
- Investigating node problems
- Simulating failures

**Procedure**:
```bash
# Label test nodes to isolate from production monitoring
kubectl label node <TEST_NODE> k8saas.nvidia.com/ManagedByNVSentinel=false

# Perform your troubleshooting or testing
# - Test health checks
# - Inject faults
# - Debug components

# When testing complete, remove label
kubectl label node <TEST_NODE> k8saas.nvidia.com/ManagedByNVSentinel-
```

**Risk level**: 🟢 **LOW** - Typically affects only test/debug nodes

---

## Best Practices

### Before Maintenance

1. **Plan ahead**: Review which operations require labeling
2. **Label proactively**: Apply labels before starting maintenance, not after issues occur
3. **Document scope**: Keep track of which nodes are labeled and why
4. **Set alerts**: Configure notifications for circuit breaker events
5. **Check circuit breaker**: Verify it's not already tripped before starting

```bash
# Check current circuit breaker status
kubectl get cm circuit-breaker -n nvsentinel -o jsonpath='{.data.status}'
```

### During Maintenance

1. **Use batch operations carefully**: Be aware of how many nodes are affected simultaneously
2. **Monitor circuit breaker**: Watch for trips even with labels applied
3. **Verify health progressively**: Check each node/batch before proceeding to the next
4. **Keep labels during transition**: Don't remove labels until fully healthy

```bash
# Monitor nodes during maintenance
watch kubectl get nodes -o wide

# Check GPU Operator health
watch kubectl get po -n gpu-operator
```

### After Maintenance

1. **Verify complete health**: Ensure all components are ready before removing labels
2. **Remove labels promptly**: Don't leave labels on longer than necessary
3. **Monitor for issues**: Watch for 15-30 minutes after removing labels
4. **Document results**: Note any issues encountered and how they were resolved

```bash
# Final health check before removing labels
kubectl get nodes
kubectl get po -n gpu-operator
kubectl get cm circuit-breaker -n nvsentinel -o yaml

# After removing labels, monitor
kubectl get nodes -w
```

### Circuit Breaker Management

If the circuit breaker trips despite following procedures:

1. **Don't panic**: The circuit breaker is doing its job
2. **Investigate**: Determine if it's a legitimate issue or expected transitional state
3. **Reset appropriately**: Use `cursor: CREATE` for maintenance scenarios to skip accumulated events

```bash
# Reset circuit breaker after maintenance
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel
```

See [Circuit Breaker Runbook](circuit-breaker.md) for detailed reset procedures.

## Automation Strategies

### Infrastructure-as-Code Integration

#### Terraform Example

```hcl
resource "kubernetes_labels" "maintenance_mode" {
  api_version = "v1"
  kind        = "Node"
  
  metadata {
    name = var.node_name
  }
  
  labels = {
    "k8saas.nvidia.com/ManagedByNVSentinel" = "false"
  }
}

# Perform maintenance operations
# ...

# Remove label using null_resource with local-exec
resource "null_resource" "remove_maintenance_mode" {
  depends_on = [
    # Your maintenance resources
  ]
  
  provisioner "local-exec" {
    command = "kubectl label node ${var.node_name} k8saas.nvidia.com/ManagedByNVSentinel-"
  }
}
```

#### Ansible Playbook Example

```yaml
---
- name: Node Maintenance with NVSentinel Management
  hosts: localhost
  tasks:
    - name: Disable NVSentinel management
      kubernetes.core.k8s:
        state: present
        definition:
          apiVersion: v1
          kind: Node
          metadata:
            name: "{{ node_name }}"
            labels:
              k8saas.nvidia.com/ManagedByNVSentinel: "false"
    
    - name: Perform maintenance
      # Your maintenance tasks here
      
    - name: Wait for node health
      kubernetes.core.k8s_info:
        kind: Node
        name: "{{ node_name }}"
      register: node_info
      until: node_info.resources[0].status.conditions | selectattr('type', 'equalto', 'Ready') | map(attribute='status') | first == 'True'
      retries: 30
      delay: 10
    
    - name: Re-enable NVSentinel management
      kubernetes.core.k8s:
        state: present
        definition:
          apiVersion: v1
          kind: Node
          metadata:
            name: "{{ node_name }}"
            labels:
              k8saas.nvidia.com/ManagedByNVSentinel: null
```

### GitOps Integration

For ArgoCD/Flux-managed clusters, create a maintenance ConfigMap that your runtime controller watches:

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: maintenance-schedule
  namespace: nvsentinel
data:
  mode: "scale-operation"  # or "driver-upgrade", "osmo-maintenance", etc.
  nodes: "node-1,node-2,node-3"
  start-time: "2026-02-07T10:00:00Z"
  end-time: "2026-02-07T12:00:00Z"
```

Your controller can automatically apply/remove labels based on this schedule.

## Troubleshooting Common Issues

### Labels Not Taking Effect

**Symptom**: Nodes are still being cordoned despite having the `ManagedByNVSentinel=false` label.

**Possible causes**:
1. Label was applied after NVSentinel already detected the issue
2. There are multiple NVSentinel deployments (check namespace)
3. Typo in label name or value

**Solution**:
```bash
# Verify label is correctly applied
kubectl get node <NODE_NAME> --show-labels | grep ManagedByNVSentinel

# Check NVSentinel logs
kubectl logs -n nvsentinel -l app=fault-quarantine | grep <NODE_NAME>

# Restart fault-quarantine to pick up labels
kubectl rollout restart deploy fault-quarantine -n nvsentinel
```

### Forgot to Apply Labels

**Symptom**: Circuit breaker tripped during maintenance because labels weren't applied proactively.

**Solution**:
```bash
# Apply labels now (even though late)
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false

# Reset circuit breaker with CREATE cursor
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'

# Restart fault-quarantine
kubectl rollout restart deploy fault-quarantine -n nvsentinel

# Continue with maintenance
# Remove labels after completion
```

### Labels Left On Too Long

**Symptom**: Nodes have `ManagedByNVSentinel=false` from previous maintenance, not being monitored.

**Solution**:
```bash
# Find nodes with the exclusion label
kubectl get nodes -l k8saas.nvidia.com/ManagedByNVSentinel=false

# Verify they're actually healthy
kubectl describe node <NODE_NAME>

# Remove labels from healthy nodes
kubectl label node -l k8saas.nvidia.com/ManagedByNVSentinel=false k8saas.nvidia.com/ManagedByNVSentinel-
```

**Prevention**: Set up monitoring/alerting for nodes with this label:

```promql
# Alert if nodes have exclusion label for > 2 hours
kube_node_labels{label_k8saas_nvidia_com_ManagedByNVSentinel="false"} == 1
```

## Decision Tree: Do I Need to Label?

```
START: Planning a cluster operation
    |
    ├─> Will this affect GPU nodes? 
    |       NO → Proceed normally (no labeling needed)
    |       YES ↓
    |
    ├─> Will GPU services (driver/DCGM) be temporarily unavailable?
    |       NO → Proceed normally (no labeling needed)
    |       YES ↓
    |
    ├─> How many nodes affected simultaneously?
    |       Single node → Low risk, optional labeling
    |       2-5 nodes → Medium risk, labeling recommended
    |       5+ nodes or >30% of cluster → High risk, labeling required
    |       ↓
    |
    └─> APPLY LABELS BEFORE STARTING OPERATION
```

## Quick Reference Commands

```bash
# === LABEL OPERATIONS ===

# Label all nodes
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel=false

# Label specific node
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel=false

# Label by selector
kubectl label node -l <selector> k8saas.nvidia.com/ManagedByNVSentinel=false

# Remove label from all nodes
kubectl label node --all k8saas.nvidia.com/ManagedByNVSentinel-

# Remove label from specific node
kubectl label node <NODE_NAME> k8saas.nvidia.com/ManagedByNVSentinel-

# List nodes with exclusion label
kubectl get nodes -l k8saas.nvidia.com/ManagedByNVSentinel=false

# === HEALTH CHECKS ===

# Check all nodes
kubectl get nodes -o wide

# Check GPU Operator pods
kubectl get po -n gpu-operator

# Check DCGM health
kubectl get po -n gpu-operator | grep dcgm

# Check node details
kubectl describe node <NODE_NAME>

# === CIRCUIT BREAKER ===

# Check circuit breaker status
kubectl get cm circuit-breaker -n nvsentinel -o yaml

# Reset with CREATE cursor (skip events)
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"CREATE"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel

# Reset with RESUME cursor (process events)
kubectl patch cm circuit-breaker -n nvsentinel \
  -p '{"data":{"status":"CLOSED","cursor":"RESUME"}}'
kubectl rollout restart deploy fault-quarantine -n nvsentinel

# === MONITORING ===

# Watch nodes
kubectl get nodes -w

# Watch GPU Operator pods
kubectl get po -n gpu-operator -w

# Check fault-quarantine logs
kubectl logs -n nvsentinel -l app=fault-quarantine --tail=100 -f

# Check for cordoned nodes
kubectl get nodes | grep SchedulingDisabled
```

## Related Documentation

- [Cluster Scale Operations](cluster-scale-operations.md) - Detailed guide for scale-up scenarios
- [Driver Upgrades](driver-upgrades.md) - Specific procedures for GPU driver maintenance
- [Circuit Breaker](circuit-breaker.md) - Understanding and resetting the circuit breaker
- [Cordoned Nodes](cordoned-nodes.md) - General troubleshooting for cordoned nodes
- [ADR-027: Automated Node Labeling](../designs/027-automated-node-labeling.md) - Future automation plans

## Feedback and Improvements

This guide is continuously being improved based on real-world usage. If you encounter:
- Scenarios not covered here
- Procedures that don't work as expected
- Suggestions for automation

Please contribute back to the documentation or file an issue with your findings.

