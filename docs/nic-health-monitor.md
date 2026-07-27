# NIC Health Monitor

## Overview

The NIC Health Monitor detects network interface failures in GPU clusters before they impact distributed training workloads. It addresses **grey failures** — subtle link degradations where a single impaired NIC can throttle thousands of GPUs across an entire job without any obvious error, since the link appears UP while silently corrupting or dropping packets.

Think of it as a three-layer network health scanner — checking whether links are up, whether error rates are climbing toward failure, and whether the driver itself can still communicate with the NIC hardware.

### Why Do You Need This?

In high-performance GPU clusters, NIC failures cause unique problems:

- **Silent job failure**: A degraded InfiniBand or RoCE link drops the effective bandwidth for every GPU communicating through it during collective operations (AllReduce, AllGather), causing training jobs to hang or crash with no obvious hardware error
- **Grey failures**: Unlike a complete link DOWN, gradual degradation (climbing symbol errors, link flaps, buffer overruns) can persist for hours before causing outright failure, wasting expensive compute
- **Driver-layer blind spots**: A NIC can report link UP with normal counters while the `mlx5_core` driver has lost all ability to issue commands to the firmware — impossible to detect without monitoring kernel logs
- **Costly remediation delays**: Without automatic detection, operators must manually correlate NCCL timeout errors, kernel logs, and InfiniBand counters across hundreds of nodes to identify the bad NIC

## How It Works

The NIC Health Monitor uses a **three-layer detection approach**, running as two DaemonSets on GPU nodes:

### Layer 1: Link State Detection (NIC Health Monitor DaemonSet)

Polls `/sys/class/infiniband/` sysfs files at `statePollingInterval` (1s by default) for hard UP/DOWN transitions:

- Port transitions to `DOWN` or `phys_state=Disabled` at runtime → **Fatal** (`REPLACE_VM`); first-poll ports without peer-card evidence are suppressed (see [NIC Health Monitor Configuration](./configuration/nic-health-monitor.md))
- NIC disappears from sysfs (fell off PCIe bus), confirmed across 3 consecutive polls → **Fatal** (`REPLACE_VM`)
- Card has fewer active ports than peer NICs of the same role (uncabled port anomaly) → **Fatal** (`REPLACE_VM`)

Management NICs (on NUMA nodes without GPUs, or carrying the host's default route) are automatically excluded. SR-IOV Virtual Functions are automatically filtered. No per-GPU-type configuration is required.

### Layer 2: Link Counter Detection (NIC Health Monitor DaemonSet)

Polls InfiniBand hardware counters every second for error rate violations:

**Fatal counters** (any increment triggers `REPLACE_VM`):
- `link_downed` — port training failure; active QPs cannot recover
- `excessive_buffer_overrun_errors` — lossless fabric contract violated
- `local_link_integrity_errors` — physical errors exceed hardware cap
- `rnr_nak_retry_err` — connection severed by retry exhaustion
- `symbol_error_fatal` > 120/hour — IBTA BER spec violation (10E-12 threshold)

**Non-fatal degradation** (monitored; 3 events in 1 hour escalates to `CONTACT_SUPPORT`):
- `symbol_error` > 10/sec — dirty fiber / PHY degradation
- `link_error_recovery` > 5/min — link flapping
- `roce_slow_restart` > 10/sec — grey failure straggler indicator
- `carrier_changes`, `port_rcv_errors`, and others

Counter breach state is persisted across pod restarts. Recovery events are emitted automatically when an admin resets counters (e.g. `perfquery -r` or `perfquery -R`) or when the node reboots.

### Layer 3: Syslog Detection (Syslog Health Monitor DaemonSet)

Watches journald for `mlx5_core` kernel messages that indicate driver/firmware failures invisible to link-state or counter polling:

**Fatal patterns** (trigger `REPLACE_VM`):
- `timeout. Will cause a leak of a command resource` — firmware hung, driver cannot issue commands
- `device's health compromised - reached miss count` — firmware heartbeat lost
- `mlx5_core.*unrecoverable hardware error` — hardware admission of failure
- `BUG: soft lockup` + mlx5 NAPI stack frames — CPU wedged in NIC poll loop

**Non-fatal diagnostic patterns** (provide correlation context for operators):
- `NETDEV WATCHDOG` TX queue stall, `TX timeout detected`, `RX timeout on channel`
- `Detected insufficient power on the PCIe slot`
- `Port module event.*High Temperature`
- `Cable unplugged`, `ACCESS_REG failed`

Repeated non-fatal syslog patterns (3 in 1 hour on the same node) escalate to `CONTACT_SUPPORT` via the Health Events Analyzer.

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

The monitors follow NVSentinel's **"Report Raw, Correlate Centrally"** pattern: each DaemonSet reports raw events as-is to the Platform Connector. The Health Events Analyzer handles correlation, repeat-failure escalation, and diagnostic context — with no code changes to the monitors required.

## What It Monitors

| Detection Layer | Data Source | Fatal Condition | Non-Fatal (Degradation) |
|----------------|-------------|-----------------|-------------------------|
| **Link State** | `/sys/class/infiniband/*/ports/*/state` | Port DOWN, device disappeared, uncabled anomaly | INIT/ARMED/Polling states (transient) |
| **Link Counter** | `/sys/class/infiniband/*/ports/*/counters/` | `link_downed`, `rnr_nak_retry_err`, buffer overrun, BER >120/hour | Symbol errors, link recovery, congestion |
| **Syslog** | journald / dmesg | `cmd_exec timeout`, health poll failed, unrecoverable, NAPI soft lockup | TX/RX timeouts, thermal, power, SFP events |

## Supported Hardware

> **Current scope**: Mellanox/NVIDIA InfiniBand and RoCE devices only. The architecture is designed to be extensible for future NIC vendors.

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

The NIC Health Monitor DaemonSet requires the metadata collector to be running on the same node (provides GPU↔NIC topology for management NIC exclusion and role classification). The monitor fails to start if the metadata file is missing — except when `nicInclusionRegexOverride` is set, which bypasses automatic discovery and the metadata dependency entirely.

For counter threshold customization, NIC exclusion patterns, and advanced configuration, see [NIC Health Monitor Configuration](./configuration/nic-health-monitor.md).

## Key Features

### Zero-Configuration NIC Role Classification
Automatically classifies NICs as Compute, Storage, or Management using a combination of NUMA locality, the `nvidia-smi topo -m` GPU↔NIC matrix, link layer (InfiniBand vs Ethernet), and default-route detection. Works across DGX, HGX, Grace-based (GB200/GH200), OEM, and cloud platforms without any per-GPU-type static configuration.

### Pre-Failure Detection
Tracks InfiniBand symbol error rates against the IBTA 10E-12 BER specification. Detects when FEC is approaching exhaustion and reports a fatal event before the link drops to zero — draining the node before the "cliff effect" causes 100% packet loss.

### Persistent State Across Restarts
Counter snapshots, breach flags, and port state are persisted to a hostPath-backed file. Recovery events are emitted automatically when a counter is cleared by an admin or on host reboot, preventing nodes from getting stuck in an unhealthy state after remediation.

### Uncabled Port Detection
Uses a card homogeneity check — comparing the active port count of each NIC card against its role-group peers — to detect cards with fewer ports than expected (indicating a failed or uncabled port) without requiring static configuration of expected port counts.
