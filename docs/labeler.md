# Labeler

## Overview

The Labeler enables NVSentinel to self-configure based on the GPU infrastructure in your cluster. It automatically detects what DCGM version, driver, and container runtime are running on each node, then applies labels that allow NVSentinel components to adapt their behavior automatically.

Think of it as auto-configuration for NVSentinel - it detects your environment and configures the system accordingly, so you don't need separate configurations for each cluster.

### Why Do You Need This?

Different clusters have different GPU software configurations:

- **DCGM versions**: Clusters may run DCGM 3.x or 4.x with different APIs
- **Container runtimes**: Some clusters use Kata Containers with different log access patterns
- **Driver variations**: Different driver installation methods across environments

The Labeler enables NVSentinel to automatically adapt:

- **No per-cluster configuration**: Deploy the same Helm chart everywhere
- **Automatic component selection**: Health monitors automatically use the right DCGM API version
- **Runtime adaptation**: Components adjust behavior for Kata Containers vs standard runtime
- **Self-healing**: Labels update automatically when infrastructure changes

Without the Labeler, you'd need to manually configure NVSentinel components differently for each cluster based on what GPU software is installed.

## How It Works

The Labeler runs as a deployment in the cluster:

1. Watches DCGM and NVIDIA driver pods using Kubernetes informers
2. When pods start on a node, examines container images to extract versions
3. Updates node labels with detected versions
4. Watches node labels to detect Kata Container runtime
5. Optionally evaluates configured device-count classes and labels current/expected GPU or NIC counts
6. NVSentinel components read these labels and configure themselves accordingly
7. Continuously keeps labels synchronized as infrastructure changes

For example:
- GPU Health Monitor uses the DCGM version label to select the correct DCGM API version
- Syslog Health Monitor uses the Kata label to adjust log collection methods
- Components automatically adapt without manual reconfiguration

## Configuration

Configure the Labeler through Helm values:

```yaml
labeler:
  enabled: true
  
  logLevel: info
  
  # Optional: Override the default Kata Containers detection label
  kataLabelOverride: ""  # Custom label to check for Kata runtime

  # Optional: Enable current/expected device-count labels
  expectedDeviceCounts:
    enabled: false
```

### Configuration Options

- **Log Level**: Control logging verbosity (info, debug, warn, error)
- **Kata Label Override**: Specify additional node label to check for Kata Container detection
- **Expected Device Counts**: Configure device-count classes that derive current and expected count labels from node labels or DRA ResourceSlices

## Labels Applied

The Labeler applies these labels to nodes:

### DCGM Version
**Label**: `nvsentinel.dgxc.nvidia.com/dcgm.version`
**Values**: `3.x`, `4.x`, or empty if not detected

Indicates which major version of DCGM is running on the node.

### Driver Installed
**Label**: `nvsentinel.dgxc.nvidia.com/driver.installed`
**Values**: `true` or `false`

Indicates whether the NVIDIA driver is installed on the node. The Labeler detects drivers in three tiers, in order:
1. `nvidia-driver-daemonset` pods (GPU Operator managed)
2. `nvidia-driver-installer` pods in `kube-system` (GKE managed)
3. GPU Feature Discovery node labels, when no driver DaemonSet schedules onto the node and the node has no driver pod

In GKE clusters with pre-installed drivers, the `nvidia-driver-daemonset` is not deployed. Instead, Google manages driver installation through `nvidia-driver-installer` DaemonSet pods. The Labeler watches these pods to detect driver installation in GKE environments with pre-installed drivers.

On platforms where the driver ships in the node image and the GPU Operator runs with `driver.enabled=false` — such as AKS with the "Driver only" node image, GKE on Container-Optimized OS, and OKE — no driver pod exists at all. In that case the Labeler falls back to the labels GPU Feature Discovery publishes from the host driver, and sets `driver.installed=true` when the node carries both `nvidia.com/gpu.present=true` and a non-empty `nvidia.com/cuda.driver-version.full`.

The fallback is gated on whether a driver DaemonSet schedules onto the node. Whenever a DaemonSet that owns driver pods targets the node, pod readiness remains authoritative and this tier never fires.

A driver DaemonSet is recognized two ways, because the GPU Operator has two render paths. Its legacy asset hardcodes the `app: nvidia-driver-daemonset` selector, which the configured driver and GKE installer app labels match directly. Its NVIDIADriver CRD path instead generates both the DaemonSet name and the `app` selector value per driver instance (`nvidia-<driverType>-driver-<os>-<hash>`), which no fixed label match can catch; those are recognized by the `app.kubernetes.io/component` label (`nvidia-driver` or `nvidia-vgpu-host-manager`) that both paths put on the pod template.

Targeting is evaluated per node rather than cluster-wide, using the DaemonSet pod template's `nodeSelector` and required `nodeAffinity` against the node, exactly as the upstream DaemonSet controller does. Driver install mode is not always a cluster-wide choice: on GKE it is selected per node pool, so a cluster can run automatic driver installation on one pool while another pool runs Google's manual installer DaemonSet, whose required node affinity excludes the automatically-installed nodes. A cluster-wide check would let that DaemonSet suppress detection on pools it never schedules onto.

DaemonSet existence rather than pod presence is the gate because absence from the pod index is not evidence that no pod source exists. A driver pod is indexed only once it is bound to a node, and the Labeler's pod update handler reacts only to readiness transitions, so the binding of a still-unready replacement pod is not observed. Under the GPU Operator's `OnDelete` update strategy a driver upgrade deletes the driver pod outright, and a replacement that never becomes ready produces no further event. The DaemonSet object is unaffected by that pod churn, so an unready, unloading, deleted or permanently stuck driver pod is never papered over by node labels left behind from the previous driver.

A driver pod without a DaemonSet behind it gates the fallback the same way. The tier also stays inert until every informer cache has synced, since it infers installation from an absence of evidence.

**Limitation.** `nvidia.com/cuda.driver-version.full` is published from an NVML read, so tier 3 keys on a signal of driver *functionality* rather than driver *installation* — the distinction drawn in [design 018](designs/018-syslog-monitor-preinstalled-driver-support.md). On a pre-installed-driver platform where the driver is installed but too broken for GFD to query it, the label is not set and the syslog monitors that would capture those errors do not schedule. Label such nodes manually using the command below. Tiers 1 and 2 are unaffected: they key on pod existence and readiness, not on NVML.

**Note**: For environments where neither a driver pod nor GPU Feature Discovery labels are available (for example, custom machine images with pre-baked drivers and `gfd.enabled=false`), use the following command to manually label the node:

```bash
kubectl label nodes {node-name} nvsentinel.dgxc.nvidia.com/driver.installed=true
```

### Kata Enabled
**Label**: `nvsentinel.dgxc.nvidia.com/kata.enabled`
**Values**: `true` or `false`

Indicates whether the node is running Kata Containers runtime (detected from node labels).

### Expected Device Counts
**Labels**:
- `nvsentinel.dgxc.nvidia.com/gpu.count.current`
- `nvsentinel.dgxc.nvidia.com/gpu.count.expected`
- `nvsentinel.dgxc.nvidia.com/nic.count.current`
- `nvsentinel.dgxc.nvidia.com/nic.count.expected`

**Values**: non-negative integer strings

When enabled, the labeler evaluates configured CEL expressions against the node and associated DRA ResourceSlices. Current labels reflect the observed count. Expected labels come from an override or the maximum learned count among nodes in the same grouping-label partition.

## Key Features

### Self-Configuration
Enables NVSentinel components to automatically adapt to different cluster environments without manual per-cluster configuration.

### Automatic Version Detection
Examines container images of DCGM and driver pods to extract version information - no manual configuration needed.

### Informer-Based Architecture
Uses Kubernetes informers for efficient, real-time monitoring of pod and node changes without polling.

### Kata Container Detection
Detects and labels nodes using Kata Containers runtime by checking node labels (default: `katacontainers.io/kata-runtime`).

### Dynamic Updates
Continuously updates labels as infrastructure changes - handles upgrades, pod moves, and runtime changes automatically.

### DCGM Bootstrap Gating
When `requireDCGMReadyForBootstrap` is enabled, the `nvsentinel.dgxc.nvidia.com/dcgm.version` label is only set after the DCGM pod is ready during the initial bootstrapping of a node. Once the DCGM pod is ready, the annotation `nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed` is written to the node which allows the label to be set regardless of DCGM pod readiness.
