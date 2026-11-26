# Runbook: Cordoned Nodes

## Overview

This runbook is for nodes that are cordoned in the cluster. A cordoned node shows a detected hardware issue that needs investigation and remediation.

**Key points:**

- Nodes progress through states: quarantined → draining → drain-succeeded → remediating → remediation-succeeded
- Terminal failure states need manual intervention
- Triage each cordoned node individually, based on its state

## Procedure

### 1. Identify Cordoned Nodes

Find the nodes that NVSentinel cordoned:

```bash
kubectl get nodes --field-selector spec.unschedulable=true -o jsonpath='{range .items[?(@.metadata.annotations.quarantineHealthEvent)]}{.metadata.name}{"\n"}{end}'
```

**If the command returns no nodes:** Another system cordoned the nodes. Do not use this runbook.

### 2. Save Health Event for Each Cordoned Node

Save the health event details for reference:

```bash
kubectl get node $NODE_NAME -o jsonpath='{.metadata.annotations.quarantineHealthEvent}' | jq > /tmp/$NODE_NAME-health-event.json
cat /tmp/$NODE_NAME-health-event.json
```

The file shows:

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
- `draining` - Drain in progress
- `drain-succeeded` - Drain completed or skipped (`drainOverrides.skip`), waiting for remediation
- `drain-failed` - **TERMINAL STATE** - Drain failed
- `remediating` - CR creation in progress (transitions quickly)
- `remediation-succeeded` - CR created, remediation possibly in progress
- `remediation-failed` - **TERMINAL STATE** - Unsupported action or CR creation failed
- *(no label)* - Not expected to be cordoned

Go to the applicable section, based on the state.

### 4a. If State is `drain-failed` (Terminal State)

The drain failed, and NVSentinel does not attempt a remediation. Examine the node for stuck pods:

```bash
kubectl get pods --all-namespaces --field-selector spec.nodeName=$NODE_NAME -o wide
```

**If pods are stuck:**

Find PodDisruptionBudgets that block eviction:

```bash
kubectl get pdb --all-namespaces
```

Force delete the stuck pods:

```bash
kubectl delete po $POD_NAME -n $NAMESPACE --force --grace-period=0
```

**After you clear the pods, remediate the node manually:**

Check the recommended action from the health event:

```bash
jq '.recommendedAction' /tmp/$NODE_NAME-health-event.json
```

Follow the remediation action manually (reboot through the CSP console, contact support, or other actions).

Monitor the health check to see if it starts to pass. If all health checks of the node pass, NVSentinel uncordons the node automatically. If the health check does not pass, investigate with your organization's support team.

### 4b. If State is `remediation-failed` (Terminal State)

This state means that the recommended action is not supported, or that the CR creation failed.

Check the fault-remediation logs:

```bash
kubectl logs -n nvsentinel deployment/fault-remediation --tail=200
```

**If the action is not supported:**

NVSentinel cannot automate the recommended action. Remediate the node manually, based on the recommended action from the saved health event.

Monitor the health check to see if it starts to pass. If all health checks of the node pass, NVSentinel uncordons the node automatically. If the health check does not pass, investigate with your organization's support team.

**If CR creation failed:**

Use the logs to find why the CR creation failed. Common causes:

- API server issues
- Invalid CR template

After you resolve the issue, remediate the node manually.

Monitor the health check to see if it starts to pass. If all health checks of the node pass, NVSentinel uncordons the node automatically. If the health check does not pass, investigate with your organization's support team.

### 4c. If State is `quarantined`, `draining`, `drain-succeeded`, or `remediating`

The node is in an active remediation flow. Wait for the process to complete.

Monitor the state changes:

```bash
kubectl get node $NODE_NAME -L dgxc.nvidia.com/nvsentinel-state -w
```

### 4d. If State is `remediation-succeeded`

This state means that fault-remediation created the CR successfully. However, the actual remediation operation (for example, reboot or reset) is possibly still in progress. It is also possible that the operation failed.

Check if the maintenance CR exists, and check its status:

```bash
kubectl get node $NODE_NAME -o jsonpath='{.metadata.annotations.latestFaultRemediationState}' | jq > /tmp/$NODE_NAME-remediation-state.json

cat /tmp/$NODE_NAME-remediation-state.json
```

For COMPONENT_RESET actions:

```bash
kubectl get gpureset {CR_NAME} -o yaml
```

For RESTART_VM and RESTART_BM actions:

```bash
kubectl get rebootnode {CR_NAME} -o yaml
```

**If CR shows successful completion:**

When the health checks pass, NVSentinel uncordons the node automatically. Monitor the nodes:

```bash
kubectl get nodes -L dgxc.nvidia.com/nvsentinel-state -w
```

**If the node stays cordoned after 10 minutes:**

To find the cause, examine the fault-quarantine logs:

```bash
kubectl logs -n nvsentinel deployment/fault-quarantine
```

If the health checks pass but the node stays cordoned, uncordon the node manually:

```bash
kubectl uncordon $NODE_NAME
```

**If CR shows failure or is stuck:**

**GPUReset:**

When Janitor processes a GPUReset request, it does these steps. It shows each step as a status condition:

- Ready: check that no other GPUReset CR is in progress on the same node.
- ServicesTornDown: remove gpu-operator services, including the nvidia-device-plugin-daemonset, nvidia-dcgm, and nvidia-dcgm-exporter.
- ResetJobCreated: create a privileged job for the given node and the GPU that needs the reset.
- ResetJobCompleted: wait for the pod from the privileged job to do these actions:
  - disable persistence mode
  - reset the target GPU with nvidia-smi
  - enable persistence mode again
  - write a syslog event that shows that the GPU was reset
- ServicesRestored: restore gpu-operator services, including the nvidia-device-plugin-daemonset, nvidia-dcgm, and nvidia-dcgm-exporter.
- Complete: finish the reconciliation of the GPUReset CR.
- Succeeded: the outcome of the GPUReset. The status is `True` when the reset succeeded. It is `False`, with the failure reason, when the reset failed. The condition is absent while the GPUReset is in progress.

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
  - lastTransitionTime: "2026-03-03T22:54:24Z"
    message: GPU(s) reset successfully
    reason: GPUResetSucceeded
    status: "True"
    type: Succeeded
  jobRef:
    kind: Job
    name: maintenance-10.0.6.34-69a7664f5ecd6c9af129dc2f-reset-job
    namespace: dgxc-janitor-system
  phase: Succeeded
  startTime: "2026-03-03T22:53:04Z"
```

A ServicesTornDown, ResetJobCreated, or ServicesRestored step can fail during the processing of a GPUReset. To find the cause, examine the Janitor controller-manager logs. If the ResetJobCompleted step fails, examine the logs of the reset pod:

```bash
kubectl get pods -n {JANITOR_NAMESPACE} | grep {CR_NAME}
 
kubectl logs {RESET_POD_NAME} -n {JANITOR_NAMESPACE}
```

**RebootNode:**

When Janitor processes a RebootNode request, it does these steps. It shows each step as a status condition:

- SignalSent: send a reboot request for the node through the given CSP.
- NodeReady: wait for the node to return to a Ready status after the reboot starts.

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

If the SignalSent or NodeReady step fails for a RebootNode, examine the Janitor controller-manager logs to find the cause.

### 5. Manual remediations

Sometimes you must retry a remediation action, or do it manually. An example is a health event with a CONTACT_SUPPORT recommended action. In this case, it can be necessary to create a RebootNode or GPUReset CR manually, outside of NVSentinel.

**WARNING:**

- If a node progressed through the `draining` -> `drain-succeeded` -> `remediating` -> `remediation-succeeded` states but NVSentinel did not uncordon it automatically, these actions are safe:
  - If the recommended action was RESTART_VM or RESTART_BM, manually create a new RebootNode CR. For these actions, NVSentinel already did a full drain of the node.
  - For COMPONENT_RESET actions, create a new GPUReset CR that targets the same node and GPU pair. The drain already removed all workload pods that were assigned to the GPU that needs the reset.
- Sometimes the previous remediation action was a GPUReset, and you want to remediate the node with a RebootNode CR. In this case, the node must be fully drained. If partial drain is enabled (`partialDrainEnabled: true`), NVSentinel drained only the pods that were assigned to the GPU that needs the reset. Before you create a RebootNode, do a full drain of the node manually. Use the same steps as in 4a for the `drain-failed` state.
- Sometimes you want to remediate a GPU that is different from the GPU in the initial GPUReset CR. In this case, drain all pods that use that GPU. Otherwise, the reset can fail, because processes can still have a handle on the GPU.

To manually trigger a reboot:

```bash
NODE={NODE_NAME} && cat <<EOF | kubectl apply -f -
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
NODE={NODE_NAME} && GPU_UUID={GPU_UUID} && cat <<EOF | kubectl apply -f -
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

After you create the maintenance CR, monitor the health check to see if it starts to pass. If all health checks of the node pass, NVSentinel uncordons the node automatically. If the health check does not pass, investigate with your organization's support team.

### 6. Handling False Positives

If you think that the health event is a false positive, review the details that you saved earlier:

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

After remediation, monitor the node status:

```bash
# Watch nodes
kubectl get nodes -L dgxc.nvidia.com/nvsentinel-state -w

# Verify node is schedulable and healthy
kubectl get node $NODE_NAME -o wide
```
