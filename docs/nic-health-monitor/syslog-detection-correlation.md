# NIC Health Monitor: Syslog Detection

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [Driver/Firmware Interface Foundations](#3-driverfirmware-interface-foundations)
4. [Integration with Syslog Health Monitor](#4-integration-with-syslog-health-monitor)
5. [Monitored Kernel Patterns](#5-monitored-kernel-patterns)
6. [Advanced Heuristics](#6-advanced-heuristics)
7. [Repeat Failure Detection](#7-repeat-failure-detection)
8. [Configuration](#8-configuration)
9. [Event Management](#9-event-management)
10. [Monitoring Scope and Limitations](#10-monitoring-scope-and-limitations)
- [Appendix A: Quick Reference - Kernel Log Patterns](#appendix-a-quick-reference---kernel-log-patterns)
- [Appendix B: Health Events Analyzer Rules for NIC Monitoring](#appendix-b-health-events-analyzer-rules-for-nic-monitoring)

**Related Documents:**
- [Link State Detection](./link-state-detection.md) - UP/DOWN state monitoring
- [Link Counter Detection](./link-counter-detection.md) - Counter-based degradation monitoring

---

## 1. Overview

### 1.1 Problem Statement

NIC hardware polling monitors (state checks, counter reads) can miss critical failures that occur at the **driver/firmware interface** level. A NIC can appear healthy (link UP, counters normal) while the driver is completely unable to communicate with the firmware, leading to silent workload failures.

### 1.2 Scope of Syslog Detection

This document covers the **Syslog Health Monitor** component for NIC driver error monitoring, which detects:

- **Driver/Firmware communication failures** - `cmd_exec timeout`, firmware hangs
- **Hardware health check failures** - `health poll failed`, unrecoverable errors
- **PCIe bus errors** - Fatal PCIe errors, device disappearance
- **Thermal and power issues** - High temperature warnings, insufficient power
- **Network watchdog timeouts** - TX queue stalls

### 1.3 Why Syslog Monitoring is Essential

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  EXAMPLE: Firmware Failure (NIC Health Monitor sees NOTHING wrong)           │
│                                                                              │
│  NIC Health Monitor:     state=ACTIVE, phys_state=LinkUp  <── Looks healthy! │
│  Syslog Health Monitor:  "mlx5_core cmd_exec timeout"     <── DETECTS FAILURE│
│                                                                              │
│  Without monitoring kernel logs, this node would appear healthy while unable │
│  to process any NIC commands. Workloads would fail with mysterious timeouts. │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 1.4 Severity Model for Kernel Log Events

Following gpud's design and forensic analysis of `mlx5_core` telemetry, kernel log events are classified based on their **determinism of failure**:

| Severity      | Meaning                                       | Example                                                                        |
|---------------|-----------------------------------------------|--------------------------------------------------------------------------------|
| **Fatal**     | Deterministically fatal hardware/driver state | `cmd_exec timeout`, `unrecoverable hardware error`, `PCIe Fatal Error`         |
| **Non-Fatal** | Diagnostic context or transient issues        | `insufficient power`, `High Temperature`, `ACCESS_REG failed`, `module absent` |

> **Key Design Principle**: Only deterministically fatal events in the logs are raised as Fatal (`IsFatal=true`). All other events are raised as Non-Fatal (`IsFatal=false`) to provide diagnostic context without triggering immediate remediation.

### 1.5 Syslog Detection Overview Diagram

```
┌────────────────────────────────────────────────────────────────────────────┐
│                       SYSLOG DETECTION FLOW                                │
├────────────────────────────────────────────────────────────────────────────┤
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │                     DATA SOURCE                                      │  │
│  ├──────────────────────────────────────────────────────────────────────┤  │
│  │                                                                      │  │
│  │  journald / /var/log/journal                                         │  │
│  │  ├── Kernel ring buffer (dmesg)                                      │  │
│  │  │   ├── mlx5_core driver messages                                   │  │
│  │  │   ├── PCIe bus error messages                                     │  │
│  │  │   └── Network watchdog messages                                   │  │
│  │  └── Systemd unit logs                                               │  │
│  │                                                                      │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                   │                                        │
│                                   ▼                                        │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │              SYSLOG HEALTH MONITOR (Event-driven)                    │  │
│  ├──────────────────────────────────────────────────────────────────────┤  │
│  │                                                                      │  │
│  │  CHECK: SysLogsNICDriverError                                        │  │
│  │                                                                      │  │
│  │  FATAL PATTERNS (IsFatal=true, RecommendedAction=REPLACE_VM):        │  │
│  │  ├── mlx5_core cmd_exec timeout       → FATAL (control plane broken) │  │
│  │  ├── mlx5_core health poll failed     → FATAL (firmware dead)        │  │
│  │  ├── mlx5_core unrecoverable          → FATAL (hardware failure)     │  │
│  │  ├── PCIe Bus Error.*Fatal            → FATAL (bus collapsed)        │  │
│  │  └── NETDEV WATCHDOG.*mlx5_core       → FATAL (data path stalled)    │  │
│  │                                                                      │  │
│  │  NON-FATAL PATTERNS (IsFatal=false, diagnostic context):             │  │
│  │  ├── High Temperature                 → Non-Fatal (thermal)          │  │
│  │  ├── Detected insufficient power      → Non-Fatal (power negotiation)│  │
│  │  ├── module absent                    → Non-Fatal (SFP unplugged)    │  │
│  │  └── ACCESS_REG.*failed               → Non-Fatal (monitoring noise) │  │
│  │                                                                      │  │
│  │  EXISTING CHECKS (GPU):                                              │  │
│  │  ├── SysLogsXIDError                                                 │  │
│  │  ├── SysLogsSXIDError                                                │  │
│  │  └── SysLogsGPUFallenOff                                             │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                   │                                        │
│                                   ▼                                        │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │              RAW EVENTS → PLATFORM CONNECTOR → MongoDB               │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                   │                                        │
│                                   ▼                                        │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │            HEALTH EVENTS ANALYZER (Correlation Rules)                │  │
│  ├──────────────────────────────────────────────────────────────────────┤  │
│  │  • Fatal kernel logs trigger immediate REPLACE_VM                    │  │
│  │  • Non-fatal logs correlated with port state for diagnostics         │  │
│  │  • Provides diagnostic context for operator investigation            │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Architecture

### 2.1 Design Rationale: NVSentinel's "Report Raw, Correlate Centrally" Pattern

The syslog-health-monitor's NIC check follows NVSentinel's established architectural pattern where:

1. **Health Monitors (DaemonSets)** report **raw events as-is** to the Platform Connector
2. **Health Events Analyzer (Centralized Deployment)** performs all correlation, aggregation, and pattern detection
3. **MongoDB** serves as the source of truth for event history and correlation queries

| Architectural Principle     | Implementation                           | Purpose                                                      |
|-----------------------------|------------------------------------------|--------------------------------------------------------------|
| **Raw Event Reporting**     | Each kernel log match → immediate event  | Enables centralized correlation with full historical context |
| **Centralized Correlation** | Health Events Analyzer MongoDB pipelines | Flexible, configurable rules without monitor code changes    |
| **Temporal Correlation**    | Analyzer rules with time windows         | Correlates `cmd_exec timeout` + `link_down` within seconds   |

### 2.2 Component Responsibilities

| Component                  | Responsibility                                        | What It Does NOT Do                  |
|----------------------------|-------------------------------------------------------|--------------------------------------|
| **Syslog Health Monitor**  | Watch journald for NIC driver errors, send raw events | Pattern correlation, burst detection |
| **Health Events Analyzer** | Correlate events, detect patterns, escalate severity  | Direct log access                    |

### 2.3 NIC Driver Error Check Data Flow

```
Reads:
└── journald → Kernel log entries (via existing syslog-health-monitor infrastructure)

NEW CHECK: SysLogsNICDriverError
Pattern matching for:
├── mlx5_core cmd_exec timeout    → Firmware communication failure (FATAL)
├── mlx5_core health poll failed  → NIC health check failed (FATAL)
├── mlx5_core unrecoverable       → Hardware in error state (FATAL)
├── PCIe Bus Error.*Fatal         → PCIe link broken (FATAL)
├── NETDEV WATCHDOG.*mlx5_core    → TX stalled (FATAL)
├── module absent                 → Transceiver removed (Non-Fatal)
├── High Temperature              → Thermal warning (Non-Fatal)
└── pci_power_insufficient        → Power negotiation status (Non-Fatal)

Implementation:
└── Add NICDriverErrorHandler to existing syslog-health-monitor
    (similar to existing XIDHandler, SXIDHandler, GPUFallenHandler)

Emits: HealthEvents (IsFatal=true/false) → Platform Connector → MongoDB
```

### 2.4 System Context

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                 NVSentinel NIC SYSLOG MONITORING ARCHITECTURE                  │
├────────────────────────────────────────────────────────────────────────────────┤
│                                                                                │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │                       PER-NODE DAEMONSET                                 │  │
│  ├──────────────────────────────────────────────────────────────────────────┤  │
│  │                                                                          │  │
│  │  ┌─────────────────────────────┐  ┌─────────────────────────────────┐   │  │
│  │  │    NIC HEALTH MONITOR       │  │    SYSLOG HEALTH MONITOR        │   │  │
│  │  │    ══════════════════       │  │    ════════════════════         │   │  │
│  │  │                             │  │                                 │   │  │
│  │  │  DATA SOURCES:              │  │  DATA SOURCE:                   │   │  │
│  │  │  • /sys/class/infiniband/   │  │  • journald / /var/log/journal  │   │  │
│  │  │  • /sys/class/net/          │  │                                 │   │  │
│  │  │                             │  │  NEW CHECK:                     │   │  │
│  │  │  CHECKS:                    │  │  • SysLogsNICDriverError        │   │  │
│  │  │  • InfiniBandStateCheck     │  │    (mlx5_core cmd_exec timeout, │   │  │
│  │  │  • InfiniBandDegradationChk │  │     health poll failed,         │   │  │
│  │  │  • EthernetStateCheck       │  │     unrecoverable, PCIe fatal)  │   │  │
│  │  │  • EthernetDegradationCheck │  │                                 │   │  │
│  │  │                             │  │  EXISTING CHECKS:               │   │  │
│  │  │                             │  │  • SysLogsXIDError              │   │  │
│  │  │                             │  │  • SysLogsSXIDError             │   │  │
│  │  │                             │  │  • SysLogsGPUFallenOff          │   │  │
│  │  └─────────────┬───────────────┘  └───────────────┬─────────────────┘   │  │
│  │                │                                  │                     │  │
│  └────────────────┼──────────────────────────────────┼─────────────────────┘  │
│                   │                                  │                        │
│                   └────────────────┬─────────────────┘                        │
│                                    │                                          │
│                                    ▼                                          │
│  ┌──────────────────────────────────────┐                                     │
│  │       PLATFORM CONNECTOR             │                                     │
│  │       ══════════════════             │                                     │
│  │  • Receives raw events               │                                     │
│  │  • Persists to MongoDB               │                                     │
│  │  • Triggers downstream               │                                     │
│  └──────────────────┬───────────────────┘                                     │
│                     │                                                         │
│                     ▼                                                         │
│  ┌──────────────────────────────────────────────────────────────────────────┐ │
│  │                    HEALTH EVENTS ANALYZER                                │ │
│  │                    ══════════════════════                                │ │
│  │                                                                          │ │
│  │  OPTIONAL CORRELATION RULES:                                             │ │
│  │  • Correlate kernel log warnings with port state events                  │ │
│  │  • Provide diagnostic context for operators                              │ │
│  │                                                                          │ │
│  │  NOTE: Port state determines health (not kernel logs)                    │ │
│  │                                                                          │ │
│  │  OUTPUT:                                                                 │ │
│  │  • Diagnostic correlation for operator investigation                     │ │
│  └──────────────────────────────────────────────────────────────────────────┘ │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

### 2.5 Packet Path vs Driver/Firmware Interface Coverage

The combined monitoring approach ensures **no blind spots** by covering both layers:

```
┌─────────────────────────────────────────────────────────────────┐
│                     Coverage Map                                 │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  PACKET PATH (Data Moving Through NIC):                          │
│  ├── NIC Health Monitor State Check: Binary UP/DOWN              │
│  └── NIC Health Monitor Degradation Check: Error rates           │
│                                                                  │
│  DRIVER/FIRMWARE INTERFACE (OS <--> NIC Communication):          │
│  └── Syslog Health Monitor: mlx5_core errors, PCIe AER events   │
│                                                                  │
│  Driver/firmware failures can occur while packet path appears    │
│  healthy.                                                        │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

> **Why this matters**: Simple monitors that only check link state miss Driver/Firmware Interface failures, leading to silent failures where the hardware appears active but the data path is broken.

---

## 3. Driver/Firmware Interface Foundations

The NVIDIA Mellanox ConnectX series (ConnectX-5 through ConnectX-7) function as sophisticated, autonomous offload engines managing RDMA and complex flow steering. The host-to-NIC interaction is governed by a **split-driver model** where `mlx5_core` handles device initialization, health monitoring, and the command interface, while `mlx5_ib` or `mlx5_en` operate as protocol clients.

### 3.1 The Command Interface (CMD IF)

The primary control pathway is the Command Interface. The driver writes command blocks (e.g., `CREATE_MKEY`, `MODIFY_QP`) to PCI BAR-mapped memory and notifies the firmware via a "doorbell" register.

- **Fatality Mechanism**: If the firmware hangs, the driver's watchdog expires (typically 60s), logging `cmd_exec timeout`.
- **Resource Leaks**: Upon timeout, the driver intentionally "leaks" the command's DMA-mapped memory. Freeing it could lead to silent memory corruption if the firmware later writes to that physical address. This makes `cmd_exec` timeouts **irreversibly fatal** to the driver's device management capability.

### 3.2 Driver/Firmware Communication Diagram

```
┌────────────────────────────────────────────────────────────────────────────┐
│                    DRIVER/FIRMWARE COMMUNICATION                           │
├────────────────────────────────────────────────────────────────────────────┤
│                                                                            │
│  ┌──────────────────────────────────────────────────────────────────────┐  │
│  │                        HOST (Linux Kernel)                           │  │
│  │  ┌─────────────────┐  ┌─────────────────┐  ┌─────────────────┐       │  │
│  │  │   mlx5_core     │  │    mlx5_ib      │  │    mlx5_en      │       │  │
│  │  │  (Core Driver)  │  │  (IB Protocol)  │  │  (Ethernet)     │       │  │
│  │  │                 │  │                 │  │                 │       │  │
│  │  │ • Initialization│  │ • RDMA ops      │  │ • TCP/IP ops    │       │  │
│  │  │ • Health Monitor│  │ • QP management │  │ • Packet I/O    │       │  │
│  │  │ • Command IF    │  │                 │  │                 │       │  │
│  │  └────────┬────────┘  └────────┬────────┘  └────────┬────────┘       │  │
│  │           │                    │                    │                │  │
│  │           └────────────────────┼────────────────────┘                │  │
│  │                                │                                     │  │
│  │                                ▼                                     │  │
│  │              ┌─────────────────────────────────┐                     │  │
│  │              │    COMMAND INTERFACE            │                     │  │
│  │              │    ═══════════════════          │                     │  │
│  │              │                                 │                     │  │
│  │              │  1. Write command to            │                     │  │
│  │              │     PCI BAR memory              │                     │  │
│  │              │  2. Ring doorbell               │                     │  │
│  │              │  3. Wait for ownership bit      │                     │  │
│  │              │     (60s timeout)               │                     │  │
│  │              │                                 │                     │  │
│  │              │  IF timeout:                    │                     │  │
│  │              │  └── "cmd_exec timeout"         │                     │  │
│  │              │      logged to dmesg            │                     │  │
│  │              │      (IRREVERSIBLY FATAL)       │                     │  │
│  │              └───────────────┬─────────────────┘                     │  │
│  └──────────────────────────────┼───────────────────────────────────────┘  │
│                                 │                                          │
│                                 │ PCIe                                     │
│                                 │                                          │
│  ┌──────────────────────────────┼───────────────────────────────────────┐  │
│  │                              ▼                                       │  │
│  │  ┌────────────────────────────────────────────────────────────────┐  │  │
│  │  │                    NIC FIRMWARE                                │  │  │
│  │  │                    ════════════                                │  │  │
│  │  │                                                                │  │  │
│  │  │  • Processes commands (CREATE_MKEY, MODIFY_QP, etc.)          │  │  │
│  │  │  • Toggles ownership bit on completion                        │  │  │
│  │  │  • Health Syndrome register (polled by driver)                │  │  │
│  │  │  • Asynchronous Event Queue (EQ) for port changes, thermal    │  │  │
│  │  │                                                                │  │  │
│  │  │  IF firmware hangs:                                            │  │  │
│  │  │  └── Ownership bit never toggles                              │  │  │
│  │  │      Driver watchdog fires → "cmd_exec timeout"               │  │  │
│  │  │                                                                │  │  │
│  │  └────────────────────────────────────────────────────────────────┘  │  │
│  │                       CONNECTX NIC HARDWARE                          │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
```

### 3.3 Asynchronous Event Queue (EQ) and Health Poller

The NIC implements a dedicated ring buffer (EQ) for control plane events (e.g., thermal excursions, port changes). Additionally, a background **Health Poller** thread periodically (1s) reads a "Health Syndrome" register. This dual mechanism ensures that even if the interrupt system fails, the driver detects unrecoverable hardware syndromes.

### 3.4 The `mlx5_core cmd_exec timeout`

This is a severe driver-level error indicating **Driver/Firmware Interface failure**. ([Reference: RHEL mlx5_core issues](https://access.redhat.com/solutions/6955682))

**Mechanism:**
1. `mlx5_core` driver sends command to NIC firmware via mailbox
2. Driver waits for firmware to toggle "Ownership bit" indicating completion
3. If firmware has crashed/hung, bit never toggles
4. Driver's watchdog expires → logs `cmd_exec timeout` to `dmesg`

**Consequence**: Usually requires driver reload (`systemctl restart openibd`) or full node reboot. This is **always Fatal**—workload cannot proceed if it cannot issue commands to the NIC. The driver intentionally "leaks" command resources because it cannot safely reclaim memory without risk of corruption if the firmware eventually responds.

---

## 4. Integration with Syslog Health Monitor

NIC driver and firmware errors are monitored by adding a new check to the **existing syslog-health-monitor** DaemonSet. This follows the established NVSentinel pattern where all kernel log monitoring is centralized in the syslog-health-monitor.

### 4.1 Handler Architecture

The syslog-health-monitor already has a modular handler architecture for different check types:

| Check Name            | Handler                | Purpose                             |
|-----------------------|------------------------|-------------------------------------|
| `XIDErrorCheck`       | XID Handler            | GPU XID errors                      |
| `SXIDErrorCheck`      | SXID Handler           | NVSwitch SXID errors                |
| `GPUFallenOffCheck`   | GPU Fallen Handler     | GPU disappeared from bus            |
| `NICDriverErrorCheck` | **NIC Driver Handler** | **NEW: NIC driver/firmware errors** |

### 4.2 Configuration (values.yaml)

```yaml
syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
    - SysLogsNICDriverError  # NEW: NIC driver/firmware errors
```

### 4.3 Verification Command (Synthetic Fault Injection)

```bash
# Generate synthetic kernel message (requires root)
# This injects a fake mlx5_core error into dmesg
echo "<3>mlx5_core 0000:3b:00.0: cmd_exec timeout, status=0x10" > /dev/kmsg
```

### 4.4 NIC Driver Error Handler Algorithm

The `NICDriverErrorHandler` follows the same pattern as the existing XID handler.

**Log Line Processing Steps:**

1. **Receive kernel log line** from journald stream
2. **Match against NIC error patterns** (see Section 5 for pattern list):
   - Check if line matches any regex pattern (e.g., `mlx5_core.*cmd_exec timeout`)
   - If no match → skip line, return nil
3. **Determine severity**:
   - Look up pattern in severity table (Section 5.2)
   - Set `IsFatal` accordingly
4. **Extract PCI address** from message using regex (e.g., `0000:3b:00.0`)
5. **Map PCI address to NIC device** name (e.g., `mlx5_0`) by resolving `/sys/bus/pci/devices/<BDF>/driver` symlink
   - If the driver is `mlx5_core` → proceed (this is a NIC event)
   - If the driver is not `mlx5_core` (e.g., `nvidia` for GPU, `nvme` for NVMe) → **skip the event**
   - This filtering is critical for the `PCIe Bus Error.*Fatal` pattern, which is a generic kernel message not scoped to any specific device. Without BDF-based filtering, a GPU PCIe error would incorrectly produce a NIC health event.
6. **Generate HealthEvent** with:
   - `Agent = "syslog-health-monitor"`
   - `CheckName = "SysLogsNICDriverError"`
   - `EntityType = "NIC"`, `EntityValue = <device_name>`
7. **Send event** to Platform Connector (no local aggregation)

---

## 5. Monitored Kernel Patterns

The NIC driver error handler monitors for the following patterns:

### 5.1 Pattern Table

Following gpud's design, kernel log events are classified as **Non-Fatal (`IsFatal=false`)** by default. Only deterministic hardware state changes (port drops/flaps) that will cause workload failure are escalated to affect component health.

| Event Name               | Regex Pattern                                            | Severity      | Kernel Source                                                                                                                                                      | Systemic Impact                                                           |
|--------------------------|----------------------------------------------------------|---------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|---------------------------------------------------------------------------|
| `pci_power_insufficient` | `Detected insufficient power on the PCIe slot`           | **Non-Fatal** | [`events.c#L295-L299`](https://github.com/torvalds/linux/blob/ac9c34d1e45a4c25174ced4fc0cfc33ff3ed08c7/drivers/net/ethernet/mellanox/mlx5/core/events.c#L295-L299) | Power negotiation issue. Often transient during boot BIOS/BMC handshake.  |
| `port_module_high_temp`  | `Port module event.*High Temperature`                    | **Non-Fatal** | [`events.c#L252-L254`](https://github.com/torvalds/linux/blob/ac9c34d1e45a4c25174ced4fc0cfc33ff3ed08c7/drivers/net/ethernet/mellanox/mlx5/core/events.c#L252-L254) | Thermal warning. May indicate cooling issue or impending PHY throttling.  |
| `cmd_exec_timeout`       | `mlx5_core.*cmd_exec timeout`                            | **Fatal**     | [`cmd.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c)                                                             | Control plane broken. Driver cannot manage device. **Always Fatal**.      |
| `health_poll_failed`     | `mlx5_core.*health poll failed`                          | **Fatal**     | [`health.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/health.c)                                                       | Firmware heartbeat lost. Device is non-functional. **Always Fatal**.      |
| `unrecoverable_err`      | `mlx5_core.*unrecoverable`                               | **Fatal**     | [`health.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/health.c)                                                       | Hardware admission of failure. **Always Fatal**.                          |
| `pcie_fatal_error`       | `PCIe Bus Error.*Fatal`                                  | **Fatal**     | [`pcieaer-howto`](https://www.kernel.org/doc/html/latest/PCI/pcieaer-howto.html)                                                                                   | PCIe link broken. Typically triggers system reboot/NMI. **Always Fatal**. |
| `access_reg_failed`      | `mlx5_cmd_out_err.*ACCESS_REG.*failed`                   | **Non-Fatal** | [`cmd.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c)                                                             | Monitoring tool conflict on restricted PFs. **Non-Fatal Noise**.          |
| `netdev_watchdog`        | `NETDEV WATCHDOG:.*mlx5_core.*transmit queue.*timed out` | **Fatal**     | [`sch_generic.c`](https://github.com/torvalds/linux/blob/master/net/sched/sch_generic.c) (generic kernel mechanism)                                                | Data path stalled. Workload will fail. **High Probability Fatal**.        |
| `module_absent`          | `mlx5_core.*module.*absent`                              | **Non-Fatal** | [`events.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/events.c)                                                       | SFP/transceiver unplugged. Informational (though port will be DOWN).      |

**Full Log Line Examples:**

| Event Name               | Full Log Line Example                                                                                                 |
|--------------------------|-----------------------------------------------------------------------------------------------------------------------|
| `pci_power_insufficient` | `mlx5_core 0000:12:00.0: mlx5_pcie_event:299: Detected insufficient power on the PCIe slot (27W).`                    |
| `port_module_high_temp`  | `mlx5_core 0000:5c:00.0: Port module event[error]: module 0, Cable error, High Temperature`                           |
| `cmd_exec_timeout`       | `mlx5_core 0000:03:00.0: wait_func:964:(pid 112): ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource` |
| `health_poll_failed`     | `mlx5_core 0000:d2:00.0: poll_health:174: device's health compromised - reached miss count.`                          |
| `unrecoverable_err`      | `mlx5_core: INFO: synd 0x8: unrecoverable hardware error.`                                                            |
| `pcie_fatal_error`       | `PCIe Bus Error: severity=Uncorrectable (Fatal), type=Transaction Layer, (Receiver ID)`                               |
| `access_reg_failed`      | `mlx5_cmd_out_err:838: ACCESS_REG(0x805) op_mod(0x1) failed, status bad operation(0x2)`                               |
| `netdev_watchdog`        | `NETDEV WATCHDOG: eth0 (mlx5_core): transmit queue 0 timed out`                                                       |
| `module_absent`          | `mlx5_core 0000:12:00.0: Port module event: module 0, absent`                                                         |

> **Design Note**: While gpud internally treats many of these as non-fatal, NVSentinel escalates **deterministically fatal** signals to Fatal (`IsFatal=true`) to trigger proactive remediation (`REPLACE_VM`) before workload failure cascades. Non-fatal signals remain as `IsFatal=false` for diagnostic correlation.

### 5.2 Pattern Classification

Patterns are classified according to their operational impact:

| Category                  | Patterns                                                                       | Severity      | Recommended Action  |
|---------------------------|--------------------------------------------------------------------------------|---------------|---------------------|
| **Always Fatal (Device)** | `cmd_exec timeout`, `health poll failed`, `unrecoverable`                      | **Fatal**     | `REPLACE_VM`        |
| **Always Fatal (System)** | `PCIe Bus Error.*Fatal`                                                        | **Fatal**     | `REPLACE_VM`        |
| **Fatal (Service)**       | `NETDEV WATCHDOG`                                                              | **Fatal**     | `REPLACE_VM`        |
| **Non-Fatal / Evidence**  | `insufficient power`, `High Temperature`, `ACCESS_REG failed`, `module absent` | **Non-Fatal** | `NONE` (Diagnostic) |

> **Key Principle**: Kernel logs provide **diagnostic context**, not **remediation triggers**. The decision to drain/replace a node is based on actual port state (via link state detection), not log messages alone.

### 5.3 Diagnostic Commands

```bash
# Check for driver/firmware errors in kernel log
dmesg | grep -E "(mlx5_core|PCIe|AER)"
# Output:
# [ 42.123] mlx5_core 0000:3b:00.0: cmd_exec timeout, status=0x10

# Check journald for NIC errors
journalctl -k | grep -E "(mlx5_core|PCIe)"

# Watch for real-time NIC driver messages
journalctl -k -f | grep --line-buffered mlx5_core
```

---

## 6. Event Correlation (via Health Events Analyzer)

Following gpud's design, kernel log events for diagnostic patterns are emitted as **Non-Fatal** (`IsFatal=false`). The Health Events Analyzer can correlate these non-fatal events with port state changes to provide richer diagnostic context.

### 6.1 Correlation Examples

The Health Events Analyzer can correlate kernel log warnings with port state events:

| Kernel Log (Non-Fatal)                    | Port State Event   | Correlation Insight     |
|-------------------------------------------|--------------------|-------------------------|
| `High Temperature` + `health poll failed` | Port DOWN          | Thermal-induced failure |
| `cmd_exec timeout`                        | Port DOWN          | Driver/firmware failure |
| `insufficient power`                      | Port DOWN          | Power delivery issue    |
| `PCIe Bus Error`                          | Device disappeared | PCIe link failure       |

### 6.2 Noise Filtering

Some kernel messages can be filtered to reduce noise:

- **`ACCESS_REG failed`**: Common on systems with restricted PFs (DGX, Umbriel). Use `--infiniband-exclude-devices` to exclude problematic devices from monitoring.
- **`insufficient power`**: Often transient during BIOS/BMC power negotiation. Can be filtered by uptime context if needed.

---

## 7. Repeat Failure Detection

Unlike a local correlation engine, all pattern detection is handled by the **Health Events Analyzer**:

1. **Raw Event Flow**: Syslog-health-monitor sends raw `mlx5_core` non-fatal events to Platform Connector → MongoDB
2. **Correlation Rules**: Health Events Analyzer queries MongoDB and correlates warnings with port state events
3. **Example**: `cmd_exec timeout` at 10:00:01 + `port state=DOWN` at 10:00:05 → Analyzer provides correlated diagnostic context

### 7.1 Correlation Purpose

Following gpud's design, kernel log warnings are **not escalated to fatal**. Instead, the Health Events Analyzer correlates them with port state events for diagnostic context:

```
1. Diagnostic Correlation:
   ├── Purpose: Link kernel log warnings to port state changes
   ├── Input: Non-fatal events (kernel logs) + Port state events
   └── Output: Correlated diagnostic information for operators

2. Port Drop/Flap Detection (via Link State Detection):
   ├── Source: NIC Health Monitor (port state monitoring)
   ├── Condition: Port drops or flaps detected via sysfs
   └── Effect: Component health set to Unhealthy + REPLACE_VM

3. Sticky Window (similar to gpud):
   ├── Purpose: Keep component unhealthy for stabilization period after port recovery
   ├── Effect: Prevents confusing Unhealthy→Healthy flips
   └── Window: Configurable (e.g., 10 minutes after port recovery)
```

> **Key Design Principle**: Kernel log events for diagnostic patterns remain Non-Fatal (`IsFatal=false`). Component health changes are triggered by **port state** (link-state-detection), not by kernel logs.

### 7.2 Repeat Failure Detection Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                   REPEAT FAILURE DETECTION (via Health Events Analyzer)          │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  TIME        EVENT SOURCE           EVENT TYPE                                   │
│  ────        ────────────           ──────────                                   │
│                                                                                  │
│  T+0:00      Syslog Monitor         cmd_exec timeout (mlx5_0)                   │
│              │                                                                   │
│              └──────────────────────────────────────────────────────────────────►│
│                                                                                  │
│  T+0:05      NIC Health Monitor     state=DOWN (mlx5_0_port1)                   │
│              │                                                                   │
│              └──────────────────────────────────────────────────────────────────►│
│                                                                                  │
│                                     ┌─────────────────────────────────────┐     │
│                    MongoDB ◄────────┤    RAW EVENTS PERSISTED             │     │
│                                     └─────────────────────────────────────┘     │
│                                                     │                            │
│                                                     ▼                            │
│                    ┌────────────────────────────────────────────────────────┐   │
│                    │              HEALTH EVENTS ANALYZER                     │   │
│                    │                                                         │   │
│                    │  Rule: NICDriverErrorCorrelation                        │   │
│                    │                                                         │   │
│                    │  MongoDB Aggregation (pseudo-query for illustration):   │   │
│                    │    db.health_events.find({                             │   │
│                    │      timestamp: {$gt: NOW() - 10s},                    │   │
│                    │      nodename: 'node-xyz',                             │   │
│                    │      $or: [{message: /cmd_exec timeout/},              │   │
│                    │            {message: /state=DOWN/}]                    │   │
│                    │    })                                                   │   │
│                    │                                                         │   │
│                    │  Result:                                                │   │
│                    │    1. cmd_exec timeout at T+0:00                       │   │
│                    │    2. state=DOWN at T+0:05                             │   │
│                    │                                                         │   │
│                    │  ┌───────────────────────────────────────────────────┐ │   │
│                    │  │  CORRELATION DETECTED                             │ │   │
│                    │  │                                                   │ │   │
│                    │  │  Pattern: cmd_exec timeout + link_down            │ │   │
│                    │  │  within 5 seconds                                 │ │   │
│                    │  │                                                   │ │   │
│                    │  │  OUTPUT: DIAGNOSTIC CORRELATION                   │ │   │
│                    │  │  Message: "Driver warning correlated with         │ │   │
│                    │  │           port state change"                      │ │   │
│                    │  │  Purpose: Context for operator investigation      │ │   │
│                    │  └───────────────────────────────────────────────────┘ │   │
│                    └────────────────────────────────────────────────────────┘   │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 8. Configuration

### 8.1 Syslog Health Monitor Configuration

```yaml
# values.yaml for syslog-health-monitor
syslog-health-monitor:
  enabledChecks:
    - SysLogsXIDError
    - SysLogsSXIDError
    - SysLogsGPUFallenOff
    - SysLogsNICDriverError  # NEW: NIC driver/firmware errors
```

### 8.2 Health Events Analyzer Configuration (for NIC rules)

```yaml
# health-events-analyzer/values.yaml additions
enableRepeatedNICLinkFlapRule: true
enableRepeatedNICDegradationRule: true
enableNICDriverErrorCorrelationRule: true
```

---

## 9. Event Management

### 9.1 Event Construction

Events are emitted with severity based on their determinism of failure:

**Example Event Fields (Fatal - cmd_exec timeout):**

| Field             | Value                                                   |
|-------------------|---------------------------------------------------------|
| Agent             | `syslog-health-monitor`                                 |
| CheckName         | `SysLogsNICDriverError`                                 |
| ComponentClass    | `NIC`                                                   |
| Message           | "mlx5_core 0000:3b:00.0: cmd_exec timeout, status=0x10" |
| IsFatal           | `true`                                                  |
| IsHealthy         | `false`                                                 |
| RecommendedAction | `REPLACE_VM`                                            |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}]`          |

**Example Event Fields (Non-Fatal - module absent):**

| Field             | Value                                                         |
|-------------------|---------------------------------------------------------------|
| Agent             | `syslog-health-monitor`                                       |
| CheckName         | `SysLogsNICDriverError`                                       |
| ComponentClass    | `NIC`                                                         |
| Message           | "mlx5_core 0000:12:00.0: Port module event: module 0, absent" |
| IsFatal           | `false`                                                       |
| IsHealthy         | `false`                                                       |
| RecommendedAction | `NONE` (informational)                                        |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}]`                |

### 9.2 Event Purpose

Kernel log events serve as both **direct failure signals** and **diagnostic context**:

| Event Severity | Purpose                               | Action                                                              |
|----------------|---------------------------------------|---------------------------------------------------------------------|
| **Fatal**      | Deterministic hardware/driver failure | Immediate remediation via `REPLACE_VM`                              |
| **Non-Fatal**  | Diagnostic information / Evidence     | Logged for investigation; provides context for state-based failures |

> **Key Design Principle**: Deterministically fatal events in logs (cmd_exec timeout, unrecoverable hardware error) trigger `REPLACE_VM` directly. Non-fatal events (insufficient power, module absent) remain as `IsFatal=false` for diagnostic correlation.

---

## 10. Monitoring Scope and Limitations

### 10.1 What This Monitor CAN Detect

The syslog monitoring capability operates at the **driver and kernel level only**:

| Data Source        | Monitor               | Detection Capability                                        |
|--------------------|-----------------------|-------------------------------------------------------------|
| **journald/dmesg** | Syslog Health Monitor | Driver errors (`mlx5_core`), firmware failures, PCIe events |

### 10.2 What This Monitor CANNOT Detect

**Application-level logs and remote failures are out of scope:**

| Category                  | Examples                                              | Why Out of Scope                       |
|---------------------------|-------------------------------------------------------|----------------------------------------|
| **Application logs**      | RDMA library errors, framework failures               | Not in kernel ring buffer              |
| **Remote node failures**  | Peer node crash, peer NIC hang                        | No local kernel/hardware signature     |
| **Fabric issues**         | Switch failures, routing black holes                  | Requires fabric-level monitoring       |
| **Subnet Manager issues** | SM unreachable (may partially detect via LID changes) | Fabric management layer, not local NIC |

### 10.3 Hardware Failures and Application Impact

The following table shows which hardware failures this monitor detects and how they may impact applications:

| Hardware Failure    | Detection Method                      | Potential Application Impact  |
|---------------------|---------------------------------------|-------------------------------|
| **Firmware freeze** | Kernel log: `cmd_exec timeout`        | All NIC operations stall      |
| **Driver crash**    | Kernel log: `health poll failed`      | NIC becomes unusable          |
| **PCIe errors**     | Kernel log: mlx5_core PCIe AER events | Device may become unavailable |
| **TX stall**        | Kernel log: `NETDEV WATCHDOG`         | Network transmission fails    |

### 11.4 Event Severity Classification

Kernel log events are classified by their determinism of failure:

| Kernel Log Event               | Severity      | Purpose                       | Recommended Action |
|--------------------------------|---------------|-------------------------------|--------------------|
| `mlx5_core cmd_exec timeout`   | **Fatal**     | Control plane broken          | `REPLACE_VM`       |
| `mlx5_core health poll failed` | **Fatal**     | Firmware heartbeat lost       | `REPLACE_VM`       |
| `mlx5_core unrecoverable`      | **Fatal**     | Hardware admission of failure | `REPLACE_VM`       |
| `PCIe Bus Error.*Fatal`        | **Fatal**     | PCIe link broken              | `REPLACE_VM`       |
| `NETDEV WATCHDOG`              | **Fatal**     | Data path stalled             | `REPLACE_VM`       |
| `Detected insufficient power`  | **Non-Fatal** | Power negotiation status      | `NONE`             |
| `High Temperature`             | **Non-Fatal** | Thermal warning               | `NONE`             |
| `module absent`                | **Non-Fatal** | SFP unplugged                 | `NONE`             |
| `ACCESS_REG failed`            | **Non-Fatal** | Monitoring noise              | `NONE`             |

> **Key Principle**: Deterministically fatal events in logs trigger `REPLACE_VM`. Diagnostic logs remain as Non-Fatal (`IsFatal=false`) for diagnostic context.

---

## Appendix A: Quick Reference - Kernel Log Patterns

The following patterns are monitored in the kernel ring buffer (dmesg/kmsg):

### Kernel Log Pattern Summary

| Pattern                                   | Severity      | Meaning                         | Action                        |
|-------------------------------------------|---------------|---------------------------------|-------------------------------|
| `mlx5_core.*cmd_exec timeout`             | **Fatal**     | Firmware/driver command timeout | `REPLACE_VM`                  |
| `mlx5_core.*health poll failed`           | **Fatal**     | Health check missed             | `REPLACE_VM`                  |
| `mlx5_core.*unrecoverable`                | **Fatal**     | Hardware error detected         | `REPLACE_VM`                  |
| `PCIe Bus Error.*Fatal`                   | **Fatal**     | PCIe error logged               | `REPLACE_VM`                  |
| `NETDEV WATCHDOG:.*mlx5_core.*timed out`  | **Fatal**     | TX queue timeout                | `REPLACE_VM`                  |
| `mlx5_core.*Detected insufficient power`  | **Non-Fatal** | Power negotiation status        | Investigate; can be transient |
| `mlx5_core.*Port module event.*High Temp` | **Non-Fatal** | Thermal warning                 | Investigate; check cooling    |
| `mlx5_cmd_out_err.*ACCESS_REG.*failed`    | **Non-Fatal** | Restricted PF access            | Filter/Ignore                 |
| `mlx5_core.*module.*absent`               | **Non-Fatal** | SFP/transceiver unplugged       | Monitor port state            |

### Design Principle

| Source                                        | IsFatal | Recommended Action | Purpose                            |
|-----------------------------------------------|---------|--------------------|------------------------------------|
| **Deterministic Logs**                        | `true`  | `REPLACE_VM`       | Fatal driver/firmware condition    |
| **Port State Changes** (link-state-detection) | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Fatal Counters** (link-counter-detection)   | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Diagnostic Logs**                           | `false` | `NONE`             | Evidence/context for investigation |

> **Key Insight**: Kernel logs for deterministic failures (cmd_exec timeout, etc.) are **Fatal (`IsFatal=true`)** with `RecommendedAction_REPLACE_VM`. Diagnostic logs (insufficient power, High Temperature, module absent) are **Non-Fatal (`IsFatal=false`)**. State and counter conditions are also **Fatal (`IsFatal=true`)** with `RecommendedAction_REPLACE_VM`.

---

## Appendix B: Health Events Analyzer Rules for NIC Monitoring (Optional)

The following example rules show how the Health Events Analyzer *could* implement NIC-specific correlation logic. These rules are **optional** and follow the same pattern as the existing XID correlation rules.

> **Design Note**: Following gpud's design, the primary health determination is via **port state monitoring** (link-state-detection), not kernel log events or analyzer rules. These rules provide optional diagnostic correlation, not automatic remediation triggers.

### B.1 Link Flap Detection Rule

```toml
[[rules]]
name = "RepeatedNICLinkFlap"
description = "Detect if link_downed occurred 3+ times within 10 minutes on same NIC port"
recommended_action = "REPLACE_VM"
message = "NIC port flapping detected - unstable hardware/cable"
evaluate_rule = {{ .Values.enableRepeatedNICLinkFlapRule }}
stage = [
  # Match events from last 10 minutes
  '''
  {
    "$match": {
      "$expr": {
        "$and": [
          {"$gte": ["$healthevent.generatedtimestamp.seconds", {"$subtract": ["this.healthevent.generatedtimestamp.seconds", 600]}]},
          {"$lte": ["$healthevent.generatedtimestamp.seconds", "this.healthevent.generatedtimestamp.seconds"]}
        ]
      }
    }
  }
  ''',
  # Filter for NIC health events with link_downed on same node and port
  '''
  {
    "$match": {
      "healthevent.agent": "nic-health-monitor",
      "healthevent.nodename": "this.healthevent.nodename",
      "healthevent.message": {"$regex": "link_downed"},
      "$expr": {
        "$gt": [
          {"$size": {"$setIntersection": [
            {"$map": {"input": {"$filter": {"input": {"$ifNull": ["$healthevent.entitiesimpacted", []]}, "cond": {"$eq": ["$$this.entitytype", "NICPort"]}}}, "in": "$$this.entityvalue"}},
            {"$map": {"input": {"$filter": {"input": "this.healthevent.entitiesimpacted", "cond": {"$eq": ["$$this.entitytype", "NICPort"]}}}, "in": "$$this.entityvalue"}}
          ]}},
          0
        ]
      }
    }
  }
  ''',
  '{"$count": "count"}',
  '{"$match": {"count": {"$gte": 3}}}'
]
```

### B.2 Repeated NIC Degradation Escalation Rule

```toml
[[rules]]
name = "RepeatedNICDegradation"
description = "Escalate to fatal if 5+ non-fatal NIC degradation events in 24 hours on same port"
recommended_action = "REPLACE_VM"
message = "Repeated NIC degradation indicates hardware issue"
evaluate_rule = {{ .Values.enableRepeatedNICDegradationRule }}
stage = [
  # Match non-fatal NIC events from last 24 hours on same node/port
  '''
  {
    "$match": {
      "$expr": {
        "$and": [
          {"$gte": ["$healthevent.generatedtimestamp.seconds", {"$subtract": ["this.healthevent.generatedtimestamp.seconds", 86400]}]},
          {"$lte": ["$healthevent.generatedtimestamp.seconds", "this.healthevent.generatedtimestamp.seconds"]}
        ]
      }
    }
  }
  ''',
  '''
  {
    "$match": {
      "healthevent.agent": "nic-health-monitor",
      "healthevent.isfatal": false,
      "healthevent.ishealthy": false,
      "healthevent.nodename": "this.healthevent.nodename",
      "$expr": {
        "$gt": [
          {"$size": {"$setIntersection": [
            {"$map": {"input": {"$filter": {"input": {"$ifNull": ["$healthevent.entitiesimpacted", []]}, "cond": {"$eq": ["$$this.entitytype", "NICPort"]}}}, "in": "$$this.entityvalue"}},
            {"$map": {"input": {"$filter": {"input": "this.healthevent.entitiesimpacted", "cond": {"$eq": ["$$this.entitytype", "NICPort"]}}}, "in": "$$this.entityvalue"}}
          ]}},
          0
        ]
      }
    }
  }
  ''',
  '{"$count": "count"}',
  '{"$match": {"count": {"$gte": 5}}}'
]
```

### B.3 Configuration in values.yaml

These rules would be added to the `health-events-analyzer` Helm chart values:

```yaml
# health-events-analyzer/values.yaml additions
enableRepeatedNICLinkFlapRule: true
enableRepeatedNICDegradationRule: true
```

---

## References

### Linux Kernel & Driver
1. [sysfs-class-infiniband (Linux Kernel)](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
2. [RHEL8 mlx5_core Stack Overflow (Red Hat)](https://access.redhat.com/solutions/6955682)
3. [mlx5_core cmd.c - Command Interface (Linux Kernel)](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c) - `cmd_exec timeout`, `ACCESS_REG failed`
4. [mlx5_core health.c - Health Poller (Linux Kernel)](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/health.c) - `health poll failed`, `unrecoverable`
5. [mlx5_core events.c - Async Events (Linux Kernel)](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/events.c) - `insufficient power`, `High Temperature`, `module absent`
6. [PCIe AER HOWTO (Linux Kernel)](https://www.kernel.org/doc/html/latest/PCI/pcieaer-howto.html) - PCIe Bus Error log format and BDF identification

### Vendor Documentation
3. [ibdiagnet User Manual (NVIDIA)](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf)

---
