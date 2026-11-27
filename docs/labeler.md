# Labeler

## Overview

The Labeler lets NVSentinel configure itself for the GPU infrastructure in your cluster. It automatically detects the DCGM version, the driver and the container runtime on each node. Then it applies labels that let NVSentinel components adapt their behavior automatically.

The Labeler works as auto-configuration for NVSentinel. It detects your environment and configures the system to match. Thus, you do not need a separate configuration for each cluster.

### Why Do You Need This?

Different clusters have different GPU software configurations:

- **DCGM versions**: Clusters can run DCGM 3.x or 4.x, which have different APIs.
- **Container runtimes**: Some clusters use Kata Containers, which have different log access patterns.
- **Driver variations**: Environments use different driver installation methods.

The Labeler lets NVSentinel adapt automatically:

- **No per-cluster configuration**: Deploy the same Helm chart everywhere.
- **Automatic component selection**: Health monitors automatically use the correct DCGM API version.
- **Runtime adaptation**: Components use different behavior for Kata Containers and for the standard runtime.
- **Self-healing**: The Labeler updates labels automatically when the infrastructure changes.

Without the Labeler, you must configure NVSentinel components manually and differently for each cluster, based on the installed GPU software.

## How It Works

The Labeler runs as two Deployments in the cluster. `nvidia-labeler` writes the DCGM and driver labels:

1. Watches DCGM and NVIDIA driver pods with Kubernetes informers.
2. When pods start on a node, examines the container images to get the versions.
3. Updates node labels with the detected versions.

`labeler` writes the vendor-neutral labels. It does not read pods:

1. Watches node labels to detect the Kata Container runtime.
2. Optionally evaluates the configured device-count classes, and labels the current and expected GPU or NIC counts.

NVSentinel components read these labels and configure themselves accordingly. Both Deployments keep the labels synchronized continuously as the infrastructure changes.

For example:

- GPU Health Monitor uses the DCGM version label to select the correct DCGM API version
- Syslog Health Monitor uses the Kata label to adjust log collection methods
- Components automatically adapt without manual reconfiguration

## Configuration

Configure the Labeler through Helm values:

```yaml
global:
  labeler:
    enabled: true

labeler:
  logLevel: info
  
  # Optional: Override the default Kata Containers detection label
  kataLabelOverride: ""  # Custom label to check for Kata runtime

  # Optional: Enable current/expected device-count labels
  expectedDeviceCounts:
    enabled: true
```

### Configuration Options

- **Log Level**: Control the logging verbosity (info, debug, warn or error).
- **Kata Label Override**: Specify an additional node label to check for Kata Container detection.
- **Expected Device Counts**: Configure device-count classes. These classes get the current and expected count labels from node labels or DRA ResourceSlices.

## Labels Applied

The Labeler applies these labels to nodes:

### DCGM Version

**Label**: `nvsentinel.dgxc.nvidia.com/dcgm.version`

**Values**: `3.x`, `4.x`, or empty if not detected

This label shows the major version of DCGM that runs on the node.

### Driver Installed

**Label**: `nvsentinel.dgxc.nvidia.com/driver.installed`

**Values**: `true` or `false`

This label shows whether the NVIDIA driver is installed on the node. To detect drivers, `nvidia-labeler` watches for these pods:

- `nvidia-driver-daemonset` pods (GPU Operator managed)
- `nvidia-driver-installer` pods in `kube-system` (GKE managed)

In GKE clusters with pre-installed drivers, there is no `nvidia-driver-daemonset`. Instead, Google manages driver installation through `nvidia-driver-installer` DaemonSet pods. The Labeler watches these pods to detect driver installation in GKE environments with pre-installed drivers.

**Note**: Some environments have pre-installed drivers and no driver pods. Examples are custom machine images with pre-baked drivers, and cloud node images that include the driver. For these environments, enable the `assumeDriverInstalled` Helm value:

```yaml
labeler:
  assumeDriverInstalled: true
```

With this setting, the Labeler sets `driver.installed=true` on every GPU node (`nvidia.com/gpu.present=true`) that it manages. It skips driver pod detection, and it automatically covers nodes that you add later. It excludes nodes that have the opt-out label `nvsentinel.dgxc.nvidia.com/managed=false`. For example, a node has this label during external remediation.

Do not apply the label manually with `kubectl label`. Instead, enable `assumeDriverInstalled`. Without this setting, the Labeler computes an empty desired value for a node with no driver pod. Then, on its next relevant reconciliation, it silently removes a manually applied `driver.installed` label as stale. For example, these events start a relevant reconciliation:

- any restart of the Labeler
- a readiness change of a driver or DCGM pod on the node
- a change to the node labels that the Labeler watches

Thus, you cannot rely on a manual label to persist.

### Kata Enabled

**Label**: `nvsentinel.dgxc.nvidia.com/kata.enabled`

**Values**: `true` or `false`

This label shows whether the node runs the Kata Containers runtime. The Labeler detects the runtime from node labels.

### Expected Device Counts

**Labels**:

- `nvsentinel.dgxc.nvidia.com/gpu.count.current`
- `nvsentinel.dgxc.nvidia.com/gpu.count.expected`
- `nvsentinel.dgxc.nvidia.com/nic.count.current`
- `nvsentinel.dgxc.nvidia.com/nic.count.expected`

**Values**: non-negative integer strings

When this feature is enabled, the Labeler evaluates the configured CEL expressions against the node and the associated DRA ResourceSlices. The current labels show the observed count. The expected labels come from an override, or from the maximum learned count among nodes in the same grouping partition.

The default GPU class works in both GPU Operator modes without extra configuration. In GPUCluster (DRA) mode, the Labeler counts the GPU devices in the `gpu.nvidia.com` ResourceSlices. In ClusterPolicy mode, the Labeler reads the GFD `nvidia.com/gpu.count` label only while the NVIDIA device plugin advertises GPUs. Thus, in GPUCluster mode, the Labeler does not count a label that remains from ClusterPolicy mode. In DRA mode, the Labeler also groups nodes by their GPU models, which it reads from the ResourceSlices. The [Labeler configuration reference](configuration/labeler.md#expected-device-counts) describes these sources.

## Key Features

### Self-Configuration

The Labeler lets NVSentinel components adapt automatically to different cluster environments, without a manual configuration for each cluster.

### Automatic Version Detection

The Labeler examines the container images of DCGM and driver pods to get the version information. No manual configuration is necessary.

### Informer-Based Architecture

The Labeler uses Kubernetes informers to monitor pod and node changes efficiently and in real time, without polling.

### Kata Container Detection

The Labeler detects and labels nodes that use the Kata Containers runtime. It checks node labels for this detection (default: `katacontainers.io/kata-runtime`).

### Dynamic Updates

The Labeler updates labels continuously as the infrastructure changes. It handles upgrades, pod moves and runtime changes automatically.

### DCGM Bootstrap Gating

When `requireDCGMReadyForBootstrap` is enabled, the Labeler sets the `nvsentinel.dgxc.nvidia.com/dcgm.version` label only after the DCGM pod is ready. This rule applies during the initial bootstrapping of a node. When the DCGM pod is ready, the Labeler writes the annotation `nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed` to the node. After that, the Labeler sets the label regardless of DCGM pod readiness.
