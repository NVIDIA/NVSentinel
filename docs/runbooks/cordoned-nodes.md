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

This means the CR was created successfully. However, the actual remediation operation (e.g. reboot or reset) may still be in progress or may have failed.

Check if the remediation CR exists and its status:

```bash
kubectl get node $NODE_NAME -o jsonpath='{.metadata.annotations.latestFaultRemediationState}' | jq > /tmp/$NODE_NAME-remediation-state.json

cat /tmp/$NODE_NAME-remediation-state.json
```

For COMPONENT_RESET actions:
```bash
kubectl get gpureset <CR_NAME> -o yaml
```

For RESTART_VM and RESTART_BM actions:
```bash
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

**GPUReset:**

When processing a GPUReset request, Janitor executes the following steps exposed through status conditions:
- Ready: check that there are no in-progress GPUReset CRs executing against the same node.
- ServicesTornDown: remove gpu-operator services including the nvidia-device-plugin-daemonset, nvidia-dcgm, and nvidia-dcgm-exporter.
- ResetJobCreated: create a privileged job against the given node with the GPU needing reset.
- ResetJobCompleted: wait for the pod from the privileged job to disable persistence mode, reset the target GPU using nvidia-smi, re-enable persistence mode, and write a syslog event indicating the GPU has been reset. 
- ServicesRestored: restore gpu-operator services including the nvidia-device-plugin-daemonset, nvidia-dcgm, and nvidia-dcgm-exporter.
- Complete: complete reconciling of the GPUReset CR.

```bash
status:
  completionTime: "2026-03-03T22:54:24Z"
  conditions:
  - lastTransitionTime: "2026-03-03T22:53:04Z"
    message: Node ready for GPU reset
    reason: ReadyForReset
    status: "True"
    type: Ready
  - lastTransitionTime: "2026-03-03T22:53:06Z"
    message: gpu-operator managed services have been removed
    reason: ServiceTeardownSucceeded
    status: "True"
    type: ServicesTornDown
  - lastTransitionTime: "2026-03-03T22:53:06Z"
    message: GPU reset job created
    reason: ResetJobCreationSucceeded
    status: "True"
    type: ResetJobCreated
  - lastTransitionTime: "2026-03-03T22:54:14Z"
    message: GPU(s) reset successfully
    reason: ResetJobSucceeded
    status: "True"
    type: ResetJobCompleted
  - lastTransitionTime: "2026-03-03T22:54:24Z"
    message: gpu-operator managed services are Ready
    reason: ServiceRestoreSucceeded
    status: "True"
    type: ServicesRestored
  - lastTransitionTime: "2026-03-03T22:54:24Z"
    message: GPU(s) reset successfully
    reason: GPUResetSucceeded
    status: "True"
    type: Complete
  jobRef:
    kind: Job
    name: maintenance-10.0.6.34-69a7664f5ecd6c9af129dc2f-reset-job
    namespace: dgxc-janitor-system
  phase: Succeeded
  startTime: "2026-03-03T22:53:04Z"
```

Check Janitor controller-manager logs for why the ServicesTornDown, ResetJobCreated, or ServicesRestored steps failed during processing of a GPUReset. If the ResetJobCompleted step fails, check the logs for the reset pod:
```bash
kubectl get pods -n <JANITOR_NAMESPACE> | grep <CR_NAME>
 
kubectl logs <RESET_POD_NAME> -n <JANITOR_NAMESPACE>
```

**RebootNode:** 

When processing a RebootNode request, Janitor executes the following steps exposed through status conditions:
- SignalSent: issue a reboot request against the node for the given CSP.
- NodeReady: wait for the node to return to a Ready status after a reboot is triggered.

```bash
status:
  completionTime: "2026-03-02T23:30:01Z"
  conditions:
  - lastTransitionTime: "2026-03-02T22:48:01Z"
    message: "2026-03-02T22:48:01Z"
    reason: Succeeded
    status: "True"
    type: SignalSent
  - lastTransitionTime: "2026-03-02T23:30:01Z"
    message: Node reached ready state post-reboot
    reason: Succeeded
    status: "True"
    type: NodeReady
  startTime: "2026-03-02T22:48:01Z"
```
Check Janitor controller-manager logs for why the SignalSent or NodeReady steps failed during processing of a RebootNode.

### 5. Manual remediations

If a remediation action needs to be retried or executed manually (for example if the given health event published a CONTACT_SUPPORT recommended action), it might be required to manually create a RebootNode or GPUReset CR outside of NVSentinel.

**WARNING:**
- If a node progressed through the `draining` -> `drain-succeeded` -> `remediating` -> `remediation-succeeded` states but was not automatically uncordoned, it is safe to manually create a new RebootNode CR for the RESTART_VM or RESTART_BM actions because the node was already fully drained. Similarly, it is safe to create a new GPUReset CR, targeting the same node and GPU pair, for COMPONENT_RESET actions because the GPU needing reset was drained of any workload pods assigned to that GPU.  
- If an operator wants to remediate the given node with a RebootNode CR and the previous remediation action which failed was a GPUReset, the operator will need to ensure that the node is fully drained because NVSentinel would have only partially drained the node for pods which were assigned to the individual GPU needing reset. Follow the same steps outlined in 4a for handling the `drain-failed` state to manually execute a full drain against the node prior to creating a RebootNode.
- If an operator wants to remediate a different GPU compared to what was targeted in the initial GPUReset CR, the operator will need to ensure that all pods leveraging the given GPU are drained or else the reset can fail because processes could still have a handle on the GPU.

To manually trigger a reboot:
```bash
NODE=<NODE_NAME> && cat <<EOF | kubectl apply -f -
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: maintenance-$NODE
spec:
  nodeName: $NODE
  force: true
EOF
```

To manually trigger a GPU reset:
```bash
NODE=<NODE_NAME> && GPU_UUID=<GPU_UUID> && cat <<EOF | kubectl apply -f -
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: GPUReset
metadata:
  name: maintenance-$NODE
spec:
  nodeName: $NODE
  selector:
    uuids:
    - $GPU_UUID
EOF
```

After creating the maintenance CR, monitor if the health check starts passing. If it does, the node should be automatically uncordoned. If the health check doesn't pass, investigate with your organization's support team.

### 6. Handling False Positives

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

### 7. Verify Node Recovery

After remediation, monitor node status:

```bash
# Watch nodes
kubectl get nodes -L dgxc.nvidia.com/nvsentinel-state -w

# Verify node is schedulable and healthy
kubectl get node $NODE_NAME -o wide
```
