# Runbook: Node Conditions Without Cordoning

## Overview

This runbook handles cases where a node has GPU health conditions but is NOT cordoned. This typically indicates:
- The fault-quarantine CEL configuration is excluding the health event from cordoning
- The node was manually uncordoned by an operator

## Procedure

### 1. Check Node Status

Verify the node has conditions but is schedulable:

```bash
kubectl get node $NODE_NAME
```

The node should show `Ready` and `SchedulingDisabled` should NOT be present.

View the conditions:

```bash
kubectl describe node $NODE_NAME
```

### 2. Check for Manual Uncordon Annotation

Check if the node was manually uncordoned:

```bash
kubectl get node $NODE_NAME -o jsonpath='{.metadata.annotations.quarantinedNodeUncordonedManually}'
```

**If the annotation is present:**

The node was manually uncordoned by an operator. When this annotation exists:
- Fault-quarantine will NOT re-cordon the node for the same health event
- Someone has acknowledged the condition and decided to keep the node schedulable
- The health issue may be deemed non-critical or remediation is planned for later

**To investigate who uncordoned:**

Check Kubernetes audit logs or your organization's access policy to determine who manually uncordoned the node and why.

### 3. Check Fault Quarantine Configuration

If no manual uncordon annotation exists, the health event is likely excluded by CEL rules.

Check the fault-quarantine configuration:

```bash
kubectl get configmap fault-quarantine -n nvsentinel -o yaml
```

Look at the `rule-sets` section. Each ruleset has CEL expressions under `match.all` or `match.any`.

**Example exclusion patterns:**

```toml
[[rule-sets.match.all]]
  kind = "HealthEvent"
  expression = "event.isFatal == true"
```

This rule only matches fatal events. Non-fatal events won't trigger cordoning.

```toml
[[rule-sets.match.all]]
  kind = "HealthEvent"
  expression = "event.isFatal == true && (event.errorCode == null || !event.errorCode.exists(e, e == 'DCGM_FR_CLOCK_THROTTLE_POWER'))"
```

This rule excludes `DCGM_FR_CLOCK_THROTTLE_POWER` errors from cordoning.

```toml
[[rule-sets.match.all]]
  kind = "Node"
  expression = "!has(node.labels['k8saas.nvidia.com/ManagedByNVSentinel']) || node.labels['k8saas.nvidia.com/ManagedByNVSentinel'] != 'false'"
```

This rule excludes nodes with the `k8saas.nvidia.com/ManagedByNVSentinel=false` label.

### 4. Determine Why the Node Wasn't Cordoned

Based on the node conditions present, identify the likely reason:

**Common reasons for no cordoning:**

1. **Non-fatal event** - The health check detected an issue but marked it as non-fatal, and rules require `event.isFatal == true`
2. **Error code exclusion** - Specific error codes are filtered (e.g., thermal warnings, transient errors)
3. **Node label exclusion** - Node has `k8saas.nvidia.com/ManagedByNVSentinel=false` label
4. **Check name exclusion** - Certain health checks are excluded from cordoning rules

Check if the node has the exclusion label:

```bash
kubectl get node $NODE_NAME -L k8saas.nvidia.com/ManagedByNVSentinel
```

If the value is `false`, NVSentinel is not managing this node.

### 5. Determining Next Steps

**If the health condition is acceptable:**

No action needed. The configuration is working as intended - monitoring the issue without impacting scheduling.

**If the node should be cordoned:**

Option 1: Update the fault-quarantine configuration to include this event type.

Option 2: Manually cordon the node:

```bash
kubectl cordon $NODE_NAME
```

Note: Manual cordoning won't trigger NVSentinel's remediation flow. For full remediation, the health event needs to match a CEL rule.

**If configuration change is needed:**

Update the fault-quarantine ConfigMap and restart the deployment:

```bash
kubectl edit configmap fault-quarantine -n nvsentinel
kubectl rollout restart deployment/fault-quarantine -n nvsentinel
```
