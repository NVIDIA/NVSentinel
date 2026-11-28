# NIC Health Monitor

## Overview

The NIC Health Monitor detects network interface failures in GPU clusters before they affect distributed training workloads. It addresses **grey failures**. A grey failure is a subtle link degradation, in which a single impaired NIC can throttle thousands of GPUs across an entire job. There is no obvious error, because the link appears UP while it silently corrupts or drops packets.

The monitor operates as a three-layer network health scanner. It checks whether links are up, whether error rates increase toward failure, and whether the driver can still communicate with the NIC hardware.

### Why Do You Need This?

In high-performance GPU clusters, NIC failures cause unique problems:

- **Silent job failure**: A degraded InfiniBand or RoCE link drops the effective bandwidth for every GPU that communicates through it during collective operations (AllReduce, AllGather). Training jobs then hang or crash with no obvious hardware error
- **Grey failures**: Unlike a complete link DOWN, gradual degradation can continue for hours before outright failure. Examples include increasing symbol errors, link flaps, and buffer overruns. This degradation wastes expensive compute
- **Driver-layer blind spots**: A NIC can report link UP with normal counters, while the `mlx5_core` driver cannot issue any commands to the firmware. Without kernel log monitoring, this failure is impossible to detect
- **Costly remediation delays**: Without automatic detection, operators must find the bad NIC manually. They must correlate NCCL timeout errors, kernel logs, and InfiniBand counters across hundreds of nodes

## How It Works

The NIC Health Monitor uses a **three-layer detection approach**. This approach runs as two DaemonSets on GPU nodes:

### Layer 1: Link State Detection (NIC Health Monitor DaemonSet)

Polls `/sys/class/infiniband/` sysfs files at `statePollingInterval` (1s by default) for hard UP/DOWN transitions:

- Port transitions to `DOWN` or `phys_state=Disabled` at runtime → **Fatal** (`REPLACE_VM`). If you set `portStateHoldDown`, the monitor reports the transition only if the port stays unhealthy for this hold-down time. If you set `portStateBootGracePeriod`, the monitor holds the transition until the host has been up for this time, and reports it only if the port is still unhealthy then. The monitor suppresses a port that is unhealthy on the first poll and has no peer-card evidence. If you set `suppressedPortReportAfter`, the monitor reports this port after this time. This applies only to a compute card for which the peers gave no verdict (see [NIC Health Monitor Configuration](./configuration/nic-health-monitor.md))
- NIC disappears from sysfs (fell off PCIe bus), confirmed across 3 consecutive polls → **Fatal** (`REPLACE_VM`)
- Card has fewer active ports than peer NICs of the same role (uncabled port anomaly) → **Fatal** (`REPLACE_VM`)

The monitor automatically excludes management NICs. A management NIC is on a NUMA node without GPUs, or it carries the default route of the host. The monitor also automatically excludes SR-IOV Virtual Functions. No per-GPU-type configuration is necessary.

### Layer 2: Link Counter Detection (NIC Health Monitor DaemonSet)

Polls InfiniBand hardware counters every second for error rate violations:

**Fatal counters** (any increment triggers `REPLACE_VM`):

- `link_downed` — port training failure; active QPs cannot recover
- `excessive_buffer_overrun_errors` — lossless fabric contract violated
- `local_link_integrity_errors` — physical errors exceed hardware cap
- `rnr_nak_retry_err` — connection severed by retry exhaustion
- `symbol_error_fatal` > 120/hour — IBTA BER spec violation (10E-12 threshold)

**Non-fatal degradation** (monitored; 3 health events in 1 hour escalate to `CONTACT_SUPPORT`):

- `symbol_error` > 10/sec — dirty fiber / PHY degradation
- `link_error_recovery` > 5/min — link flapping
- `roce_slow_restart` > 10/sec — grey failure straggler indicator
- `carrier_changes`, `port_rcv_errors`, and others

The monitor keeps the counter breach state across pod restarts. The monitor automatically sends recovery health events when an administrator resets the counters, for example with `perfquery -r` or `perfquery -R`. It also sends them when the node reboots.

### Layer 3: Syslog Detection (Syslog Health Monitor DaemonSet)

Link-state polling and counter polling cannot see some driver/firmware failures. This layer watches journald for the `mlx5_core` kernel messages that indicate these failures:

**Fatal patterns** (trigger `REPLACE_VM`):

- `timeout. Will cause a leak of a command resource` — firmware hung, driver cannot issue commands
- `device's health compromised - reached miss count` — firmware heartbeat lost
- `mlx5_core.*unrecoverable hardware error` — hardware admission of failure
- `BUG: soft lockup` + mlx5 NAPI stack frames — CPU stuck in NIC poll loop

**Non-fatal diagnostic patterns** (provide correlation context for operators):

- `NETDEV WATCHDOG` TX queue stall, `TX timeout detected`, `RX timeout on channel`
- `Detected insufficient power on the PCIe slot`
- `Port module event.*High Temperature`
- `Cable unplugged`, `ACCESS_REG failed`

The Health Events Analyzer escalates repeated non-fatal syslog patterns (3 in 1 hour on the same node) to `CONTACT_SUPPORT`.

## Architecture

```text
Per-Node DaemonSets                     Centralized
─────────────────────────────────────   ────────────────────────────────
NIC Health Monitor                      Health Events Analyzer
  • sysfs state polling (1s)          ←   • Correlation rules (MongoDB)
  • sysfs counter polling (1s)            • RepeatedNICDegradation
                                          • RepeatedNICDriverError
Syslog Health Monitor
  • journald watch (event-driven)
  • SysLogsNICDriverError check

Both → Platform Connector → MongoDB → Fault Quarantine / Node Drainer
```

The monitors use the NVSentinel **"Report Raw, Correlate Centrally"** pattern. Each DaemonSet reports raw health events as-is to the Platform Connector. The Health Events Analyzer handles correlation, repeat-failure escalation, and diagnostic context. These functions require no code changes to the monitors.

## What It Monitors

| Detection Layer | Data Source | Fatal Condition | Non-Fatal (Degradation) |
|----------------|-------------|-----------------|-------------------------|
| **Link State** | `/sys/class/infiniband/*/ports/*/state` | Port DOWN, device disappeared, uncabled anomaly | INIT/ARMED/Polling states (transient) |
| **Link Counter** | `/sys/class/infiniband/*/ports/*/counters/` | `link_downed`, `rnr_nak_retry_err`, buffer overrun, BER >120/hour | Symbol errors, link recovery, congestion |
| **Syslog** | journald / dmesg | `cmd_exec timeout`, health poll failed, unrecoverable, NAPI soft lockup | TX/RX timeouts, thermal, power, SFP events |

## Supported Hardware

> **Scope**: Mellanox/NVIDIA InfiniBand and RoCE devices only.

| Vendor | Detection | State Monitoring | Counter Monitoring | Syslog Monitoring |
|--------|-----------|-----------------|-------------------|-------------------|
| **Mellanox ConnectX (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes | Yes | Yes (`mlx5_core` patterns) |

Validated on: DGX A100, DGX H100, H100 OCI, A100 OCI RoCE, L40S OCI, on-prem L40S, GB200 NVL4.

## Configuration

Enable the NIC Health Monitor in your Helm values:

```yaml
global:
  nicHealthMonitor:
    enabled: true

syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
    - SysLogsNICDriverError  # NIC driver/firmware error patterns
```

The NIC Health Monitor DaemonSet requires the metadata collector to run on the same node. The metadata collector provides the GPU↔NIC topology for management NIC exclusion and role classification. If the metadata file is missing, the monitor fails to start. But if you set `nicInclusionRegexOverride`, the monitor starts without the metadata file. This setting bypasses automatic discovery and the metadata dependency entirely.

For counter threshold customization, NIC exclusion patterns, and advanced configuration, see [NIC Health Monitor Configuration](./configuration/nic-health-monitor.md).

## Key Features

### Zero-Configuration NIC Role Classification

The monitor automatically classifies NICs as Compute, Storage, or Management. For this classification, it uses a combination of NUMA locality, the `nvidia-smi topo -m` GPU↔NIC matrix, the link layer (InfiniBand or Ethernet), and default-route detection. The classification works across DGX, HGX, Grace-based (GB200/GH200), OEM, and cloud platforms, without any per-GPU-type static configuration.

### Pre-Failure Detection

The monitor tracks InfiniBand symbol error rates against the IBTA 10E-12 BER specification. It detects when FEC is near exhaustion, and it reports a fatal health event before the link drops to zero. Thus, NVSentinel drains the node before the "cliff effect" causes 100% packet loss.

### Persistent State Across Restarts

The NIC Health Monitor saves counter snapshots, breach flags, and port state to a hostPath-backed file. Timestamp-only counter updates can wait until a poll runs at least one minute after the last successful save. Any earlier save includes the latest counter snapshots. At a graceful shutdown, the monitor tries to save any pending counter updates. A pod that restarts on the same host boot resumes from the last saved counter snapshots.

The monitor sends a recovery health event when an administrator clears a breached counter. After a host reboot, the monitor first completes device discovery. Then it clears the counter conditions of the previous boot and reports fresh baselines.

### Uncabled Port Detection

The monitor uses a card homogeneity check to detect cards with fewer ports than expected. The check compares the active port count of each NIC card with the port counts of its role-group peers. A card with fewer active ports indicates a failed or uncabled port. The check does not require a static configuration of expected port counts.
