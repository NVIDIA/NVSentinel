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

## Enabled Checks

Configures which health checks are active. The module monitors journald logs for specific error patterns. The only supported checks at this time are SysLogsXIDError, SysLogsSXIDError and SysLogsGPUFallenOff.

```yaml
syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
```

### Check Types

#### SysLogsXIDError
Monitors for XID (GPU error) and GPU reset messages in system logs. XIDs are NVIDIA GPU error codes that indicate hardware or software issues.

#### SysLogsSXIDError
Monitors for SXID messages specific to NVSwitch errors in multi-GPU configurations.

#### SysLogsGPUFallenOff
Monitors for GPU fallen off events where the GPU becomes unresponsive or inaccessible to the system.

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
