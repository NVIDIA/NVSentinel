# NIC Health Monitor: Link Counter Detection

---

## Table of Contents

1. [Overview](#1-overview)
2. [Theoretical Foundation](#2-theoretical-foundation)
3. [Architecture](#3-architecture)
4. [Complete Counter Specification](#4-complete-counter-specification)
5. [Counter Reading and Parsing](#5-counter-reading-and-parsing)
6. [Counter Reset Handling and Persistent State](#6-counter-reset-handling)
   - [6.1 The Problem](#61-the-problem)
   - [6.2 Counter Reset Causes](#62-counter-reset-causes)
   - [6.3 Counter Reset Handling Algorithm](#63-counter-reset-handling-algorithm)
   - [6.4 Admin Counter Reset: Recovery Event Scenario](#64-admin-counter-reset-recovery-event-scenario)
   - [6.5 Boot ID Handling](#65-boot-id-handling)
   - [6.6 Persistent State File](#66-persistent-state-file)
   - [6.7 Rationale](#67-rationale)
7. [Missing Counter Handling](#7-missing-counter-handling)
8. [RDMA vs TCP/IP Counter Domains](#8-rdma-vs-tcpip-counter-domains)
9. [Data Structures](#9-data-structures)
10. [Configuration](#10-configuration)
11. [Event Management](#11-event-management)
- [Appendix A: Quick Reference - Counter Thresholds](#appendix-a-quick-reference---counter-thresholds)

**Related Documents:**
- [Link State Detection](./link-state-detection.md) - UP/DOWN state monitoring
- [Syslog Detection & Correlation](./syslog-detection-correlation.md) - Kernel log monitoring and repeat failure detection

---

## 1. Overview

### 1.1 Problem Statement

Modern GPU clusters suffer from **Grey Failures** (subtle degradations) and **straggler effects** where a single degraded link throttles thousands of GPUs. Simple UP/DOWN polling is insufficient; a deterministic degradation detection system is required that can detect both hard failures and **gradual degradation** before FEC exhaustion causes catastrophic packet loss.

### 1.2 Scope of Link Counter Detection

This document covers the **Degradation Monitoring** component of the NIC Health Monitor, which detects:

- **Fatal counter violations** - Counters that guarantee workload failure when incremented
- **Rate-based degradation** - Error rates exceeding thresholds that predict impending failure
- **Pre-failure prediction** - Detecting BER climbing before FEC exhaustion

### 1.3 Binary Severity Model

This monitor uses a binary severity model based on **workload impact**:

| Severity      | Meaning                                  | Example                                                      |
|---------------|------------------------------------------|--------------------------------------------------------------|
| **Fatal**     | Workload WILL fail or HAS failed         | `link_downed` (any), `excessive_buffer_overrun_errors` (any) |
| **Non-Fatal** | Degradation detected, workload continues | Symbol errors, congestion, link flapping                     |

**Key Design Principle**: The only question that matters is **"Will the running workload fail because of this?"**

### 1.4 Counter Detection Overview Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                      LINK COUNTER DETECTION FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │                     DATA SOURCES (sysfs)                                 │    │
│  ├─────────────────────────────────────────────────────────────────────────┤    │
│  │  /sys/class/infiniband/<dev>/ports/<port>/                              │    │
│  │  ├── counters/                                                           │    │
│  │  │   ├── symbol_error                →  PHY bit errors (before FEC)     │    │
│  │  │   ├── link_error_recovery         →  Link retraining events          │    │
│  │  │   ├── link_downed                 →  Port training failures (FATAL)  │    │
│  │  │   ├── port_rcv_errors             →  Malformed packets               │    │
│  │  │   ├── local_link_integrity_errors →  Physical errors (FATAL)         │    │
│  │  │   ├── excessive_buffer_overrun    →  Lossless violation (FATAL)      │    │
│  │  │   └── port_xmit_discards          →  TX discards (congestion)        │    │
│  │  │                                                                       │    │
│  │  └── hw_counters/                    →  Extended counters               │    │
│  │      ├── roce_slow_restart           →  Victim flow oscillation         │    │
│  │      ├── local_ack_timeout_err       →  ACK timeout (path issues)       │    │
│  │      ├── rnr_nak_retry_err           →  Connection severed (FATAL)      │    │
│  │      └── req_transport_retries_exceeded → IB only (FATAL)               │    │
│  │                                                                          │    │
│  │  /sys/class/net/<interface>/statistics/                                  │    │
│  │  └── carrier_changes                 →  Link flap counter               │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                     │                                            │
│                                     ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │              DEGRADATION MONITOR (1s polling interval)                   │    │
│  ├─────────────────────────────────────────────────────────────────────────┤    │
│  │                                                                          │    │
│  │  CALCULATES (locally, for threshold comparison):                         │    │
│  │  ├── Δ (delta)      →  current_value − snapshot.value                    │    │
│  │  ├── Δt (elapsed)   →  now − snapshot.timestamp (wall-clock)             │    │
│  │  └── Δ/Δt (rate)    →  Errors per unit time, only after Δt ≥ window     │    │
│  │                                                                          │    │
│  │  PERSISTS (hostPath-backed state file):                                  │    │
│  │  ├── Per-counter snapshot (value + timestamp for delta/velocity)         │    │
│  │  ├── Per-counter breach flag (for recovery event emission)              │    │
│  │  └── Boot ID (clear state + emit healthy baselines on reboot)            │    │
│  │                                                                          │    │
│  │  FATAL COUNTERS (immediate event):                                       │    │
│  │  ├── link_downed (Delta > 0)               →  FATAL                     │    │
│  │  ├── excessive_buffer_overrun (any)        →  FATAL                     │    │
│  │  ├── local_link_integrity_errors (any)     →  FATAL                     │    │
│  │  ├── rnr_nak_retry_err (any)               →  FATAL                     │    │
│  │  └── symbol_error_fatal (> 120/hour)       →  FATAL                     │    │
│  │                                                                          │    │
│  │  NON-FATAL THRESHOLDS (degradation event):                               │    │
│  │  ├── symbol_error           > 10/sec       →  NON-FATAL                 │    │
│  │  ├── link_error_recovery    > 5/min        →  NON-FATAL                 │    │
│  │  ├── roce_slow_restart      > 10/sec       →  NON-FATAL                 │    │
│  │  └── carrier_changes        > 2/interval   →  NON-FATAL                 │    │
│  │                                                                          │    │
│  │  RECOVERY (when previously breached counter clears):                     │    │
│  │  └── Admin counter reset detected          →  RECOVERY (IsHealthy=true) │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                     │                                            │
│                                     ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │       RAW EVENTS + RECOVERY EVENTS → PLATFORM CONNECTOR → MongoDB       │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                     │                                            │
│                                     ▼                                            │
│  ┌─────────────────────────────────────────────────────────────────────────┐    │
│  │            HEALTH EVENTS ANALYZER (Escalation Rules)                     │    │
│  ├─────────────────────────────────────────────────────────────────────────┤    │
│  │  • RepeatedNICDegradation: "3 non-fatal degradation events on           │    │
│  │    same NIC port in 1h → CONTACT_SUPPORT"                               │    │
│  │  • Pattern detection across time windows                                 │    │
│  └─────────────────────────────────────────────────────────────────────────┘    │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Theoretical Foundation

### 2.1 The Physics of High-Speed Signaling Degradation

Modern interconnects (HDR/NDR InfiniBand, 100/200/400GbE) use **PAM4 modulation** (Pulse Amplitude Modulation, 4-level) to achieve high bandwidth. This represents a fundamental paradigm shift from previous generations.

#### 2.1.1 PAM4 vs NRZ: Why Velocity Monitoring is Required

| Aspect                  | NRZ (EDR/100GbE)   | PAM4 (NDR/400GbE)           |
|-------------------------|--------------------|-----------------------------|
| **Bits per symbol**     | 1                  | 2                           |
| **Voltage levels**      | 2 (0, 1)           | 4 (00, 01, 10, 11)          |
| **Eye height**          | Maximum            | **1/3 of NRZ**              |
| **SNR**                 | High               | **Drastically reduced**     |
| **Raw bit errors**      | Rare anomaly       | **Guaranteed and constant** |
| **Monitoring approach** | "Any error is bad" | **Velocity-based only**     |

> **Critical**: In PAM4 systems, raw bit errors are a **physical certainty**. A monitor that alerts on "Any Error > 0" would be permanently alarming. The velocity-based approach is the **only valid monitoring strategy** for 400G+ networks. ([Reference: PAM4 Test Challenges](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/))

### 2.2 Signal Degradation Progression

**Degradation flow**: Physical impairment (cable/SFP) → Eye diagram closes (DSP struggles) → Symbol errors (PHY layer) → FEC corrections (recoverable) → CRC failures (unrecoverable) → Packet loss (FATAL)

**Monitoring opportunity**: Detect degradation at the `symbol_error` stage, before FEC exhaustion causes packet loss.

### 2.3 Bit Error Rate (BER), FEC, and the "Cliff Effect"

Because errors are inevitable in PAM4, **Forward Error Correction (FEC) is mandatory** for 200G/400G/NDR links.

| Link Health State  | Bit Error Rate | Symbol Errors        | Action                 |
|--------------------|----------------|----------------------|------------------------|
| **Healthy**        | < 10E-15       | ~0 post-FEC          | None                   |
| **Failed (Fatal)** | > 10E-12       | FEC margin exhausted | **Fatal (REPLACE_VM)** |

#### 2.3.1 The FEC "Cliff Effect"

FEC masks physical degradation until the error rate exceeds correction capacity—then packet loss spikes instantly from 0% to ~100% (the "cliff"). The Degradation Monitor tracks **Pre-FEC BER** via `symbol_error` velocity, enabling node draining **before** the cliff is reached.

> **PAM4 Note (HDR/NDR)**: On 200G/400G adapters, non-zero raw BER is expected. Use rate-based thresholds (e.g., `symbol_error > 10/sec`) for degradation detection, not `symbol_error > 0`.

### 2.4 The Lossless Assumption and Deterministic Failure Horizons

Unlike general-purpose TCP/IP networks, which are architected to be resilient to packet loss, latency variation, and out-of-order delivery, RDMA fabrics—specifically InfiniBand (IB) and RDMA over Converged Ethernet (RoCE)—are designed under a **"lossless" assumption**. This architectural premise dictates that once a packet is admitted to the fabric, its delivery is guaranteed by credit-based flow control (in IB) or Priority Flow Control (in RoCE), relieving the transport layer of heavy congestion management overhead.

> **Key Insight**: This reliance on near-perfect transmission introduces a **binary fragility** to the system. When the physical or link layer violates the lossless assumption, the impact on the application is often not merely performance degradation, but **catastrophic failure**. For tightly coupled distributed workloads using MPI or NCCL, a failure in a single link **deterministically terminates the entire job**.

#### 2.4.1 Soft vs Hard Errors: The Determinism Boundary

The critical operational requirement is distinguishing between:

| Error Type      | Characteristics                            | Impact                                      |
|-----------------|--------------------------------------------|---------------------------------------------|
| **Soft Errors** | Probabilistic, recoverable via FEC/retries | Performance degradation, workload continues |
| **Hard Errors** | Deterministic, exceed recovery capacity    | Application failure **guaranteed**          |

The boundary between soft and hard errors is defined by:
1. **Counter thresholds** that indicate recovery mechanism exhaustion
2. **Rate of change** that exceeds retry bandwidth
3. **Specific counter types** that indicate fundamental violation of the lossless contract

#### 2.4.2 The 10E-12 BER Threshold

The InfiniBand specification defines a compliant link as maintaining a Bit Error Rate (BER) of better than **10E-12**. This physical constant provides the basis for threshold calculations:

- At a BER of 10E-12, a link running at high speed (e.g., HDR 200Gb/s) experiences a predictable number of errors per unit time
- **IBTA-compliant threshold**: Maximum allowable symbol error rate is **120 errors per hour** ([IBTA Specification](https://www.infinibandta.org/ibta-specification/) / [Oracle Documentation](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html))
- Below this rate, FEC algorithms can typically correct errors without retransmission
- Above this rate, the "effective" error rate (post-FEC) rises, leading to packet corruption and Link Level Retransmission (LLR) or transport layer retries

> **Monitoring Implication**: While a single `SymbolError` is not fatal, a rate exceeding **120/hour** (≈2/minute) is a **deterministic predictor of impending link instability**. Monitoring systems should treat this as a **Fatal condition** requiring node replacement.

#### 2.4.3 Deterministic Failure Mechanisms

The following counters represent **absolute deterministic failure** when they increment:

| Counter                             | Mechanism                                                | Why Deterministic                                                                       |
|-------------------------------------|----------------------------------------------------------|-----------------------------------------------------------------------------------------|
| **link_downed**                     | Port Training State Machine fails to maintain LinkUp     | Standard HPC applications do not support transparent dynamic rerouting of active QPs    |
| **excessive_buffer_overrun_errors** | HCA internal ingress buffer overflows                    | Violates fundamental "lossless" contract; packet causing overrun is dropped immediately |
| **RNR_nak_retry_err**               | Receiver Not Ready NAK retry exhausted                   | Terminal state of error handling; connection is severed                                 |
| **local_link_integrity_errors**     | Raw physical errors exceed LocalPhyErrors hardware limit | Link is operating outside design specifications                                         |

> **Note**: These four counters represent absolute deterministic failure. Additionally, `symbol_error` has a fatal threshold at > 120/hour (IBTA BER spec violation) via the `symbol_error_fatal` config entry. All other counters (symbol_error at > 10/sec, port_rcv_errors, etc.) are non-fatal degradation indicators.

### 2.5 The Transport Layer Retry Window

When hardware counters increment, they don't directly cause application failure—they trigger a reaction in the software stack. Understanding this interaction defines the "Fatal" threshold:

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    TRANSPORT LAYER RETRY WINDOW                                  │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  Hardware: SymbolError ──► FEC fails ──► Packet Corrupted                       │
│                │                                                                 │
│                ▼                                                                 │
│  Receiver: drops packet ──► PortRcvErrors increments                            │
│                │                                                                 │
│                ▼                                                                 │
│  Sender: waits for ACK ──► Timeout ──► Retry (1) ──► ... ──► Retry (N)         │
│                │                                             │                   │
│                │                                             ▼                   │
│                │                                      GIVE UP                    │
│                │                                             │                   │
│                ▼                                             ▼                   │
│  Application: NCCL_IB_RETRY_CNT (default: 7) exhausted                          │
│                │                                                                 │
│                ▼                                                                 │
│  Result: QP transitions to ERROR state ──► Application crashes                  │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 2.6 Transport Retry Count Exceeded (Error 12)

When the NIC sends a packet and the ACK never arrives:

```
Send Packet → Wait for ACK → Timeout → Retry (1) → Timeout → ... → Retry (N) → GIVE UP
```

After `retry_cnt` attempts (default: 7), the NIC tears down the connection and the application receives `IBV_WC_RETRY_EXC_ERR`.

**Implications:**
- Confirms **Logical Link is broken** even if physical link is UP
- Often indicates "Silent Drop" or **Black Hole** in the fabric
- Local symptom of a **remote** problem

> **Important: Application-Triggered Timeouts.** A rising `local_ack_timeout_err` counter does NOT necessarily indicate a local NIC fault. If a remote NCCL rank crashes or hangs, the remote NIC stops responding to RDMA requests. The local NIC retries and eventually exhausts `retry_cnt`, incrementing `local_ack_timeout_err` on the local side. This means the counter can be triggered by: (1) fabric black hole (network issue), (2) remote NIC failure, or (3) **remote application crash/hang** — which is not a NIC problem at all. This is why `local_ack_timeout_err` is classified as **Non-Fatal** (`IsFatal=false`) — it requires correlation with other signals (port state, remote node health) to determine the root cause.

**What This Monitor CAN Detect**: The `local_ack_timeout_err` and `req_transport_retries_exceeded` (native IB) hardware counters track these retry events at the NIC level. Rising counter values indicate transport-layer problems even if we can't see the application error.

**Diagnostic Commands:**
```bash
# Read hardware counters directly from sysfs
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/req_transport_retries_exceeded
# Output: 42 (non-zero value indicates connection-severing retries)

# Detailed link quality and error counters via Mellanox diagnostic tools
mlxlink -d /dev/mst/mt4126_pciconf0 --show_ber
# Output: Symbol Errors, BER counters

# Query eye opening (signal quality indicator)
mlxlink -d /dev/mst/mt4126_pciconf0 --eye_open
# Output: Eye height/width for each PAM4 lane (identifies physical cable degradation)
```

**Correlation**: Use with [ibdiagnet](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf) to determine if issue is local (NIC) or remote (Switch/Fabric).

**Fabric-wide Diagnostic Command:**
```bash
# Perform comprehensive fabric-wide diagnostics (requires Subnet Manager access)
ibdiagnet -o /tmp/ibdiag_output
# Output: Summary of fabric errors, including symbol errors on switches and remote ports
```

---

## 3. Architecture

### 3.1 Design Rationale: NVSentinel's "Report Raw, Correlate Centrally" Pattern

The Degradation Monitor follows NVSentinel's established architectural pattern where:

1. **Health Monitors (DaemonSets)** report **raw events as-is** to the Platform Connector
2. **Health Events Analyzer (Centralized Deployment)** performs all correlation, aggregation, and pattern detection
3. **MongoDB** serves as the source of truth for event history and correlation queries

| Architectural Principle     | Implementation                             | Purpose                                                      |
|-----------------------------|--------------------------------------------|--------------------------------------------------------------|
| **Raw Event Reporting**     | Each threshold violation → immediate event | Enables centralized correlation with full historical context |
| **Centralized Correlation** | Health Events Analyzer MongoDB pipelines   | Flexible, configurable rules without monitor code changes    |
| **Temporal Correlation**    | Analyzer rules with time windows           | Detects repeated non-fatal port degradation                  |

### 3.2 Component Responsibilities

| Component                                  | Responsibility                                                                                                               | What It Does NOT Do                                        |
|--------------------------------------------|------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------|
| **NIC Health Monitor (Degradation Check)** | Poll sysfs counters, calculate deltas/rates, persist counter snapshots and breach state, emit raw events and recovery events | Aggregation, deduplication, correlation, pattern detection |
| **Health Events Analyzer**                 | Correlate events, detect patterns, escalate severity                                                                         | Direct hardware access                                     |

> **Local State Persistence**: The Degradation Check maintains a hostPath-backed state file containing per-counter snapshots, breach flags, and the host boot ID. This enables precise velocity windows across pod restarts, recovery events after administrator counter resets, and reboot detection that clears counter/observational state and emits healthy baselines. Outstanding card/device FATAL latches owned by the state checks are preserved until matching recovery evidence arrives. This local state is strictly operational — all correlation and pattern detection remains centralized in the Health Events Analyzer.

**Analyzer Escalation**: The Health Events Analyzer only escalates repeated **non-fatal** degradation events. The `RepeatedNICDegradation` rule triggers when 3 non-fatal `InfiniBandDegradationCheck` or `EthernetDegradationCheck` events occur on the same `NIC` + `NICPort` within 1 hour, and recommends `CONTACT_SUPPORT`. Fatal counters still emit `REPLACE_VM` directly from the monitor on first breach.

### 3.3 Degradation Check Data Flow (1s polling interval)

```
Reads:
├── counters/      → Standard IB counters (symbol_error, link_error_recovery, etc.)
├── hw_counters/   → Extended counters (roce_slow_restart, rnr_nak_retry_err, etc.)
├── statistics/    → Ethernet statistics (rx_crc_errors, rx_missed_errors, etc.)
└── carrier_changes → Link flap counter (catches UP/DOWN events between polls)

Calculates (locally, for threshold comparison):
├── Δ (delta)      → current_value − snapshot.value
├── Δt (elapsed)   → now − snapshot.timestamp (real wall-clock time)
└── Δ/Δt (rate)    → For velocity thresholds, evaluated only after Δt ≥ window
                     (window = 1s / 1m / 1h, matching the configured velocityUnit)

When threshold exceeded for the first time, emits a single RAW event with:
├── Counter name   → e.g., "symbol_error_fatal"
├── Current value  → e.g., 12500
├── Delta          → accumulated since the snapshot was taken
├── Rate           → e.g., 200/hour
└── Threshold      → e.g., 120/hour

Subsequent polls while breached emit nothing (latching breach). A
recovery event is emitted when the counter is reset (admin clear), when
the host reboots, or when the latched key can never be evaluated again
(its counter was removed from the configuration, or its device is still
discovered but no longer in monitoring scope — e.g., classified as a
management NIC); the last case is swept automatically so the downstream
condition is not orphaned.

Fatal counter thresholds (configurable, defaults shown):
├── link_downed (Delta > 0)                    → QP disconnect (FATAL)
├── excessive_buffer_overrun_errors (any)      → Lossless violation (FATAL)
├── local_link_integrity_errors (any)          → Link outside spec (FATAL)
├── rnr_nak_retry_err (any)                    → Connection severed (FATAL)
└── symbol_error_fatal (> 120/hour)            → IBTA BER spec violation (FATAL)

Non-fatal thresholds (configurable, defaults shown):
├── symbol_error           > 10/sec
├── link_error_recovery    > 5/min
├── roce_slow_restart      > 10/sec
└── carrier_changes        > 2/interval

Persists (to hostPath-backed state file after each poll cycle):
├── Per-counter snapshot  → Value + wall-clock timestamp (for delta/velocity)
├── Per-counter breach    → Whether threshold is currently exceeded (for recovery)
└── Boot ID              → Detects host reboot to clear state + emit healthy baselines

Emits: Raw DEGRADATION events → Platform Connector → MongoDB
       Recovery events (IsHealthy=true) when breached counter clears (e.g., admin reset)
       (Pattern detection and escalation handled by Health Events Analyzer)
```

---

## 4. Complete Counter Specification

### 4.1 Complete Counter Set ("Golden Counters" + Extended)

This monitor tracks both **fatal counters** (deterministic workload failure) and **non-fatal counters** (degradation indicators). The `IsFatal` field in the HealthEvent distinguishes between them.

#### 4.1.1 Standard Counters (`/sys/class/infiniband/<dev>/ports/<port>/counters/`)

| Counter                    | File Name                         | Degradation Meaning                                                                      | IsFatal | Alert Threshold                         | Source                                                                                                              |
|----------------------------|-----------------------------------|------------------------------------------------------------------------------------------|---------|-----------------------------------------|---------------------------------------------------------------------------------------------------------------------|
| **Symbol Error**           | `symbol_error`                    | Raw bit errors before FEC. Expected non-zero for PAM4 (HDR/NDR).                         | **No**  | Rate-based (e.g., > 10/sec for warning) | [Oracle/IBTA](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)                                |
| **Link Error Recovery**    | `link_error_recovery`             | PHY-initiated link retraining (micro-flapping). Causes millisecond-scale latency spikes. | **No**  | > 5/min (watchdog trigger)              | [NVIDIA UFM IB Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters) |
| **Link Downed**            | `link_downed`                     | Port Training State Machine failed to maintain LinkUp.                                   | **YES** | **Delta > 0 (Runtime)**                 | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us)                            |
| **Port Receive Errors**    | `port_rcv_errors`                 | Malformed packets (CRC, length errors). Saturates retry bandwidth at high rates.         | **No**  | > 10/sec (retry saturation)             | [NVIDIA UFM IB Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters) |
| **Local Link Integrity**   | `local_link_integrity_errors`     | Raw physical errors exceeded LocalPhyErrors hardware cap. Link operating outside spec.   | **YES** | **> 0 (any)**                           | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us)                            |
| **Buffer Overrun**         | `excessive_buffer_overrun_errors` | HCA internal buffer overflow—**lossless contract violated**. Packet dropped immediately. | **YES** | **> 0 (any)**                           | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)                                             |
| **Port Transmit Discards** | `port_xmit_discards`              | TX discards due to congestion.                                                           | **No**  | > 100/sec                               |                                                                                                                     |

#### 4.1.2 Extended Counters (`/sys/class/infiniband/<dev>/ports/<port>/hw_counters/`) — Non-Fatal

All extended counters are **non-fatal** by default. They indicate congestion, retransmissions, or recoverable transport events. RDMA's reliable transport handles these automatically; workloads continue with potential performance impact.

**Key Non-Fatal Counters** (monitor for performance degradation):

| Category       | Counters                | IsFatal | Alert Threshold | Justification                                                                                                                        |
|----------------|-------------------------|---------|-----------------|--------------------------------------------------------------------------------------------------------------------------------------|
| **Physical**   | `symbol_error`          | **No**  | > 10/sec        | PHY signal degradation / Dirty fiber.                                                                                                |
| **Link**       | `link_error_recovery`   | **No**  | > 5/min         | Link Flapping / PTSM Instability.                                                                                                    |
| **Integrity**  | `port_rcv_errors`       | **No**  | > 10/sec        | FCS/CRC Corruption (Bit Rot).                                                                                                        |
| **Congestion** | `port_xmit_discards`    | **No**  | > 100/sec       | Congestion Collapse / PFC breakdown.                                                                                                 |
| **Transport**  | `roce_slow_restart`     | **No**  | > 10/sec        | Victim Flow / Transport Oscillation (Straggler).                                                                                     |
| **Transport**  | `rnr_nak_retry_err`     | **YES** | **> 0 (any)**   | RNR NAK retry exhausted; QP enters error state ([ref](https://man7.org/linux/man-pages/man3/ibv_modify_qp.3.html)).                  |
| **Timeout**    | `local_ack_timeout_err` | **No**  | > 1/sec         | Broken Path / Fabric Black Hole. Can be caused by remote app crash (see [Section 2.6](#26-transport-retry-count-exceeded-error-12)). |
| **Interface**  | `carrier_changes`       | **No**  | > 2/interval    | Physical instability visible to OS.                                                                                                  |

> **Key Insights:**
> - **`rnr_nak_retry_err` > 0**: **FATAL** - Indicates RNR NAK retry exhausted; the connection has been severed.
> - **`roce_slow_restart` > 10/sec**: Primary indicator for Grey Failures. Indicates flow oscillation and straggler behavior.
> - **`port_xmit_discards` > 100/sec**: Flow control breakdown. Network physically unable to handle load.
> - **`symbol_error` > 10/sec**: Signature of "Dirty Fiber" or microscopic dust on connectors.

### 4.2 Counter Locations

- **Standard IB counters**: `/sys/class/infiniband/<dev>/ports/<port>/counters/` (symbol_error, link_downed, local_link_integrity_errors, etc.)
- **Extended counters (Mellanox)**: `/sys/class/infiniband/<dev>/ports/<port>/hw_counters/` (rnr_nak_retry_err, roce_slow_restart, etc.)
- **Ethernet stats (RoCE)**: `/sys/class/net/<iface>/statistics/` (rx/tx error counters) and the netdev root `/sys/class/net/<iface>/` for `carrier_changes` (which lives beside `operstate`, not under `statistics/`)

### 4.3 Diagnostic Commands

```bash
# Read standard counters
cat /sys/class/infiniband/mlx5_0/ports/1/counters/symbol_error
cat /sys/class/infiniband/mlx5_0/ports/1/counters/port_rcv_errors
cat /sys/class/infiniband/mlx5_0/ports/1/counters/port_xmit_discards

# Read extended hw_counters (degradation monitoring)
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/local_ack_timeout_err
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/roce_slow_restart
cat /sys/class/infiniband/mlx5_0/ports/1/hw_counters/rnr_nak_retry_err

# Fabric-wide diagnostics (requires Subnet Manager access)
ibdiagnet -o /tmp/ibdiag_output
```

### 4.4 Key Design Decisions

- **`link_downed`** is **Fatal**. In running MPI/NCCL jobs, any increment (Delta > 0) guarantees job crash.
- **`excessive_buffer_overrun_errors`** is **Fatal**. Violates fundamental "lossless" contract; packet causing overrun is dropped immediately.
- **`rnr_nak_retry_err`** is **Fatal**. Indicates Receiver Not Ready NAK retry exhausted; the connection has been severed.
- **`local_link_integrity_errors`** is **Fatal**. This counter is a "meta-threshold"—it only increments when raw physical errors exceed the hardware-defined LocalPhyErrors cap.
- **`symbol_error`** uses **PAM4 (HDR/NDR)** considerations. Zero-tolerance is obsolete for modern links; non-zero raw BER is expected. Monitor velocity for degradation trends.
- Most `hw_counters` are **Non-Fatal** by default—they indicate degradation that should be monitored but doesn't immediately crash workloads. Exception: `rnr_nak_retry_err` is fatal.

### 4.5 Consolidated Deterministic Failure Thresholds (Defaults)

> **Configuration Note**: Thresholds are configurable. Counter severity, sysfs path, recommended action, and event description are owned by the monitor's hardcoded counter definitions. See [Section 10: Configuration](#10-configuration) for customization options.

**Table 1: Absolute Deterministic Failure Thresholds (Default: Fatal - IsFatal=true)**

Breaching these thresholds **guarantees application failure** or mandatory node exclusion.

| Counter Name                      | Type     | Fatal Threshold         | IsFatal | Deterministic Mechanism                                                                                                                                           | Source                                                                                                                                                                                                                               |
|-----------------------------------|----------|-------------------------|---------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `link_downed`                     | Standard | **Delta > 0 (Runtime)** | **YES** | Logical path destruction; QP disconnect. Standard HPC apps don't support transparent QP rerouting.                                                                | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us)                                                                                                                                             |
| `excessive_buffer_overrun_errors` | Standard | **> 0 (Any)**           | **YES** | Lossless guarantee violation; packet dropped immediately. HCA ingress buffer overflow.                                                                            | [IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)                                                                                                                                                              |
| `rnr_nak_retry_err`               | Extended | **> 0 (Any)**           | **YES** | Receiver Not Ready NAK retry exhausted; QP transitions to error state (`IBV_WC_RNR_RETRY_EXC_ERR`). Connection cannot recover without application-level teardown. | [ibv_modify_qp(3) - rnr_retry QP attr](https://man7.org/linux/man-pages/man3/ibv_modify_qp.3.html), [NVIDIA RDMA Programming](https://docs.nvidia.com/networking/display/rdmaawareprogrammingv17/queue+pair+bringup+(ibv_modify_qp)) |
| `local_link_integrity_errors`     | Standard | **> 0 (Any)**           | **YES** | Physical error density exceeds hardware-defined LocalPhyErrors cap. Link outside spec.                                                                            | [HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us)                                                                                                                                             |
| `symbol_error_fatal`              | Standard | **> 120/hour**          | **YES** | IBTA BER spec violation (10E-12). Link operating outside specification; FEC margin exhausted.                                                                     | [Oracle/IBTA BER Threshold](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)                                                                                                                                   |

**Table 2: Predictive Thresholds (Non-Fatal - IsFatal=false)**

Breaching these rates indicates **degradation** requiring monitoring. Workloads continue but performance may be impacted.

> **Threshold Source Note**: These thresholds are derived from a combination of IBTA BER specifications, cloud provider operational heuristics (Azure, AWS), vendor documentation, and field experience. Specific rate values are **configurable defaults**, not specification mandates. See [Section 10: Configuration](#10-configuration) for customization options.

| Counter Name            | Type      | Alert Threshold  | IsFatal | Rationale                                                                                                                                                        | Source                                                                                                                                                                                                                  |
|-------------------------|-----------|------------------|---------|------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `symbol_error`          | PHY       | **> 10/sec**     | **No**  | Physical layer degradation (Dirty Fiber). Derived from IBTA BER spec (10E-12); 10/sec implies BER degraded to ~1E-8.                                             | [Oracle/IBTA BER Threshold](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html), [NVIDIA UFM IB Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters) |
| `link_error_recovery`   | Link      | **> 5/min**      | **No**  | PTSM Instability. Each retrain causes 50ms-2s stall. 5/min = flapping link.                                                                                      | [NVIDIA UFM IB Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters) (counter definition); threshold is operational heuristic                                            |
| `port_rcv_errors`       | Standard  | **> 10/sec**     | **No**  | Bit Rot / CRC Corruption. Saturates transport replay buffer.                                                                                                     | [NVIDIA UFM IB Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters)                                                                                                     |
| `port_xmit_discards`    | Standard  | **> 100/sec**    | **No**  | Congestion Collapse / PFC breakdown.                                                                                                                             | [NVIDIA UFM IB Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters) (counter definition); threshold is operational heuristic                                            |
| `roce_slow_restart`     | RoCE      | **> 10/sec**     | **No**  | "Victim Flow" oscillation. Jitter impacts AllReduce synchronization.                                                                                             | [NVIDIA DOCA Telemetry](https://docs.nvidia.com/doca/archive/2-9-3/doca+telemetry+service+guide/index.html)                                                                                                             |
| `local_ack_timeout_err` | Transport | **> 1/sec**      | **No**  | ACK timeouts indicate path issues (Black Hole). Can also be caused by remote application crash (see [Section 2.6](#26-transport-retry-count-exceeded-error-12)). | Operational heuristic                                                                                                                                                                                                   |
| `carrier_changes`       | Interface | **> 2/interval** | **No**  | Link instability (catches UP/DOWN events between polls).                                                                                                         | Operational heuristic                                                                                                                                                                                                   |

### 4.6 Technical Justification for Non-Fatal Thresholds

The following analysis validates the efficacy of the proposed monitoring design based on hardware specifications and empirical reliability studies.

**1. Physical Layer (L1) Justifications**
*   **Symbol Error (`symbol_error` > 10/sec)**: A rate of 10/sec is a robust indicator of physical degradation. In modern PAM4 links, a healthy optical connection operates with a BER better than 1E-12 (roughly one error every few hours). A rate of 10/sec implies the BER has degraded by orders of magnitude (to ~1E-8). This is the classic signature of "Dirty Fiber" or microscopic dust on connectors.
*   **Link Error Recovery (`link_error_recovery` > 5/min)**: Tracks the Port Training State Machine (PTSM). 5 events per minute represents a "Flapping" link. While the link recovers (non-fatal), each retrain causes 50ms to 2s of stall, decimating performance for synchronous GPU workloads.
*   **Carrier Changes (`carrier_changes` > 2/interval)**: The OS-visible shadow of link recovery. Confirms that physical instability was severe enough to disrupt the driver layer.

**2. Data Link Layer (L2) Justifications**
*   **Port Receive Errors (`port_rcv_errors` > 10/sec)**: Indicates "Bit Rot"—data corruption surviving the PHY but failing the CRC/FCS check. Triggers "Phantom Congestion" as the network repeatedly retransmits corrupted frames.
*   **Port Transmit Discards (`port_xmit_discards` > 100/sec)**: Indicates flow control breakdown. The network is physically unable to handle the load, and backpressure mechanisms (PFC) are failing. Definitive signal of Congestion Collapse.

**3. Transport Layer (L4) Justifications**
*   **RoCE Slow Restart (`roce_slow_restart` > 10/sec)**: Primary indicator for Grey Failures. Indicates a flow is timing out and resetting its congestion window repeatedly. This creates stragglers that stall the entire GPU fleet during collective operations (AllReduce).
*   **Local ACK Timeout (`local_ack_timeout_err` > 1/sec)**: In a reliable lossless network, ACKs should not be lost. A persistent rate of 1/sec implies a "Fabric Black Hole" (e.g., a specific bad ECMP path).

> **Note on `rnr_nak_retry_err`**: This counter is **FATAL** (not a non-fatal threshold). Any increment indicates the Receiver Not Ready NAK retry limit has been exhausted and the connection has been severed. This is a terminal state of error handling.

> **Final Verdict**: These thresholds are calibrated to distinguish between background noise (standard FEC activity) and pathological hardware degradation that threatens AI training efficiency.

---

## 5. Counter Reading and Parsing

### 5.1 Mellanox Counter Reading

For Mellanox devices (IB and RoCE), the monitor reads:
1.  **Standard Counters**: `/sys/class/infiniband/<dev>/ports/1/counters/`
    *   Fatal counters: `link_downed`, `local_link_integrity_errors`, `excessive_buffer_overrun_errors`
    *   Two-tier counter: `symbol_error` — non-fatal at > 10/sec (degradation warning), fatal at > 120/hour (IBTA BER spec violation)
2.  **Extended Counters**: `/sys/class/infiniband/<dev>/ports/1/hw_counters/`
    *   Fatal counter: `rnr_nak_retry_err`
    *   Non-fatal counters for degradation monitoring

> **Note**: Mellanox throughput counters (`port_rcv_data`, `port_xmit_data`) are in 4-byte words. Multiply by 4 to get bytes.

### 5.2 Mellanox Fatal Counter Paths

| Counter                           | Path                                                                                | Fatal Threshold |
|-----------------------------------|-------------------------------------------------------------------------------------|-----------------|
| `symbol_error_fatal`              | `/sys/class/infiniband/<dev>/ports/<port>/counters/symbol_error`                    | > 120/hour      |
| `local_link_integrity_errors`     | `/sys/class/infiniband/<dev>/ports/<port>/counters/local_link_integrity_errors`     | Delta > 0       |
| `excessive_buffer_overrun_errors` | `/sys/class/infiniband/<dev>/ports/<port>/counters/excessive_buffer_overrun_errors` | Delta > 0       |
| `rnr_nak_retry_err`               | `/sys/class/infiniband/<dev>/ports/<port>/hw_counters/rnr_nak_retry_err`            | Delta > 0       |

> **Note**: `symbol_error` has two default config entries: `symbol_error` (non-fatal, > 10/sec for degradation) and `symbol_error_fatal` (fatal, > 120/hour per [IBTA specification (10E-12 BER)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)). Both read from the same sysfs file. On PAM4 links (HDR/NDR), some non-zero symbol errors are expected; tune the fatal threshold if 120/hour is too sensitive for your environment.

---

## 6. Counter Reset Handling

Hardware counters may reset due to driver reloads, device resets, **administrator-initiated clears** (e.g., `perfquery -x`, `echo 0 > /sys/...`), or (rarely) uint64 overflow. The monitor must handle cases where `Current < Previous` to avoid incorrect delta calculations and must emit **recovery events** when a counter reset clears a previously breached threshold.

### 6.1 The Problem

```
Poll N:   symbol_error = 1,000,000
Driver Reload / Counter Reset / Admin Clear
Poll N+1: symbol_error = 50
Naive Delta = 50 - 1,000,000 = NEGATIVE (or overflow to huge positive)
```

**Additionally**, if `symbol_error` had previously triggered a FATAL event (e.g., exceeding 120/hour), and an administrator resets the counters to remediate the issue, the monitor must detect this and emit a **recovery event** (`IsHealthy=true`) to clear the unhealthy condition on the platform.

### 6.2 Counter Reset Causes

| Cause                                          | Detection                                                          | Expected Behavior                                                                              |
|------------------------------------------------|--------------------------------------------------------------------|------------------------------------------------------------------------------------------------|
| **Driver reload** (`modprobe -r mlx5_core`)    | `current < previous`; syslog monitor reports correlated kernel log | Treat `current` as delta, check for recovery                                                   |
| **Device reset** (firmware/hardware initiated) | `current < previous`; may correlate with syslog events             | Treat `current` as delta, check for recovery                                                   |
| **Administrator clear** (CSP/cluster admin)    | `current < previous` (typically to 0); no correlated syslog event  | Treat `current` as delta, **emit recovery event if previously breached**                       |
| **Host reboot**                                | Boot ID changes; all counters restart from 0                       | Clear counter and observational state, preserve outstanding state-event latches, and emit healthy baselines (see Section 6.5) |
| **uint64 overflow**                            | `current < previous` (extremely rare)                              | Treat `current` as delta                                                                       |

### 6.3 Counter Reset Handling Algorithm

**Reset Detection** (uses an in-memory `lastPollValue` per counter
plus the persisted snapshot as fallback after pod restart):

1. **If `lastPollValue` is recorded** for this counter (steady state):
   - Reset detected when `current < lastPollValue`
2. **Else if a persisted snapshot exists** (first poll after pod restart,
   `lastPollValue` not yet rebuilt):
   - Reset detected when `current < snapshot.value`
3. **Else** (truly first poll for this counter):
   - No reset to detect; just initialize the snapshot

Sysfs counters are kernel-maintained and monotonic between resets, so
`current < previous` is a definitive reset signal.

**Threshold Evaluation and Recovery Steps** (latching breach):

1. **If reset detected**:
   - If counter was previously breached → emit **recovery event**
     (`IsHealthy=true`, `IsFatal=false`, `RecommendedAction=NONE`),
     clear `breached` flag
   - If not previously breached → no event
   - Update snapshot to `(current, now)` so the next window starts
     fresh from the post-reset baseline
2. **Else if counter is already breached** (latched):
   - **No event** — breach stays latched until counter reset or boot ID change
3. **Else** (no reset, not currently breached) — evaluate the threshold:
   - For `delta`: `breach = (current − snapshot.value) > threshold`,
     update snapshot every poll
   - For `velocity`: skip until `now − snapshot.timestamp ≥ window`,
     then `breach = rate > threshold`, update snapshot
4. **If breach detected** in step 3:
   - Emit **unhealthy event** (`IsHealthy=false`, `IsFatal` per counter
     config), set `breached=true` in persistent state
5. **If no breach** in step 3:
   - **No event** — still healthy

> **Latching breach rationale**: Once a fatal counter increments
> (e.g. `link_downed=1`), the underlying physical event has happened.
> The fact that no further increments occur in the next poll does not
> mean the issue is resolved — only that no more events are accumulating
> right now. Recovery therefore requires explicit remediation (admin
> counter reset or host reboot), not merely the absence of new errors.
> This is consistent with the Section 6.4 admin-reset timeline.

### 6.4 Admin Counter Reset: Recovery Event Scenario

The following timeline illustrates why persistent breach tracking and recovery events are required:

```
Timeline: Admin Counter Reset Recovery

T=0s    Poll:  link_downed = 0       (delta=0, no breach, breached=false)
T=5s    Poll:  link_downed = 1       (delta=1, BREACH → emit FATAL event, breached=true)
T=10s   Poll:  link_downed = 1       (delta=0, still breached, no event)
T=15s   --- CSP admin resets counters (perfquery -x) ---
T=20s   Poll:  link_downed = 0       (current < previous → reset detected)
        delta = 0 (current value), threshold NOT breached
        breached was true → transition to healthy
        → Emit RECOVERY event (IsHealthy=true, IsFatal=false)
        → Set breached=false in persistent state
T=25s   Poll:  link_downed = 0       (delta=0, not breached, no event)
```

**Without breach tracking**: After the admin reset at T=15s, the monitor would see `delta=0`, emit nothing, and the node would remain stuck in an unhealthy state on the platform indefinitely — even though the admin fixed the issue.

**With pod restart between T=15s and T=20s**: Without persistent state, the monitor loses all knowledge that `link_downed` was previously breached. The new pod starts fresh, sees `link_downed=0`, and never emits a recovery event. The persistent state file ensures the `breached=true` flag survives pod restarts.

### 6.5 Boot ID Handling

On host reboot, the node may come back with **entirely different hardware** (the CSP may have replaced NICs during maintenance). Kernel-maintained counters, port snapshots, and the observed device set are stale and must be discarded. On the first poll with a **complete enumeration** after the reboot, each check emits a **check-scoped clear** — a healthy event with empty `EntitiesImpacted`, which downstream consumers (platform-connector node conditions and fault-quarantine) treat as "every entity of this check recovered". This is what clears conditions whose entities no longer exist on the new boot (renamed devices, replaced hardware, removed counters); the **per-entity healthy baseline events** that follow re-assert health for the entities that are still observable. The clear is emitted first in the batch and backdated so that consumers ordering events by `GeneratedTimestamp` can never apply it after — and thereby wipe — the same batch's current-boot events.

While any device is enumerable but unreadable, the reconciliation is **deferred** (persisted as a pending flag so it survives pod restarts): running the clear then would wipe the unreadable device's previous-boot conditions with nothing able to re-assert them. Readable devices are monitored normally in the meantime, and the reconciliation poll **replays current truth** in the same batch, right after the clear: still-fatal port states are re-asserted through the first-poll peer-evidence gate; breaches latched during the partial window are re-emitted — from live evaluation when the counter is readable, otherwise from the entity identity persisted with the breach flag (absent device, missing counter file, vanished netdev), with an administratively reset counter consuming its latch instead of replaying it; and device- and port-disappearances detected during the window are re-asserted with their latches retained (window provenance is persisted, so this survives pod restarts). Outstanding card/device/port event latches from the previous boot are consumed by the clear (anything still wrong on the new boot re-latches with fresh entities).

**Algorithm:**

1. On startup, read current boot ID from `/proc/sys/kernel/random/boot_id`
2. Compare to the boot ID stored in the persistent state file
3. **If boot IDs differ** (host rebooted):
   - Clear counter snapshots, breach flags, port states, known devices, and partial disappearance miss counts; preserve outstanding card/device FATAL latches
   - Update the stored boot ID
   - On the **first poll cycle with a complete enumeration** (deferred while any device is unreadable; the owed reconciliation is persisted across pod restarts), emit baseline events:
     - **All enabled checks**: Emit one **check-scoped clear** (healthy event, empty `EntitiesImpacted`) as the first event of the batch, wiping every stale condition recorded under the check's name — including conditions for entities that no longer exist on this boot — then replay any faults observed during the pre-reconciliation window so they are not lost with the clear.
     - **State checks**: For every port that is currently `ACTIVE/LinkUp`, emit a **healthy event** (`IsHealthy=true`). This clears any stale FATAL port conditions on the platform from the previous boot. Ports that are currently unhealthy (e.g., `DOWN`, `Disabled`) emit fatal events only when first-poll peer evidence shows the card is anomalous; unhealthy ports without peer evidence are logged and suppressed.
     - **Counter checks**: Emit a **healthy event** (`IsHealthy=true`) for every configured counter. Since counters reset to 0 on reboot and there is no previous value to compute a delta from, all counters are below threshold on the first poll. This clears any stale counter breach conditions on the platform. The first poll also establishes the counter baseline:
       - **Delta counters** evaluate against the new baseline starting from the **second poll** (one polling interval later) — any increment above threshold triggers an unhealthy event.
       - **Velocity counters** wait for their full `velocityUnit` window (1s / 1m / 1h) to elapse against the new baseline before evaluating; they do not extrapolate from a partial sample.
   - Rationale: the node is effectively a fresh machine after reboot — NICs may have been replaced, firmware updated, cables reseated. The platform must be told that all previously-reported conditions are resolved unless new issues are detected on this boot.
4. **If boot IDs match and the discovery scope is unchanged** (pod restart, same host boot):
   - Restore all persisted state (counter snapshots, breach flags, port/device snapshots, debounce state, and outstanding latches)
   - Resume normal boundary-crossing detection with full context
5. **If boot IDs match but the discovery scope changed** (`nicExclusionRegex` / `nicInclusionRegexOverride` differ from the persisted scope):
   - Discard port/device snapshots, known devices, and disappearance miss counts — the state checks re-baseline against the new scope and re-emit healthy baselines
   - Preserve counter snapshots, breach flags, and outstanding card/device FATAL latches: hardware counters do not reset on a scope change, and an outstanding FATAL held downstream must not be orphaned

> **Consistency with sibling monitors**: This boot ID mechanism matches the pattern used by the GPU health monitor (`--state-file` with boot ID) and the syslog health monitor (`state.json` with `boot_id` and journal cursors).

### 6.6 Persistent State File

The monitor persists its operational state to a JSON file on a hostPath-backed volume, enabling it to survive pod restarts without losing counter context.

**State File Path**: `/var/run/nic_health_monitor/state.json`

**Kubernetes Volume Mount:**

```yaml
volumes:
  - name: nic-state-vol
    hostPath:
      path: /var/run/nic_health_monitor
      type: DirectoryOrCreate

volumeMounts:
  - name: nic-state-vol
    mountPath: /var/run/nic_health_monitor
```

**State File Structure:**

```go
// MonitorState is the persistent state written to disk as JSON.
// This single state file is shared by both state checks and counter checks.
type MonitorState struct {
    Version          int                          `json:"version"`
    BootID           string                       `json:"boot_id"`
    Scope            string                       `json:"scope"`

    // Counter detection state
    CounterSnapshots map[string]CounterSnapshot   `json:"counter_snapshots"`
    BreachFlags      map[string]CounterBreachFlag `json:"breach_flags"`

    // State detection state (port state and device presence)
    PortStates       map[string]PortStateSnapshot `json:"port_states"`
    KnownDevices     []string                     `json:"known_devices"`
    AnomalousCards   map[string]AnomalousCardFlag `json:"anomalous_cards"`
    DisappearedDevices map[string]DisappearedDeviceFlag `json:"disappeared_devices"`
    DeviceMissCounts map[string]DeviceMissCount   `json:"device_miss_counts"`
}

// CounterSnapshot stores the value and wall-clock timestamp of a counter
// reading. It plays two distinct roles depending on the configured
// thresholdType:
//   - For "delta" thresholds, the snapshot is updated every poll. Δ is
//     just current_value − snapshot.value over the polling interval.
//   - For "velocity" thresholds, the snapshot is held for the full
//     velocityUnit window (1s / 1m / 1h). Evaluation only happens once
//     elapsed ≥ window, and the rate is computed as delta / elapsed in
//     the configured unit. After evaluation the snapshot is advanced
//     to the current reading so the next window starts fresh. This
//     avoids extrapolating a 1s sample into an hourly rate.
//
// Because the timestamp is persisted, velocity windows survive pod
// restarts: the new pod resumes from the persisted (value, timestamp)
// instead of restarting the window from zero.
type CounterSnapshot struct {
    Value     uint64    `json:"value"`
    Timestamp time.Time `json:"timestamp"`
}

// CounterBreachFlag tracks whether a counter has an active threshold breach.
// This is needed because the breach state cannot be derived from the counter
// value alone — it depends on the delta at the time of the original breach,
// not the absolute value. Without this flag, the monitor cannot emit recovery
// events after an admin counter reset.
type CounterBreachFlag struct {
    Breached  bool      `json:"breached"`
    CheckName string    `json:"check_name"`
    IsFatal   bool      `json:"is_fatal"`
    Since     time.Time `json:"since"`
}

// PortStateSnapshot captures the last-known state of a port for the state
// checks. Persisting this enables recovery event emission after pod restart:
// if a port was DOWN (fatal event sent) and an admin fixes the cable while
// the pod is restarting, the new pod can detect the DOWN→ACTIVE transition
// and emit a recovery event (IsHealthy=true). Without this, the platform
// would remain stuck in the FATAL state for that port.
// Also enables device disappearance detection across restarts via KnownDevices.
//
// LinkLayer ("InfiniBand" or "Ethernet") lets each state check filter the
// shared map to its own ports on startup so the IB and Ethernet checks
// don't treat each other's entries as "disappeared" during the seed.
type PortStateSnapshot struct {
    State         string `json:"state"`                  // raw sysfs value, e.g., "4: ACTIVE", "1: DOWN"
    PhysicalState string `json:"physical_state"`         // raw sysfs value, e.g., "5: LinkUp", "3: Disabled"
    Device        string `json:"device"`                 // e.g., "mlx5_0"
    Port          int    `json:"port"`
    LinkLayer     string `json:"link_layer,omitempty"`   // "InfiniBand" | "Ethernet"
}
```

**Map keys**: Counter snapshots and breach flags use the key format `<device>:<port>:<counter_name>` (e.g., `mlx5_0:1:link_downed`). Port state snapshots use `<device>_<port>` (e.g., `mlx5_0_1`). `KnownDevices` is a flat list of device names (e.g., `["mlx5_0", "mlx5_1", ...]`).

**Save triggers**: The state file is written after each poll cycle completes (both state and counter checks). Errors during save are logged as warnings but do not halt monitoring.

**Load behavior**: On startup, the monitor attempts to load the state file. If the file is missing or corrupt, the monitor starts with empty state (equivalent to first boot). A warning is logged.

### 6.7 Rationale

- When a counter resets, the new value represents errors accumulated since the reset
- This is a conservative approach: we may slightly undercount errors immediately after a reset
- Alternative (treating reset as zero delta) could miss real errors that occurred during/after reset
- **Admin-initiated resets are a legitimate remediation action** — the monitor must recognize them and clear the unhealthy condition by emitting a recovery event
- Driver reloads are logged separately by the Syslog Health Monitor, providing correlation context
- Persistent state ensures recovery events survive pod restarts, preventing nodes from being permanently stuck in an unhealthy state
- **Per-counter timestamps enable accurate velocity calculation** — the snapshot is held for the configured velocityUnit window (1s / 1m / 1h) and the rate is computed over the real wall-clock elapsed time, not extrapolated from a single poll. This means a `120/hour` threshold genuinely observes one hour of data, not 3600 × the per-second rate. Persisting the timestamp lets the window survive pod restarts: the new pod resumes from the persisted snapshot instead of starting a fresh window.

---

## 7. Missing Counter Handling

Not all counters are available on all NIC versions or firmware revisions. The monitor must gracefully handle missing counters to ensure portability across different hardware generations (ConnectX-5, ConnectX-6, ConnectX-7, etc.).

### 7.1 Design Principles

- **Fail-open for missing counters**: If a counter file does not exist, skip it silently. Do not emit errors or events.
- **Log at startup only**: On monitor initialization, log which counters are available vs. unavailable for debugging purposes. Do not repeatedly log missing counters during polling.
- **Graceful degradation**: The monitor should function with whatever subset of counters is available. A node with an older NIC still benefits from the counters that do exist.
- **Configuration flexibility**: Allow operators to disable specific counters via configuration if they are known to be unavailable or irrelevant for their environment.

### 7.2 Common Counter Availability by NIC Generation

| Counter                 | ConnectX-5 | ConnectX-6 | ConnectX-7 |
|-------------------------|------------|------------|------------|
| `symbol_error`          | Yes        | Yes        | Yes        |
| `link_error_recovery`   | Yes        | Yes        | Yes        |
| `link_downed`           | Yes        | Yes        | Yes        |
| `port_rcv_errors`       | Yes        | Yes        | Yes        |
| `roce_slow_restart`     | No         | Yes        | Yes        |
| `local_ack_timeout_err` | Yes        | Yes        | Yes        |

> **Note**: Counter availability may also depend on firmware version and driver configuration. The monitor should always verify counter existence at runtime rather than relying on static assumptions.

---

## 8. RDMA vs TCP/IP Counter Domains

> **Critical Architectural Note: RDMA vs TCP/IP Counter Domains**
>
> For RoCE devices, there are **TWO separate counter domains**:
>
> | Counter Location | Tracks | Example Traffic |
> |------------------|--------|-----------------|
> | `/sys/class/infiniband/<dev>/ports/<port>/counters/` | **RDMA traffic only** | ib_write_bw, distributed apps |
> | `/sys/class/net/<iface>/statistics/` | **TCP/IP traffic only** | ping, ssh, HTTP |
>
> **Field-validated observation**: Running `ping` through a RoCE interface does NOT increment
> InfiniBand counters (`port_rcv_data`, `port_xmit_data` stay at 0). The ping goes through the
> TCP/IP stack and is tracked in Ethernet statistics instead.
>
> **Implication for monitoring**: To detect RDMA-specific degradation (which affects distributed
> workloads), you MUST monitor the InfiniBand counters. Ethernet statistics alone will miss
> RDMA-layer issues like `roce_slow_restart` errors.

### 8.1 Counter Domain Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                    RDMA vs TCP/IP COUNTER DOMAINS                                │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│                         ┌─────────────────────────────┐                         │
│                         │      APPLICATION LAYER      │                         │
│                         └──────────────┬──────────────┘                         │
│                                        │                                         │
│                    ┌───────────────────┼───────────────────┐                    │
│                    │                   │                   │                    │
│                    ▼                   │                   ▼                    │
│  ┌─────────────────────────┐           │     ┌─────────────────────────┐        │
│  │      RDMA STACK         │           │     │      TCP/IP STACK       │        │
│  │  (NCCL, MPI, ib_*)      │           │     │  (HTTP, SSH, ping)      │        │
│  └───────────┬─────────────┘           │     └───────────┬─────────────┘        │
│              │                         │                 │                      │
│              ▼                         │                 ▼                      │
│  ┌─────────────────────────┐           │     ┌─────────────────────────┐        │
│  │ InfiniBand Counters     │           │     │ Ethernet Statistics     │        │
│  │ /sys/class/infiniband/  │           │     │ /sys/class/net/         │        │
│  │ <dev>/ports/<p>/counters│           │     │ <iface>/statistics/     │        │
│  │                         │           │     │                         │        │
│  │ • symbol_error          │           │     │ • rx_bytes              │        │
│  │ • port_rcv_errors       │           │     │ • tx_bytes              │        │
│  │ • roce_slow_restart     │           │     │ • rx_errors             │        │
│  │ • port_rcv_data         │           │     │ • carrier_changes       │        │
│  └───────────┬─────────────┘           │     └───────────┬─────────────┘        │
│              │                         │                 │                      │
│              └─────────────────────────┴─────────────────┘                      │
│                                        │                                         │
│                                        ▼                                         │
│                         ┌─────────────────────────────┐                         │
│                         │     PHYSICAL NIC HARDWARE   │                         │
│                         │        (ConnectX-6)         │                         │
│                         └─────────────────────────────┘                         │
│                                                                                  │
│  ═══════════════════════════════════════════════════════════════════════════    │
│                                                                                  │
│  KEY INSIGHT: Monitor InfiniBand counters for RDMA workload health              │
│               Ethernet stats won't catch roce_slow_restart!                      │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## 9. Data Structures

### 9.1 Counter Structures

```go
// CounterSnapshot represents a point-in-time reading of all counters for a port
type CounterSnapshot struct {
    Device    string             `json:"device"`
    Port      int                `json:"port"`
    Timestamp time.Time          `json:"timestamp"`
    Counters  map[string]uint64  `json:"counters"`  // counter_name -> value
}

// CounterDelta represents the change between two snapshots
type CounterDelta struct {
    Device       string             `json:"device"`
    Port         int                `json:"port"`
    IntervalSec  float64            `json:"interval_sec"`
    Deltas       map[string]uint64  `json:"deltas"`       // counter_name -> delta
    Rates        map[string]float64 `json:"rates"`        // counter_name -> rate/sec
}
```

### 9.2 Persistent State Structures

The monitor persists operational state to survive pod restarts. See [Section 6.6](#66-persistent-state-file) for the on-disk schema and field-level rationale; the structures defined there (`MonitorState`, `CounterSnapshot`, `CounterBreachFlag`, `PortStateSnapshot`) are the canonical reference.

**Example state file content:**

```json
{
  "version": 1,
  "boot_id": "a1b2c3d4-e5f6-7890-abcd-ef1234567890",
  "counter_snapshots": {
    "mlx5_0:1:link_downed": {"value": 3, "timestamp": "2025-06-15T10:30:00Z"},
    "mlx5_0:1:symbol_error": {"value": 1500000, "timestamp": "2025-06-15T10:30:00Z"}
  },
  "breach_flags": {
    "mlx5_0:1:link_downed": {
      "breached": true,
      "check_name": "InfiniBandDegradationCheck",
      "is_fatal": true,
      "since": "2025-06-15T10:25:00Z"
    }
  },
  "port_states": {
    "mlx5_0_1": {"state": "1: DOWN", "physical_state": "3: Disabled", "device": "mlx5_0", "port": 1, "link_layer": "InfiniBand"},
    "mlx5_1_1": {"state": "4: ACTIVE", "physical_state": "5: LinkUp", "device": "mlx5_1", "port": 1, "link_layer": "InfiniBand"}
  },
  "known_devices": ["mlx5_0", "mlx5_1", "mlx5_2", "mlx5_3"],
  "disappeared_devices": {
    "InfiniBand/mlx5_4": {"device": "mlx5_4", "link_layer": "InfiniBand"}
  },
  "device_miss_counts": {
    "InfiniBand/mlx5_3": {"device": "mlx5_3", "link_layer": "InfiniBand", "count": 2}
  }
}
```

### 9.3 Entity Model

NICs and Ports are modeled as separate entity types to enable precise fault localization:

| Entity Type | Entity Value Format | Example  | Use Case                      |
|-------------|---------------------|----------|-------------------------------|
| `NIC`       | `<device_name>`     | `mlx5_0` | Device-level failures         |
| `NICPort`   | `<port_number>`     | `1`      | Port-level counter violations |

---

## 10. Configuration

The counter monitoring system is configurable within a hardcoded allowlist of NIC error/degradation counters. Counter paths, default severity, recommended action, and event descriptions are owned by code. Operators can:

- Define which allowed counters to monitor
- Configure threshold types (delta-based or velocity-based)
- Override default thresholds for specific environments

### 10.1 Configuration Schema

```yaml
# NIC Health Monitor - Counter Detection Configuration
counterDetection:
  # Enable/disable counter monitoring
  enabled: true

  # Counter definitions - configurable for allowed counters only
  # Each counter can specify:
  #   - name:           Allowed counter identifier (used in events)
  #   - enabled:        Enable/disable this counter (default: true)
  #   - thresholdType:  "delta" (absolute change) or "velocity" (rate per time unit)
  #   - threshold:      Numeric threshold value
  #   - velocityUnit:   For velocity thresholds: "second", "minute", "hour"

  counters: []  # See defaults below
```

> **Polling interval**: The polling interval is set globally on the
> Helm chart (`pollingInterval`, default `1s`) and is not configured
> per counter. Velocity thresholds are evaluated against a window
> matching the configured `velocityUnit` (1s / 1m / 1h), independent of
> the polling interval — so a fast 1s poll is suitable for every
> counter type without producing false alerts on long windows.

### 10.2 Default Counter Configuration

The following counters are monitored by default. Operators can override enablement and threshold settings, or add additional counters from the allowed set. The monitor resolves each `name` to its sysfs path, severity, recommended action, and event description.

```yaml
counterDetection:
  enabled: true

  counters:
    #--------------------------------------------------------------------------
    # FATAL COUNTER PROFILES (severity/action owned by code)
    # These counters indicate deterministic workload failure
    #--------------------------------------------------------------------------

    - name: link_downed
      enabled: true
      thresholdType: delta
      threshold: 0              # Any increment (> 0) is fatal
      
    - name: excessive_buffer_overrun_errors
      enabled: true
      thresholdType: delta
      threshold: 0              # Any increment is fatal
      
    - name: local_link_integrity_errors
      enabled: true
      thresholdType: delta
      threshold: 0              # Any increment is fatal
      
    - name: rnr_nak_retry_err
      enabled: true
      thresholdType: delta
      threshold: 0              # Any increment is fatal
    
    #--------------------------------------------------------------------------
    # NON-FATAL COUNTER PROFILES (severity/action owned by code)
    # These counters indicate degradation requiring monitoring
    #--------------------------------------------------------------------------
    
    # Physical Layer (PHY) - two-tier symbol_error monitoring
    - name: symbol_error
      enabled: true
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: second

    - name: symbol_error_fatal
      enabled: true
      thresholdType: velocity
      threshold: 120.0
      velocityUnit: hour
      
    - name: link_error_recovery
      enabled: true
      thresholdType: velocity
      threshold: 5.0
      velocityUnit: minute
    
    # Transport Layer
    - name: port_rcv_errors
      enabled: true
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: second
      
    - name: out_of_sequence
      enabled: true
      thresholdType: velocity
      threshold: 100.0
      velocityUnit: second
      
    - name: local_ack_timeout_err
      enabled: true
      thresholdType: velocity
      threshold: 1.0
      velocityUnit: second
    
    # Congestion Indicators
    - name: port_xmit_discards
      enabled: true
      thresholdType: velocity
      threshold: 100.0
      velocityUnit: second
      
    - name: port_xmit_wait
      enabled: true
      thresholdType: velocity
      threshold: 10000.0
      velocityUnit: second
    
    # RoCE-specific
    - name: roce_slow_restart
      enabled: true
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: second
    
    # Interface Level
    - name: carrier_changes
      enabled: true
      thresholdType: delta
      threshold: 2               # > 2 changes per interval
```

### 10.3 Allowed Counter Customization

The monitor rejects arbitrary counter names at startup. Operators can add entries from the allowlist or override thresholds for existing entries:

```yaml
counterDetection:
  counters:
    # Override: Make the documented fatal symbol_error profile more sensitive.
    - name: symbol_error_fatal
      enabled: true
      thresholdType: velocity
      threshold: 60.0                  # Stricter than the default 120/hour
      velocityUnit: hour
    
    # Add: monitor an allowed Linux interface-level error counter.
    - name: rx_missed_errors
      enabled: true
      thresholdType: velocity
      threshold: 10.0
      velocityUnit: second
    
    # Disable: Turn off a default counter
    - name: port_xmit_wait
      enabled: false
```

Allowed counter selections are hardcoded in `pkg/config/config.go`. They include:

- Standard IB error/degradation counters under `counters/`, such as `symbol_error`, `link_error_recovery`, `link_downed`, `port_rcv_errors`, `port_xmit_discards`, and `port_xmit_wait`.
- mlx5/RDMA hardware error, retry, and RoCE congestion counters under `hw_counters/`, such as `rnr_nak_retry_err`, `local_ack_timeout_err`, `req_transport_retries_exceeded`, `out_of_sequence`, and `roce_slow_restart`.
- Linux interface-level error counters under `statistics/` (`rx_errors`, `rx_crc_errors`, `rx_missed_errors`, `tx_errors`, `tx_carrier_errors`) and the netdev-root attribute `carrier_changes`.

### 10.4 Threshold Processing Algorithm

The evaluator uses **latching breach** semantics: once a counter
breaches its threshold, the breach flag stays set until the counter is
reset (`current < previous`), the host reboots, or the key becomes
permanently unevaluable (counter removed from the configuration, or the
device discovered but out of monitoring scope) — the last case emits an
automatic sweep recovery so the condition cannot be orphaned. Polls
while a counter is already breached emit nothing. A latch whose device
is merely **absent** from discovery is deliberately held: absence can be
a transient firmware reset, and the reboot baseline clear resolves it if
the device never returns.

```
On startup, before the first poll cycle:
  0. Check boot ID (see Section 6.5):
     IF boot ID changed (host rebooted):
       - Clear ALL persisted counter state (snapshots and breach flags)
       - Set reboot_detected = true
     ELSE (pod restart, same boot):
       - Restore all persisted counter state (counter snapshots and breach
         flags also survive a same-boot discovery-scope change — only
         port/device discovery state is scope-sensitive; see Section 6.5)
       - Set reboot_detected = false

For each configured counter (key = <device>:<port>:<counter_name>):
  1. Read current_value from sysfs and capture wall-clock now.

  2. IF reboot_detected AND this is the first poll for the key:
     → Emit HEALTHY baseline event (clears any stale platform condition):
         - IsHealthy = true
         - IsFatal = false
         - RecommendedAction = NONE
         - Message = "Counter {name} healthy after reboot on port {device} port {port}"
         - CheckName: degradation check name
     → Store snapshot = (current_value, now). Continue to next counter.

  3. Reset detection (in this priority order):
     a. IF an in-memory lastPollValue exists for this key AND
        current_value < lastPollValue → reset
     b. ELSE IF a persisted snapshot exists AND
        current_value < snapshot.value → reset
     The lastPollValue path catches mid-window resets in steady state;
     the snapshot.value fallback catches resets that happened during pod
     downtime when lastPollValue is gone.

     IF reset:
       IF previously_breached:
         → Emit RECOVERY event (IsHealthy=true, IsFatal=false,
           RecommendedAction=NONE, CheckName matching the original breach)
         → Clear breach flag
       Update snapshot to (current_value, now). Continue to next counter.

  4. IF previously_breached:
     → No event (latched). Skip evaluation entirely.

  5. Evaluate based on thresholdType:

     IF thresholdType == "delta":
         delta = current_value − snapshot.value
         breach = (delta > threshold)
         Update snapshot to (current_value, now) every poll.

     IF thresholdType == "velocity":
         elapsed = now − snapshot.timestamp
         window = velocityUnit (1s | 1m | 1h)

         IF elapsed < window:
             → Skip evaluation; leave snapshot untouched. The window is
               not yet full. Continue to next counter.

         delta   = current_value − snapshot.value
         rate    = delta / elapsed expressed in velocityUnit
         breach  = (rate > threshold)
         Update snapshot to (current_value, now). The next window starts
         from this reading.

  6. IF breach (and we got here, so we were not previously breached):
     → Emit UNHEALTHY event:
         - IsHealthy = false
         - IsFatal = counter.isFatal
         - RecommendedAction = REPLACE_VM if counter.isFatal, else NONE
         - Message = "{port}: {name} - {description} (value=..., delta=..., rate=...)"
         - CheckName: degradation check name
         - ComponentClass = "NIC"
     → Set breached = true in persistent breach flags.

     IF NOT breach: no event.

  7. Record current_value as lastPollValue (in-memory) for the next poll.

After all counters evaluated:
  8. Save persistent state to disk only if any snapshot or breach flag
     actually changed (to avoid unnecessary writes every poll).
```

### 10.5 Configuration Validation

The monitor validates configuration at startup:

| Validation           | Requirement                               | Action on Failure         |
|----------------------|-------------------------------------------|---------------------------|
| Counter selection    | Allowed `name` resolved to hardcoded path | Reject configuration      |
| Counter path exists  | Internal path must be readable in sysfs   | Log warning, skip counter |
| Threshold is valid   | finite threshold >= 0                     | Reject configuration      |
| velocityUnit valid   | Must be `second`, `minute`, or `hour`     | Reject configuration      |
| thresholdType valid  | Must be `delta` or `velocity`             | Reject configuration      |
| Unique counter names | No duplicate `name` fields                | Reject configuration      |

---

## 11. Event Management

### 11.1 Event Construction

**Example Event Fields (Fatal - link_downed):**

> **Note**: Fatal counter events retain the **degradation check name** (`InfiniBandDegradationCheck` / `EthernetDegradationCheck`). This keeps the counter breach and its eventual counter-reset recovery under one event identity, separate from link-state conditions.

| Field             | Value                                                                                                                    |
|-------------------|--------------------------------------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                                                     |
| CheckName         | `InfiniBandDegradationCheck`                                                                                             |
| ComponentClass    | `NIC`                                                                                                                    |
| Message           | "Port mlx5_0 port 1: link_downed - Port Training State Machine failed - QP disconnect (value=1, delta=1, rate=0.20/sec)" |
| IsFatal           | `true`                                                                                                                   |
| IsHealthy         | `false`                                                                                                                  |
| RecommendedAction | `REPLACE_VM`                                                                                                             |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}, {EntityType: "NICPort", EntityValue: "1"}]`                                |

**Example Event Fields (Non-Fatal - Degradation):**

> **Note**: Non-fatal counter events use the same **degradation check identity** as fatal counter events; `IsFatal` and `RecommendedAction` distinguish their severity and routing.

| Field             | Value                                                                                                                              |
|-------------------|------------------------------------------------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                                                               |
| CheckName         | `InfiniBandDegradationCheck`                                                                                                       |
| ComponentClass    | `NIC`                                                                                                                              |
| Message           | "Port mlx5_0 port 1: symbol_error - PHY bit errors before FEC - physical layer degradation (value=100, delta=100, rate=20.00/sec)" |
| IsFatal           | `false`                                                                                                                            |
| IsHealthy         | `false`                                                                                                                            |
| RecommendedAction | `NONE`                                                                                                                             |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}, {EntityType: "NICPort", EntityValue: "1"}]`                                          |

**Example Event Fields (Recovery - Counter Reset by Admin):**

> **Note**: Recovery events are emitted when a previously breached counter is reset — typically after an administrator clears the counters. The `CheckName` **and** `ErrorCode` (the counter name) match the original breach event, so the recovery clears exactly its own counter's condition: multiple counters breached on the same port hold independent downstream conditions, and recovering one never clears a sibling's still-active fault. Only the check-scoped baseline clear is deliberately code-less (empty `ErrorCode` = clear everything for the check).

| Field             | Value                                                                                     |
|-------------------|-------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                      |
| CheckName         | `InfiniBandDegradationCheck`                                                             |
| ComponentClass    | `NIC`                                                                                     |
| Message           | "Counter link_downed recovered on port mlx5_0 port 1"                                     |
| IsFatal           | `false`                                                                                   |
| IsHealthy         | `true`                                                                                    |
| RecommendedAction | `NONE`                                                                                    |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}, {EntityType: "NICPort", EntityValue: "1"}]` |

### 11.2 Event Routing

| IsFatal | IsHealthy | Action                                        | Use Case                           |
|---------|-----------|-----------------------------------------------|------------------------------------|
| `true`  | `false`   | Immediate gRPC dispatch to Platform Connector | link_downed, buffer overrun        |
| `false` | `false`   | Batched gRPC dispatch (periodic)              | Symbol errors, congestion          |
| `false` | `true`    | Immediate gRPC dispatch to Platform Connector | Counter recovery after admin reset |

---

## Appendix A: Quick Reference - Default Counter Thresholds

> **Note**: Supported counter names and thresholds are configurable via the monitor configuration. Severity, sysfs path, recommended action, and event description come from the monitor's hardcoded counter definitions. See [Section 10: Configuration](#10-configuration) for customization options.

### Fatal Counters (Default: IsFatal = true)

| Counter                           | Path           | Default Threshold | Default Action | Configurable |
|-----------------------------------|----------------|-------------------|----------------|--------------|
| `link_downed`                     | `counters/`    | Delta > 0         | **REPLACE_VM** | Yes          |
| `excessive_buffer_overrun_errors` | `counters/`    | Delta > 0         | **REPLACE_VM** | Yes          |
| `local_link_integrity_errors`     | `counters/`    | Delta > 0         | **REPLACE_VM** | Yes          |
| `rnr_nak_retry_err`               | `hw_counters/` | Delta > 0         | **REPLACE_VM** | Yes          |
| `symbol_error_fatal`              | `counters/`    | > 120/hour        | **REPLACE_VM** | Yes          |

### Driver/Firmware Logs

For kernel log pattern details (fatal and non-fatal classifications, regex patterns, and kernel source references), see [Syslog Detection & Correlation](./syslog-detection-correlation.md).

### Non-Fatal Counters (Default: IsFatal = false)

| Counter                 | Path           | Default Threshold | Default Action | Configurable |
|-------------------------|----------------|-------------------|----------------|--------------|
| `symbol_error`          | `counters/`    | > 10/sec          | Monitor        | Yes          |
| `link_error_recovery`   | `counters/`    | > 5/min           | Monitor        | Yes          |
| `port_rcv_errors`       | `counters/`    | > 10/sec          | Monitor        | Yes          |
| `port_xmit_discards`    | `counters/`    | > 100/sec         | Monitor        | Yes          |
| `roce_slow_restart`     | `hw_counters/` | > 10/sec          | Monitor        | Yes          |
| `local_ack_timeout_err` | `hw_counters/` | > 1/sec           | Monitor        | Yes          |
| `carrier_changes`       | interface      | > 2/interval      | Monitor        | Yes          |

> **Note**: `rnr_nak_retry_err` is **FATAL** by default (see Fatal Counters table above). Counter thresholds can be overridden via configuration; severity changes should use platform connector / analyzer overrides.

### Design Principle

| Source                                        | IsFatal | Recommended Action | Purpose                            |
|-----------------------------------------------|---------|--------------------|------------------------------------|
| **Deterministic Logs**                        | `true`  | `REPLACE_VM`       | Fatal driver/firmware condition    |
| **Port State Changes** (link-state-detection) | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Fatal Counters** (link-counter-detection)   | `true`  | `REPLACE_VM`       | Fatal NIC condition detected       |
| **Diagnostic Logs**                           | `false` | `NONE`             | Evidence/context for investigation |
| **Repeated Non-Fatal Counter Degradation**    | `true`  | `CONTACT_SUPPORT`  | Operator escalation                |

> **Key Insight**: Deterministically fatal events in logs (cmd_exec timeout, etc.) are **Fatal (`IsFatal=true`)** with `RecommendedAction_REPLACE_VM`. Diagnostic logs (insufficient power, High Temperature, module absent) are **Non-Fatal (`IsFatal=false`)**. State and fatal counter conditions are **Fatal (`IsFatal=true`)** with `RecommendedAction_REPLACE_VM`. Repeated non-fatal counter degradation is an analyzer-generated `CONTACT_SUPPORT` escalation, not automatic remediation.

---

## References

### PHY & Signal Integrity
1. [PAM4 Error Correction Challenges in 400GbE (EDN)](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/)
2. [Determine Which Links Are Experiencing Significant Errors - Sun/Oracle (citing IBTA BER Threshold)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)

### Linux Kernel & Driver
3. [sysfs-class-infiniband (Linux Kernel)](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)

### Fabric Diagnostics
4. [ibdiagnet User Manual (NVIDIA)](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf)
5. [Black Hole Detection (sFlow)](https://blog.sflow.com/2016/05/black-hole-detection.html)
6. [InfiniBand™ Architecture Specification (IBTA)](https://www.infinibandta.org/ibta-specification/)

### Vendor Monitoring Guides
7. [InfiniBand Errors Dashboard - HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US)
8. [HPC Clusters Using InfiniBand on IBM Power Systems - IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)
9. [NVIDIA UFM InfiniBand Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv41814/infiniband-port-counters)
10. [NVIDIA DOCA Telemetry Service Guide](https://docs.nvidia.com/doca/archive/2-9-3/doca+telemetry+service+guide/index.html)
11. [NVIDIA UFM Telemetry - InfiniBand Cluster Bring-Up](https://docs.nvidia.com/networking/display/infinibandclusterbringupprocedure/UFM+Telemetry)

### RDMA Programming References
12. [ibv_modify_qp(3) — Linux Manual Page (rnr_retry, retry_cnt)](https://man7.org/linux/man-pages/man3/ibv_modify_qp.3.html)
13. [NVIDIA RDMA-Aware Programming - Queue Pair Bringup](https://docs.nvidia.com/networking/display/rdmaawareprogrammingv17/queue+pair+bringup+(ibv_modify_qp))

---
