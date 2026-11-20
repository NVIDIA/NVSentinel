# Node Drainer

## Overview

The Node Drainer module is the evacuation coordinator of NVSentinel. When the Fault Quarantine module quarantines nodes because of hardware faults, Node Drainer safely evacuates all running workloads from the affected nodes. Kubernetes then reschedules the workloads on healthy nodes.

Node Drainer acts as an evacuation coordinator. Before repairs on a building start, an evacuation makes sure that everyone exits safely and in an orderly manner. In the same way, Node Drainer makes sure that all your important workloads leave the faulty nodes safely before maintenance or repairs occur.

### Why Do You Need This?

When the Fault Quarantine module cordons a node, the cordon only stops the scheduler from placing *new* workloads there. Existing workloads continue to run on the faulty hardware. This can cause these problems:

- **Continued failures**: Training jobs continue to crash on faulty GPUs
- **Data corruption**: Computation results become unreliable
- **Resource waste**: Other nodes in distributed jobs wait for the slow or faulty node
- **Delayed repairs**: You cannot fix the hardware while workloads still run on it

Node Drainer solves these problems in these ways:

- **Evicts pods gracefully** from quarantined nodes
- **Respects pod disruption budgets** to keep applications available
- **Handles different workload types** with appropriate eviction strategies
- **Gives status updates**, so that you can track the drain progress
- **Works with Kubernetes**, so that Kubernetes reschedules the workloads on healthy nodes
- **Does partial drains**, to drain only the pods that use unhealthy GPUs (when possible)

## How It Works

Node Drainer watches the datastore for quarantined nodes, and safely evacuates their workloads. It does these steps:

1. Receives the health events of quarantined nodes from the datastore
2. Decides if a full drain or a partial drain is necessary
3. Determines the eviction mode from the namespace configuration or pod policies (labels or owner kind)
4. Evicts pods with the Kubernetes Eviction API (respects PodDisruptionBudgets)
5. Monitors the progress, and handles stuck or slow-terminating pods
6. Updates the status when the drain is complete

Node Drainer skips pods in system namespaces. It typically does not evict DaemonSets, because they are system-critical.

## Configuration

Configure the Node Drainer module through Helm values:

```yaml
node-drainer:
  enabled: true
  dryRun: false          # Test mode - logs actions without executing
  
  evictionTimeoutInSeconds: "60"     # Max time to wait for pod termination
  systemNamespaces: "^(nvsentinel|kube-system|gpu-operator)$"  # Namespaces to skip
  deleteAfterTimeoutMinutes: 60      # Force delete after this timeout
  notReadyTimeoutMinutes: 5          # Timeout for stuck pods
  drainGPUPods: false                # Only drain pods requesting GPU
  
  userNamespaces:
    - name: "*"                      # Pattern matching namespaces
      mode: "AllowCompletion"        # Eviction mode

  podDrainPolicies: []               # Alternative to userNamespaces
  
  partialDrainEnabled: true
```

### Eviction Modes

Node Drainer supports three eviction modes for different workload types:

**AllowCompletion**: Wait for pods to terminate gracefully

- Respects the `terminationGracePeriodSeconds` of the pod
- Best for most workloads

**Immediate**: Evict pods immediately without waiting

- Minimal grace period
- Use for stateless workloads

**DeleteAfterTimeout**: Wait for the configured timeout, then force delete

- Waits up to `deleteAfterTimeoutMinutes` from the creation time of the health event
- Force deletes the remaining pods after the timeout
- Use for training jobs that need time to checkpoint

### Configuration Options

- **Dry Run**: Test drain behavior without evicting pods
- **Eviction Timeout**: How long to wait for individual pod eviction (in seconds)
- **System Namespaces**: Regex pattern for namespaces to skip (system pods)
- **Delete Timeout**: Minutes to wait before force deleting pods
- **Not Ready Timeout**: Minutes before Node Drainer considers a pod stuck
- **Drain GPU Pods**: When enabled, drains only pods that request GPU resources. CPU-only workloads stay on the node
- **User Namespaces**: Define eviction mode per namespace pattern (supports `*` wildcard)
- **Pod Drain Policies**: Select eviction modes by pod labels or owner kind. Mutually exclusive with `userNamespaces`
- **Partial Drain**: Enable or disable partial drain functionality

## Key Features

### Namespace or Pod-Based Eviction Modes

Configure how Node Drainer evacuates different workloads:

- **AllowCompletion**: Graceful termination for most workloads
- **Immediate**: Fast eviction for stateless services
- **DeleteAfterTimeout**: Wait for training jobs to checkpoint, then force delete

### Pod Drain Policies

If workloads in the same namespace need different eviction modes, use `podDrainPolicies`. To clear the default namespace rule, set `userNamespaces: []`. If you set both lists, NVSentinel rejects the configuration.

```yaml
node-drainer:
  userNamespaces: []
  podDrainPolicies:
    - name: finish-training
      namespace: training-*
      podSelector: "example.com/drain-mode=finish"
      mode: AllowCompletion
    - name: replaceable-workers
      podSelector: "example.com/drain-mode=immediate"
      mode: Immediate
```

The first matching policy wins. Each policy needs these items:

- a unique name
- a Kubernetes label selector, a list of controller `ownerKinds`, or both
- an eviction mode

The optional `namespace` glob limits where the policy applies. Pods that match no policy are outside the drain scope, and do not block completion. Thus, your selectors must cover every workload that you want Node Drainer to drain.

The exclusions for system namespaces and DaemonSets still apply. GPU-only filtering and the partial GPU drain scope also still apply. Force drain changes the selected pods to `Immediate`, but does not include unmatched pods. You cannot combine custom drain with policies, unless `customDrain.nodeSelector` limits custom drain to part of the cluster.

For selector syntax, owner kinds, label updates and timeout behavior, see [Pod Drain Policies configuration](configuration/node-drainer.md#pod-drain-policies).

### Graceful Eviction

- Uses the Kubernetes Eviction API
- Respects PodDisruptionBudgets
- Honors pod termination grace periods
- Skips system pods automatically

### Timeout Handling

Multiple timeout mechanisms prevent stuck drains:

- Eviction timeout for individual pods
- NotReady timeout for unhealthy pods
- DeleteAfterTimeout for long-running workloads

### Cold Start Recovery

Node Drainer automatically resumes drain operations after restarts. It queries the datastore for drains in progress, and continues from where it stopped.

### Partial Drain Functionality

For GPU faults that a GPU reset can remediate, Node Drainer drains only the selected pods that use the unhealthy GPU. For faults that need a node reboot, it drains all eligible pods on the node. The configured namespace rules or pod policies select these pods.

### GPU-Only Draining

If you set `drainGPUPods: true`, Node Drainer evicts only the workloads that request GPU resources. To detect GPU resources, it uses device annotations from the Metadata Collector. The Metadata Collector tracks GPU allocation across the cluster. Node Drainer also drains a pod without the annotation if a DRA ResourceClaim requests the `gpu.nvidia.com` DeviceClass for it. Default is `false`.
