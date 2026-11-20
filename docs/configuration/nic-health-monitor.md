# NIC Health Monitor Configuration

## Overview

The NIC Health Monitor module detects InfiniBand and RoCE NIC failures in GPU clusters. It uses three detection layers: link state polling, error counter thresholds, and kernel log monitoring (through the Syslog Health Monitor). This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether Helm deploys the NIC Health Monitor DaemonSet in the cluster.

```yaml
global:
  nicHealthMonitor:
    enabled: false
```

> **Note**: The NIC Health Monitor requires the metadata collector to run on every GPU node. The metadata collector must also publish `/var/lib/nvsentinel/gpu_metadata.json` before the monitor starts. If that file is missing or does not contain GPU NUMA and NIC topology data, the monitor fails to start. Exception: if you set `nicInclusionRegexOverride`, the monitor bypasses automatic NIC discovery and NUMA-based management exclusion. Then the monitor does not require the metadata file.

### Resources

Defines CPU and memory resource requests and limits for the NIC Health Monitor pod.

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

Sets the verbosity level for the NIC Health Monitor logs.

```yaml
nic-health-monitor:
  logLevel: info  # Options: debug, info, warn, error
```

### Processing Strategy

Controls whether detected health events trigger downstream remediation, or whether NVSentinel only stores them for observability.

```yaml
nic-health-monitor:
  processingStrategy: EXECUTE_REMEDIATION
```

| Value | Behavior |
|-------|----------|
| `EXECUTE_REMEDIATION` | Normal operation; fault-quarantine and node-drainer act on the health events |
| `STORE_ONLY` | NVSentinel persists and exports the health events, but they do not modify cluster resources |

When you deploy new detection patterns, use `STORE_ONLY` to observe coverage before you enable remediation. This applies especially to `mlx5_napi_soft_lockup` in the Syslog Health Monitor.

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

Polls `/sys/class/infiniband/*/ports/*/state` and `phys_state` every `statePollingInterval`. Emits fatal health events for these conditions:

- port DOWN
- device disappearance (after 3 consecutive missed enumerations)
- uncabled port anomalies (the card has fewer active ports than peer cards of the same role)

Automatically excludes management NICs by NUMA locality and default-route detection. Automatically excludes SR-IOV Virtual Functions.

#### InfiniBandDegradationCheck

Polls InfiniBand hardware counters every 1 second. Emits fatal health events when counters such as `link_downed`, `rnr_nak_retry_err`, or `excessive_buffer_overrun_errors` increment. For a rate-based threshold breach, the check emits a health event at the severity configured for each counter. For example, `symbol_error > 10/sec` is non-fatal, and `symbol_error_fatal > 120/hour` is fatal. The check keeps the counter breach state across pod restarts.

#### InfiniBandCharDeviceCheck

Verifies that the character-device nodes of each InfiniBand device exist. The check reads them from `/sys/class/infiniband_verbs` and `/sys/class/infiniband_mad`. It expects these nodes:

- one `uverbs` per device
- one `umad` per InfiniBand-mode port
- only when [`charDeviceCheck.issm`](#chardevicecheckissm) is `always`, also one `issm` per InfiniBand-mode port

udev creates the `/dev/infiniband/*` nodes from these class entries. Thus, if an entry is missing, workloads fail with errors such as `lstat /dev/infiniband/issm9: no such file or directory`. This occurs even while the port reads ACTIVE/LinkUp.

The check derives the expectation for each device from the ports that it discovered on that device, never from an absolute device count. The check expects `umad`/`issm` only on InfiniBand-mode ports, because RoCE ports legitimately have none.

If a character-device node is missing for 3 consecutive polls, the check emits a fatal `REPLACE_VM` health event. The fault latches, and it persists across pod restarts and reboots. The fault clears only when the check positively observes the node again. If an entire class directory is absent (for example, the `ib_umad` module is not loaded), the check treats this as an uncertain observation. The check holds this observation and does not report it.

The per-port `issm` expectation is opt-in through [`charDeviceCheck.issm`](#chardevicecheckissm), and it is **off by default**. The reason is that the creation of `issm` nodes is architecture- and provider-dependent. Some platforms, for example GB300, legitimately create none. `umad` and `uverbs` are always required.

#### EthernetStateCheck

Same as `InfiniBandStateCheck` but for Ethernet/RoCE devices (reads `link_layer = Ethernet`). Monitors `operstate` in addition to `state` and `phys_state`.

#### EthernetDegradationCheck

Same as `InfiniBandDegradationCheck` but for Ethernet/RoCE devices. Tracks the same counter set where it is available on RoCE adapters. Also monitors `/sys/class/net/{iface}/statistics/carrier_changes`.

## State Polling Interval

Controls how frequently the state checks poll sysfs for link state changes.

```yaml
nic-health-monitor:
  statePollingInterval: "1s"
```

Counter checks always run on a fixed 1-second cadence, regardless of this setting. They need fresh data for velocity window calculations, so they cannot share the state check interval.

## Port State Hold-Down

By default, the monitor immediately reports a port that drops for a few seconds and then recovers. A `DOWN` port is fatal. Thus, a short fabric blip can cordon a node. With `portStateHoldDown`, a port must stay unhealthy for this duration before `InfiniBandStateCheck` or `EthernetStateCheck` reports it.

```yaml
nic-health-monitor:
  portStateHoldDown: "15s"
```

- **Off by default.** `"0s"` reports on the first unhealthy poll.
- **Port and card events alike.** While the monitor holds a port, the homogeneity check of its card also sees the port as up. Thus, the card health event also waits.
- **Blips are counted, not reported.** If a port recovers before the check reports it, the check publishes no health event. Instead, it increments `nic_health_monitor_port_state_blips_total{node,check,device,port}`.
- **Recovery is immediate.** The monitor never delays the healthy health event.
- **A restart restarts the hold.** The monitor does not persist the hold-down.

The hold-down applies only to port state. The `link_downed` counter (InfiniBandDegradationCheck) is fatal on any increase. It stays latched until the counter resets or the host reboots. Thus, a blip that increments the counter still produces a fatal health event. If you see blips there, you can disable that counter (see [Disable a noisy counter](#disable-a-noisy-counter)).

## Reporting Ports Suppressed on the First Poll

On the first poll, the monitor reports an unhealthy port only if peer evidence shows that the port is expected to be up. This evidence is that its card has fewer active ports than the decisive mode of its role group. Otherwise, the monitor suppresses the port, because the port can be uncabled or disabled by design. That happens when:

- the card is at or above its group's mode, like the uncabled second port of a dual-port card. This is the most common case.
- the card shares a PCI card with a management NIC, so the monitor excludes it from the peer comparison.
- the card is alone in its role group.
- the group's vote is tied.
- the group's mode is 0, because most of its cards are down.

By default, the monitor never reports a suppressed port that stays down. If half the compute links of a node are down from boot, the vote is a tie, and the node stays silent.

With `suppressedPortReportAfter`, the monitor reports such a port after it stays unhealthy for this duration. This applies only to a compute card for which the peers gave no verdict (a singleton group, a tie, or a mode of 0):

```yaml
nic-health-monitor:
  suppressedPortReportAfter: "10m"
```

- **Off by default.** `"0s"` keeps the port suppressed. If some compute ports are down by design, leave it off. Alternatively, exclude those devices with `nicExclusionRegex`.
- **Compute cards only.** A card qualifies when at least one monitored device on it is classified as compute and has a port on the same link layer. Then every port on the card qualifies, including the ports of a device classified as storage. For example, a GB300 SMI device has no topology matrix entry, but it shares its card with a compute device. The monitor never reports a port on a card with no compute device. Storage and frontend ports can be down by design with no peer to compare against. An example is the unused frontend aux port on OCI BM.GPU.H100.8.
- **Healthy cards are never reported.** The monitor does not report a port on a card at or above its group's mode, however long the port stays down. The same applies to a port on a card that shares a PCI card with a management NIC. If a later poll finds the card at or above the mode, the monitor drops the port. For example, this occurs when the other ports of the card finish training.
- **Unreadable devices.** While a device is unreadable, the peer comparison does not run. Thus, a port on a card with an active port waits for a complete poll, because its card can be at its group's mode. The monitor reports a port on a card with no active port, because no peer vote can find that card healthy. But if one of the card's own functions is unreadable, the monitor does not report the port. If the monitor cannot find the PCI card of an unreadable device, every card waits. A card with an unreadable function also keeps its ports, even when the monitor read no compute device on it. The reason is that the unreadable function can be its compute device.
- **Severity.** An InfiniBand port that is still `DOWN` is fatal with `REPLACE_VM`, and this includes `Polling`. The homogeneity check gives the same result for a card below its mode. `INIT` and `ARMED` stay non-fatal, so a subnet manager outage at boot does not mark every node fatal. The monitor reports a RoCE port only when it is `DOWN`, which is fatal.
- **Ports still training at boot are not reported.** If a port is up before the delay ends, the monitor drops it.
- **Survives restarts.** The monitor keeps the suppression time in the state file, so a pod restart does not restart the wait. This includes a restart while the reboot baseline is still pending because a device is unreadable. A reboot or a discovery-scope change resets the suppression time, and the next first poll decides again.
- **Recorded while off.** The monitor saves the suppression time even when the option is off. If you enable the option later, the monitor reports a port that has stayed down since its suppression. It reports the port at the first poll after the delay, without a reboot.

## Poll Stall Detection

Reports when the monitor itself cannot observe NIC state. If a state or counter poll stays in flight for `pollStallDeadline`, the monitor publishes an unhealthy, non-fatal `NICPollStallCheck` health event (recommended action `NONE`). After the stall ends, it publishes the healthy health event. No check runs while a publish waits for the deployment platform connector. Thus, the monitor does not report a connector outage as a NIC stall.

For the same condition as a metric, use `time() - nic_health_monitor_poll_cycle_last_completed_timestamp_seconds`.

If a stall health event fails to publish, the monitor tries again until the event is delivered. It does this even if the poll completed in the meantime, so the episode is still recorded. The first poll that completes after the monitor starts also publishes a healthy health event. This health event closes a stall that stayed open when the liveness probe restarted the container.

A stalled poll usually means that the host stalls the sysfs reads, for example through lock contention in the network stack. A container restart does not fix this problem. Without this check, the only trace is the restart count.

```yaml
nic-health-monitor:
  pollStallDeadline: "10s"
  pollStallStoreOnly: true
```

Keep `pollStallDeadline` well below the liveness restart window, which is about 60 to 90 seconds into a stall. Otherwise, the monitor never reports the stall. `"0s"` disables detection. `pollStallStoreOnly` emits the health events as `STORE_ONLY`, whatever the `processingStrategy` value is. The reason is that the cause is the host, not a NIC.

## Character-Device Check Tuning

### charDeviceCheck.issm

Controls whether `InfiniBandCharDeviceCheck` expects the per-port `issm` character device. `umad` and `uverbs` are always required, and you cannot configure them. Only `issm` is configurable, because the creation of `issm` nodes is architecture- and provider-dependent.

```yaml
nic-health-monitor:
  charDeviceCheck:
    issm: never  # never (default) | always
```

| Value | Behavior |
|-------|----------|
| `never` (default) | Never expect `issm`. `umad`/`uverbs` still cover the RDMA-fatal cases. The check never falsely flags a platform that legitimately does not create `issm` nodes (for example, GB300). |
| `always` | Expect one `issm` per InfiniBand-mode port. Enable it only on platforms that always create `issm` nodes. |

**Why this exists:** On newer hardware such as GB300, the platform does not create `issm` device nodes. Thus, an unconditional `issm` check falsely marks every such GPU node `REPLACE_VM`. The check cannot infer `issm` presence from any port attribute. On a fabric that an external subnet manager runs, every compute port reads `SM_DISABLED` (`cap_mask` bit `0x400`), whether or not `issm` exists. Thus, the check is an explicit opt-in, not an inferred default.

A change to this value forces a one-time baseline reconciliation at the next pod start, if there are outstanding `issm` conditions to clear. Thus, the monitor clears stale `issm` conditions immediately and does not hold them until the next reboot.

## NIC Discovery Filtering

### nicExclusionRegex

Comma-separated regex patterns for device names to exclude from discovery. The monitor applies them after vendor detection and VF filtering, and before NUMA-based management NIC exclusion.

```yaml
nic-health-monitor:
  nicExclusionRegex: "^veth.*,^docker.*,^br-.*,^lo$"
```

Use this value to prevent virtual interfaces, bridge devices, or loopback from appearing as potential NICs.

### nicInclusionRegexOverride

When non-empty, bypasses all automatic NIC discovery and monitors only the devices whose names match these comma-separated regex patterns. The monitor skips all automatic filters: vendor detection, VF filtering, NUMA-based management exclusion, and `nicExclusionRegex`. The monitor reports pinned devices unconditionally. The first-poll peer-evidence gate and the card homogeneity check do not apply.

```yaml
nic-health-monitor:
  nicInclusionRegexOverride: ""  # Default: empty (auto-discovery enabled)
```

> **Warning**: A change to `nicInclusionRegexOverride` or `nicExclusionRegex` resets the persisted port and device state of the monitor at the next pod start. The monitor then emits the healthy baselines again. The monitor keeps the counter snapshots and the latched fatal counter breaches across scope changes.

**When to use**: Emergency override when automatic discovery misclassifies a NIC on an unusual platform. In normal deployments, leave it empty.

## Sysfs Paths

The locations where the monitor reads network and InfiniBand device state. The DaemonSet mounts the host's `/sys` under these paths. Thus, the values must match the host mounts in the pod spec.

```yaml
nic-health-monitor:
  sysClassNetPath: "/nvsentinel/sys/class/net"
  sysClassInfinibandPath: "/nvsentinel/sys/class/infiniband"
```

If you change where the chart mounts the host `/sys`, change these values to match. Otherwise, keep the defaults. If a path does not exist, the monitor has no devices to discover.

## Counter Detection

### Enable/Disable

Enables or disables counter-based degradation monitoring globally.

```yaml
nic-health-monitor:
  counterDetection:
    enabled: true
```

### Counter Profiles

The monitor validates counter names against its hardcoded allowlist. You can enable or disable individual counters, tune thresholds, and change velocity windows. The code defines the sysfs paths, severity, recommended action, and event descriptions. You cannot configure them here.

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

#### Counter Snapshots Across Restarts

The monitor updates each live delta snapshot on evaluation and each live velocity snapshot when its window ends. Timestamp-only updates can wait until a poll runs at least one minute after the last successful save. A save that another change triggers also saves the latest timestamps.

At a graceful shutdown, the monitor tries to save pending counter updates after polling stops. A pod that restarts on the same host boot uses the last saved snapshot. This also applies when a counter changes from `delta` to `velocity`. An abrupt exit or a failed save can leave an older timestamp on disk. An older timestamp or a gap in polling can lengthen the first velocity sample. This can understate a recent burst and miss a threshold breach.

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

At startup, the monitor rejects counters that are not in this list, with a validation error.

**Standard IB counters** (`counters/`): `excessive_buffer_overrun_errors`, `link_downed`, `link_error_recovery`, `local_link_integrity_errors`, `port_rcv_discards`, `port_rcv_errors`, `port_rcv_remote_physical_errors`, `port_rcv_switch_relay_errors`, `port_xmit_discards`, `port_xmit_wait`, `symbol_error`, `symbol_error_fatal`

**Extended counters** (`hw_counters/`): `implied_nak_seq_err`, `local_ack_timeout_err`, `out_of_sequence`, `packet_seq_err`, `req_transport_retries_exceeded`, `rnr_nak_retry_err`, `roce_slow_restart`

**Ethernet statistics** (`statistics/`): `carrier_changes`, `rx_crc_errors`, `rx_errors`, `rx_missed_errors`, `tx_carrier_errors`, `tx_errors`

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

> **Note**: A counter list in your values overrides the full default list. To add or modify one counter, include the complete default list together with your changes.

## NIC Driver Error Detection (Syslog Health Monitor)

The **Syslog Health Monitor** DaemonSet monitors NIC driver and firmware errors, not the NIC Health Monitor. Use the Helm values of the Syslog Health Monitor to enable the `SysLogsNICDriverError` check and to select the patterns:

```yaml
syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
    - SysLogsNICDriverError  # Add this to enable NIC driver log monitoring
```

### NIC Driver Pattern Configuration

The code defines the pattern regexes, severity, and recommended actions. The following Helm/YAML configuration selects the active patterns. Optionally, it also overrides the per-pattern processing strategy:

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

For the complete Syslog Health Monitor options, see [Syslog Health Monitor Configuration](./syslog-health-monitor.md).

## Health Events Analyzer Rules

The Health Events Analyzer escalates repeated non-fatal NIC health events to `CONTACT_SUPPORT`. Enable these rules in the Helm values of the Health Events Analyzer:

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
