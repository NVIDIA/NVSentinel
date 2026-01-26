# Node Drainer

## Overview

The Node Drainer module is NVSentinel's evacuation coordinator. When nodes are quarantined due to hardware faults, this module safely evacuates all running workloads from the affected nodes, moving them to healthy ones. 

Think of it as an evacuation coordinator - similar to how a building evacuation ensures everyone exits safely and in an orderly manner before repairs begin, Node Drainer ensures all your important workloads are safely moved off faulty nodes before maintenance or repairs take place.

### Why Do You Need This?

When the Fault Quarantine module cordons a node, it only prevents *new* workloads from being scheduled there. Existing workloads continue running on the faulty hardware, which can lead to:

- **Continued failures**: Training jobs keep crashing on faulty GPUs
- **Data corruption**: Computation results become unreliable
- **Resource waste**: Other nodes in distributed jobs wait for the slow/faulty node
- **Delayed repairs**: Hardware can't be fixed while workloads are still running

The Node Drainer module solves this by:
- **Gracefully evicting pods** from quarantined nodes
- **Respecting pod disruption budgets** to maintain application availability  
- **Handling different workload types** with appropriate eviction strategies
- **Providing status updates** so you can track drain progress
- **Working with Kubernetes** to ensure workloads are rescheduled on healthy nodes
- **Executing partial drains** to only drain pods using unhealthy GPUs (when possible)

## How It Works

The Node Drainer watches the datastore for quarantined nodes and safely evacuates their workloads:

1. Receives quarantined node events from the datastore
2. Determines if a full drain or a partial drain needs to be executed
3. Determines eviction mode based on namespace configuration
4. Evicts pods using Kubernetes Eviction API (respects PodDisruptionBudgets)
5. Monitors progress and handles stuck or slow-terminating pods
6. Updates status when complete

System namespace pods are skipped. DaemonSets are typically not evicted as they're system-critical.

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
  
  userNamespaces:
    - name: "*"                      # Pattern matching namespaces
      mode: "AllowCompletion"        # Eviction mode
  
  partialDrainEnabled: true
```

### Eviction Modes

The module supports three eviction modes for different workload types:

**AllowCompletion**: Wait for pods to terminate gracefully
- Respects pod's `terminationGracePeriodSeconds`
- Best for most workloads

**Immediate**: Evict pods immediately without waiting
- Minimal grace period
- Use for stateless workloads

**DeleteAfterTimeout**: Wait for configured timeout, then force delete
- Waits up to `deleteAfterTimeoutMinutes` from event creation time
- Force deletes remaining pods after timeout
- Use for training jobs that need time to checkpoint

### Configuration Options

- **Dry Run**: Test drain behavior without evicting pods
- **Eviction Timeout**: How long to wait for individual pod eviction (in seconds)
- **System Namespaces**: Regex pattern for namespaces to skip (system pods)
- **Delete Timeout**: Minutes to wait before force deleting pods
- **Not Ready Timeout**: Minutes before considering a pod stuck
- **User Namespaces**: Define eviction mode per namespace pattern (supports `*` wildcard)
- **Partial Drain**: Enable or disable partial drain functionality

## Key Features

### Namespace-Based Eviction Modes
Configure how different workloads are evacuated:
- **AllowCompletion**: Graceful termination for most workloads
- **Immediate**: Fast eviction for stateless services
- **DeleteAfterTimeout**: Wait for training jobs to checkpoint, then force delete

### Graceful Eviction
- Uses Kubernetes Eviction API
- Respects PodDisruptionBudgets
- Honors pod termination grace periods
- System pods automatically skipped

### Timeout Handling
Multiple timeout mechanisms prevent stuck drains:
- Eviction timeout for individual pods
- NotReady timeout for unhealthy pods
- DeleteAfterTimeout for long-running workloads

### Cold Start Recovery
Automatically resumes drain operations after restarts - queries datastore for in-progress drains and continues from where it left off.

### Partial Drain Functionality
For GPU faults that can be remediated with a GPU reset, the Node Drainer will only drain pods which are leveraging the unhealthy GPU. For GPU faults that require a node reboot, all pods on the given node in the configured namespaces will be drained.