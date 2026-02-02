# Syslog Health Monitor

## Overview

The Syslog Health Monitor watches system logs for GPU-related errors that may not be caught by DCGM. It monitors journald/syslog for XID errors, SXID errors (NVSwitch/NVLink errors), and GPU fallen-off-bus events - critical failures that indicate serious GPU, NVSwitch, or driver problems. In addition to failures, it monitors system logs for other GPU-related events such as GPU resets to indicate that a required remediation action has completed. 

Think of it as a log analyzer that reads between the lines - catching GPU and NVSwitch problems recorded in system logs that other monitoring might miss.

### Why Do You Need This?

Some GPU and NVSwitch failures or events manifest in system logs before DCGM can detect them:

- **XID errors**: GPU hardware errors logged by the NVIDIA driver
- **SXID errors**: NVSwitch errors related to NVSwitch and NVLink interconnects
- **GPU fallen off the bus**: GPU became inaccessible to the system
- **GPU Reset**: A GPU reset was executed by nvidia-smi

These errors or events often appear in system logs first and can indicate imminent GPU or fabric failure, making early detection critical for preventing workload disruptions or returning GPUs to service.

## How It Works

The Syslog Health Monitor runs as a DaemonSet on GPU nodes:

1. Reads journald logs from the host system
2. Parses log entries for GPU-related error patterns (XID, SXID, fallen-off-bus, GPU reset)
3. Maintains cursor position to avoid re-processing old logs
4. For XID errors, uses embedded NVIDIA XID Catalog spreadsheet to determine recommended actions
5. Optionally analyzes XID errors via XID analyzer sidecar for custom logic
6. Sends health events to Platform Connectors via gRPC

The monitor maintains persistent state across restarts, ensuring logs are processed exactly once even if the pod is restarted.

## Configuration

Configure the Syslog Health Monitor through Helm values:

```yaml
syslog-health-monitor:
  enabled: true
  
  enabledChecks:
    - SysLogsXIDError       # GPU XID hardware errors and GPU reset events
    - SysLogsSXIDError      # NVSwitch/NVLink SXID errors
    - SysLogsGPUFallenOff   # GPU fallen off the bus
  
  logLevel: info
  
  # Optional XID analyzer for custom error analysis logic
  xidSideCar:
    enabled: false
    image:
      repository: ""
      tag: ""
```

### Configuration Options

- **Enabled Checks**: Select which types of errors to monitor (XID, SXID, fallen-off-bus)
- **Log Level**: Control logging verbosity (info, debug, warn, error)
- **XID Analyzer Sidecar**: Optional sidecar for injecting custom XID analysis logic
- **Polling Interval**: Configure how frequently to check system logs

## What It Monitors

### XID Errors (GPU Hardware Errors)
Critical GPU hardware failures logged by the NVIDIA driver. Uses embedded NVIDIA XID Catalog (Excel spreadsheet) to map XID codes to recommended actions:
- **XID 48**: Double-bit ECC error (memory corruption, requires GPU replacement)
- **XID 64**: Page retirement limit (GPU memory degradation)
- **XID 79**: GPU has fallen off the bus
- Many other XID codes with appropriate remediation actions from NVIDIA's official catalog

### SXID Errors (NVSwitch/NVLink Errors)
NVSwitch errors related to the high-speed NVLink interconnect fabric:
- NVSwitch hardware errors
- NVLink connection failures
- Fabric-level issues affecting multi-GPU communication

### GPU Fallen Off Bus
A GPU became inaccessible to the system - critical failure requiring immediate attention.

### GPU Reset
A GPU was reset by nvidia-smi, indicating that a remediation action for a previous GPU failure has completed.

## Key Features

### Log Parsing with State Persistence
Efficiently parses journald logs for error patterns and maintains cursor position across restarts - ensures logs are processed exactly once.

### Embedded NVIDIA XID Catalog
Uses official NVIDIA XID Error Catalog (Excel spreadsheet) embedded in the binary to determine recommended actions for each XID code - no external dependencies required.

### Custom Logic via Sidecar
Optional XID analyzer sidecar allows injecting custom logic for XID analysis:
- Override default remediation actions
- Add custom error categorization
- Integrate with proprietary error handling systems
- HTTP API for extensibility

When enabled, the sidecar receives XID messages and can return custom recommended actions, allowing you to tailor remediation to your environment without modifying the main monitor.

### Driver Watcher
Separate sidecar monitors NVIDIA driver pod logs for driver-specific issues.

### Configurable Checks
Enable/disable specific check types based on your monitoring needs - monitor only what's relevant to your hardware.
