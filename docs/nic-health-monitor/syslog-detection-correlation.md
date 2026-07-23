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
    - [10.4 Event Severity Classification](#104-event-severity-classification)
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

- **Driver/Firmware communication failures** - command timeouts (`timeout. Will cause a leak of a command resource`), firmware hangs
- **Hardware health check failures** - `health poll failed`, unrecoverable errors
- **Driver/firmware-level PCIe events** - surfaced via `mlx5_core` (e.g., insufficient power); fatal PCIe link loss / device disappearance is covered by [link-state-detection](./link-state-detection.md)
- **Thermal and power issues** - High temperature warnings, insufficient power
- **Network watchdog timeouts** - TX queue stalls (Non-Fatal diagnostic context; auto-recovery via `mlx5e_tx_timeout`)

### 1.3 Why Syslog Monitoring is Essential

```
┌──────────────────────────────────────────────────────────────────────────────┐
│  EXAMPLE: Firmware Failure (NIC Health Monitor sees NOTHING wrong)           │
│                                                                              │
│  NIC Health Monitor:     state=ACTIVE, phys_state=LinkUp  <── Looks healthy! │
│  Syslog Health Monitor:  "timeout. Will cause a leak..."  <── DETECTS FAILURE│
│                                                                              │
│  Without monitoring kernel logs, this node would appear healthy while unable │
│  to process any NIC commands. Workloads would fail with mysterious timeouts. │
└──────────────────────────────────────────────────────────────────────────────┘
```

### 1.4 Severity Model for Kernel Log Events

Following gpud's design and forensic analysis of `mlx5_core` telemetry, kernel log events are classified based on their **determinism of failure**:

| Severity      | Meaning                                       | Example                                                                           |
|---------------|-----------------------------------------------|-----------------------------------------------------------------------------------|
| **Fatal**     | Deterministically fatal hardware/driver state | `timeout. Will cause a leak`, `unrecoverable hardware error`                      |
| **Non-Fatal** | Diagnostic context or transient issues        | `insufficient power`, `High Temperature`, `ACCESS_REG failed`, `module unplugged` |

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
│  │  │   ├── mlx5_core driver/firmware messages                          │  │
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
│  │  ├── mlx5_core command timeout leak   → FATAL (control plane broken) │  │
│  │  ├── mlx5_core health poll failed     → FATAL (firmware dead)        │  │
│  │  └── mlx5_core unrecoverable          → FATAL (hardware failure)     │  │
│  │                                                                      │  │
│  │  NON-FATAL PATTERNS (IsFatal=false, diagnostic context):             │  │
│  │  ├── High Temperature                 → Non-Fatal (thermal)          │  │
│  │  ├── Detected insufficient power      → Non-Fatal (power negotiation)│  │
│  │  ├── module unplugged                 → Non-Fatal (SFP unplugged)    │  │
│  │  ├── ACCESS_REG.*failed               → Non-Fatal (monitoring noise) │  │
│  │  └── NETDEV WATCHDOG.*mlx5_core       → Non-Fatal (TX stall)        │  │
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
| **Temporal Correlation**    | Analyzer rules with time windows         | Correlates command timeout + `link_down` within seconds      |

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
├── mlx5_core command timeout leak → Firmware communication failure (FATAL)
├── mlx5_core health poll failed  → NIC health check failed (FATAL)
├── mlx5_core unrecoverable       → Hardware in error state (FATAL)
├── module unplugged              → Transceiver removed (Non-Fatal)
├── NETDEV WATCHDOG.*mlx5_core    → TX stall, auto-recovery (Non-Fatal)
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
│  │  │  • InfiniBandStateCheck     │  │    (mlx5_core command timeout,  │   │  │
│  │  │  • InfiniBandDegradationChk │  │     health poll failed,         │   │  │
│  │  │  • EthernetStateCheck       │  │     unrecoverable)              │   │  │
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
│  └── Syslog Health Monitor: mlx5_core driver/firmware events    │
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

- **Fatality Mechanism**: If the firmware hangs, the driver's watchdog expires (typically 60s), logging `timeout. Will cause a leak of a command resource`.
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
│  │              │  └── "timeout. Will cause..."   │                     │  │
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
│  │  │      Driver watchdog fires → "timeout. Will cause..."         │  │  │
│  │  │                                                                │  │  │
│  │  └────────────────────────────────────────────────────────────────┘  │  │
│  │                       CONNECTX NIC HARDWARE                          │  │
│  └──────────────────────────────────────────────────────────────────────┘  │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
```

### 3.3 Asynchronous Event Queue (EQ) and Health Poller

The NIC implements a dedicated ring buffer (EQ) for control plane events (e.g., thermal excursions, port changes). Additionally, a background **Health Poller** thread periodically (1s) reads a "Health Syndrome" register. This dual mechanism ensures that even if the interrupt system fails, the driver detects unrecoverable hardware syndromes.

### 3.4 The `mlx5_core` Command Timeout Resource Leak

This is a severe driver-level error indicating **Driver/Firmware Interface failure**. ([Reference: RHEL mlx5_core issues](https://access.redhat.com/solutions/6955682))

**Mechanism:**
1. `mlx5_core` driver sends command to NIC firmware via mailbox
2. Driver waits for firmware to toggle "Ownership bit" indicating completion
3. If firmware has crashed/hung, bit never toggles
4. Driver's watchdog expires → logs `timeout. Will cause a leak of a command resource` to `dmesg`

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
echo "<3>mlx5_core 0000:3b:00.0: ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource" > /dev/kmsg
```

### 4.4 NIC Driver Error Handler Algorithm

The `NICDriverErrorHandler` follows the same pattern as the existing XID handler.

**Log Line Processing Steps:**

1. **Receive kernel log line** from journald stream
2. **Match against NIC error patterns** (see Section 5 for pattern list):
   - Pattern definitions (regex, severity, recommended action, description) are owned by Go code
   - The TOML file only selects supported pattern names and enables/disables them (see Section 8.3)
   - Check if line matches any regex pattern (e.g., `mlx5_core.*timeout\. Will cause a leak of a command resource`)
   - If no match → skip line, return nil
3. **Determine severity**:
   - Look up the pattern definition in code (summarized in Section 5.2)
   - Set `IsFatal` accordingly
4. **Extract PCI address** from message using regex (e.g., `0000:3b:00.0`)
5. **Best-effort NIC entity enrichment** by resolving `/sys/bus/pci/devices/<BDF>/driver` symlink
   - If the driver is `mlx5_core` → attach NIC entity (e.g., `mlx5_0`) to the event
   - Otherwise → emit the event with no NIC entity attached
   - The handler does **not** drop events based on driver type. Supported patterns are mlx5-specific by regex (e.g., they require `mlx5_core` or `mlx5_cmd_out_err` in the message), so non-mlx5 matches are not expected.
6. **Generate HealthEvent** with:
   - `Agent = "syslog-health-monitor"`
   - `CheckName = "SysLogsNICDriverError"`
   - `EntityType = "NIC"`, `EntityValue = <device_name>`
7. **Send event** to Platform Connector (no local aggregation)

> **Why live sysfs lookup instead of metadata collector**: The metadata collector publishes relatively static GPU/NVSwitch inventory. The BDF itself is usually stable during a boot, but the kernel state used for NIC enrichment is not: `/sys/bus/pci/devices/<BDF>/driver` can disappear or reappear when `mlx5_core` is unloaded, reloaded, rebound, or recovered, and the exposed `infiniband/*` or `net/*` names can be recreated. The syslog handler therefore reads the current sysfs state at event-processing time. This lookup is best-effort enrichment only; if it fails, the raw syslog event is still emitted without a `NIC` entity.

---

## 5. Monitored Kernel Patterns

The NIC driver error handler monitors for the following patterns:

### 5.1 Pattern Table

Following gpud's design, kernel log events are classified as **Non-Fatal (`IsFatal=false`)** by default. Only deterministic hardware state changes (port drops/flaps) that will cause workload failure are escalated to affect component health.

| Event Name               | Regex Pattern                                                  | Severity      | Kernel Source                                                                                                                                                      | Systemic Impact                                                          |
|--------------------------|----------------------------------------------------------------|---------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------|
| `pci_power_insufficient` | `mlx5_core.*Detected insufficient power on the PCIe slot`      | **Non-Fatal** | [`events.c#L295-L299`](https://github.com/torvalds/linux/blob/ac9c34d1e45a4c25174ced4fc0cfc33ff3ed08c7/drivers/net/ethernet/mellanox/mlx5/core/events.c#L295-L299) | Power negotiation issue. Often transient during boot BIOS/BMC handshake. |
| `port_module_high_temp`  | `mlx5_core.*Port module event.*High Temperature`               | **Non-Fatal** | [`events.c#L252-L254`](https://github.com/torvalds/linux/blob/ac9c34d1e45a4c25174ced4fc0cfc33ff3ed08c7/drivers/net/ethernet/mellanox/mlx5/core/events.c#L252-L254) | Thermal warning. May indicate cooling issue or impending PHY throttling. |
| `cmd_exec_timeout`       | `mlx5_core.*timeout\. Will cause a leak of a command resource` | **Fatal**     | [`cmd.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c)                                                             | Control plane broken. Driver cannot manage device. **Always Fatal**.     |
| `health_poll_failed`     | `mlx5_core.*device's health compromised.*reached miss count`   | **Fatal**     | [`health.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/health.c)                                                       | Firmware heartbeat lost. Device is non-functional. **Always Fatal**.     |
| `unrecoverable_err`      | `mlx5_core.*unrecoverable hardware error`                      | **Fatal**     | [`health.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/health.c)                                                       | Hardware admission of failure. **Always Fatal**.                         |
| `access_reg_failed`      | `mlx5_cmd_out_err.*ACCESS_REG.*failed`                         | **Non-Fatal** | [`cmd.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c)                                                             | Monitoring tool conflict on restricted PFs. **Non-Fatal Noise**.         |
| `netdev_watchdog`        | `(NETDEV WATCHDOG.*mlx5_core\|mlx5_core.*NETDEV WATCHDOG).*transmit queue.*timed out` | **Non-Fatal** | [`sch_generic.c`](https://github.com/torvalds/linux/blob/master/net/sched/sch_generic.c) (generic kernel mechanism)                                                | TX queue stall with auto-recovery via `mlx5e_tx_timeout`. **Non-Fatal**. Matches both message orders: kernel commit `e316dd1cf135` (v6.8, backported to 6.6.47/6.1.107) moved the driver prefix in front of `NETDEV WATCHDOG`. |
| `module_unplugged`       | `mlx5_core.*Port module event.*Cable unplugged`                | **Non-Fatal** | [`events.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/events.c)                                                       | SFP/transceiver unplugged. Informational (though port will be DOWN).     |
| `mlx5_tx_timeout_detected` | `mlx5_core.*TX timeout detected`                             | **Non-Fatal** | [`en_main.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/en_main.c) (`mlx5e_tx_timeout`)                                | Driver-reported TX timeout. Stable across kernel versions (unlike the `NETDEV WATCHDOG` core message) and re-fires each watchdog cycle while a queue stays stuck. Driver attempts auto-recovery. |
| `mlx5_rx_timeout_detected` | `mlx5_core.*RX timeout on channel`                           | **Non-Fatal** | [`reporter_rx.c`](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/en/reporter_rx.c) (via `mlx5e_health_report`)              | Driver-reported RX timeout. Same class as the TX variant; known onset flavor of NAPI poll-loop wedges. Driver attempts auto-recovery. |
| `mlx5_napi_soft_lockup`  | Stateful (see §5.4): `BUG: soft lockup` header + `mlx5e_(poll_ico_cq\|napi_poll)\+0x` | **Fatal**     | [`watchdog.c`](https://github.com/torvalds/linux/blob/master/kernel/watchdog.c) + mlx5 NAPI stack frames                                                          | CPU stuck in the mlx5 NAPI poll loop. Driver is wedged; auto-recovery has failed by definition. Node needs reboot/replacement. **Always Fatal**. |

**Full Log Line Examples:**

| Event Name               | Full Log Line Example                                                                                                 |
|--------------------------|-----------------------------------------------------------------------------------------------------------------------|
| `pci_power_insufficient` | `mlx5_core 0000:12:00.0: mlx5_pcie_event:299: Detected insufficient power on the PCIe slot (27W).`                    |
| `port_module_high_temp`  | `mlx5_core 0000:5c:00.0: Port module event[error]: module 0, Cable error, High Temperature`                           |
| `cmd_exec_timeout`       | `mlx5_core 0000:03:00.0: wait_func:964:(pid 112): ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource` |
| `health_poll_failed`     | `mlx5_core 0000:d2:00.0: poll_health:174: device's health compromised - reached miss count.`                          |
| `unrecoverable_err`      | `mlx5_core: INFO: synd 0x8: unrecoverable hardware error.`                                                            |
| `access_reg_failed`      | `mlx5_cmd_out_err:838: ACCESS_REG(0x805) op_mod(0x1) failed, status bad operation(0x2)`                               |
| `netdev_watchdog`        | `NETDEV WATCHDOG: eth0 (mlx5_core): transmit queue 0 timed out` (pre-6.8) / `mlx5_core 0000:65:00.0 ens15np0: NETDEV WATCHDOG: CPU: 94: transmit queue 3 timed out 5032 ms` (6.8+) |
| `module_unplugged`       | `mlx5_core 0000:12:00.0: Port module event: module 0, Cable unplugged`                                                |
| `mlx5_tx_timeout_detected` | `mlx5_core 0000:65:00.0 ens15np0: TX timeout detected`                                                              |
| `mlx5_rx_timeout_detected` | `mlx5_core 0000:65:00.0 ens15np0: RX timeout on channel: 20, ICOSQ: 0x1ee0, RQ: 0x1e43, CQ: 0x3ea6`                 |
| `mlx5_napi_soft_lockup`  | `watchdog: BUG: soft lockup - CPU#94 stuck for 226906s! [swapper/94:0]` followed by `RIP: 0010:mlx5e_poll_ico_cq+0x8b/0x1a0 [mlx5_core]` |

> **Design Note**: While gpud internally treats many of these as non-fatal, NVSentinel escalates **deterministically fatal** signals to Fatal (`IsFatal=true`) to trigger proactive remediation (`REPLACE_VM`) before workload failure cascades. Non-fatal signals remain as `IsFatal=false` for diagnostic correlation.

### 5.2 Pattern Classification

Patterns are classified according to their operational impact:

| Category                  | Patterns                                                                                             | Severity      | Recommended Action  |
|---------------------------|------------------------------------------------------------------------------------------------------|---------------|---------------------|
| **Always Fatal (Device)** | command timeout with resource leak, `health poll failed`, `unrecoverable`, `mlx5_napi_soft_lockup`   | **Fatal**     | `REPLACE_VM`        |
| **Non-Fatal / Evidence**  | `insufficient power`, `High Temperature`, `ACCESS_REG failed`, `module unplugged`, `NETDEV WATCHDOG`, `mlx5_tx_timeout_detected`, `mlx5_rx_timeout_detected` | **Non-Fatal** | `NONE` (Diagnostic) |

> **Key Principle**: Only **deterministic fatal signatures** in kernel logs (command-interface death, firmware heartbeat loss, unrecoverable hardware error, mlx5-attributed NAPI soft lockup) act as remediation triggers. All other kernel log events provide **diagnostic context** only — the decision to drain/replace for those is based on actual port state (via link state detection) or analyzer correlation, not single log messages.

### 5.3 Diagnostic Commands

```bash
# Check for driver/firmware errors in kernel log
dmesg | grep -E "(mlx5_core|PCIe|AER)"
# Output:
# [ 42.123] mlx5_core 0000:3b:00.0: ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource

# Check journald for NIC errors
journalctl -k | grep -E "(mlx5_core|PCIe)"

# Watch for real-time NIC driver messages
journalctl -k -f | grep --line-buffered mlx5_core
```

### 5.4 Multi-line Soft Lockup Detection (`mlx5_napi_soft_lockup`)

A kernel soft-lockup report spans many separate journal entries: the header
line (`watchdog: BUG: soft lockup - CPU#94 stuck for 226906s!`) carries the CPU
and duration but no driver attribution, while the `RIP:` and call-trace lines
that attribute the lockup to mlx5 arrive as subsequent entries. No single-line
regex can match both, so this pattern is implemented as a small stateful
detector inside the NIC driver handler rather than as an entry in the
first-match-wins single-line loop:

1. **Arm**: a line matching `BUG: soft lockup - CPU#(\d+) stuck for (\d+)s`
   opens a bounded window (150 kernel log lines). A newer header re-arms the
   window and refreshes the captured CPU/duration (the kernel re-reports a
   persistent lockup with a growing duration).
2. **Confirm**: within the window, a line matching
   `mlx5e_(poll_ico_cq|napi_poll)\+0x` — the `RIP:` line or a call-trace
   frame — confirms mlx5 attribution and emits one **Fatal** event
   (`REPLACE_VM`). Frames prefixed with `?` (followed by a space) are speculative stack remnants
   and are ignored; generic frames such as `net_rx_action` never count as
   evidence, so soft lockups in other drivers are not attributed to the NIC.
3. **Rate-limit**: the watchdog re-reports a persistent lockup every few tens
   of seconds; emission is suppressed for 30 minutes after each event.

The window is line-count based (not wall-clock based) so that journal backlog
replay after a monitor restart behaves identically to live tailing. The kernel
dump carries no PCI address, so the event is node-scoped (no NIC entity); the
parsed CPU and duration are attached as event metadata
(`metadata.cpu`, `metadata.durationSeconds`).

> **Why fatal on first confirmation**: an mlx5-attributed lockup means the
> driver's own recovery (TX/RX reporters) is not making progress — observed
> wedges persist for days and clear only on reboot. The `RIP`/non-speculative
> frame requirement is the false-positive guard; transient generic soft
> lockups do not carry mlx5 frames at `RIP`.
>
> The kernel defines a soft lockup as a failure to reschedule beyond the
> watchdog threshold — the CPU dumps its stack and *resumes*, so a single
> report does not by itself prove a permanent wedge
> ([lockup watchdogs](https://docs.kernel.org/admin-guide/lockup-watchdogs.html)).
> Two properties bound the risk of acting on the first report anyway:
> the parsed duration cannot distinguish outcomes at onset (the first report
> of what becomes a multi-day wedge also says ~26s), and repetition gates
> never fire on pre-5.13 kernels, which warn once per lockup episode.
>
> **Rollout**: like every pattern in this check, `mlx5_napi_soft_lockup`
> ships disabled in the default chart. When enabling it, start with
> `processingStrategy: STORE_ONLY` (shadow mode) — events are persisted and
> observable but drive no cordon or remediation. Graduate to
> `EXECUTE_REMEDIATION` after the shadow phase shows adequate precision on
> your fleet's real journals.

---

## 6. Event Correlation (via Health Events Analyzer)

Fatal syslog events are emitted immediately with their configured remediation action. The Health Events Analyzer can still correlate those raw events with port state changes to provide incident context. Non-Fatal diagnostic syslog events are emitted as `IsFatal=false` and can also be correlated with state changes for operator investigation.

### 6.1 Correlation Examples

The Health Events Analyzer can correlate kernel log events with port state events:

| Kernel Log Event                          | Port State Event | Correlation Insight     |
|-------------------------------------------|------------------|-------------------------|
| `High Temperature` + `health poll failed` | Port DOWN        | Thermal-induced failure |
| command timeout with resource leak        | Port DOWN        | Driver/firmware failure |
| `insufficient power`                      | Port DOWN        | Power delivery issue    |

### 6.2 Noise Filtering

Some kernel messages can be filtered to reduce noise:

- **`ACCESS_REG failed`**: Common on systems with restricted PFs (DGX, Umbriel). Use `--infiniband-exclude-devices` to exclude problematic devices from monitoring.
- **`insufficient power`**: Often transient during BIOS/BMC power negotiation. Can be filtered by uptime context if needed.

---

## 7. Repeat Failure Detection

Unlike a local correlation engine, repeat/cross-signal pattern detection is handled by the **Health Events Analyzer**:

1. **Raw Event Flow**: Syslog-health-monitor sends raw `mlx5_core` events to Platform Connector → MongoDB
2. **Correlation Rules**: Health Events Analyzer queries MongoDB and correlates syslog events with port state events
3. **Example**: command timeout with resource leak at 10:00:01 is emitted immediately as Fatal; `port state=DOWN` at 10:00:05 can be correlated later to provide diagnostic context

### 7.1 Correlation Purpose

Kernel log events are emitted with their configured severity. The Health Events Analyzer can correlate them with port state events for diagnostic context:

```
1. Diagnostic Correlation:
   ├── Purpose: Link kernel log events to port state changes
   ├── Input: Kernel log events + Port state events
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
│  T+0:00      Syslog Monitor         command timeout leak (mlx5_0)               │
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
│                    │      $or: [{message: /timeout. Will cause a leak/},     │   │
│                    │            {message: /state=DOWN/}]                    │   │
│                    │    })                                                   │   │
│                    │                                                         │   │
│                    │  Result:                                                │   │
│                    │    1. command timeout leak at T+0:00                   │   │
│                    │    2. state=DOWN at T+0:05                             │   │
│                    │                                                         │   │
│                    │  ┌───────────────────────────────────────────────────┐ │   │
│                    │  │  CORRELATION DETECTED                             │ │   │
│                    │  │                                                   │ │   │
│                    │  │  Pattern: command timeout + link_down             │ │   │
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
enableRepeatedNICDriverErrorRule: true
enableRepeatedNICDegradationRule: true
```

### 8.3 NIC Driver Pattern Configuration

NIC driver error pattern definitions are hardcoded in the syslog-health-monitor. Configuration selects supported pattern names, enables/disables them, and can optionally override the processing strategy. Regexes, severity, recommended action, and descriptions are intentionally not user-configurable.

**TOML Shape:**

```toml
[[nicDriverDetection.patterns]]
name = "cmd_exec_timeout"
enabled = true
processingStrategy = "EXECUTE_REMEDIATION"

[[nicDriverDetection.patterns]]
name = "netdev_watchdog"
enabled = true
processingStrategy = "EXECUTE_REMEDIATION"
```

**Fields:**

| Field                | Type     | Description                                                                                                                 |
|----------------------|----------|-----------------------------------------------------------------------------------------------------------------------------|
| `name`               | `string` | Supported pattern identifier; must exist in the Go-owned pattern registry                                                   |
| `enabled`            | `bool`   | Whether the pattern is active; set `false` to disable without removing                                                      |
| `processingStrategy` | `string` | Optional per-pattern override (`EXECUTE_REMEDIATION` or `STORE_ONLY`); falls back to the monitor-wide strategy when omitted |

> **Operational Note**: Operators can disable noisy supported patterns or use `STORE_ONLY` for observability-only collection. Severity and recommended action changes should be handled through platform connector / analyzer overrides rather than by changing monitor regex definitions.

---

## 9. Event Management

### 9.1 Event Construction

Events are emitted with severity based on their determinism of failure:

**Example Event Fields (Fatal - command timeout resource leak):**

| Field             | Value                                                                                        |
|-------------------|----------------------------------------------------------------------------------------------|
| Agent             | `syslog-health-monitor`                                                                      |
| CheckName         | `SysLogsNICDriverError`                                                                      |
| ComponentClass    | `NIC`                                                                                        |
| Message           | "mlx5_core 0000:3b:00.0: ENABLE_HCA(0x104) timeout. Will cause a leak of a command resource" |
| IsFatal           | `true`                                                                                       |
| IsHealthy         | `false`                                                                                      |
| RecommendedAction | `REPLACE_VM`                                                                                 |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}]`                                               |

**Example Event Fields (Non-Fatal - module unplugged):**

| Field             | Value                                                                  |
|-------------------|------------------------------------------------------------------------|
| Agent             | `syslog-health-monitor`                                                |
| CheckName         | `SysLogsNICDriverError`                                                |
| ComponentClass    | `NIC`                                                                  |
| Message           | "mlx5_core 0000:12:00.0: Port module event: module 0, Cable unplugged" |
| IsFatal           | `false`                                                                |
| IsHealthy         | `false`                                                                |
| RecommendedAction | `NONE` (informational)                                                 |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}]`                         |

### 9.2 Event Purpose

Kernel log events serve as both **direct failure signals** and **diagnostic context**:

| Event Severity | Purpose                               | Action                                                              |
|----------------|---------------------------------------|---------------------------------------------------------------------|
| **Fatal**      | Deterministic hardware/driver failure | Immediate remediation via `REPLACE_VM`                              |
| **Non-Fatal**  | Diagnostic information / Evidence     | Logged for investigation; provides context for state-based failures |

> **Key Design Principle**: Deterministically fatal events in logs (command timeout resource leaks, unrecoverable hardware error) trigger `REPLACE_VM` directly. Non-fatal events (insufficient power, module unplugged) remain as `IsFatal=false` for diagnostic correlation.

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

| Hardware Failure    | Detection Method                                               | Potential Application Impact |
|---------------------|----------------------------------------------------------------|------------------------------|
| **Firmware freeze** | Kernel log: `timeout. Will cause a leak of a command resource` | All NIC operations stall     |
| **Driver crash**    | Kernel log: `device's health compromised - reached miss count` | NIC becomes unusable         |
| **TX stall**        | Kernel log: `NETDEV WATCHDOG` / `TX timeout detected`          | Network transmission fails   |
| **NAPI poll wedge** | Kernel log: `BUG: soft lockup` + mlx5 NAPI stack frames        | CPU pinned at 100%; all network I/O through the NIC hangs (incl. network-backed storage); node may stay `Ready` for days |

### 10.4 Event Severity Classification

Kernel log events are classified by their determinism of failure:

| Kernel Log Event                         | Severity      | Purpose                       | Recommended Action |
|------------------------------------------|---------------|-------------------------------|--------------------|
| `mlx5_core.*timeout. Will cause a leak`  | **Fatal**     | Control plane broken          | `REPLACE_VM`       |
| `mlx5_core.*device's health compromised` | **Fatal**     | Firmware heartbeat lost       | `REPLACE_VM`       |
| `mlx5_core unrecoverable`                | **Fatal**     | Hardware admission of failure | `REPLACE_VM`       |
| soft lockup + mlx5 NAPI frames (stateful) | **Fatal**    | Driver wedged in NAPI poll    | `REPLACE_VM`       |
| `NETDEV WATCHDOG`                        | **Non-Fatal** | TX stall with auto-recovery   | `NONE`             |
| `TX timeout detected`                    | **Non-Fatal** | TX stall (driver-reported)    | `NONE`             |
| `RX timeout on channel`                  | **Non-Fatal** | RX stall (driver-reported)    | `NONE`             |
| `Detected insufficient power`            | **Non-Fatal** | Power negotiation status      | `NONE`             |
| `High Temperature`                       | **Non-Fatal** | Thermal warning               | `NONE`             |
| `module unplugged`                       | **Non-Fatal** | SFP unplugged                 | `NONE`             |
| `ACCESS_REG failed`                      | **Non-Fatal** | Monitoring noise              | `NONE`             |

> **Key Principle**: Deterministically fatal events in logs trigger `REPLACE_VM`. Diagnostic logs remain as Non-Fatal (`IsFatal=false`) for diagnostic context.

---

## Appendix A: Quick Reference - Kernel Log Patterns

The following patterns are monitored in the kernel ring buffer (dmesg/kmsg):

### Kernel Log Pattern Summary

| Pattern                                                        | Severity      | Meaning                         | Action                                                        |
|----------------------------------------------------------------|---------------|---------------------------------|---------------------------------------------------------------|
| `mlx5_core.*timeout\. Will cause a leak of a command resource` | **Fatal**     | Firmware/driver command timeout | `REPLACE_VM`                                                  |
| `mlx5_core.*device's health compromised.*reached miss count`   | **Fatal**     | Health check missed             | `REPLACE_VM`                                                  |
| `mlx5_core.*unrecoverable hardware error`                      | **Fatal**     | Hardware error detected         | `REPLACE_VM`                                                  |
| `NETDEV WATCHDOG` + `mlx5_core` (either order) + `timed out`   | **Non-Fatal** | TX queue timeout                | Investigate; persistent stalls caught by link-state-detection |
| `mlx5_core.*TX timeout detected`                               | **Non-Fatal** | TX queue timeout (driver line)  | Investigate; repeated occurrences escalate via analyzer       |
| `mlx5_core.*RX timeout on channel`                             | **Non-Fatal** | RX timeout (driver line)        | Investigate; repeated occurrences escalate via analyzer       |
| `BUG: soft lockup` + `mlx5e_(poll_ico_cq\|napi_poll)+0x` (stateful) | **Fatal** | CPU wedged in mlx5 NAPI poll    | `REPLACE_VM`                                                  |
| `mlx5_core.*Detected insufficient power`                       | **Non-Fatal** | Power negotiation status        | Investigate; can be transient                                 |
| `mlx5_core.*Port module event.*High Temp`                      | **Non-Fatal** | Thermal warning                 | Investigate; check cooling                                    |
| `mlx5_cmd_out_err.*ACCESS_REG.*failed`                         | **Non-Fatal** | Restricted PF access            | Filter/Ignore                                                 |
| `mlx5_core.*Port module event.*Cable unplugged`                | **Non-Fatal** | SFP/transceiver unplugged       | Monitor port state                                            |

### Design Principle

| Source                                        | IsFatal | Recommended Action | Purpose                            |
|-----------------------------------------------|---------|--------------------|------------------------------------|
| **Deterministic Logs**                        | `true`  | `REPLACE_VM`       | Fatal driver/firmware condition    |
| **Port State Changes** (link-state-detection) | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Fatal Counters** (link-counter-detection)   | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Diagnostic Logs**                           | `false` | `NONE`             | Evidence/context for investigation |

> **Key Insight**: Kernel logs for deterministic failures (command timeout resource leaks, etc.) are **Fatal (`IsFatal=true`)** with `RecommendedAction_REPLACE_VM`. Diagnostic logs (insufficient power, High Temperature, module unplugged) are **Non-Fatal (`IsFatal=false`)**. State and counter conditions are also **Fatal (`IsFatal=true`)** with `RecommendedAction_REPLACE_VM`.

---

## Appendix B: Health Events Analyzer Rules for NIC Monitoring

The Health Events Analyzer should only add value where the raw monitors do not already emit a deterministic fatal event. Fatal NIC state/counter events and fatal NIC driver syslog patterns already trigger remediation directly, so repeated rules are limited to **non-fatal recurrence signals** and use `CONTACT_SUPPORT`.

> **Design Note**: The count threshold of 3 is externally grounded by the Linux mlx5 health-poll miss threshold (`MAX_MISSES = 3`) and gpud's InfiniBand flap threshold of 3 reverts-to-active. The 1-hour window is an NVSentinel operational choice to catch clustered recurrence while avoiding stale daily maintenance or boot noise.

### B.1 Minimal Repeated NIC Escalation Rules

`RepeatedNICDriverError`:

- Input events: `syslog-health-monitor`, `SysLogsNICDriverError`, `IsFatal=false`.
- Included pattern names: `netdev_watchdog`, `port_module_high_temp`, `pci_power_insufficient`, `module_unplugged`, `mlx5_tx_timeout_detected`, `mlx5_rx_timeout_detected`.
- Grouping: same node + same `errorcode[0]` pattern name.
- Threshold: 3 events in 1 hour.
- Action: `CONTACT_SUPPORT`.
- Purpose: escalate repeated driver/firmware diagnostic warnings when auto-recovery or environmental stabilization is failing.

`RepeatedNICDegradation`:

- Input events: `nic-health-monitor`, `InfiniBandDegradationCheck` or `EthernetDegradationCheck`, `IsFatal=false`, `IsHealthy=false`.
- Grouping: same node + same `NIC` + same `NICPort`.
- Threshold: 3 events in 1 hour.
- Action: `CONTACT_SUPPORT`.
- Purpose: escalate repeated non-fatal counter degradation on the same physical port.

`access_reg_failed` is intentionally excluded from `RepeatedNICDriverError`. Public gpud context identifies repeated `mlx5_cmd_out_err.*ACCESS_REG.*failed` messages as restricted-PF/query noise on some systems, not a standalone hardware recurrence signal.

Fatal syslog patterns are also intentionally excluded:

- `cmd_exec_timeout`
- `health_poll_failed`
- `unrecoverable_err`

Those are already emitted as fatal by the syslog health monitor and do not need analyzer-side repeat detection.

### B.2 External Source Basis

- Linux mlx5 health poll uses `MAX_MISSES = 3` and logs `device's health compromised - reached miss count`: [Linux mlx5 `health.c`](https://raw.githubusercontent.com/torvalds/linux/master/drivers/net/ethernet/mellanox/mlx5/core/health.c).
- Linux mlx5 command timeout logs `timeout. Will cause a leak of a command resource`: [Linux mlx5 `cmd.c`](https://raw.githubusercontent.com/torvalds/linux/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c).
- Linux mlx5 async events log `High Temperature`, `Cable unplugged`, and insufficient PCIe slot power: [Linux mlx5 `events.c`](https://raw.githubusercontent.com/torvalds/linux/master/drivers/net/ethernet/mellanox/mlx5/core/events.c).
- Linux mlx5 TX timeout has a devlink health reporter recovery path: [Linux mlx5 `reporter_tx.c`](https://raw.githubusercontent.com/torvalds/linux/master/drivers/net/ethernet/mellanox/mlx5/core/en/reporter_tx.c).
- gpud uses 3 reverts-to-active as the InfiniBand port-flap threshold: [leptonai/gpud#971](https://github.com/leptonai/gpud/pull/971).
- gpud ACCESS_REG context identifies `mlx5_cmd_out_err.*ACCESS_REG.*failed` as restricted-device/query noise: [leptonai/gpud#1164](https://github.com/leptonai/gpud/issues/1164).

### B.3 Configuration in values.yaml

These rules are configured in the `health-events-analyzer` Helm chart values:

```yaml
enableRepeatedNICDriverErrorRule: true
enableRepeatedNICDegradationRule: true
```

---

## References

### Linux Kernel & Driver
1. [sysfs-class-infiniband (Linux Kernel)](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
2. [RHEL8 mlx5_core Stack Overflow (Red Hat)](https://access.redhat.com/solutions/6955682)
3. [mlx5_core cmd.c - Command Interface (Linux Kernel)](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/cmd.c) - command timeout resource leaks, `ACCESS_REG failed`
4. [mlx5_core health.c - Health Poller (Linux Kernel)](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/health.c) - health compromise miss count, `unrecoverable`
5. [mlx5_core events.c - Async Events (Linux Kernel)](https://github.com/torvalds/linux/blob/master/drivers/net/ethernet/mellanox/mlx5/core/events.c) - `insufficient power`, `High Temperature`, `module unplugged`
6. [PCIe AER HOWTO (Linux Kernel)](https://www.kernel.org/doc/html/latest/PCI/pcieaer-howto.html) - PCIe Bus Error log format and BDF identification

### Vendor Documentation
1. [ibdiagnet User Manual (NVIDIA)](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf)

---
