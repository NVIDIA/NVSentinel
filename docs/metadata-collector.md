# Metadata Collector

## Overview

The Metadata Collector collects GPU and NVSwitch topology information from nodes and writes it to a local file. Health monitors, for example the Syslog Health Monitor, use this metadata to correlate errors with specific hardware components.

The Metadata Collector makes an inventory of all GPUs, their connections, and the NVSwitch fabric topology. This information is then available for error analysis and troubleshooting.

In addition to the local topology file, the Metadata Collector also exposes the pod-to-GPU mapping as an annotation on each pod that requests GPUs. Thus, components that run outside the node can find this device mapping through the Kubernetes API.

### Why Do You Need This?

Health monitors need detailed hardware information to create accurate health events:

- **Error correlation**: Map PCI addresses and NVLink IDs to specific GPUs
- **Topology awareness**: Understand GPU interconnect fabric for SXID error analysis
- **Hardware identification**: Track GPU UUIDs, serial numbers, and device names
- **NVSwitch mapping**: Identify which NVSwitches connect which GPUs

Without metadata collection, health monitors can report only generic errors. They cannot identify the specific GPU or NVLink that has the error. Also, the node drainer module needs the pod-to-GPU mapping to find the pods that a health event affects:

- **Partial drains**: For GPU faults that require component resets, the node drainer module uses this mapping to drain only the pods that use that GPU

## How It Works

To collect GPU and NVSwitch topology information, the Metadata Collector does these steps:

1. Initializes NVML (NVIDIA Management Library)
2. Queries GPU information (UUID, PCI address, serial number, device name)
3. Parses the NVLink topology from nvidia-smi
4. Builds the NVSwitch fabric map
5. Writes the metadata to a JSON file

The JSON file persists on the node. Health monitors read it through a shared volume. The collector writes the file again each time it starts. Thus, after a reboot, the file lists the GPUs of the previous boot until the collector runs. The GPU and syslog health monitors read the new copy after the collector writes it.

To create the pod-to-GPU mapping annotation, the Metadata Collector does these steps:

1. To find all pods that run on the node, the Metadata Collector calls the Kubelet /pods HTTPS endpoint.
2. To find the GPU devices that are allocated to each pod, the Metadata Collector uses the Kubelet PodResourcesLister gRPC service:
   - Device-plugin allocations (`nvidia.com/gpu`, `nvidia.com/pgpu`): the collector records them with the device IDs that the plugin reported. For the NVIDIA device plugin, these IDs are GPU UUIDs.
   - DRA allocations (GPU Operator GPUCluster mode, driver `gpu.nvidia.com`): they arrive as device names such as `gpu-1`. The NVIDIA DRA driver derives these names from the GPU minor number (`/dev/nvidia1`). The collector maps them to UUIDs through NVML on the node and writes them under the `gpu.nvidia.com` key. The kubelet reports DRA allocations only with the `KubeletPodResourcesDynamicResources` feature gate. This feature gate is enabled by default from Kubernetes 1.34.
3. If the GPU device allocation of a pod changes, the Metadata Collector updates the tracking annotation on the pod object.
4. The Metadata Collector runs this logic in a loop at a fixed interval, to continually update the mapping for new and existing pods.

## Configuration

Configure the Metadata Collector through Helm values:

```yaml
global:
  metadataCollector:
    enabled: true

metadata-collector:
  # Runtime class for GPU access. Set to "" for CRI-O; see nriPlugin for GPU Operator NRI mode.
  runtimeClassName: "nvidia"
```

### Configuration Options

- **Runtime Class**: Specify the runtime class name for GPU access (usually `"nvidia"` for containerd). Some cluster types need a different setting:
  - CRI-O clusters: set `runtimeClassName: ""` to omit the field. The pod then uses the default runtime of the cluster.
  - GPU Operator NRI-mode clusters (`cdi.nriPluginEnabled: true`): set `nriPlugin.enabled: true`. The DaemonSet then requests a management CDI device from the NRI plugin and needs no RuntimeClass.
  - GPU Operator GPUCluster (DRA) clusters: set `global.gpuDraEnabled: true` instead. The DaemonSet then holds a DRA admin-access GPU claim and needs no RuntimeClass (see [Metadata Collector Configuration](./configuration/metadata-collector.md)).
- **Output Path**: Path where the collector writes the metadata JSON (default: `/var/lib/nvsentinel/gpu_metadata.json`)
- **Host Authentication**: Use `--kubeconfig` for Kubernetes API access and `--kubelet-kubeconfig` for kubelet HTTPS access. For credentials, TLS, and permissions, see [host-native authentication](./configuration/metadata-collector.md#host-native-authentication). Hardware inventory and pod-to-GPU mapping stay enabled.

## What It Collects

The Metadata Collector collects this information:

### GPU Information

- GPU UUID (unique identifier)
- PCI address
- Serial number
- Device name/model
- GPU index

### Pod Information

- GPU UUIDs allocated to each pod

### NVLink Topology

- NVLink connections between GPUs
- Remote GPU endpoints for each link
- Link status and capability
- Peer-to-peer connectivity map

### NVSwitch Information

- NVSwitch PCI addresses
- Which GPUs connect through each switch
- Fabric topology

### System Information

- Node name (hostname)
- Chassis serial number (if available)
- Timestamp of collection

## Key Features

### NVML-Based Collection

Uses the NVIDIA Management Library for direct hardware queries without external dependencies.

### Topology Parsing

Parses the nvidia-smi output to build a complete NVLink topology map that shows the GPU interconnections.

### Shared Volume

Writes metadata to a shared volume that health monitor sidecars can access for error correlation.

### JSON Output

Structured JSON format for simple parsing and use by health monitors.

### Kubernetes API

The Metadata Collector exposes the pod-to-GPU mapping on pod objects as an annotation. External components can read this annotation.
