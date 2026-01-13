# Metadata Collector

## Overview

The Metadata Collector gathers comprehensive GPU and NVSwitch topology information from nodes and writes it to a local file. This metadata is consumed by health monitors (like Syslog Health Monitor) to correlate errors with specific hardware components.

Think of it as a hardware inventory scanner - it catalogs all GPUs, their connections, and NVSwitch fabric topology, making this information available for error analysis and troubleshooting.

In addition to persisting the GPU and NVSwitch topology information from nodes in a local file, the Metadata Collector will also expose the pod-to-GPU mapping as an annotation on each pod requesting GPUs. This allows components running externally to the node to discover this device mapping through the Kubernetes API.

### Why Do You Need This?

Health monitors need detailed hardware information to create accurate health events:

- **Error correlation**: Map PCI addresses and NVLink IDs to specific GPUs
- **Topology awareness**: Understand GPU interconnect fabric for SXID error analysis
- **Hardware identification**: Track GPU UUIDs, serial numbers, and device names
- **NVSwitch mapping**: Identify which NVSwitches connect which GPUs

Without metadata collection, health monitors can only report generic errors without knowing which specific GPU or NVLink is affected. Additionally, the node drainer module needs the pod-to-GPU mapping to determine which set of pods is impacted by a given health event:

- **Partial drains**: For GPU faults requiring component resets, the node drainer module will reference this mapping to only drain pods leveraging that GPU

## How It Works

GPU and NVSwitch topology information collection:

1. Initializes NVML (NVIDIA Management Library)
2. Queries GPU information (UUID, PCI address, serial number, device name)
3. Parses NVLink topology from nvidia-smi
4. Builds NVSwitch fabric map
5. Writes comprehensive metadata to JSON file

The JSON file persists on the node and is read by health monitors via a shared volume.

GPU-to-pod mapping annotation:

1. To discover all pods running on the given node, this component will call the Kubelet /pods HTTPS endpoint.
2. To discover the GPU devices allocated to each pod, this component will leverage the Kubelet PodResourcesLister gRPC service.
3. If any pod has a change in its GPU device allocation, we will update the tracking annotation on the pod object.
4. The Metadata Collector will run this logic in a loop on a fixed threshold to continually update the mapping for new and existing pods.

## Configuration

Configure the Metadata Collector through Helm values:

```yaml
metadata-collector:
  enabled: true
  
  # Runtime class for GPU access (omit for CRI-O environments)
  runtimeClassName: "nvidia"
```

### Configuration Options

- **Runtime Class**: Specify runtime class name for GPU access (typically "nvidia" for containerd). For CRI-O environments, do not set this field.
- **Output Path**: Path where metadata JSON is written (default: `/var/lib/nvsentinel/gpu_metadata.json`)

## What It Collects

The metadata collector gathers:

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
Uses NVIDIA Management Library for reliable, direct hardware queries without external dependencies.

### Topology Parsing
Parses nvidia-smi output to build complete NVLink topology map showing GPU interconnections.

### Shared Volume
Writes metadata to shared volume accessible by health monitor sidecars for error correlation.

### JSON Output
Structured JSON format for easy parsing and consumption by health monitors.

### Kubernetes API
The pod-to-GPU mapping is exposed on pods objects as an annotation which can be consumed by external components.
