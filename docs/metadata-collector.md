# Metadata Collector

## Overview

The Metadata Collector collects GPU and NVSwitch topology information from nodes and writes it to a local file. Health monitors, for example the Syslog Health Monitor, use this metadata to correlate errors with specific hardware components.

The Metadata Collector makes an inventory of all GPUs, their connections, and the NVSwitch fabric topology. This information is then available for error analysis and troubleshooting.

The Metadata Collector runs in the `nvidia-metadata-collector` DaemonSet. The same Helm chart also runs the [Pod Device Mapper](pod-device-mapper.md) in its own DaemonSet. The mapper writes the devices of each pod as an annotation on the pod, which partial drains read.

### Why Do You Need This?

Health monitors need detailed hardware information to create accurate health events:

- **Error correlation**: Map PCI addresses and NVLink IDs to specific GPUs
- **Topology awareness**: Understand GPU interconnect fabric for SXID error analysis
- **Hardware identification**: Track GPU UUIDs, serial numbers, and device names
- **NVSwitch mapping**: Identify which NVSwitches connect which GPUs

Without metadata collection, health monitors can report only generic errors. They cannot identify the specific GPU or NVLink that has the error.

## How It Works

To collect GPU and NVSwitch topology information, the Metadata Collector does these steps:

1. Initializes NVML (NVIDIA Management Library)
2. Queries GPU information (UUID, PCI address, serial number, device name)
3. Parses the NVLink topology from nvidia-smi
4. Builds the NVSwitch fabric map
5. Writes the metadata to a JSON file

The JSON file persists on the node. Health monitors read it through a shared volume. The collector writes the file again each time it starts. Thus, after a reboot, the file lists the GPUs of the previous boot until the collector runs. The GPU and syslog health monitors read the new copy after the collector writes it.

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

## What It Collects

The Metadata Collector collects this information:

### GPU Information

- GPU UUID (unique identifier)
- PCI address
- Serial number
- Device name/model
- GPU index

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
