# NIC Health Monitor Configuration

## Overview

The NIC Health Monitor module detects InfiniBand and RoCE NIC failures in GPU clusters using three detection layers: link state polling, error counter thresholds, and kernel log monitoring (via the Syslog Health Monitor). This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the nic-health-monitor DaemonSet is deployed in the cluster.

```yaml
global:
  nicHealthMonitor:
    enabled: false
```

> **Note**: The NIC Health Monitor requires the metadata collector to be running on every GPU node and to have published `/var/lib/nvsentinel/gpu_metadata.json`. The monitor fails to start if that file is missing or does not contain GPU NUMA and NIC topology data. Exception: when `nicInclusionRegexOverride` is set, automatic NIC discovery and NUMA-based management exclusion are bypassed, so the metadata file is not required.

### Resources

Defines CPU and memory resource requests and limits for the nic-health-monitor pod.

```yaml
nic-health-monitor:
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### Logging

Sets the verbosity level for nic-health-monitor logs.

```yaml
nic-health-monitor:
  logLevel: info  # Options: debug, info, warn, error
```

### Processing Strategy

Controls whether detected health events trigger downstream remediation or are stored for observability only.

```yaml
nic-health-monitor:
  processingStrategy: EXECUTE_REMEDIATION
```

| Value | Behavior |
|-------|----------|
| `EXECUTE_REMEDIATION` | Normal operation; fault-quarantine and node-drainer act on events |
| `STORE_ONLY` | Events are persisted and exported but do not modify cluster resources |

Use `STORE_ONLY` when rolling out new detection patterns (especially `mlx5_napi_soft_lockup` in the Syslog Health Monitor) to observe coverage before enabling remediation.

## Enabled Checks

Selects which detection checks are active on each node.

```yaml
nic-health-monitor:
  enabledChecks:
    - InfiniBandStateCheck
    - InfiniBandDegradationCheck
    - InfiniBandCharDeviceCheck
    - EthernetStateCheck
    - EthernetDegradationCheck
```

### Check Types

#### InfiniBandStateCheck
Polls `/sys/class/infiniband/*/ports/*/state` and `phys_state` every `statePollingInterval`. Emits fatal events on port DOWN, device disappearance (after 3 consecutive missed enumerations), and uncabled port anomalies (card has fewer active ports than peer cards of the same role). Auto-excludes management NICs by NUMA locality and default-route detection; auto-filters SR-IOV Virtual Functions.

#### InfiniBandDegradationCheck
Polls InfiniBand hardware counters every 1 second. Emits fatal events when counters like `link_downed`, `rnr_nak_retry_err`, or `excessive_buffer_overrun_errors` increment. Rate-based threshold breaches emit events at the severity configured for each counter — `symbol_error > 10/sec` is non-fatal, `symbol_error_fatal > 120/hour` is fatal. Counter breach state is persisted across pod restarts.

#### InfiniBandCharDeviceCheck
Verifies that each InfiniBand device's character-device nodes exist: one `uverbs` per device and one `umad` per InfiniBand-mode port (plus, only when [`charDeviceCheck.issm`](#chardevicecheckissm) is `always`, one `issm` per InfiniBand-mode port), read from `/sys/class/infiniband_verbs` and `/sys/class/infiniband_mad` (udev creates the `/dev/infiniband/*` nodes from these class entries, so a missing entry means workloads fail with errors like `lstat /dev/infiniband/issm9: no such file or directory` even while the port reads ACTIVE/LinkUp). The expectation is derived per device from its own discovered ports — never from an absolute device count — and `umad`/`issm` are only expected on InfiniBand-mode ports (RoCE ports legitimately have none). A node missing for 3 consecutive polls emits a fatal `REPLACE_VM` event; the fault latches (persisted across pod restarts and reboots) and clears only when the node is positively observed again. An entirely absent class directory (e.g. `ib_umad` module not loaded) is treated as an uncertain observation and held rather than reported.

The per-port `issm` expectation is opt-in via [`charDeviceCheck.issm`](#chardevicecheckissm) and **off by default**, because whether `issm` nodes are created is architecture- and provider-dependent (some platforms, e.g. GB300, legitimately create none). `umad` and `uverbs` are always required.

#### EthernetStateCheck
Same as `InfiniBandStateCheck` but for Ethernet/RoCE devices (reads `link_layer = Ethernet`). Monitors `operstate` in addition to `state` and `phys_state`.

#### EthernetDegradationCheck
Same as `InfiniBandDegradationCheck` but for Ethernet/RoCE devices. Tracks the same counter set where available on RoCE adapters; additionally monitors `/sys/class/net/{iface}/statistics/carrier_changes`.

## State Polling Interval

Controls how frequently the state checks poll sysfs for link state changes.

```yaml
nic-health-monitor:
  statePollingInterval: "1s"
```

Counter checks always run on a fixed 1-second cadence regardless of this setting — they need fresh data for velocity window calculations and cannot share the state check interval.

## Character-Device Check Tuning

### charDeviceCheck.issm

Controls whether `InfiniBandCharDeviceCheck` expects the per-port `issm` character device. `umad` and `uverbs` are always required and are not configurable; only `issm` is, because whether `issm` nodes are created is architecture- and provider-dependent.

```yaml
nic-health-monitor:
  charDeviceCheck:
    issm: never  # never (default) | always
```

| Value | Behavior |
|-------|----------|
| `never` (default) | Never expect `issm`. `umad`/`uverbs` still cover the RDMA-fatal cases, and a platform that legitimately does not create `issm` nodes (e.g. GB300) is never falsely flagged. |
| `always` | Expect one `issm` per InfiniBand-mode port. Enable only where `issm` nodes are guaranteed to be created (behavior prior to this option). |

**Why this exists:** on newer hardware such as GB300 the platform does not create `issm` device nodes, so the previous unconditional check falsely marked every such GPU node `REPLACE_VM`. `issm` presence cannot be inferred from any port attribute — on a fabric run by an external subnet manager every compute port reads `SM_DISABLED` (`cap_mask` bit `0x400`) whether or not `issm` exists — so the check is an explicit opt-in rather than an inferred default.

Changing this value forces a one-time baseline reconciliation on the next pod start (when there are outstanding `issm` conditions to clear), so stale `issm` conditions are cleared immediately rather than being held until the next reboot.

## NIC Discovery Filtering

### nicExclusionRegex

Comma-separated regex patterns for device names to exclude from discovery. Applied after vendor detection and VF filtering, before NUMA-based management NIC exclusion.

```yaml
nic-health-monitor:
  nicExclusionRegex: "^veth.*,^docker.*,^br-.*,^lo$"
```

Use this to suppress virtual interfaces, bridge devices, or loopback from appearing as potential NICs.

### nicInclusionRegexOverride

When non-empty, bypasses all automatic NIC discovery and monitors only devices whose names match these comma-separated regex patterns. All automatic filters — vendor detection, VF filtering, NUMA-based management exclusion, and `nicExclusionRegex` — are skipped. Pinned devices are reported unconditionally: the first-poll peer-evidence gate and the card homogeneity check do not apply.

```yaml
nic-health-monitor:
  nicInclusionRegexOverride: ""  # Default: empty (auto-discovery enabled)
```

> **Warning**: Changing `nicInclusionRegexOverride` or `nicExclusionRegex` resets the monitor's persisted port and device state on the next pod start (healthy baselines are re-emitted). Counter snapshots and latched fatal counter breaches are preserved across scope changes.

**When to use**: Emergency override when automatic discovery misclassifies a NIC on an unusual platform. In normal deployments, leave empty.

## Counter Detection

### Enable/Disable

Enables or disables counter-based degradation monitoring globally.

```yaml
nic-health-monitor:
  counterDetection:
    enabled: true
```

### Counter Profiles

Counter names are validated against a hardcoded allowlist in the monitor. Operators can enable or disable individual counters, tune thresholds, and change velocity windows. Sysfs paths, severity, recommended action, and event descriptions are owned by code and are not configurable here.

```yaml
nic-health-monitor:
  counterDetection:
    enabled: true
    counters:
      - name: link_downed
        enabled: true
        thresholdType: delta
        threshold: 0
```

#### Counter Profile Fields

| Field | Type | Description |
|-------|------|-------------|
| `name` | string | Counter identifier from the allowlist (see below) |
| `enabled` | bool | Whether this counter is monitored (default: `true`) |
| `thresholdType` | string | `delta` (absolute change per poll) or `velocity` (rate per `velocityUnit`) |
| `threshold` | number | Numeric threshold; breach when exceeded |
| `velocityUnit` | string | For velocity thresholds: `second`, `minute`, or `hour` |

#### Default Counter Configuration

```yaml
nic-health-monitor:
  counterDetection:
    enabled: true
    counters:
      # Fatal counters — any increment triggers REPLACE_VM
      - name: link_downed                    # Port training failure
        enabled: true
        thresholdType: delta
        threshold: 0

      - name: excessive_buffer_overrun_errors  # Lossless contract violated
        enabled: true
        thresholdType: delta
        threshold: 0

      - name: local_link_integrity_errors    # Physical errors exceed hardware cap
        enabled: true
        thresholdType: delta
        threshold: 0

      - name: rnr_nak_retry_err              # Connection severed by retry exhaustion
        enabled: true
        thresholdType: delta
        threshold: 0

      # Fatal PHY threshold
      - name: symbol_error_fatal             # IBTA BER spec violation (> 120/hour = fatal)
        enabled: true
        thresholdType: velocity
        threshold: 120.0
        velocityUnit: hour

      # PHY degradation — non-fatal
      - name: symbol_error                   # PHY bit errors (> 10/sec = degradation)
        enabled: true
        thresholdType: velocity
        threshold: 10.0
        velocityUnit: second

      - name: link_error_recovery            # Link retraining / micro-flapping
        enabled: true
        thresholdType: velocity
        threshold: 5.0
        velocityUnit: minute

      # Transport — non-fatal
      - name: port_rcv_errors                # Malformed packets / CRC errors
        enabled: true
        thresholdType: velocity
        threshold: 10.0
        velocityUnit: second

      - name: local_ack_timeout_err          # ACK timeout (fabric path issue or remote crash)
        enabled: true
        thresholdType: velocity
        threshold: 1.0
        velocityUnit: second

      # Congestion — non-fatal
      - name: port_xmit_discards             # TX discards due to flow control breakdown
        enabled: true
        thresholdType: velocity
        threshold: 100.0
        velocityUnit: second

      # RoCE-specific — non-fatal
      - name: roce_slow_restart              # Victim flow oscillation (grey failure indicator)
        enabled: true
        thresholdType: velocity
        threshold: 10.0
        velocityUnit: second

      # Interface level — non-fatal
      - name: carrier_changes                # OS-visible link instability
        enabled: true
        thresholdType: delta
        threshold: 2
```

#### Allowed Counter Names

Counters outside this list are rejected at startup with a validation error.

**Standard IB counters** (`counters/`):
`excessive_buffer_overrun_errors`, `link_downed`, `link_error_recovery`, `local_link_integrity_errors`, `port_rcv_discards`, `port_rcv_errors`, `port_rcv_remote_physical_errors`, `port_rcv_switch_relay_errors`, `port_xmit_discards`, `port_xmit_wait`, `symbol_error`, `symbol_error_fatal`

**Extended counters** (`hw_counters/`):
`implied_nak_seq_err`, `local_ack_timeout_err`, `out_of_sequence`, `packet_seq_err`, `req_transport_retries_exceeded`, `rnr_nak_retry_err`, `roce_slow_restart`

**Ethernet statistics** (`statistics/`):
`carrier_changes`, `rx_crc_errors`, `rx_errors`, `rx_missed_errors`, `tx_carrier_errors`, `tx_errors`

### Counter Customization Examples

#### Stricter BER threshold

```yaml
nic-health-monitor:
  counterDetection:
    counters:
      - name: symbol_error_fatal
        enabled: true
        thresholdType: velocity
        threshold: 60.0       # Stricter than the default 120/hour
        velocityUnit: hour
```

#### Add an allowed counter not enabled by default

```yaml
nic-health-monitor:
  counterDetection:
    counters:
      - name: rx_missed_errors
        enabled: true
        thresholdType: velocity
        threshold: 10.0
        velocityUnit: second
```

#### Disable a noisy counter

```yaml
nic-health-monitor:
  counterDetection:
    counters:
      - name: port_xmit_wait
        enabled: false
```

> **Note**: Specifying a counter list in your values overrides the full default list. If you want to add or modify one counter, include the complete default list alongside your changes.

## NIC Driver Error Detection (Syslog Health Monitor)

NIC driver and firmware errors are monitored by the **Syslog Health Monitor** DaemonSet, not the NIC Health Monitor. Enable the `SysLogsNICDriverError` check and configure pattern selection in the Syslog Health Monitor's Helm values:

```yaml
syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
    - SysLogsNICDriverError  # Add this to enable NIC driver log monitoring
```

### NIC Driver Pattern Configuration

Pattern regexes, severity, and recommended actions are owned by code. The following Helm/YAML configuration selects which patterns are active and optionally overrides the per-pattern processing strategy:

```yaml
syslog-health-monitor:
  nicDriverDetection:
    patterns:
      - name: cmd_exec_timeout        # Fatal: firmware hung, driver cannot issue commands
        enabled: true
        processingStrategy: EXECUTE_REMEDIATION

      - name: health_poll_failed      # Fatal: firmware heartbeat lost
        enabled: true
        processingStrategy: EXECUTE_REMEDIATION

      - name: unrecoverable_err       # Fatal: hardware admission of failure
        enabled: true
        processingStrategy: EXECUTE_REMEDIATION

      - name: mlx5_napi_soft_lockup   # Fatal: CPU wedged in NIC NAPI poll loop
        enabled: true
        processingStrategy: STORE_ONLY  # Start in shadow mode; graduate to EXECUTE_REMEDIATION after validation

      - name: netdev_watchdog         # Non-fatal: TX queue stall with auto-recovery
        enabled: true

      - name: mlx5_tx_timeout_detected  # Non-fatal: TX timeout (driver-reported)
        enabled: true

      - name: mlx5_rx_timeout_detected  # Non-fatal: RX timeout (driver-reported)
        enabled: true

      - name: port_module_high_temp   # Non-fatal: thermal warning
        enabled: true

      - name: pci_power_insufficient  # Non-fatal: PCIe power negotiation
        enabled: true

      - name: module_unplugged        # Non-fatal: SFP/transceiver removed
        enabled: true

      - name: access_reg_failed       # Non-fatal: monitoring tool conflict noise
        enabled: true
```

See [Syslog Health Monitor Configuration](./syslog-health-monitor.md) for complete syslog-health-monitor options.

## Health Events Analyzer Rules

Repeated non-fatal NIC events are escalated to `CONTACT_SUPPORT` by the Health Events Analyzer. Enable these rules in the health-events-analyzer Helm values:

```yaml
health-events-analyzer:
  enableRepeatedNICDegradationRule: true   # 3 non-fatal counter events on same NIC+port in 1 hour
  enableRepeatedNICDriverErrorRule: true   # 3 non-fatal syslog events of same pattern on same node in 1 hour
```

## Scheduling

Configure pod placement for the NIC Health Monitor DaemonSet.

```yaml
nic-health-monitor:
  nodeSelector: {}
  tolerations: []
  affinity: {}
```

The DaemonSet runs on all nodes by default. Use `nodeSelector` or `affinity` to limit it to nodes with monitored NICs.
