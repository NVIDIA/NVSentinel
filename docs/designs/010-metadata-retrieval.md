# ADR-010: Metadata Retrieval

## Context

Currently, health events sent by our system contain limited metadata:

**GPU Health Monitor:**
- GPU ID

**Syslog Health Monitor:**
- GPU UUID (not always present)
- PCI ID
- NVSwitch number
- Link number

**Platform Connector:**
- Node name

### Problems

1. **Node Identification**: As the number of clusters grows, distinguishing nodes by name alone becomes difficult. In cloud environments like OCI and AWS, node names are often distinguished by IP addresses, which can repeat across different clusters in the fleet.

2. **Inconsistent Metadata**: Different monitors send different identifiers for the same entity. For example, GPU health monitor sends GPU ID, while syslog health monitor sends PCI ID for XID errors, making correlation challenging.

3. **Limited Rack-Level Context**: In environments with multiple interconnected racks, comparing health events across racks is cumbersome because manual lookups are required to determine which rack a health event originated from.


## Solution

Use a DaemonSet with init container pattern to collect GPU metadata via go-nvml and write to hostPath. Health monitors read JSON file instead of calling nvidia-smi.

### Metadata Classification

We can classify the metadata into two categories:

1. **Node-level information**: Node labels, E/W topology, provider ID, annotations
2. **Entity-level information**: GPU UUID, PCI addresses, device serial numbers, NVLink topology, Chassis serial number

### Approach

Following the separation of concerns in our architecture:
- **Platform Connector**: Handles all Kubernetes-related logic and cluster-level metadata
- **Health Monitors**: Handle node-specific monitoring and read entity-specific metadata from files
- **GPU Metadata Collector DaemonSet**: Collects and persists GPU metadata to hostpath

Since our health monitors are written in different languages (GPU Health Monitor in Python, Syslog Health Monitor in Go), we'll use a **DaemonSet with init container** pattern to collect GPU metadata once and write it to a shared hostpath. This approach is language-agnostic, uses go-nvml directly instead of parsing `nvidia-smi` output, and follows the DaemonSet pattern consistent with other node-level services.

## Architecture

```
    ┌────────────────────────────────────────────────────────┐
    │  GPU Metadata Collector DaemonSet (per node)           │
    │                                                        │
    │  Init Container:                                       │
    │  - Uses go-nvml to collect GPU metadata                │
    │  - Writes to /var/lib/nvsentinel/                      │
    │    - gpu_metadata.json (all GPU info + PCI mapping)    │
    │  - Exits after writing files                           │
    │                                                        │
    │  Main Container: pause (minimal resource usage)        │
    └────────────────────────────────────────────────────────┘
                              │
                              │ (writes to hostPath)
                              ▼
                    ┌─────────────────────┐
                    │  Host Path Mount    │
                    │                     │
                    │  /var/lib/nvsentinel│
                    │                     │
                    └─────────────────────┘
                              │
                              │ (mounted read-only)
                ┌─────────────┴─────────────┐
                │                           │
                ▼                           ▼
    ┌─────────────────────┐     ┌─────────────────────┐
    │ Syslog Health       │     │ GPU Health Monitor  │
    │ Monitor             │     │ (Python)            │
    │ (Go)                │     │                     │
    │                     │     │                     │
    │ Input: dmesg/syslog │     │ Input: DCGM         │
    │ (has PCI ID)        │     │                     │
    │                     │     │                     │
    │ Reads from:         │     │ Reads from:         │
    │ /var/lib/nvsentinel/│     │ /var/lib/nvsentinel/│
    │                     │     │                     │
    │ Enriches with:      │     │ Enriches with:      │
    │ - GPU UUID          │     │ - GPU UUID          │
    │   (lookup via PCI)  │     │ - PCI addresses     │
    │ - Chassis serial    │     │ - Chassis serial    │
    └─────────────────────┘     └─────────────────────┘
                │                           │
                │ (health events)           │ (health events)
                └─────────────┬─────────────┘
                              │
                              ▼
                    ┌─────────────────────┐
                    │ Platform Connector  │
                    │                     │
                    │ Augments with:      │
                    │ - Other labels      │─────────┐
                    │ - Network topology  │         │
                    │ - Provider ID       │         │
                    └─────────────────────┘         │
                                                    │
                                          ┌─────────▼───────────┐
                                          │ Kubernetes API      │
                                          │ Server              │
                                          │                     │
                                          │ Provides:           │
                                          │ - Node labels       │
                                          │ - Node annotations  │
                                          │ - Provider ID       │
                                          └─────────────────────┘
```

**Data Flow:**

1. **GPU Metadata Collector DaemonSet** (runs on each GPU node):
   - **Init Container**:
     - Runs on pod startup (scheduled only once GPU driver is ready via node selector)
     - Uses `go-nvml` library to collect comprehensive GPU metadata
     - Writes single JSON file to `/var/lib/nvsentinel/` (hostPath mount):
       - `gpu_metadata.json` - Complete GPU information (ID, UUID, PCI address, serial number, NVLink topology)
     - Exits after successful write
   - **Main Container**:
     - Simple `pause` container
     - Keeps pod alive with minimal resource usage (~1Mi memory)

2. **Health Monitors** (Syslog & GPU):
   - Mount `/var/lib/nvsentinel/` as **read-only** volume from host
   - Detect hardware issues from their respective data sources (dmesg/syslog or DCGM)
   - Read `gpu_metadata.json` lazily when needed
   - **Syslog Health Monitor** (Go):
     - For XID errors: Has GPU PCI ID from syslog (e.g., `0000:1b:00.0`), looks up GPU UUID
     - For SXID errors: Has NVSwitch PCI + Link ID, uses `nvlinks` array to find which GPU is connected
     - Reads `chassis_serial` from root level if available
     - Can check `nvswitches` to determine if NVSwitches are present
   - **GPU Health Monitor** (Python):
     - Has GPU ID from DCGM (e.g., `0`)
     - Looks up GPU UUID and PCI address from `gpus[gpu_id]`
     - Reads `chassis_serial` from root level if available
   - Send enriched health events to Platform Connector

3. **Platform Connector**:
   - Receives health events from all monitors (already enriched with chassis serial if available)
   - Queries Kubernetes API Server for node-level metadata (labels, annotations)
   - Augments events with **cluster-level context**:
     - **Network Topology** (from node labels):
       - Availability Zone / Region
       - East/West network segment
     - **Provider ID** (from node spec):
       - Cloud provider node identifier (e.g., AWS instance ID, GCP node ID, OCI instance OCID)
       - Useful for correlating with cloud provider APIs
   - Forwards complete, enriched events downstream

### Benefits

- Language agnostic (any language can read JSON files)
- Zero network overhead (direct file system reads)
- Minimal resource usage (~1Mi per node)
- Simple and reliable (no API server to manage)

## File Format

`/var/lib/nvsentinel/gpu_metadata.json`:

```json
{
  "version": "1.0",
  "timestamp": "2025-11-04T10:30:00Z",
  "node_name": "gpu-node-01",
  "chassis_serial": "GB200-CHASSIS-001",
  "gpus": [
    {
      "gpu_id": 0,
      "uuid": "GPU-faf06843-eefc-ed13-aa08-0c3e7c442f07",
      "pci_address": "0000:1b:00.0",
      "serial_number": "1234567890",
      "device_name": "NVIDIA A100-SXM4-80GB",
      "memory_total_mib": 81920,
      "nvlinks": [
        {
          "link_id": 0,
          "remote_pci_address": "00000008:00:00.0",
          "remote_link_id": 29
        }
      ]
    }
  ],
  "nvswitches": {
    "00000005:00:00.0": {},
    "00000008:00:00.0": {}
  }
}
```

**Fields:**
- `chassis_serial`: Optional, null for non-chassis systems (e.g., H100)
- `nvlinks`: Array per GPU containing topology information. Each entry has:
  - `link_id`: Local GPU link number
  - `remote_pci_address`: NVSwitch PCI address
  - `remote_link_id`: Remote port on NVSwitch
- `nvswitches`: Object at root level listing all unique NVSwitch PCI addresses found in topology
- Empty `nvlinks` array if no NVLinks present (e.g., single GPU systems)

### Usage Examples

**XID Error - Lookup GPU UUID from PCI Address:**
```go
pciAddress := "0000:1b:00.0"
for _, gpu := range metadata.GPUs {
    if gpu.PCIAddress == pciAddress {
        return gpu.UUID
    }
}
```

**SXID Error - Find GPU from NVSwitch PCI + Link:**
```go
nvswitchPCI := "00000008:00:00.0"
nvswitchLink := 29
for _, gpu := range metadata.GPUs {
    for _, nvlink := range gpu.NVLinks {
        if nvlink.RemotePCIAddress == nvswitchPCI && 
           nvlink.RemoteLinkID == nvswitchLink {
            return gpu.UUID, nvlink.LinkID
        }
    }
}
```

**DCGM - Lookup from GPU ID:**
```python
gpu_id = 0
gpu_uuid = metadata["gpus"][gpu_id]["uuid"]
pci_address = metadata["gpus"][gpu_id]["pci_address"]
chassis_serial = metadata.get("chassis_serial")
```

## Deployment

```yaml
DaemonSet:
  nodeSelector:
    nvidia.com/gpu.present: "true"
    nvsentinel.dgxc.nvidia.com/driver.installed: "true"
  volumes:
    - hostPath: /var/lib/nvsentinel/
  initContainers:
    - image: ghcr.io/nvidia/nvsentinel/metadata-collector:v1
      resources: {cpu: 100m, memory: 128Mi}
  containers:
    - image: gcr.io/google_containers/pause:3.9
      resources: {cpu: 10m, memory: 1Mi}
```
