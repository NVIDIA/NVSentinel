# Syslog Health Monitor

## Overview

The Syslog Health Monitor watches system logs for GPU-related errors that DCGM does not always detect. It monitors journald/syslog for XID errors, SXID errors (NVSwitch/NVLink errors), and GPU fallen-off-bus events. These critical failures show serious GPU, NVSwitch, or driver problems. The monitor also watches system logs for other GPU-related events, such as GPU resets. A GPU reset shows that a necessary remediation action is complete.

The monitor operates as a log analyzer. It finds GPU and NVSwitch problems in the system logs that other monitoring can miss.

### Why Do You Need This?

Some GPU and NVSwitch failures or events appear in system logs before DCGM can detect them:

- **XID errors**: GPU hardware errors that the NVIDIA driver logs
- **SXID errors**: NVSwitch errors related to NVSwitch and NVLink interconnects
- **GPU fallen off the bus**: the system cannot access the GPU
- **GPU Reset**: nvidia-smi did a GPU reset

These errors or events often appear in system logs first. They can show that a GPU or fabric failure is about to occur. Thus, early detection is critical to prevent workload disruptions or to return GPUs to service.

## How It Works

The Syslog Health Monitor runs as a DaemonSet on GPU nodes:

1. Reads journald logs from the host system
2. Parses log entries for GPU-related error patterns (XID, SXID, fallen-off-bus, GPU reset)
3. Keeps a cursor position, so that it does not process old logs again
4. For XID errors, uses the embedded NVIDIA XID Catalog spreadsheet to find the recommended actions
5. Optionally, uses the XID analyzer sidecar to analyze XID errors with custom logic
6. Sends health events to Platform Connectors through gRPC

The monitor keeps a persistent state across restarts. Thus, it processes each log entry exactly once, even if the pod restarts.

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

- **Enabled Checks**: Select the types of errors to monitor (XID, SXID, fallen-off-bus)
- **Log Level**: Control the logging verbosity (info, debug, warn, error)
- **XID Analyzer Sidecar**: Optional sidecar that adds custom XID analysis logic

## What It Monitors

### XID Errors (GPU Hardware Errors)

XID errors are critical GPU hardware failures that the NVIDIA driver logs. The monitor uses the embedded NVIDIA XID Catalog (an Excel spreadsheet) to map XID codes to recommended actions:

- **XID 48**: Double-bit ECC error (memory corruption, requires GPU replacement)
- **XID 64**: Page retirement limit (GPU memory degradation)
- **XID 79**: GPU has fallen off the bus
- Many other XID codes, each with the applicable remediation action from the official NVIDIA catalog

### SXID Errors (NVSwitch/NVLink Errors)

NVSwitch errors related to the high-speed NVLink interconnect fabric:

- NVSwitch hardware errors
- NVLink connection failures
- Fabric-level issues that affect multi-GPU communication

### GPU Fallen Off Bus

The system cannot access a GPU. This critical failure requires immediate attention.

### GPU Reset

nvidia-smi reset a GPU. This shows that a remediation action for a previous GPU failure is complete.

### Clearing XID conditions

XID conditions require an explicit recovery signal. A quiet period does not clear them.

After a successful GPU reset, Janitor writes a syslog message. The monitor then sends a health event that marks the reset GPU as healthy.

The monitor also sends a healthy health event when XID 154 reports the recovery action `None`. This health event clears the XID conditions for the same PCI address and GPU UUID, when the UUID is available.

Without Janitor, the reset message is not available. Thus, the conditions and the related cordons can stay until the node reboots. After a reboot, the monitor sends a healthy health event for each check. These health events clear the conditions from the previous boot.

## Key Features

### Log Parsing with State Persistence

The monitor parses journald logs for error patterns and keeps the cursor position across restarts. Thus, it processes each log entry exactly once.

### Embedded NVIDIA XID Catalog

The monitor uses the official NVIDIA XID Error Catalog (an Excel spreadsheet) to find the recommended action for each XID code. The binary contains the catalog, so the monitor needs no external dependencies.

### Custom Logic Through a Sidecar

The optional XID analyzer sidecar lets you add custom logic to the XID analysis:

- Override default remediation actions
- Add custom error categorization
- Integrate with proprietary error handling systems
- HTTP API for extensibility

If you enable the sidecar, it receives XID messages and can return custom recommended actions. Thus, you can adapt remediation to your environment without changes to the main monitor.

The monitor appends a non-empty `investigatory_action` value from the analyzer to the original XID message. It uses the format `, Investigatory Action=...`. This guidance helps the investigation, and it does not change the recommended recovery action. The message does not change if the field is absent, is empty, or contains only whitespace. The message of a Kubernetes node condition can lose diagnostic details if it exceeds the configured length limits.

### Configurable Checks

Enable or disable specific check types to match your monitoring needs. Monitor only what is relevant to your hardware.
