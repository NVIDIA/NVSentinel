# Runbook: Cordoned Nodes

## Overview

This runbook handles nodes that have been cordoned in the cluster. Cordoned nodes indicate detected hardware issues that require investigation and remediation.

**Key points:**
- Nodes progress through states: quarantined → draining → drain-succeeded → remediating → remediation-succeeded
- Terminal failure states require manual intervention
- Each cordoned node must be individually triaged based on its state

## Procedure

### 1. Identify Cordoned Nodes

Find nodes cordoned by NVSentinel:

```bash
kubectl get nodes --field-selector spec.unschedulable=true -o jsonpath='{range .items[?(@.metadata.annotations.quarantineHealthEvent)]}{.metadata.name}{"\n"}{end}'
```

**If no nodes are returned:** The cordoning was done by another system. Do not use this runbook.

### 2. Save Health Event for Each Cordoned Node

Save the health event details for reference:

```bash
kubectl get node $NODE_NAME -o jsonpath='{.metadata.annotations.quarantineHealthEvent}' | jq > /tmp/$NODE_NAME-health-event.json
cat /tmp/$NODE_NAME-health-event.json
```

This shows:
- `errorCode` - The problem with the node
- `recommendedAction` - How to remediate
- Other diagnostic information

### 3. Check Node State

Check the NVSentinel state label:

```bash
kubectl get node $NODE_NAME -L dgxc.nvidia.com/nvsentinel-state
```

Possible states:
- `quarantined` - Fault detected, waiting for drain to start
- `draining` - Node is being drained
- `drain-succeeded` - Drain completed, waiting for remediation
- `drain-failed` - **TERMINAL STATE** - Drain failed
- `remediating` - CR creation in progress (transitions quickly)
- `remediation-succeeded` - CR created, remediation may be in progress
- `remediation-failed` - **TERMINAL STATE** - Unsupported action or CR creation failed
- *(no label)* - Should not be cordoned

Proceed to the appropriate section based on the state.

### 4a. If State is `drain-failed` (Terminal State)

Drain failed and no remediation will be attempted. Check for stuck pods:

```bash
kubectl get pods --all-namespaces --field-selector spec.nodeName=$NODE_NAME -o wide
```

**If pods are stuck:**

Check for PodDisruptionBudgets blocking eviction:
```bash
kubectl get pdb --all-namespaces
```

Force delete stuck pods:
```bash
kubectl delete po $POD_NAME -n $NAMESPACE --force --grace-period=0
```

**After pods are cleared, manually remediate:**

Check the recommended action from the health event:

```bash
jq '.recommendedAction' /tmp/$NODE_NAME-health-event.json
```

Follow the remediation action manually (reboot via CSP console, contact support, etc.).

Monitor if the health check starts passing. If it does, the node should be automatically uncordoned. If the health check doesn't pass, investigate with your organization's support team.

### 4b. If State is `remediation-failed` (Terminal State)

This means either the recommended action is unsupported or CR creation failed.

Check fault-remediation logs:

```bash
kubectl logs -n nvsentinel deployment/fault-remediation --tail=200
```

**If action is unsupported:**

The recommended action cannot be automated. Manually remediate based on the recommended action from the saved health event.

Monitor if the health check starts passing. If it does, the node should be automatically uncordoned. If the health check doesn't pass, investigate with your organization's support team.

**If CR creation failed:**

Investigate why CR creation failed from the logs. Common causes:
- API server issues
- Invalid CR template

After resolving the issue, manually remediate the node.

Monitor if the health check starts passing. If it does, the node should be automatically uncordoned. If the health check doesn't pass, investigate with your organization's support team.

### 4c. If State is `quarantined`, `draining`, `drain-succeeded`, or `remediating`

Node is in an active remediation flow. Wait for the process to complete.

Monitor state changes:
```bash
kubectl get node $NODE_NAME -L dgxc.nvidia.com/nvsentinel-state -w
```

### 4d. If State is `remediation-succeeded`

This means the CR was created successfully. However, the actual remediation operation (e.g., reboot) may still be in progress or may have failed.

Check if the remediation CR exists and its status:

```bash
# Find the CR name
kubectl get rebootnodes | grep maintenance-$NODE_NAME

# Check CR status
kubectl get rebootnode <CR_NAME> -o yaml
```

**If CR shows successful completion:**

The node should automatically uncordon once health checks pass. Monitor:
```bash
kubectl get nodes -L dgxc.nvidia.com/nvsentinel-state -w
```

**If node remains cordoned after 10 minutes:**

Check fault-quarantine logs to determine why:
```bash
kubectl logs -n nvsentinel deployment/fault-quarantine
```

If health checks are passing but node remains cordoned, manually uncordon:
```bash
kubectl uncordon $NODE_NAME
```

**If CR shows failure or is stuck:**

Manually remediate the node based on the recommended action.

Monitor if the health check starts passing. If it does, the node should be automatically uncordoned. If the health check doesn't pass, investigate with your organization's support team.

### 5. Handling False Positives

If you believe the health event is a false positive, review the details saved earlier:

```bash
cat /tmp/$NODE_NAME-health-event.json
```

To override the cordoning:

```bash
# Uncordon the node
kubectl uncordon $NODE_NAME

# If the same node(s) is repeatedly cordoned, disable NVSentinel management
kubectl label node $NODE_NAME k8saas.nvidia.com/ManagedByNVSentinel=false
```

Report the issue to your organization's support team for investigation.

### 6. Verify Node Recovery

After remediation, monitor node status:

```bash
# Watch nodes
kubectl get nodes -L dgxc.nvidia.com/nvsentinel-state -w

# Verify node is schedulable and healthy
kubectl get node $NODE_NAME -o wide
```
