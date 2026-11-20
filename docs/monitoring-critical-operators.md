# Monitoring Critical Operators (DaemonSet Pods in gpu-operator & network-operator Namespace)

## Overview

For an NVIDIA GPU cluster to operate correctly, the critical infrastructure components in the **gpu-operator** and **network-operator** namespaces must be healthy. If these operators fail, the cluster cannot use the underlying hardware effectively.

NVSentinel has a built-in mechanism that monitors these operators and reports health events when their pods do not run correctly.

## Configuration

To monitor the GPU and Network operators, enable the `kubernetes-object-monitor` component in your NVSentinel `values.yaml`. Then define the monitoring policies in the same file.

These policies monitor **DaemonSet pods** in the `gpu-operator` and `network-operator` namespaces. The kubernetes-object-monitor generates a health event if a DaemonSet pod:

- Is assigned to a node, AND
- Has a `status.startTime` that is older than the configured threshold (`30m` in the example policies), AND
- Is unhealthy: either **not** in `Running`/`Succeeded` state, OR has a container in `CrashLoopBackOff`

The policy detects pods that stay stuck in any non-progressing state, for example:

- Stuck in init container execution (pod phase is `Pending`)
- `Pending` because of resource constraints
- `CrashLoopBackOff` in main containers (pod phase is `Running`, but the container crashes)
- `CrashLoopBackOff` in init containers (pod phase is `Pending`)
- `ImagePullBackOff` errors
- Any other state that prevents the pod from becoming healthy

### Pod Health Tracking (DaemonSet Only)

The policies track each pod that a DaemonSet owns, by the name of the pod. When the health state of a pod changes:

- **Pod becomes unhealthy** → NVSentinel cordons the node
- **Pod becomes healthy** → NVSentinel uncordons the node
- **Pod is deleted** → NVSentinel uncordons the node (if a replacement pod starts and is unhealthy, NVSentinel cordons the node again)

This approach ensures that:

- Healthy pods always result in uncordoned nodes
- The policies track multiple unhealthy pods on the same node independently
- Each pod must become healthy (or be deleted) before NVSentinel uncordons the node

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
| Time threshold  | Time since `status.startTime` is more than the configured threshold    |
| Unhealthy       | Pod phase is NOT `Running`/`Succeeded` OR container in CrashLoopBackOff |

> **Note:** These policies monitor only DaemonSet pods. They do not monitor pods that ReplicaSets, Deployments or Jobs own, or standalone pods. The reason is that DaemonSet pods are the critical infrastructure components that affect GPU node health.

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

The kubernetes-object-monitor tracks each pod individually, by its name. This simple approach gives clear and predictable behavior:

### Scenario: Pod Becomes Unhealthy

1. **Pod enters unhealthy state** (for example, init container stuck, CrashLoopBackOff) → NVSentinel cordons the node after the threshold
2. **Pod becomes healthy** (Running with all containers ready) → NVSentinel uncordons the node

### Scenario: Main Container CrashLoopBackOff

1. **Container crashes repeatedly** → Pod phase stays `Running`, but the container enters `CrashLoopBackOff`
2. **Policy detects it through the containerStatuses check** → NVSentinel cordons the node
3. **Container is fixed and becomes healthy** → NVSentinel uncordons the node

### Scenario: Pod Deletion

1. **Pod fails** → NVSentinel cordons the node
2. **Admin deletes the pod** → NVSentinel **uncordons** the node
3. **Replacement pod is created** → If the replacement pod is unhealthy, NVSentinel cordons the node again after the threshold

### Scenario: Multiple Unhealthy Pods

1. **Pod A fails** → NVSentinel cordons the node
2. **Pod B also fails** → The annotation tracks both pods
3. **Pod A becomes healthy** → The node **stays cordoned** (Pod B is still unhealthy)
4. **Pod B becomes healthy** → NVSentinel uncordons the node

### State Key Format

The monitor uses a simple state key format: `policyName/namespace/podName`

With this format, the monitor tracks each pod independently. NVSentinel uncordons the node only when all tracked pods are healthy or deleted.

## Configuration Options

### Adjusting the Time Threshold

You can adjust the `30m` (30 minutes) threshold for your environment:

- `duration('10m')` - 10 minutes (more aggressive; can cause false positives for slow image pulls)
- `duration('1h')` - 1 hour (more lenient; delays the detection of stuck pods)

Choose a value that is longer than the longest pod initialization time that you expect.

### How the Time Check Works

The `now - timestamp(resource.status.startTime) > duration('30m')` expression has these parts:

- `now` - Current timestamp (provided by the CEL environment)
- `timestamp(resource.status.startTime)` - When the kubelet acknowledged the pod on its node (before the kubelet pulls the container images)
- `duration('30m')` - The threshold duration (30 minutes)

### Resync Period

The `resyncPeriod` controls how often the monitor evaluates all resources again:

- Default: `5m` (5 minutes)
- For faster detection, decrease it to `1m` or `30s`
- Trade-off: Lower values increase the API server load

```yaml
kubernetes-object-monitor:
  resyncPeriod: 1m  # Re-evaluate every minute
```

## Troubleshooting

### Investigating Health Events

If you receive these health events, examine the status of the pod:

1.  **Check pod status and events:**

    ```bash
    kubectl get pods -n gpu-operator -o wide
    kubectl describe pod {pod-name} -n gpu-operator
    ```

2.  **If the containers started, check the container logs:**

    ```bash
    kubectl logs -n gpu-operator {pod-name}
    # For init containers:
    kubectl logs -n gpu-operator {pod-name} -c {init-container-name}
    ```

3.  **Common issues to look for:**

    - `ImagePullBackOff` - Check the image name and the registry credentials
    - `CrashLoopBackOff` - Check the container logs for the crash reason
    - `Pending` - Check the node resources and the scheduling constraints
    - Init container stuck - Check the init container logs

4.  **Verify node health:**

    ```bash
    kubectl get node {node-name}
    kubectl describe node {node-name}
    ```

### Common Issues

#### Problem: Pods are not being monitored (no health events)

1. **Check if the pod matches the policy predicate:**

   ```bash
   kubectl get pod {pod-name} -n {namespace} -o yaml
   ```

   Verify that the pod is in the correct namespace and that it meets all predicate conditions.

2. **Check the kubernetes-object-monitor logs:**

   ```bash
   kubectl logs -n nvsentinel deployment/kubernetes-object-monitor
   ```

3. **Verify that the policy is enabled in your configuration.**

#### Problem: Node stays cordoned after pod is healthy

1. **Check if the DaemonSet still targets the node:**

   ```bash
   kubectl get ds {daemonset-name} -n {namespace} -o yaml | grep -A 10 nodeSelector
   ```

2. **Check the policy match annotation on the node:**

   ```bash
   kubectl get node {node-name} -o jsonpath='{.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/policy-matches}'
   ```
