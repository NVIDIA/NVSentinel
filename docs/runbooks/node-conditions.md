# Runbook: Node Conditions Without Cordoning

## Overview

This runbook is for cases where a node has GPU health conditions but is NOT cordoned. This typically indicates one of these causes:

- The fault-quarantine CEL configuration excludes the health event from cordoning
- An operator manually uncordoned the node

## Procedure

### 1. Check Node Status

Verify that the node has conditions but is schedulable:

```bash
kubectl get node $NODE_NAME
```

In the output, the node shows `Ready`, and `SchedulingDisabled` is NOT present.

View the conditions:

```bash
kubectl describe node $NODE_NAME
```

### 2. Check for Manual Uncordon Annotation

Check if an operator manually uncordoned the node:

```bash
kubectl get node $NODE_NAME -o jsonpath='{.metadata.annotations.quarantinedNodeUncordonedManually}'
```

**If the annotation is present:**

An operator manually uncordoned the node. When this annotation exists:

- Fault-quarantine does NOT re-cordon the node for the same health event
- Someone acknowledged the condition and decided to keep the node schedulable
- Possibly, the operator considers the health issue non-critical, or plans the remediation for later

**To investigate who uncordoned:**

To find who manually uncordoned the node and why, check the Kubernetes audit logs or your organization's access policy.

### 3. Check Fault Quarantine Configuration

If no manual uncordon annotation exists, CEL rules probably exclude the health event.

Check the fault-quarantine configuration:

```bash
kubectl get configmap fault-quarantine -n nvsentinel -o yaml
```

Examine the `rule-sets` section. Each ruleset has CEL expressions under `match.all` or `match.any`.

**Example exclusion patterns:**

```toml
[[rule-sets.match.all]]
  kind = "HealthEvent"
  expression = "event.isFatal == true"
```

This rule matches only fatal health events. Non-fatal health events do not trigger cordoning.

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

### 4. Determine Why the Node Was Not Cordoned

From the node conditions, identify the likely reason:

**Common reasons for no cordoning:**

1. **Non-fatal health event** - The health check detected an issue but marked it as non-fatal, and the rules require `event.isFatal == true`
2. **Error code exclusion** - The rules filter specific error codes, for example thermal warnings and transient errors
3. **Node label exclusion** - The node has the `k8saas.nvidia.com/ManagedByNVSentinel=false` label
4. **Check name exclusion** - The cordoning rules exclude certain health checks

Check if the node has the exclusion label:

```bash
kubectl get node $NODE_NAME -L k8saas.nvidia.com/ManagedByNVSentinel
```

If the value is `false`, NVSentinel does not manage this node.

### 5. Determining Next Steps

**If the health condition is acceptable:**

No action is necessary. The configuration works as intended: it monitors the issue and does not affect scheduling.

**If the node must be cordoned:**

Option 1: Update the fault-quarantine configuration to include this health event type.

Option 2: Manually cordon the node:

```bash
kubectl cordon $NODE_NAME
```

Note: Manual cordoning does not trigger the NVSentinel break-fix workflow. For full remediation, the health event must match a CEL rule.

**If a configuration change is necessary:**

Update the fault-quarantine ConfigMap. Then restart the deployment:

```bash
kubectl edit configmap fault-quarantine -n nvsentinel
kubectl rollout restart deployment/fault-quarantine -n nvsentinel
```
