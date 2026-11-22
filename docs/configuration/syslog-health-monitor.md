# Syslog Health Monitor Configuration

## Overview

The syslog-health-monitor module reads journald logs. It watches the system logs for GPU errors (XID/SXID), GPU-fallen-off events, and GPU reset events. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether Helm deploys the syslog-health-monitor module in the cluster.

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

Host directory that holds the systemd journal that the monitor reads.

```yaml
syslog-health-monitor:
  journalHostPath: /var/log
```

If your distribution stores the journal in a different location, change this value. Otherwise, keep the default. The DaemonSet mounts this path into the pod. Thus, with a wrong value, the monitor has no journal to read.

`journalHostPath` is the directory that *contains* `journal/`. The DaemonSet mounts it at `/nvsentinel/var/log`, and the monitor reads `/nvsentinel/var/log/journal/`. If you set it to the journal directory itself, the monitor has nothing to read.

To mount only the journal directory, set `journalDirectoryHostPath` instead. The DaemonSet mounts it directly at `/nvsentinel/var/log/journal` and ignores `journalHostPath`:

```yaml
syslog-health-monitor:
  journalDirectoryHostPath: /var/log/journal
```

If the DaemonSet cannot mount the parent directory as-is, use this value. For example, the vcluster syncer rewrites a hostPath of exactly `/var/log` to a per-tenant directory. Thus, the default mount points at a directory with no journal. The syncer does not change `/var/log/journal`.

The directory must exist on every node, or the pod does not start. The default mount of the parent directory prevents this problem on nodes without a persistent journal.

This value applies to regular mode only. Kata mode always mounts `/var/log/journal`.

## Boot Lookback Window

How far back the monitor scans the journal after a node reboot.

```yaml
syslog-health-monitor:
  bootLookbackWindow: "2h"
```

The monitor skips the entries that are older than this window. Thus, a reboot does not report again the XIDs that an operator already remediated manually. For unlimited lookback, set it to `"0"`. The monitor then scans from the head of the journal, with no clipping.

The monitor keeps its state file under `/var/run`, and a reboot clears that directory. Thus, the monitor runs this scan each time it starts with no saved boot ID. A first install and a new node also start with no saved boot ID, so they read the same window. With `"0"`, the scan reads everything that the current boot logged.

If nodes stay down for long service windows and you want their faults reported when they return, widen the window. If a node collects historical errors that you already resolved, narrow the window.

## Cancellation Rules

Sends synthetic healthy health events when one error code implies recovery from another. Without a cancellation, a fault stays latched until something explicitly clears it.

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
| `enabled` | Enables or disables the rule set without deleting it |
| `rules[].onErrorCode` | Error code that triggers the cancellation when the monitor observes it |
| `rules[].cancelErrorCodes` | Error codes that the monitor clears on the same entities, as healthy health events |

The default rule reads XID 162 (PSHC re-engaged) as recovery from XID 163 (PSHC disengaged). Cancellation applies to the same entities that carried the original fault. Thus, a cancellation that clears one GPU does not clear another GPU.

Add a rule only for a pair of codes in which one code truly proves recovery from the other. A wrong pairing clears a fault that is still present, and a broken GPU returns to service.

## Enabled Checks

Configures the health checks that are active. The module monitors journald logs for specific error patterns. The supported checks are `SysLogsXIDError`, `SysLogsSXIDError`, `SysLogsGPUFallenOff`, and `SysLogsNICDriverError`.

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

Monitors the system logs for XID (GPU error) and GPU reset messages. XIDs are NVIDIA GPU error codes that indicate hardware or software issues.

#### SysLogsSXIDError

Monitors for SXID messages specific to NVSwitch errors in multi-GPU configurations.

#### SysLogsGPUFallenOff

Monitors for GPU fallen off events, in which the GPU stops responding or the system cannot access it.

#### SysLogsNICDriverError

Monitors the system logs for NIC driver error patterns, for example mlx5 TX/RX timeouts and NAPI soft lockups. NVSentinel correlates the detected errors with the syslog-detection-correlation logic. The NIC Health Monitor documentation describes this logic. Enable this check together with the `nicDriverDetection` configuration.

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

Enables the XID analyzer sidecar container. If you disable the sidecar, the monitor uses an embedded XID mapping file.

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

When `xidSideCar.enabled: true`, the monitor sends XID messages to the sidecar service through HTTP for analysis.

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

If you enable the sidecar, syslog-health-monitor communicates with it through an HTTP REST API.

### Endpoint

```http
POST http://localhost:8080/decode-xid
```

The sidecar must listen on `localhost:8080`, because it runs in the same pod as syslog-health-monitor.

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

- `number` (integer) - XID error code number (for example, 48, 64, 79)
- `name` (string) - Human-readable name of the XID error
- `mnemonic` (string) - Short mnemonic code for the error type
- `context` (string) - Additional context about the error
- `resolution` (string) - Recommended resolution action for the system to perform
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

For nodes labeled with `nvsentinel.dgxc.nvidia.com/kata.enabled: "true"`, the module:

- Adds a containerd service filter to journald queries
- Removes the `SysLogsSXIDError` check (not supported in a Kata environment)
- Uses a separate DaemonSet with the `-kata` suffix

The module configures this automatically from the node labels. No manual configuration is necessary.
