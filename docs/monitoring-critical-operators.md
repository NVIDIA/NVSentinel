# Monitoring Critical Operators (DaemonSet Pods in gpu-operator & network-operator Namespace)

## Overview
For an NVIDIA GPU cluster to function correctly, critical infrastructure components under **gpu-operator** and **network-operator** namespaces must be healthy. If these operators fail, the underlying hardware cannot be utilized effectively.

NVSentinel provides a built-in mechanism to monitor these operators and report health events when their pods are not running correctly.

## Configuration
To monitor the GPU and Network operators, you must enable the `kubernetes-object-monitor` component and define the monitoring policies in your NVSentinel `values.yaml`.

These policies monitor **DaemonSet pods** in the `gpu-operator` and `network-operator` namespaces. A health event is generated if a DaemonSet pod:
- Has been assigned to a node, AND
- Is unhealthy: either **not** in `Running`/`Succeeded` state, OR has a container in `CrashLoopBackOff`

The policy detects pods that are genuinely stuck in any non-progressing state such as:
- Stuck in init container execution (pod phase is `Pending`)
- `Pending` due to resource constraints
- `CrashLoopBackOff` in main containers (pod phase is `Running` but container is crashing)
- `CrashLoopBackOff` in init containers (pod phase is `Pending`)
- `ImagePullBackOff` errors
- Any other state preventing the pod from becoming healthy

### Pod Health Tracking (DaemonSet Only)
The policies track individual pods owned by daemonsets by name. When a pod's health state changes:
- **Pod becomes unhealthy** → Node is cordoned
- **Pod becomes healthy** → Node is uncordoned
- **Pod is deleted** → Node is uncordoned (if a replacement pod comes up unhealthy, it will re-cordon the node)

This approach ensures that:
- Healthy pods always result in uncordoned nodes
- Multiple unhealthy pods on the same node are tracked independently
- Each pod must become healthy (or be deleted) for the node to be uncordoned

Add the following configuration to your `values.yaml`:

```yaml
# 1. Enable the component
global:
  kubernetesObjectMonitor:
    enabled: true

# 2. Configure the policies
kubernetes-object-monitor:
  maxConcurrentReconciles: 1
  resyncPeriod: 5m
  policies:
    # Policy 1: Monitor GPU Operator DaemonSet Pods
    - name: gpu-operator-pods-health
      enabled: true
      resource:
        group: ""
        version: v1
        kind: Pod
      predicate:
        # Trigger event if:
        # 1. Pod is in gpu-operator namespace
        # 2. Pod is owned by a DaemonSet (we only monitor DaemonSet pods)
        # 3. Pod has been assigned to a node (nodeName is set)
        # 4. Pod is unhealthy: either NOT in Running/Succeeded state OR in CrashLoopBackOff
        # 5. Pod has been running for at least the configured threshold (grace period)
        #
        # Note: CrashLoopBackOff pods have phase=Running but container is in Waiting state
        # with reason=CrashLoopBackOff, so we must check containerStatuses explicitly.
        expression: |
          resource.metadata.namespace == 'gpu-operator' && 
          has(resource.metadata.ownerReferences) &&
          resource.metadata.ownerReferences.exists(r, r.kind == 'DaemonSet') &&
          has(resource.spec.nodeName) && resource.spec.nodeName != "" &&
          has(resource.status.startTime) &&
          now - timestamp(resource.status.startTime) > duration('30m') &&
          (
            (resource.status.phase != 'Running' && resource.status.phase != 'Succeeded') ||
            (
              has(resource.status.containerStatuses) &&
              resource.status.containerStatuses.exists(cs,
                has(cs.state.waiting) && 
                has(cs.state.waiting.reason) && 
                cs.state.waiting.reason == 'CrashLoopBackOff'
              )
            )
          )
      nodeAssociation:
        expression: resource.spec.nodeName
      healthEvent:
        componentClass: Software
        isFatal: true
        message: "GPU Operator DaemonSet pod is not healthy"
        recommendedAction: CONTACT_SUPPORT
        errorCode:
          - GPU_OPERATOR_POD_UNHEALTHY

    # Policy 2: Monitor Network Operator DaemonSet Pods
    - name: network-operator-pod-health
      enabled: true
      resource:
        group: ""
        version: v1
        kind: Pod
      predicate:
        expression: |
          resource.metadata.namespace == 'network-operator' && 
          has(resource.metadata.ownerReferences) &&
          resource.metadata.ownerReferences.exists(r, r.kind == 'DaemonSet') &&
          has(resource.spec.nodeName) && resource.spec.nodeName != "" &&
          has(resource.status.startTime) &&
          now - timestamp(resource.status.startTime) > duration('30m') &&
          (
            (resource.status.phase != 'Running' && resource.status.phase != 'Succeeded') ||
            (
              has(resource.status.containerStatuses) &&
              resource.status.containerStatuses.exists(cs,
                has(cs.state.waiting) && 
                has(cs.state.waiting.reason) && 
                cs.state.waiting.reason == 'CrashLoopBackOff'
              )
            )
          )
      nodeAssociation:
        expression: resource.spec.nodeName
      healthEvent:
        componentClass: Software
        isFatal: true
        message: "Network Operator DaemonSet pod is not healthy"
        recommendedAction: CONTACT_SUPPORT
        errorCode:
          - NETWORK_OPERATOR_POD_UNHEALTHY
```

## Detection Logic

The policy triggers when **all** of the following conditions are true:

| Condition       | Check                                                                  |
|:----------------|:-----------------------------------------------------------------------|
| Namespace       | Pod is in `gpu-operator` or `network-operator` namespace               |
| DaemonSet owned | Pod has a DaemonSet owner reference                                    |
| Node assigned   | Pod has `spec.nodeName` set (scheduled to a node)                      |
| Time threshold  | Pod has been running for more than the configured threshold            |
| Unhealthy       | Pod phase is NOT `Running`/`Succeeded` OR container in CrashLoopBackOff |

> **Note:** Only DaemonSet pods are monitored. Pods owned by ReplicaSets, Deployments, Jobs, or standalone pods are not monitored by these policies. This is because DaemonSet pods are the critical infrastructure components that affect GPU node health.

### What This Catches

| Stuck State                            | Pod Phase   | Detected?                     |
|:---------------------------------------|:------------|:------------------------------|
| Stuck in init containers               | `Pending`   | Yes (phase check)             |
| Init container `CrashLoopBackOff`      | `Pending`   | Yes (phase check)             |
| Main container `CrashLoopBackOff`      | `Running`   | Yes (containerStatuses check) |
| `Pending` (scheduling/resource issues) | `Pending`   | Yes (phase check)             |
| `ImagePullBackOff` / `ErrImagePull`    | `Pending`   | Yes (phase check)             |
| `Failed` phase                         | `Failed`    | Yes (phase check)             |
| Normal initialization (< threshold)    | Any         | No (grace period)             |
| Healthy pod                            | `Running`   | No (healthy)                  |
| Completed job                          | `Succeeded` | No (completed)                |


## Pod Tracking Behavior

The kubernetes-object-monitor tracks each pod individually by name. This simple approach provides clear and predictable behavior:

### Scenario: Pod Becomes Unhealthy

1. **Pod enters unhealthy state** (e.g., init container stuck, CrashLoopBackOff) → Node is cordoned after threshold
2. **Pod becomes healthy** (Running with all containers ready) → Node is uncordoned

### Scenario: Main Container CrashLoopBackOff

1. **Container crashes repeatedly** → Pod phase stays `Running`, but container enters `CrashLoopBackOff`
2. **Policy detects via containerStatuses check** → Node is cordoned
3. **Container is fixed and becomes healthy** → Node is uncordoned

### Scenario: Pod Deletion

1. **Pod fails** → Node is cordoned
2. **Admin deletes the pod** → Node is **uncordoned**
3. **Replacement pod is created** → If unhealthy, node is re-cordoned after threshold

### Scenario: Multiple Unhealthy Pods

1. **Pod A fails** → Node is cordoned
2. **Pod B also fails** → Both tracked in annotation
3. **Pod A becomes healthy** → Node **stays cordoned** (Pod B still unhealthy)
4. **Pod B becomes healthy** → Node is uncordoned

### State Key Format

The monitor uses a simple state key format: `policyName/namespace/podName`

This ensures each pod is tracked independently, and the node is only uncordoned when all tracked pods are healthy or deleted.

## Configuration Options

### Adjusting the Time Threshold
You can adjust the `30m` (30 minutes) threshold based on your environment:
- `duration('10m')` - 10 minutes (more aggressive, may cause false positives for slow image pulls)
- `duration('1h')` - 1 hour (more lenient, delays detection of stuck pods)

Choose a value that exceeds your longest expected pod initialization time.

### How the Time Check Works
The `now - timestamp(resource.status.startTime) > duration('30m')` expression:
- `now` - Current timestamp (provided by the CEL environment)
- `timestamp(resource.status.startTime)` - When the pod was created
- `duration('30m')` - The threshold duration (30 minutes)

### Resync Period
The `resyncPeriod` controls how often the monitor re-evaluates all resources:
- Default: `5m` (5 minutes)
- For faster detection, reduce to `1m` or `30s`
- Trade-off: Lower values increase API server load

```yaml
kubernetes-object-monitor:
  resyncPeriod: 1m  # Re-evaluate every minute
```

## Troubleshooting

### Investigating Health Events
If you receive these events, investigate the pod status:

1.  **Check pod status and events:**
    ```bash
    kubectl get pods -n gpu-operator -o wide
    kubectl describe pod <pod-name> -n gpu-operator
    ```

2.  **Check container logs (if containers have started):**
    ```bash
    kubectl logs -n gpu-operator <pod-name>
    # For init containers:
    kubectl logs -n gpu-operator <pod-name> -c <init-container-name>
    ```

3.  **Common issues to look for:**
    - `ImagePullBackOff` - Check image name and registry credentials
    - `CrashLoopBackOff` - Check container logs for crash reason
    - `Pending` - Check node resources and scheduling constraints
    - Init container stuck - Check init container logs

4.  **Verify node health:**
    ```bash
    kubectl get node <node-name>
    kubectl describe node <node-name>
    ```

### Common Issues


#### Problem: Pods are not being monitored (no health events)

1. **Check if the pod matches the policy predicate:**
   ```bash
   kubectl get pod <pod-name> -n <namespace> -o yaml
   ```
   Verify the pod is in the correct namespace and meets all predicate conditions.

2. **Check kubernetes-object-monitor logs:**
   ```bash
   kubectl logs -n nvsentinel deployment/kubernetes-object-monitor
   ```

3. **Verify the policy is enabled in your configuration.**

#### Problem: Node stays cordoned after pod is healthy

1. **Check if the DaemonSet still targets the node:**
   ```bash
   kubectl get ds <daemonset-name> -n <namespace> -o yaml | grep -A 10 nodeSelector
   ```

2. **Check the policy match annotation on the node:**
   ```bash
   kubectl get node <node-name> -o jsonpath='{.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/policy-matches}'
   ```
