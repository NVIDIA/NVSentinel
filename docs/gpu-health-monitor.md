# GPU Health Monitor

## Overview

The GPU Health Monitor uses DCGM (Data Center GPU Manager) to monitor the health of the NVIDIA GPUs in your Kubernetes cluster continuously. It detects GPU hardware faults, for example ECC errors, thermal problems and PCIe failures, before they affect your workloads.

The GPU Health Monitor examines the GPUs continuously, and it sends a health event when it finds a fault.

### Why Do You Need This?

GPU failures are expensive and can cause silent data corruption or job crashes:

- **Early detection**: Detect GPU faults before they crash training jobs.
- **Prevent data corruption**: Detect ECC errors that can silently corrupt computation results.
- **Avoid cascading failures**: Identify faulty GPUs before they affect multi-node workloads.
- **Save time**: Detect faults automatically, without manual investigation.

Without GPU monitoring, you often do not see a failure until a job crashes or produces incorrect results. This wastes valuable compute time and makes manual troubleshooting necessary.

## How It Works

The GPU Health Monitor runs as a DaemonSet on GPU nodes. It does these steps:

1. Connects to the DCGM service that runs on each node.
2. Polls the DCGM health checks at a regular interval. The `gpu-health-monitor.dcgm.pollIntervalSeconds` Helm value sets this interval (default: 15 seconds).
3. Detects GPU faults, for example ECC errors, thermal problems and PCIe issues.
4. Sends health events to Platform Connectors through gRPC.
5. Keeps state, so that it does not report duplicate health events.

DCGM monitors many areas of GPU health, for example memory errors, thermal violations and PCIe problems. The GPU Health Monitor converts the results of these DCGM health checks to standard health events. NVSentinel can act on these health events.

## Configuration

Configure the GPU Health Monitor through Helm values:

```yaml
global:
  gpuHealthMonitor:
    enabled: true
  dcgm:
    service:
      endpoint: "nvidia-dcgm-dra.gpu-operator.svc,nvidia-dcgm.gpu-operator.svc"  # DCGM service hosts
      port: 5555

gpu-health-monitor:
  useHostNetworking: false  # Enable if DCGM requires host network access
  
  verbose: "False"  # Enable debug logging
```

### Configuration Options

- **DCGM Service**: Configure the DCGM endpoint and port.
- **Host Networking**: If the DCGM service requires host network access, enable this option.
- **Polling Interval**: Configure how frequently the GPU Health Monitor checks GPU health. To set this interval, use the `gpu-health-monitor.dcgm.pollIntervalSeconds` Helm value. The default is 15 seconds.
- **Verbose Logging**: Enable the detailed debug output.
- **Incident Suppression**: Drop specific DCGM error codes before they generate a health event. For more information, see [DCGM Health Check Incident Suppression](configuration/gpu-health-monitor.md#dcgm-health-check-incident-suppression).
- **Probe Watchdog**: Report a DCGM probe that hangs instead of returning an error. For more information, see [Unresponsive DCGM Detection](configuration/gpu-health-monitor.md#unresponsive-dcgm-detection).

## What It Monitors

### DCGM Health Watches

The GPU Health Monitor checks several areas of GPU health through DCGM. The list that follows is not complete, and it can change when DCGM adds new capabilities. Some of the key health watches are:

- **Memory Errors**: Single-bit and double-bit ECC errors
- **Thermal Issues**: Temperature violations and throttling events
- **PCIe Problems**: PCIe replay errors and link issues
- **Power Issues**: Power violations and power capping events
- **InfoROM Errors**: GPU InfoROM corruption
- **NVLink Errors**: NVLink connectivity and error detection

> **Note**: The available health watches depend on your DCGM version and GPU model. NVIDIA regularly adds new health checks and monitoring capabilities to DCGM. For the complete list of health watches that your environment supports, refer to your DCGM documentation.

## Key Features

### DCGM Integration

The GPU Health Monitor uses NVIDIA Data Center GPU Manager (DCGM) to monitor GPU health.

### Automatic State Management

The GPU Health Monitor keeps a cache for each entity, which tracks the issues that it reported. This cache prevents duplicate health events in one boot session.

### Categorized Health Events

The GPU Health Monitor maps the result of each DCGM health check to a fatal or a non-fatal health event. A health event is fatal when the recommended action for its DCGM error code is not `NONE`. NVSentinel uses this classification to select the level of response.

### GPU Metadata Enrichment

The GPU Health Monitor adds the chassis serial number, GPU UUIDs and other metadata to health events. With this metadata, you can identify and track the hardware precisely.

### Connectivity Monitoring

The GPU Health Monitor detects and reports DCGM connectivity issues to make sure that the monitoring stays functional.

### Unresponsive DCGM Detection

The GPU Health Monitor detects a DCGM call that stops answering instead of returning an error:

- In embedded mode, it reports the node-local hang as `GpuDcgmUnresponsive`. By default, this check is observe-only.
- In remote modes, it reports a connectivity failure and does not recommend a node reboot.

For more information, see [Unresponsive DCGM Detection](configuration/gpu-health-monitor.md#unresponsive-dcgm-detection).
