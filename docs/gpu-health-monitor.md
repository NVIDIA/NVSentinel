# GPU Health Monitor

## Overview

The GPU Health Monitor continuously monitors the health of NVIDIA GPUs in your Kubernetes cluster using DCGM (Data Center GPU Manager). It detects GPU hardware issues like ECC errors, thermal problems, and PCIe failures before they impact your workloads.

Think of it as a heart rate monitor for your GPUs - constantly checking vitals and alerting when something goes wrong.

### Why Do You Need This?

GPU failures are expensive and can cause silent data corruption or job crashes:

- **Early detection**: Catch GPU issues before they crash training jobs
- **Prevent data corruption**: Detect ECC errors that could silently corrupt computation results
- **Avoid cascading failures**: Identify problematic GPUs before they impact multi-node workloads
- **Save time**: Automatically detect issues instead of manual investigation

Without GPU monitoring, failures often go unnoticed until jobs crash or produce incorrect results, wasting valuable compute time and requiring manual troubleshooting.

## How It Works

The GPU Health Monitor runs as a DaemonSet on GPU nodes:

1. Connects to DCGM service running on each node
2. Polls DCGM health checks at regular intervals (default: configured polling interval)
3. Detects GPU health issues (ECC errors, thermal problems, PCIe issues, etc.)
4. Sends health events to Platform Connectors via gRPC
5. Maintains state to avoid duplicate event reporting

DCGM provides comprehensive GPU health monitoring including memory errors, thermal violations, PCIe problems, and more. The monitor translates these DCGM health checks into standardized health events that NVSentinel can act upon.

## Configuration

Configure the GPU Health Monitor through Helm values:

```yaml
gpu-health-monitor:
  enabled: true
  
  dcgm:
    dcgmK8sServiceEnabled: true
    service:
      endpoint: "nvidia-dcgm.gpu-operator.svc"  # DCGM service endpoint
      port: 5555
  
  useHostNetworking: false  # Enable if DCGM requires host network access
  
  verbose: "False"  # Enable debug logging
```

### Configuration Options

- **DCGM Service**: Configure DCGM endpoint and port
- **Host Networking**: Enable if DCGM service requires host network access
- **Polling Interval**: Configure how frequently to check GPU health (set via DCGM configuration)
- **Verbose Logging**: Enable detailed debug output

## What It Monitors

### DCGM Health Watches

The monitor checks multiple GPU health aspects through DCGM. Below are some of the key health watches - this is not an exhaustive list and may evolve over time as DCGM capabilities expand:

**Memory Errors**: Single-bit and double-bit ECC errors
**Thermal Issues**: Temperature violations and throttling events
**PCIe Problems**: PCIe replay errors and link issues
**Power Issues**: Power violations and power capping events
**InfoROM Errors**: GPU InfoROM corruption
**NVLink Errors**: NVLink connectivity and error detection

> **Note**: The specific health watches available depend on your DCGM version and GPU model. NVIDIA regularly adds new health checks and monitoring capabilities to DCGM. Consult your DCGM documentation for the complete list of supported health watches for your environment.

## Key Features

### DCGM Integration
Leverages NVIDIA's Data Center GPU Manager for comprehensive GPU health monitoring with proven reliability.

### Automatic State Management
Maintains entity-level cache to track reported issues and avoid sending duplicate events within a single boot session.

### Categorized Health Events
Maps DCGM health checks to categorized events (fatal, warning, info) for appropriate response levels.

### GPU Metadata Enrichment
Includes GPU serial numbers, UUIDs, and other metadata in health events for precise identification and tracking.

### Connectivity Monitoring
Detects and reports DCGM connectivity issues to ensure monitoring remains functional.
