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
5. NVSentinel components read these labels and configure themselves accordingly
6. Continuously keeps labels synchronized as infrastructure changes

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
```

### Configuration Options

- **Log Level**: Control logging verbosity (info, debug, warn, error)
- **Kata Label Override**: Specify additional node label to check for Kata Container detection

## Labels Applied

The Labeler applies these labels to nodes:

### DCGM Version
**Label**: `nvsentinel.dgxc.nvidia.com/dcgm.version`
**Values**: `3.x`, `4.x`, or empty if not detected

Indicates which major version of DCGM is running on the node.

### Driver Installed
**Label**: `nvsentinel.dgxc.nvidia.com/driver.installed`
**Values**: `true` or `false`

Indicates whether the NVIDIA driver pod is running on the node.

### Kata Enabled
**Label**: `nvsentinel.dgxc.nvidia.com/kata.enabled`
**Values**: `true` or `false`

Indicates whether the node is running Kata Containers runtime (detected from node labels).

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
