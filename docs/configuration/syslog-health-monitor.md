# Syslog Health Monitor Configuration

## Overview

The Syslog Health Monitor module watches system logs for GPU errors (XID/SXID), GPU-fallen-off, and GPU reset events by reading journald logs. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the syslog-health-monitor module is deployed in the cluster.

```yaml
global:
  syslogHealthMonitor:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the syslog-health-monitor container.

```yaml
syslog-health-monitor:
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### Logging

Sets the verbosity level for syslog-health-monitor logs.

```yaml
syslog-health-monitor:
  logLevel: info  # Options: debug, info, warn, error
```

## Journal Host Path

Host directory holding the systemd journal the monitor reads.

```yaml
syslog-health-monitor:
  journalHostPath: /var/log
```

Change it only when your distribution stores the journal elsewhere. The DaemonSet mounts this path into the pod, so a wrong value leaves the monitor with no journal to read.

## Boot Lookback Window

How far back the monitor scans the journal after a node reboot.

```yaml
syslog-health-monitor:
  bootLookbackWindow: "2h"
```

Entries older than this window are skipped, so a reboot does not re-report XIDs that an operator already remediated by hand. Set it to `"0"` for unlimited lookback, which scans from the head of the journal with no clipping.

Widen the window on nodes that stay down for long service windows and whose faults you still want reported on return. Narrow it where a node accumulates historical errors you have already dealt with.

## Cancellation Rules

Emits synthetic healthy events when one error code implies recovery from another. Without a cancellation, a fault stays latched until something explicitly clears it.

```yaml
syslog-health-monitor:
  cancellations:
    - name: SysLogsXIDError
      enabled: true
      rules:
        - onErrorCode: "162"
          cancelErrorCodes: ["163"]
```

| Field | Purpose |
|---|---|
| `name` | The check these rules apply to, matching an entry in `enabledChecks` |
| `enabled` | Turns the rule set on or off without deleting it |
| `rules[].onErrorCode` | Error code that, when observed, triggers the cancellation |
| `rules[].cancelErrorCodes` | Error codes cleared on the same entities, as healthy events |

The shipped rule reads XID 162 (PSHC re-engaged) as recovery from XID 163 (PSHC disengaged). Cancellation applies to the same entities that carried the original fault, so clearing one GPU does not clear another.

Add a rule only where one code genuinely proves recovery from the other. A wrong pairing clears a fault that is still present and returns a broken GPU to service.

## Enabled Checks

Configures which health checks are active. The module monitors journald logs for specific error patterns. Supported checks are `SysLogsXIDError`, `SysLogsSXIDError`, `SysLogsGPUFallenOff`, and `SysLogsNICDriverError`.

```yaml
syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
    - SysLogsNICDriverError
```

### Check Types

#### SysLogsXIDError
Monitors for XID (GPU error) and GPU reset messages in system logs. XIDs are NVIDIA GPU error codes that indicate hardware or software issues.

#### SysLogsSXIDError
Monitors for SXID messages specific to NVSwitch errors in multi-GPU configurations.

#### SysLogsGPUFallenOff
Monitors for GPU fallen off events where the GPU becomes unresponsive or inaccessible to the system.

#### SysLogsNICDriverError
Monitors for NIC driver error patterns (e.g. mlx5 TX/RX timeouts, NAPI soft lockups) in system logs. Detected errors are correlated with the syslog-detection-correlation logic described in the NIC Health Monitor documentation. Enable alongside `nicDriverDetection` configuration.

## XID Analyzer Sidecar

Optional sidecar container that provides enhanced XID error analysis and mapping.

### Configuration

```yaml
syslog-health-monitor:
  xidSideCar:
    enabled: false
    image:
      repository: ""
      tag: ""
      pullPolicy: IfNotPresent
```

### Parameters

#### enabled
Enables the XID analyzer sidecar container. When disabled, the monitor uses an embedded XID mapping file.

#### image.repository
Container image for the XID analyzer sidecar service.

#### image.tag
Image tag for the XID analyzer sidecar.

#### image.pullPolicy
Pull policy for the sidecar image.

### XID Parsing Modes

#### Embedded Parser (Default)
When `xidSideCar.enabled: false`, the monitor uses an embedded Excel-based XID mapping file for parsing and analysis.

**Characteristics:**
- No additional container needed
- Uses static XID mapping data
- Suitable for most deployments

#### Sidecar Parser
When `xidSideCar.enabled: true`, the monitor sends XID messages to the sidecar service via HTTP for analysis.

**Characteristics:**
- Dedicated analysis service
- Dynamic XID interpretation
- Can provide enhanced error context

### Example with Sidecar Enabled

```yaml
syslog-health-monitor:
  xidSideCar:
    enabled: true
    image:
      repository: dockerhub.io/acme/xid-analyzer-sidecar
      tag: "v1.0"
      pullPolicy: IfNotPresent
```

## XID Analyzer Sidecar API

When the sidecar is enabled, the syslog health monitor communicates with it via HTTP REST API.

### Endpoint

```http
POST http://localhost:8080/decode-xid
```

The sidecar should listen on `localhost:8080` as it runs in the same pod as the syslog health monitor.

### Request Format

```json
{
  "xid_message": "NVRM: Xid (PCI:0000:43:00): 48, pid=12345, name=python, GPU has fallen off the bus."
}
```

#### Request Fields

- `xid_message` (string, required) - Raw XID error message from system logs

### Response Format

#### Success Response

```json
{
  "success": true,
  "result": {
    "number": 48,
    "name": "DBE (Double Bit Error) ECC Error",
    "mnemonic": "GPU_HAS_FALLEN_OFF_THE_BUS",
    "context": "An uncorrectable ECC error has occurred",
    "resolution": "COMPONENT_RESET",
    "investigatory_action": "Check GPU health and reseat if needed",
    "pcie_bdf": "0000:43:00",
    "driver": "580",
    "machine": "x86_64",
    "decoded_xid_string": "48"
  }
}
```

#### Error Response

```json
{
  "success": false,
  "error": "Failed to parse XID message: invalid format"
}
```

### Response Fields

#### Top Level

- `success` (boolean, required) - Indicates if parsing was successful
- `result` (object, optional) - XID details object, present when `success` is `true`
- `error` (string, optional) - Error message, present when `success` is `false`

#### Result Object

- `number` (integer) - XID error code number (e.g., 48, 64, 79)
- `name` (string) - Human-readable name of the XID error
- `mnemonic` (string) - Short mnemonic code for the error type
- `context` (string) - Additional context about the error
- `resolution` (string) - Recommended resolution action that should be performed by the system
- `investigatory_action` (string) - Steps to investigate the error
- `pcie_bdf` (string) - PCIe Bus:Device.Function identifier
- `driver` (string) - Driver version
- `machine` (string) - Machine architecture
- `decoded_xid_string` (string) - Human-readable decoded error message

### Implementation Requirements

The XID analyzer sidecar must:

1. Listen on port `8080`
2. Implement `POST /decode-xid` endpoint
3. Accept JSON requests with `xid_message` field
4. Return JSON responses with the documented format
5. Handle malformed or unparseable XID messages gracefully
6. Return appropriate HTTP status codes (200 for success, 4xx/5xx for errors)

## Kata Containers Support

The module automatically deploys separate DaemonSets for standard and Kata Container nodes.

### Kata Mode Differences

For nodes labeled with `nvsentinel.dgxc.nvidia.com/kata: "true"`:
- Adds containerd service filter to journald queries
- Removes `SysLogsSXIDError` check (not supported in Kata environment)
- Uses separate DaemonSet with `-kata` suffix

Configuration is automatic based on node labels. No manual configuration needed.
