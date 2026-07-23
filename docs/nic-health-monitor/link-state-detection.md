# NIC Health Monitor: Link State Detection

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [State Monitoring Specification](#3-state-monitoring-specification)
4. [Management NIC Exclusion, NIC Role Classification, and Uncabled Port Detection](#4-management-nic-exclusion-and-uncabled-port-detection)
5. [Device Discovery and Parsing](#5-device-discovery-and-parsing)
6. [State Change Handling](#6-state-change-handling)
7. [Device Disappearance Handling](#7-device-disappearance-handling)
8. [SR-IOV Virtual Function Handling](#8-sr-iov-virtual-function-handling)
9. [RoCE State Monitoring](#9-roce-state-monitoring)
10. [Supported Hardware](#10-supported-hardware)
11. [Data Structures](#11-data-structures)
12. [Configuration](#12-configuration)
13. [Event Management](#13-event-management)
- [Appendix A: Quick Reference - Fatal Condition Classification](#appendix-a-quick-reference---fatal-condition-classification)

**Related Documents:**
- [Link Counter Detection](./link-counter-detection.md) - Counter-based degradation monitoring
- [Syslog Detection & Correlation](./syslog-detection-correlation.md) - Kernel log monitoring and repeat failure detection

---

## 1. Overview

### 1.1 Problem Statement

Modern GPU clusters suffer from **Grey Failures** (subtle degradations) and **straggler effects** where a single degraded link throttles thousands of GPUs. Simple UP/DOWN polling is the first line of defense for detecting hard failures where the NIC becomes completely unavailable.

### 1.2 Scope of Link State Detection

This document covers the **State Monitoring** component of the NIC Health Monitor, which detects:

- **Hard UP/DOWN transitions** - Link completely lost, no connectivity
- **Device disappearance** - NIC no longer visible in sysfs (fell off PCIe bus)
- **Physical state changes** - Port disabled, polling, or in error recovery
- **Uncabled port anomaly detection** - Card has fewer active ports than its peers (via homogeneity check)
- **Management NIC auto-exclusion** - NICs on NUMA nodes with no compute GPU are automatically excluded

### 1.3 Binary Severity Model

This monitor uses a binary severity model based on **workload impact**:

| Severity      | Meaning                                  | Example                                            |
|---------------|------------------------------------------|----------------------------------------------------|
| **Fatal**     | Workload WILL fail or HAS failed         | NIC DOWN, device disappeared, phys_state=Disabled  |
| **Non-Fatal** | Degradation detected, workload continues | Transient state changes that recover automatically |

**Key Design Principle**: The only question that matters is **"Will the running workload fail because of this?"**

### 1.4 State Detection Overview Diagram

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                     LINK STATE DETECTION FLOW                               │
├─────────────────────────────────────────────────────────────────────────────┤
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                     DATA SOURCES (sysfs)                             │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │  /sys/class/infiniband/<dev>/ports/<port>/                          │   │
│  │  ├── state           →  Logical state (DOWN, INIT, ARMED, ACTIVE)   │   │
│  │  └── phys_state      →  Physical state (LinkUp, Disabled, Polling)  │   │
│  │                                                                      │   │
│  │  /sys/class/net/<interface>/                                         │   │
│  │  └── operstate       →  Interface state (up, down, unknown)         │   │
│  │                                                                      │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                  STATE MONITOR (1s polling interval)                 │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │                                                                      │   │
│  │  DETECTS:                                                            │   │
│  │  ├── Hard DOWN            → Link completely lost (FATAL)            │   │
│  │  ├── Device disappeared   → NIC not in sysfs (FATAL)               │   │
│  │  ├── Uncabled port anomaly→ Card below peer mode (FATAL)            │   │
│  │  ├── Physical disabled    → Port disabled (FATAL)                   │   │
│  │  └── Link error recovery  → Active link problems (FATAL)           │   │
│  │                                                                      │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │              RAW EVENTS → PLATFORM CONNECTOR → MongoDB               │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                     │                                       │
│                                     ▼                                       │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │            HEALTH EVENTS ANALYZER (Correlation Rules)                │   │
│  ├─────────────────────────────────────────────────────────────────────┤   │
│  │  • Repeated non-fatal NIC degradation/syslog escalation              │   │
│  │  • Stabilization Windows: Prevent alert blinking                     │   │
│  │  • Cross-node correlation: Detect fabric-wide issues                 │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## 2. Architecture

### 2.1 Design Rationale: NVSentinel's "Report Raw, Correlate Centrally" Pattern

The State Monitor follows NVSentinel's established architectural pattern where:

1. **Health Monitors (DaemonSets)** report **raw events as-is** to the Platform Connector
2. **Health Events Analyzer (Centralized Deployment)** performs all correlation, aggregation, and pattern detection
3. **MongoDB** serves as the source of truth for event history and correlation queries

| Architectural Principle     | Implementation                             | Purpose                                                                   |
|-----------------------------|--------------------------------------------|---------------------------------------------------------------------------|
| **Raw Event Reporting**     | Health transition or fatal severity escalation → immediate event | One event per affected entity (port, card, or device) per reportable transition                    |
| **Centralized Correlation** | Health Events Analyzer MongoDB pipelines   | Flexible, configurable rules without monitor code changes                 |
| **Temporal Correlation**    | Analyzer rules with time windows           | Detects patterns like "3 link flaps in 10 minutes"                        |
| **Stabilization Windows**   | Analyzer rules with sticky XID-style logic | Prevents "Alert Blinking" where transient recoveries hide critical issues |

### 2.2 Component Responsibilities

| Component                            | Responsibility                                                                                                                              | What It Does NOT Do                                        |
|--------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------|
| **NIC Health Monitor (State Check)** | Poll sysfs state files, detect transitions and confirmed disappearance, persist lifecycle state, emit raw events and recovery events | Aggregation, deduplication, correlation, pattern detection |
| **Health Events Analyzer**           | Correlate events, detect link flap patterns, escalate severity                                                                              | Direct hardware access                                     |

> **Local State Persistence**: The State Check persists port snapshots, known devices, disappearance debounce counters, outstanding card/device/port FATAL latches (with this-boot provenance), and any owed baseline reconciliation in the shared hostPath-backed state file (see [Link Counter Detection, Section 6.6](./link-counter-detection.md#66-persistent-state-file)). This enables recovery after pod restart, debounced disappearance detection across restarts, and a matching healthy event when a disappeared device re-enumerates — or a disappeared port reappears (port disappearances latch just like device disappearances, so the reappeared port's healthy observation clears the FATAL instead of being suppressed as first-seen). On a baseline run (host reboot or discovery-scope change) each check first emits a **check-scoped clear** — a healthy event with empty entities that wipes every stale downstream condition for the check, including conditions whose devices were renamed or removed — then re-asserts current truth: per-port healthy baselines, still-fatal ports (through the first-poll peer-evidence gate), and window-detected device/port disappearances. The reconciliation waits for the first complete enumeration (monitoring of readable devices starts earlier) and survives pod restarts via persisted pending flags and latch provenance. Previous-boot latches are consumed by the clear; anything still wrong on the new boot re-latches with fresh entities. See [Link Counter Detection, Section 6.5](./link-counter-detection.md#65-boot-id-handling) for the full reconciliation contract.

### 2.3 State Check Data Flow (1s polling interval)

```
Reads:
├── state          → Logical link state (DOWN, INIT, ARMED, ACTIVE)
├── phys_state     → Physical layer state (LinkUp, Disabled, Polling, LinkErrorRecovery)
└── operstate      → Ethernet interface state (up, down, unknown)

Detects:
├── Hard DOWN            → Link completely lost, no connectivity
├── Device disappearance → NIC no longer visible in sysfs
├── Uncabled port anomaly→ Card has fewer active ports than peers
└── Physical disabled    → Port administratively or physically disabled

On device disappearance:
└── Device absent from 3 complete enumerations → Hardware failure (FATAL)

Persists (after successful event publication, or immediately for a no-event poll):
├── Port state snapshots       → state + phys_state per port
├── Known devices/miss counts  → disappearance debounce across same-boot pod restarts (reset on host reboot)
└── Outstanding event latches  → card/device recovery after restart or reboot

Emits: Raw STATE_CHANGE events → Platform Connector → MongoDB
       Recovery events (IsHealthy=true) when previously-DOWN port recovers
       (Link flap detection handled by Health Events Analyzer)
```

### 2.4 System Context

```
┌────────────────────────────────────────────────────────────────────────────────┐
│                      NVSentinel NIC STATE MONITORING                           │
├────────────────────────────────────────────────────────────────────────────────┤
│                                                                                │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │                       PER-NODE DAEMONSET                                 │  │
│  ├──────────────────────────────────────────────────────────────────────────┤  │
│  │                                                                          │  │
│  │  ┌────────────────────────────────────┐                                  │  │
│  │  │     NIC HEALTH MONITOR             │                                  │  │
│  │  │     (State Check - 1s interval)    │                                  │  │
│  │  │     ══════════════════════════     │                                  │  │
│  │  │                                    │                                  │  │
│  │  │  DATA SOURCES:                     │                                  │  │
│  │  │  • /sys/class/infiniband/          │                                  │  │
│  │  │  • /sys/class/net/                 │                                  │  │
│  │  │  • /sys/bus/pci/devices/           │                                  │  │
│  │  │                                    │                                  │  │
│  │  │  CHECKS:                           │                                  │  │
│  │  │  • InfiniBandStateCheck            │                                  │  │
│  │  │  • EthernetStateCheck              │                                  │  │
│  │  │                                    │                                  │  │
│  │  │  BEHAVIOR:                         │                                  │  │
│  │  │  • Reports RAW state events        │                                  │  │
│  │  │  • Persistent local state          │                                  │  │
│  │  │    (port states, known devices,    │                                  │  │
│  │  │    counter snapshots, breach flags,│                                  │  │
│  │  │    boot ID)                        │                                  │  │
│  │  │  • Correlation centralized         │                                  │  │
│  │  └──────────────┬─────────────────────┘                                  │  │
│  │                 │                                                        │  │
│  └─────────────────┼────────────────────────────────────────────────────────┘  │
│                    │                                                           │
│                    ▼                                                           │
│  ┌──────────────────────────────────┐                                          │
│  │     PLATFORM CONNECTOR           │                                          │
│  │     ══════════════════           │                                          │
│  │  • Receives raw events           │                                          │
│  │  • Persists to MongoDB           │                                          │
│  │  • Triggers downstream           │                                          │
│  └──────────────┬───────────────────┘                                          │
│                 │                                                              │
│                 ▼                                                              │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │                    HEALTH EVENTS ANALYZER                                │  │
│  │                    (Repeated Non-Fatal Escalation)                       │  │
│  │                    ══════════════════════                                │  │
│  │                                                                          │  │
│  │  NIC CORRELATION RULES:                                                  │  │
│  │  • RepeatedNICDegradation: 3 non-fatal degradation events in 1h          │  │
│  │    (same NIC + same NICPort)                                             │  │
│  │  • RepeatedNICDriverError: 3 selected non-fatal syslog events in 1h      │  │
│  │    (same node + same pattern)                                             │  │
│  └──────────────────────────────────────────────────────────────────────────┘  │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

---

## 3. State Monitoring Specification

### 3.1 Port States (Full Enumeration)

Port states are defined by the Linux kernel InfiniBand sysfs interface. **Reference**: [Linux Kernel sysfs-class-infiniband ABI](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)

```go
const (
    // Port logical states
    IBStateDown    = "1: DOWN"      // No connectivity
    IBStateInit    = "2: INIT"      // Initializing (problematic if stuck >30s)
    IBStateArmed   = "3: ARMED"     // Armed but not active (check SM)
    IBStateActive  = "4: ACTIVE"    // Normal operational state

    // Port physical states
    IBPhysStateSleep     = "1: Sleep"
    IBPhysStatePolling   = "2: Polling"                    // Link training
    IBPhysStateDisabled  = "3: Disabled"                   // CRITICAL - port disabled
    IBPhysStateTraining  = "4: PortConfigurationTraining"  // Link negotiation
    IBPhysStateLinkUp    = "5: LinkUp"                     // Normal
    IBPhysStateLinkErr   = "6: LinkErrorRecovery"          // Active error recovery
    IBPhysStatePhyTest   = "7: Phy Test"                   // Diagnostic mode
)
```

### 3.2 State Transitions

**Logical State Flow**: `DOWN (1)` → `INIT (2)` → `ARMED (3)` → `ACTIVE (4)`

- **DOWN**: No connectivity. **Fatal** on a runtime transition from healthy; on the first poll the severity is gated by peer evidence (see Section 3.4 and Section 4.3.2).
- **INIT**: Initializing — normal transient state during startup. Every port passes through INIT during boot and Subnet Manager configuration. For **InfiniBand ports**, a runtime transition into INIT is classified as **Non-Fatal** (`IsFatal=false`) because INIT can persist while waiting for SM configuration. For **Ethernet/RoCE ports**, INIT is a brief sub-second transient during link training and is not reported (logged at DEBUG level only). A port stuck in INIT does not satisfy the `ACTIVE/LinkUp` condition, so **on the first poll** a card stuck in INIT falls below its role group's decisive mode and is flagged Fatal by the card homogeneity check (see Section 4.3); at runtime the transition remains non-fatal (the homogeneity check does not re-run after the first poll).
- **ARMED**: Waiting for Subnet Manager — same rationale as INIT. For **InfiniBand ports**, classified as **Non-Fatal** (`IsFatal=false`). For **Ethernet/RoCE ports**, this state is rare/transient and is not reported. As with INIT, a card stuck in ARMED is caught by the homogeneity check on the first poll only.
- **ACTIVE**: Normal operational state (HEALTHY)

**Physical State Substates**: `Sleep (1)`, `Polling (2)`, `Disabled (3)`, `Training (4)`, `LinkUp (5)`, `LinkErrorRecovery (6)`

- **Polling (2)**: Transient state during link training. Every port passes through Polling when establishing a connection. Classified as **Non-Fatal** (`IsFatal=false`). A port that remains in Polling does not count as active, so **on the first poll** its card falls below the role group's decisive mode and is flagged Fatal by the homogeneity check (Section 4.3); a runtime transition into Polling remains non-fatal. Sustained link instability is additionally covered by the `link_downed` and `link_error_recovery` counter checks (see [Link Counter Detection](./link-counter-detection.md)).
- **LinkErrorRecovery (6)**: Active error recovery in progress. Classified as **Non-Fatal** (`IsFatal=false`) because the HCA firmware is actively retrying. If recovery fails before the monitor's first poll, the card homogeneity check (Section 4.3) escalates to Fatal by detecting fewer active ports than its role peers; at runtime the transition remains non-fatal and degradation is tracked by the counter checks.

### 3.3 Diagnostic Commands

```bash
# Check logical and physical port states
ibstat
# Output:
# CA 'mlx5_0'
# 	Port 1:
# 		State: Active
# 		Physical state: LinkUp
# 		Rate: 400
# 		Link layer: InfiniBand

# Check specific port state via sysfs
cat /sys/class/infiniband/mlx5_0/ports/1/state
# Output: 4: ACTIVE

cat /sys/class/infiniband/mlx5_0/ports/1/phys_state
# Output: 5: LinkUp
```

### 3.4 State-Based Event Generation Algorithm

**Port Health Evaluation Steps:**

1. **Read port state** from `/sys/class/infiniband/<dev>/ports/<port>/state` and `phys_state`
2. **Load previous port state** from persistent state file (or in-memory if available from a prior poll in this pod's lifetime)
3. **Determine health status:**
   - If `state = ACTIVE` AND `phys_state = LinkUp` → **Healthy**
   - Otherwise → **Unhealthy** (the specific state/phys_state combination determines the message)

4. **Emit event on a health boundary crossing or fatal severity escalation:**
   - **First poll after host reboot (boot ID changed — state cleared)**:
     - Observational state has been discarded; outstanding card/device FATAL latches remain pending (see [Link Counter Detection, Section 6.5](./link-counter-detection.md#65-boot-id-handling))
     - Healthy ports (`ACTIVE/LinkUp`): Emit **healthy event** (`IsHealthy=true`) — this clears any stale FATAL conditions on the platform from the previous boot (the node may have had NICs replaced, cables reseated, etc.)
     - Unhealthy ports on **anomalous cards** (active-port count below the role group's decisive mode, group of ≥2 cards — see Section 4.3): Emit **fatal event** — peer comparison provides positive evidence the port is supposed to be up
     - Unhealthy ports on **all other cards** — cards at/above their role mode, **singleton role groups**, and groups with a **tied or zero mode**: Log and **suppress** the event. A port that has never been observed healthy carries no evidence it should be up: an uncabled second port or an intentionally-disabled/unprovisioned port (e.g., the unused Aux frontend port on OCI `BM.GPU.H100.8`, left as a singleton after its Prime twin is excluded as the default-route NIC) is numerically indistinguishable from a failure. Without peer evidence the monitor keeps the state local and does not publish an external `HealthEvent`
   - **First poll with no persisted state (fresh node, corrupt/missing state file)**:
     - Same behavior as the reboot case above
   - **First poll with persisted previous state (pod restart, same boot)**:
     - Compare current health against **persisted** previous state
     - Emit events on boundary crossings as with subsequent polls below (this is the key benefit of persistence — a port that was DOWN before restart and is now ACTIVE triggers a recovery event)
   - **Subsequent polls**: Emit when `wasHealthy ≠ isHealthy`, or when an unhealthy condition escalates from non-fatal to fatal
     - Healthy → Unhealthy: Emit the event selected by the link-layer severity policy (for example, `DOWN/Disabled` is fatal)
     - Unhealthy → Healthy: **HEALTHY event** (e.g., "healthy (ACTIVE, LinkUp)")
     - Unhealthy non-fatal → Unhealthy fatal (e.g., `INIT/LinkUp` → `DOWN/Disabled`): Emit the **FATAL escalation**
     - Other Unhealthy → Unhealthy changes: **No event** — intermediate transition or de-escalation suppressed
     - Healthy → Healthy: **No event** — still healthy

4. **One consolidated event per port per transition:**
   - Logical state and physical state are combined into a single message
   - For Ethernet/RoCE, the operstate is also included in the same event
   - `EntitiesImpacted` includes both NIC and Port entities
   - `RecommendedAction = REPLACE_VM` for fatal events

---

## 4. Management NIC Exclusion and Uncabled Port Detection

This section describes three zero-configuration mechanisms that replace the previous `gpu_port_config` / `AtLeastPorts` / `AtLeastRate` approach. These mechanisms require no per-GPU-type configuration and work automatically across DGX, HGX, Grace-based superchips (GB200/GH200), OEM servers, and cloud VMs.

- **Section 4.1**: NUMA-based management NIC exclusion (exclude NICs on non-GPU NUMA nodes)
- **Section 4.2**: NIC role classification (topo matrix + link layer + default-route exclusion)
- **Section 4.3**: Role-based card homogeneity (detect uncabled ports and failures within each role group)

The classification of each NIC uses a **three-step decision** built from four complementary signals:

1. **Step 1 — Management gate (NUMA locality, Section 4.1)**: Is the NIC on a CPU socket that hosts GPUs? If not, exclude it.
2. **Step 2 — Compute vs Storage (topo matrix + link layer, Section 4.2)**: For NICs that pass Step 1, consult the `nvidia-smi topo -m` GPU↔NIC relationship. If the topo matrix shows PCIe proximity (PIX/PXB), classify as Compute. Otherwise, use the NIC's **link layer** as a tiebreaker: InfiniBand NICs are Compute fabric; Ethernet NICs are Storage.
3. **Step 3 — Default route exclusion (Section 4.2)**: If the NIC carries the host's default IP route, classify as Management regardless of topo or link layer. This catches management NICs that share a NUMA node with GPUs (e.g., on-prem L40S, GB200). The classifier reads the host's `/proc/net/route` (bind-mounted at `/nvsentinel/proc/net/route`) at startup to resolve the default route interface.

These steps use four **complementary signals**, each covering platforms where the others fail:

| Signal                          | What it answers                                 | Platforms where it's the decisive signal                                                |
|---------------------------------|-------------------------------------------------|-----------------------------------------------------------------------------------------|
| **NUMA locality**               | "Is this NIC near any GPU?"                     | A100 DGX (4-socket: mgmt NICs on non-GPU sockets)                                       |
| **Topo matrix (PIX/PXB)**       | "Does this NIC share a PCIe switch with a GPU?" | H100 OCI, A100 OCI (SXM systems with PCIe switch pairing)                               |
| **Link layer (IB vs Ethernet)** | "Is this NIC on the InfiniBand compute fabric?" | On-prem L40S, GB200 (PCIe-only/Grace where topo can't distinguish compute from storage) |
| **Default route**               | "Does this NIC carry host networking?"          | On-prem L40S, GB200 (management NIC shares NUMA with GPUs)                              |

Removing any one signal causes at least one platform to misclassify. Together they cover x86 SXM (DGX/HGX), x86 PCIe (L40S), Grace (GB200/GH200), on-prem datacenter, and OEM/cloud platforms.

> **Hard dependency on metadata**: The NIC Health Monitor requires the raw GPU↔NIC topology matrix (and the GPU list) published by the metadata collector in `/var/lib/nvsentinel/gpu_metadata.json`. The monitor **fails to start** if the file is missing or unreadable, or if `nic_topology` is absent/empty. There is no silent-fallback mode. This is enforced at startup by `topology.LoadFromMetadata()`, which is called before any polling begins; failure returns an error that causes the process to exit. See [Section 12.1](#121-state-monitoring-configuration).
>
> **Responsibility split**: The metadata collector publishes raw facts: per-GPU NUMA nodes (from the `nvidia-smi topo -m` NUMA Affinity column) and the raw per-NIC topology-level matrix (one entry per GPU in `gpus[]` order). The NIC Health Monitor reads these together with per-NIC NUMA nodes (from its own sysfs access — the collector does not enumerate InfiniBand devices) and performs the compute/storage/management classification locally. The monitor never invokes `nvidia-smi` itself; the matrix and GPU NUMA are produced once by the collector and cached in JSON.
>
> **Why `nvidia-smi topo -m` text parsing**: The GPU↔NIC topology relationship is not available through any structured API. NVML exposes three topology functions (`DeviceGetTopologyCommonAncestor`, `DeviceGetTopologyNearestGpus`, `SystemGetTopologyGpuSet`), but all operate exclusively on `nvmlDevice_t` handles which represent GPUs only — NVML has no concept of NIC/InfiniBand devices. DCGM's `dcgmGetDeviceTopology` has the same GPU-only limitation. The `nvidia-smi topo` subcommand does not support `--format=json/xml/yaml` (unlike `nvidia-smi --query-gpu`); the only output format is the whitespace-aligned ASCII matrix (`-m`). No existing open-source library parses the full GPU↔NIC matrix from this output — HAMi's `parseNvidiaNumaInfo` only extracts GPU NUMA affinity (not NIC columns) and was itself replaced with sysfs reads due to parsing fragility. The metadata collector therefore includes a purpose-built parser with handling for known format variations (ANSI escape codes, `NICn` legend remapping, wrapped headers, Grace NUMA ranges).

### 4.1 Management NIC Exclusion (NUMA-Based)

#### 4.1.1 The Problem

DGX systems (e.g., DGX A100) have Mellanox ConnectX management NICs that appear in `/sys/class/infiniband/` alongside compute fabric NICs. If monitored, a management NIC going DOWN would trigger `IsFatal=true` with `RecommendedAction_REPLACE_VM` — an incorrect remediation for a NIC that doesn't affect GPU workloads. The design doc's severity model (Fatal = "workload WILL fail") is specifically designed for compute and storage NICs, not management NICs.

#### 4.1.2 Detection Mechanism

Management NICs on DGX systems are placed on CPU sockets that have **no compute GPUs**. The monitor exploits this by checking whether each NIC's NUMA node has a compute GPU on it:

1. Read `gpus[].numa_node` from `/var/lib/nvsentinel/gpu_metadata.json` (the metadata collector parses this from the `nvidia-smi topo -m` NUMA Affinity column and publishes it per GPU).
2. Build `gpu_numa_set` from the distinct `numa_node` values across all GPUs (ignoring -1 / unknown).
3. For each `mlx5_*` NIC discovered in `/sys/class/infiniband/`, read `/sys/class/infiniband/<dev>/device/numa_node`.
4. If `nic_numa ∉ gpu_numa_set` → **exclude** (management NIC on separate socket).

**Edge case — GPU**: If `gpus[].numa_node = -1` (unknown, common in VMs or single-socket systems), that GPU is excluded from the `gpu_numa_set`. If *all* GPUs have -1, the set is empty and the NIC Health Monitor **fails to start** — without GPU NUMA information the NUMA gate cannot distinguish management NICs from compute NICs, and monitoring everything would risk false `REPLACE_VM` on management NIC failures.

**Edge case — NIC**: If a NIC's `numa_node = -1` (unknown), the NIC is **excluded**. Under-monitoring (missing a NIC failure) is preferable to over-monitoring (issuing a false `REPLACE_VM` on a management NIC that happens to go down).

#### 4.1.3 Field Validation

| Cluster                          | Management NICs                                                                            | NUMA Check Result | Correct? |
|----------------------------------|--------------------------------------------------------------------------------------------|-------------------|----------|
| **A100 OCI RoCE** (4-socket AMD) | `mlx5_0` (NUMA 0), `mlx5_13` (NUMA 6) — no compute GPU on those NUMAs                      | Excluded          | Yes      |
| **L40 on-prem** (2-socket)       | None visible (BMC is non-Mellanox, invisible in `/sys/class/infiniband/`)                  | Nothing excluded  | Yes      |
| **L40S OCI** (2-socket Intel)    | None (all 6 Mellanox PFs share NUMA with GPUs)                                             | All monitored     | Yes      |
| **H100 DGX** (2-socket)          | Storage/mgmt NICs share NUMA with GPUs — correctly kept for monitoring                     | All monitored     | Yes      |
| **H100 OCI** (2-socket Intel)    | None (all 18 Mellanox PFs share NUMA with GPUs)                                            | All monitored     | Yes      |
| **GB200 NVL4** (Grace 2-socket)  | None (all 6 Mellanox PFs share NUMA with GPUs; management handled in Section 4.2 fallback) | All monitored     | Yes      |

> **Design Note**: Storage NICs (e.g., H100 Slot1/Slot2 ConnectX-7 cards) share a NUMA node with compute GPUs. They are intentionally **not excluded** because storage NIC failures also impact workloads (I/O hangs, checkpoint failures). The NUMA check only excludes NICs on NUMA nodes with **zero** compute GPUs.

### 4.2 NIC Role Classification (Topo Matrix)

#### 4.2.1 The Problem

DGX/HGX systems have both **compute fabric NICs** (OSFP ports on the GPU tray) and **storage NICs** (Slot1/Slot2 on the CPU motherboard). These are the same hardware (ConnectX-7) but serve different roles, may have different port counts, and run at different speeds. The card homogeneity check (Section 4.3) must compare NICs of the same role — compute against compute, storage against storage — to avoid false positives.

#### 4.2.2 Detection Mechanism: `nvidia-smi topo -m` Matrix Lookup

The metadata collector runs `nvidia-smi topo -m` on the node at startup, parses the GPU↔NIC relationship matrix into a raw per-NIC array of topology levels (one entry per GPU in `gpus[]` order), and publishes it to `/var/lib/nvsentinel/gpu_metadata.json` under the `nic_topology` field. The NIC Health Monitor consumes this matrix and applies the classification rules below to each NIC locally — no sysfs path walking, no PCIe-depth heuristics, and no direct invocation of `nvidia-smi` in the monitor.

The mapping from NVIDIA topology levels (the `nvmlGpuTopologyLevel_t` enum, displayed as `nvidia-smi topo -m` abbreviations) to NIC roles is:

| NVML topology level   | `nvidia-smi topo` | Meaning                                     | NIC Role                                                                                   |
|-----------------------|-------------------|---------------------------------------------|--------------------------------------------------------------------------------------------|
| `TOPOLOGY_SINGLE`     | **PIX**           | Single PCIe bridge between NIC and GPU      | **Compute** (shares a PCIe switch with a GPU — standard compute fabric NIC on DGX/HGX)     |
| `TOPOLOGY_MULTIPLE`   | **PXB**           | Multiple PCIe bridges between NIC and GPU   | **Compute** (still within a shared PCIe switch hierarchy)                                  |
| `TOPOLOGY_HOSTBRIDGE` | **PHB**           | Shared PCIe host bridge (CPU root complex)  | **Storage** (same host bridge but no switch — behaves like NODE for compute fabric intent) |
| `TOPOLOGY_NODE`       | **NODE**          | Same NUMA node, different PCIe host bridges | **Storage** (on same CPU socket but no PCIe proximity — typical storage NIC layout)        |
| `TOPOLOGY_SYSTEM`     | **SYS**           | Cross-NUMA (SMP interconnect like QPI/UPI)  | Falls through to NUMA-based classification (see Level 1 gate and fallback below)           |

**Classification algorithm** (applied per NIC after discovery):

```
classify_nic(nic):
    # Step 1: Default route exclusion
    # Catches management NICs that share a NUMA node with GPUs
    # (e.g., on-prem L40S, GB200). Runs first so the management NIC
    # is excluded even if it has PCIe proximity to a GPU.
    if device == default_route_device:
        return Management

    # Step 2: NUMA isolation gate
    if nic_numa not in gpu_numa_set:
        return Management

    # Step 3: Role determination (topo + link layer)
    topo = topo_matrix[nic]  # array of relationships, one per GPU

    if any GPU has PIX or PXB:
        return Compute

    # Step 4: Topology-based classification
    if link_layer == "InfiniBand":
        return Compute

    if any GPU has NODE or PHB:
        return Storage

    # All-SYS fallback (Grace/GB200 where GPUs aren't on PCIe)
    return Storage
```

**Precedence explained**:

1. **PIX/PXB → Compute**: The topo matrix authoritatively identifies NICs that share a PCIe switch with a GPU. This is the primary signal on SXM systems (DGX/HGX A100, H100).

2. **Default route → Management**: Runs before topology classification. The classifier reads `/proc/net/route` at startup, finds the default route interface, and maps it to an IB device via `/sys/class/net/<iface>/device/infiniband/`. This prevents the management NIC from being monitored as Storage, avoiding false REPLACE_VM for control-plane network failures. If `/proc/net/route` is unavailable or the interface has no IB backing, the check is silently skipped.

3. **InfiniBand → Compute**: On platforms where no NIC has PIX/PXB to a GPU (PCIe-only GPUs like L40S, or Grace where GPUs aren't on PCIe), the link layer distinguishes compute fabric NICs (InfiniBand) from storage/management NICs (Ethernet). This is the decisive signal on on-prem L40S and GB200.

4. **NODE/PHB → Storage**: NICs that share a NUMA node or host bridge with a GPU but don't share a PCIe switch and aren't InfiniBand. Typical storage NIC layout on H100 OCI (Slot1/Slot2 ConnectX-7 Ethernet cards).

5. **All-SYS fallback → Storage**: NICs on a GPU NUMA but with no PCIe relationship and Ethernet link layer. Safe default: monitored.

#### 4.2.3 Three-Tier Classification

Combined with the NUMA gate from Section 4.1, the monitor assigns each NIC to one of three roles:

| Role           | Detection                                                                                                                     | Monitoring Behavior                                            |
|----------------|-------------------------------------------------------------------------------------------------------------------------------|----------------------------------------------------------------|
| **Management** | NIC NUMA has no compute GPU, **or** NIC carries the host's default route, **or** NIC is a BlueField DPU in the all-SYS branch | Excluded from monitoring entirely — by both the state checks and the counter (degradation) checks, which share one device-eligibility predicate |
| **Compute**    | Any GPU has PIX or PXB relationship to this NIC, **or** NIC link layer is InfiniBand (when no PIX/PXB exists)                 | Monitored; compared against other compute NICs for homogeneity |
| **Storage**    | Ethernet NIC with NODE or PHB to a GPU, **or** Ethernet NIC in all-SYS fallback on GPU NUMA                                   | Monitored; compared against other storage NICs for homogeneity |

> **Key design property**: On every validated platform, InfiniBand NICs and Ethernet NICs end up in **separate classification groups** (Compute vs Storage). This ensures the card homogeneity check (Section 4.3) never compares IB compute fabric NICs against Ethernet storage/management NICs, preventing false positives from hardware diversity (e.g., different port counts, different link speeds).

#### 4.2.4 Field Validation

Verified against real hardware on five distinct platforms covering x86 SXM (A100, H100), x86 PCIe (L40S OCI, on-prem L40S), and Grace (GB200). The link-layer check improves classification on on-prem and GB200 compared to the previous sysfs PCIe path-walk algorithm, while producing identical results on all other platforms.

**A100 OCI RoCE (4-socket AMD EPYC, 8 GPUs, 18 PF NICs):**

| NIC Pattern                             | Topo relationship       | Classification        |
|-----------------------------------------|-------------------------|-----------------------|
| `mlx5_0` (NUMA 0)                       | All SYS, NUMA ∉ GPU set | **Management**        |
| `mlx5_13` (NUMA 6)                      | All SYS, NUMA ∉ GPU set | **Management**        |
| `mlx5_1`–`mlx5_12`, `mlx5_14`–`mlx5_17` | PXB to paired GPUs      | **Compute** (16 NICs) |

Result: 2 Management + 16 Compute + 0 Storage. 18/18 match current algorithm.

**H100 OCI (2-socket Intel Xeon Platinum 8480+, 8 GPUs, 18 PF NICs):**

| NIC       | Topo relationship         | Classification |
|-----------|---------------------------|----------------|
| `mlx5_2`  | NODE to all GPUs (no PXB) | **Storage**    |
| `mlx5_11` | NODE to all GPUs (no PXB) | **Storage**    |
| Other 16  | PXB to one paired GPU     | **Compute**    |

Result: 0 Management + 16 Compute + 2 Storage. Matches documented storage NIC layout on OCI H100.

With the default-route check: `mlx5_2` (Prime frontend port, carries `eth0` and the default route) → **Management** (excluded). Result: 1 Management + 16 Compute + 1 Storage — `mlx5_11` (the intentionally-unprovisioned Aux frontend port, `phys_state: Disabled`, no VNIC) becomes a **singleton** Storage group. Singleton groups are exempt from the homogeneity check and their never-seen-healthy DOWN ports are logged and **suppressed** on the first poll (see Section 4.3.2) — this is what prevents a false `REPLACE_VM` and avoids noisy external events for the by-design-idle Aux port on `BM.GPU.H100.8`.

**L40S OCI (2-socket Intel, 4 PCIe GPUs, 6 PF NICs — all Ethernet/RoCE):**

Every NIC shows NODE to some GPUs and SYS to others; no NIC has any PIX or PXB (L40S is PCIe-attached, not SXM — there are no shared PCIe switches). All 6 NICs are Ethernet (RoCE). The link-layer check does not promote any to Compute (no InfiniBand). NODE → Storage for all.

Result: 0 Management + 0 Compute + 6 Storage. All NICs monitored in a single Storage homogeneity group. This is correct because OCI L40S uses RoCE for all cluster networking — there is no separate compute fabric link layer.

**On-prem L40S (2-socket Intel, 8 PCIe GPUs, 5 PF NICs: 1 Ethernet mgmt + 4 IB compute):**

On-prem datacenter nodes with PCIe GPUs and native InfiniBand for the compute fabric typically have a separate Ethernet NIC for pod networking. The topo matrix shows NODE to local GPUs for all 5 NICs (PCIe-only system, no shared switches). Without the link-layer check, all 5 would be classified as Storage (same group), corrupting the homogeneity check if port counts differ.

With the link-layer check:

| NIC      | Link layer | Topo               | Classification | Reason                               |
|----------|------------|--------------------|----------------|--------------------------------------|
| `mlx5_0` | Ethernet   | NODE to local GPUs | **Storage**    | Ethernet, not PIX/PXB → Storage      |
| `mlx5_1` | InfiniBand | NODE to local GPUs | **Compute**    | IB → Compute (link-layer tiebreaker) |
| `mlx5_2` | InfiniBand | NODE to local GPUs | **Compute**    | IB → Compute                         |
| `mlx5_3` | InfiniBand | NODE to local GPUs | **Compute**    | IB → Compute                         |
| `mlx5_4` | InfiniBand | NODE to local GPUs | **Compute**    | IB → Compute                         |

Result: 0 Management + 4 Compute + 1 Storage. The 4 IB NICs are in the Compute homogeneity group; the Ethernet management NIC is in a separate Storage group. No cross-comparison between IB and Ethernet, preventing false positives from hardware diversity.

With the default-route check: `mlx5_0` (carries default route) → **Management** (excluded). Result: 1 Management + 4 Compute + 0 Storage.

**GB200 NVL4 (2-socket Grace Neoverse-V2, 4 GPUs, 6 PF NICs: 4 ConnectX-7 IB + 2 BlueField-3 DPU):**

Every NIC↔GPU cell is SYS (GPUs are on NVLink-C2C, not PCIe — no shared PCIe ancestor exists). All NIC NUMAs are in the GPU NUMA set. No PIX/PXB or NODE/PHB relationships exist. The **link-layer check** and **HCA-based DPU detection** are the only signals that can distinguish roles:

| NIC           | Link layer | HCA type              | Classification | Reason                   |
|---------------|------------|-----------------------|----------------|--------------------------|
| `ibp3s0`      | InfiniBand | MT4129 (ConnectX-7)   | **Compute**    | IB → Compute             |
| `ibP2p3s0`    | InfiniBand | MT4129 (ConnectX-7)   | **Compute**    | IB → Compute             |
| `ibP16p3s0`   | InfiniBand | MT4129 (ConnectX-7)   | **Compute**    | IB → Compute             |
| `ibP18p3s0`   | InfiniBand | MT4129 (ConnectX-7)   | **Compute**    | IB → Compute             |
| `roceP6p3s0`  | Ethernet   | MT41692 (BlueField-3) | **Management** | BlueField DPU → excluded |
| `roceP22p3s0` | Ethernet   | MT41692 (BlueField-3) | **Management** | BlueField DPU → excluded |

Result: 2 Management (BlueField DPUs) + 4 Compute (IB ConnectX-7) + 0 Storage. The 4 IB NICs are monitored in the Compute homogeneity group; the 2 DPUs are excluded.

Known BlueField HCA types excluded: MT41682 (BlueField-2), MT41686 (BlueField-2 variant), MT41692 (BlueField-3). Unrecognised HCA types fall through to Storage (monitored) — the safe direction for future hardware.

With the default-route check: `roceP6p3s0` (carries default route) would be excluded by Step 3 before the HCA check even runs. Same result, different detection path.

### 4.3 Uncabled Port Detection (Role-Based Card Homogeneity)

#### 4.3.1 The Problem

Some NIC cards have multiple ports, but not all ports are cabled. For example, dual-port ConnectX cards may have only port 1 cabled and port 2 unused. The monitor must distinguish between a genuinely failed port and an intentionally uncabled one — without requiring static configuration like `gpu_port_config`.

Additionally, compute and storage NICs may have different port counts (e.g., dual-port compute cards vs single-port storage cards). The homogeneity check must compare NICs within the same role group to avoid false positives.

#### 4.3.2 Detection Mechanism

NICs are grouped by **role** (Compute or Storage, from Section 4.2), then within each role group:

1. Group NICs by **physical card** (PCI `bus:device` address — e.g., `0000:47:00` groups `0000:47:00.0` and `0000:47:00.1`)
2. **Exempt cards with a management-classified sibling function**: if any function of a card was classified Management (e.g., it carries the default route), the whole card is frontend plumbing and is removed from peer comparison. Its active member is the excluded management NIC itself, so counting only the monitored remainder would misread an intentionally-uncabled sibling function as a failure. Exempt cards keep per-port runtime transition detection; an outstanding card-anomaly latch on a newly-exempt card is cleared with a card-healthy event so no downstream FATAL is orphaned. (VF siblings do **not** exempt a card — they are filtered by discovery before classification and carry no frontend signal.)
3. Count active (`ACTIVE` + `LinkUp`) ports per card
4. Calculate the **decisive mode** (most common active-port-count) within the role group. A group is **indecisive** — and skipped — when it has fewer than 2 cards (nothing to compare against), when the mode is 0 (every card down — no meaningful pattern), or when the top frequency is **tied** between two distinct counts (no majority; an intentionally-idle card, such as the Aux half of a Prime/Aux frontend pair, is numerically indistinguishable from a failed one)
5. Any card with fewer active ports than its role's decisive mode → **FATAL event** (card-level), and its unhealthy ports keep their fatal severity on the first poll

**Anomaly membership is the single source of "peer evidence" on the first poll**: a first-poll unhealthy port is fatal **only** when its card is in the anomaly set. Ports on cards at/above the mode, in singleton groups, or in indecisive groups are logged and **suppressed** — without peers there is no evidence the port is supposed to be up, and a false external event is worse than a local diagnostic log (consistent with the fail-safe direction in Section 4.1.2).

#### 4.3.3 Algorithm

```
For all monitored PF NICs:
  Classify each NIC as Compute or Storage (Section 4.2)
  Group by physical card (PCI bus:device)
  Assign each card's role from its NICs

  For each role group (Compute, Storage):
    If group has < 2 cards: skip (no peers — no evidence either way)
    Calculate mode_active = most common active-port-count in this group
    If mode_active == 0 or the top frequency is tied: skip (indecisive)
    For each card in this group:
      If card_active_count < mode_active:
        Mark card ANOMALOUS
        FATAL event: "Card <pci> (<role>) has <n> active ports, expected <mode>"

  For each unhealthy port on the first poll:
    If its card is ANOMALOUS: emit fatal event (per Section 3 severity rules)
    Else: suppress external event (no peer evidence the port should be up)
```

#### 4.3.4 Field Validation

**H100 OCI (compute dual-port + storage single-port):**
```
Compute group (8 cards): all dual-port, 2 active each → mode = 2
Storage group (2 cards): all single-port, 1 active each → mode = 1
→ No false positives (storage NICs NOT compared against compute mode)

If compute card drops to 1 active → 1 < mode 2 → FATAL
If storage card drops to 0 active → 0 < mode 1 → FATAL
```

**L40 (dual-port compute NICs, 1 port cabled per card):**
```
Compute group (2 cards): Card A (1 active, 1 down), Card B (1 active, 1 down) → mode = 1
→ Uncabled ports NOT flagged (consistent pattern)

If Card A drops to 0 active → 0 < mode 1 → FATAL
```

> **Probability analysis**: For the mode to be incorrect (masking real failures), more than half of the cards in a role group would need to be independently failed at startup. With a ~1% per-NIC failure rate, the probability of 4+ out of 8 NICs failing simultaneously is ~0.00003% — effectively impossible.
>
> **Singleton and tied groups (field-validated)**: On OCI `BM.GPU.H100.8`, the frontend ConnectX-6 Dx exposes a Prime port (`mlx5_2`/`eth0`, carries the default route and node IP) and an Aux port (`mlx5_11`/`eth1`, no VNIC attached, `phys_state: Disabled` by design — confirmed identical across sibling nodes and against OCI's published shape spec of one frontend NIC + 16 RDMA ports). Default-route exclusion classifies `mlx5_2` as Management, leaving `mlx5_11` a **singleton** Storage group; and even without that exclusion the pair would be a **1-active vs 0-active tie**. Both situations are indecisive: the monitor logs and suppresses the DOWN Aux port instead of publishing a `HealthEvent`. The trade-off is explicit: a genuinely dead lone NIC at first poll is logged but not externally reported — a runtime `ACTIVE → DOWN` transition on the same NIC remains fatal because the monitor then has direct evidence the port was in use.
>
> **Management-sibling exemption (field-validated)**: On OCI `BM.GPU.L40S.4`, the Prime/Aux pair are two functions of the **same card** (`0000:5a:00.0`/eth0 and `0000:5a:00.1`, uncabled), and the storage role group also contains the RoCE fabric NICs and a second frontend card — so after `mlx5_1`/eth0 is excluded as the default-route NIC, the Aux card is **not** left a singleton: it counts 0 active ports against a decisive peer mode of 1 and would be flagged as failed hardware on every node of the SKU. The card-level exemption in step 2 above covers this: a management sibling removes the whole card from peer comparison, so the Aux port takes the same log-and-suppress path as the H100 case regardless of what else shares its role group.

### 4.4 Design Decision: Why Speed Degradation Detection Was Removed

The previous design included a speed degradation check that compared the sysfs `rate` against an expected rate from `gpu_port_config`. This was removed for the following reasons:

1. **Required per-GPU-type static configuration** (`gpu_port_config`) that doesn't exist for non-DGX systems (L40, T4, cloud VMs, OEM servers)
2. **Cannot distinguish compute from storage NICs**: On H100 DGX, compute NICs run at 400 Gb/s (InfiniBand) while storage NICs may run at different speeds (Ethernet). Applying the same rate threshold to both causes false positives
3. **Counter checks already detect the underlying degradation**: When a cable degrades enough to cause speed fallback, the physical layer generates errors. The `symbol_error` and `link_error_recovery` counters (see [Link Counter Detection](./link-counter-detection.md)) detect this degradation before or during the retrain event
4. **Sysfs does not expose the expected/supported speed**: The `rate` file only shows the current negotiated speed, not the maximum supported speed of the NIC or cable

> **Note**: Speed degradation remains a real failure mode in GPU clusters. A 400G link dropping to 200G halves collective operation throughput. However, this is better addressed by counter-based degradation monitoring (Layer 2) which detects the physical signal degradation that causes the speed fallback, rather than by comparing the negotiated speed against a static configuration value.

---

## 5. Device Discovery and Parsing

### 5.1 Discovery Logic

The NIC Health Monitor discovers and parses InfiniBand/RoCE devices by iterating over sysfs:

1. Iterating over `/sys/class/infiniband`
2. Parsing `hca_type`, `fw_ver`, and `board_id`
3. Enumerating ports and reading `link_layer`, `state`, and `phys_state`
4. Identifying device type (PF vs VF) for proper alerting

### 5.2 Device Discovery Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                        NIC DEVICE DISCOVERY FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  /sys/class/infiniband/                                                          │
│  │                                                                               │
│  ├── mlx5_0/                     ◄── Physical Function (PF)                      │
│  │   ├── hca_type                    → "MT4123" (ConnectX-6)                     │
│  │   ├── fw_ver                      → "20.31.1014"                              │
│  │   ├── board_id                    → "MT_0000000010"                           │
│  │   ├── device/                                                                 │
│  │   │   ├── sriov_totalvfs          → "16" (PF indicator)                       │
│  │   │   └── uevent                  → PCI_SLOT_NAME=0000:3b:00.0               │
│  │   └── ports/                                                                  │
│  │       └── 1/                                                                  │
│  │           ├── state               → "4: ACTIVE"                               │
│  │           ├── phys_state          → "5: LinkUp"                               │
│  │           ├── link_layer          → "InfiniBand"                              │
│  │           └── counters/           → (see counter detection doc)               │
│  │                                                                               │
│  ├── mlx5_1/ ... mlx5_17/        ◄── More Physical Functions                     │
│  │                                                                               │
│  └── mlx5_18/ ... mlx5_33/       ◄── Virtual Functions (VFs)                     │
│      └── device/                                                                 │
│          └── physfn → ../0000:3b:00.0  (VF indicator - points to parent PF)     │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### 5.3 Vendor Detection

The monitor detects Mellanox devices using the following logic:
1. Check if device name matches `mlx5_\d+` (Mellanox).
2. Fallback: Check driver symlink in `/sys/class/infiniband/<dev>/device/driver` for `mlx5_core`.

| Vendor                 | Detection                              | State Monitoring  | Fatal Detection    |
|------------------------|----------------------------------------|-------------------|--------------------|
| **Mellanox (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes - state files | State + PCI checks |

---

## 6. State Change Handling

The NIC Health Monitor reports **health boundary events** — one event per port when the port transitions between healthy and unhealthy states. Intermediate unhealthy transitions are suppressed unless the new state escalates the condition from non-fatal to fatal. A runtime port `DOWN` transition and fatal counter signals are remediation triggers on first occurrence (first-poll `DOWN` severity is gated by peer evidence — see Section 3.4 and Section 4.3.2); repeated analyzer rules are reserved for non-fatal degradation/syslog recurrence.

### 6.1 Architecture

1. NIC Health Monitor reports health boundary crossings and non-fatal→fatal severity escalations
2. Events flow to MongoDB via Platform Connector
3. Health Events Analyzer applies correlation rules only for repeated non-fatal NIC signals

### 6.2 Port Down and Fatal Counter Handling

Port `DOWN` state changes, `link_downed` counter increments, and other fatal NIC counters emit fatal events directly from the NIC Health Monitor with `RecommendedAction_REPLACE_VM`. They do not require an analyzer-side repeated flap rule.

### 6.3 Repeated Non-Fatal Escalation

The Health Events Analyzer escalates only repeated non-fatal NIC signals:

- `RepeatedNICDegradation`: 3 non-fatal `InfiniBandDegradationCheck` or `EthernetDegradationCheck` events on the same `NIC` + `NICPort` within 1 hour.
- `RepeatedNICDriverError`: 3 selected non-fatal `SysLogsNICDriverError` events of the same pattern on the same node within 1 hour.

Both analyzer rules use `CONTACT_SUPPORT`. See [Syslog Detection & Correlation, Appendix B](./syslog-detection-correlation.md#appendix-b-health-events-analyzer-rules-for-nic-monitoring) for exact rule definitions and external source citations.

---

## 7. Device Disappearance Handling

### 7.1 Purpose

When the State Monitor detects a device has disappeared from `/sys/class/infiniband/`, this is treated as a **FATAL** condition requiring VM replacement.

### 7.2 Detection

Device disappearance is detected through three complementary mechanisms:

**Case 1: Runtime disappearance (monitor has in-memory or persisted state, same boot)**

The monitor tracks devices across polling cycles via an in-memory device set and persisted lifecycle state (see [Link Counter Detection, Section 6.6](./link-counter-detection.md#66-persistent-state-file)). A previously-seen device must be absent from **three consecutive complete enumerations** before the monitor emits a FATAL event with the exact device name. The first two misses retain the last-known port/device snapshots. If the device directory is still enumerated but its contents cannot be read, the observation is treated as unknown and retained indefinitely rather than misreported as disappearance. If the top-level sysfs tree is temporarily unavailable, the poll does not advance state.

This works both during normal operation and **after pod restart on the same boot** because the known state and partial miss count are persisted. Once the FATAL is emitted, a separate persisted disappearance latch survives restarts and reboots. A healthy port observed after the device re-enumerates clears that latch and emits **two** recoveries: the port-scoped healthy (clears any port-level conditions) and a **device-scoped healthy whose entity set mirrors the disappearance FATAL's** (NIC entity only) — consumers that require every entity of a healthy event to match a stored condition entry (platform-connector node conditions) could not clear the device entry from the port-scoped healthy alone.

Device lifecycle tracking is **scoped per link layer**: each state check tracks a device only while it exposes at least one port of that check's layer, so removing a pure-IB NIC never produces an Ethernet-check disappearance (and vice versa). If a latched device re-enumerates without any target-layer ports (for example, its ports were reflashed to the sibling layer while it was absent), the check emits a **device-level healthy event** and clears its latch — the per-port recovery path could never reach such a device, and the downstream FATAL must not be orphaned.

- Example: `mlx5_3` was in the persisted `KnownDevices`, but is absent from sysfs on startup → `EntityType: "NIC", EntityValue: "mlx5_3"`

**Case 2: Device missing after host reboot (boot ID changed — state cleared)**

On host reboot, observational state (`KnownDevices`, port snapshots, and partial miss counts) is cleared because the node may have had NICs replaced. Outstanding card/device FATAL latches are preserved so a healthy replacement or re-enumerated device can clear the downstream condition. New disappearance detection falls through to Case 3.

**Case 3: Device missing on startup (no persisted state — fresh node, post-reboot, or corrupt state file)**

On the **first complete poll cycle after startup with no persisted observational state**, the monitor uses the **card homogeneity check** (see Section 4.3) to detect anomalies without requiring prior state or static configuration. This covers fresh nodes, post-reboot startups, and corrupt state files. Homogeneity continues on later complete polls for card-latch recovery, but is skipped while discovery is partial or a disappearance miss is still being debounced:

1. Group all monitored PF NICs by physical card (PCI `bus:device`), within each role group (Section 4.2)
2. Count active (`ACTIVE/LinkUp`) ports per card
3. Calculate the **decisive mode** (most common active-port-count) per role group; groups with fewer than 2 cards, a zero mode, or a tied top frequency are indecisive and skipped (Section 4.3.2)
4. Any card with fewer active ports than its role group's decisive mode → **FATAL event**; unhealthy ports on all other cards are logged and suppressed (no external `HealthEvent`)

This startup homogeneity check requires no persisted state and works immediately as a fallback. It detects missing ports by comparing against **peer NICs on the same node** rather than against a static expected count.

- Example: 8 single-port NIC cards, 7 are ACTIVE, 1 is DOWN → mode is 1, the DOWN card has 0 active → FATAL
- Message: "Card 0000:XX:00 has 0 active ports, expected 1 (peer mode)"

> **Why the homogeneity assumption is safe**: Compute fabric NICs are all the same model on GPU cluster nodes (DGX, HGX, or OEM). This approach works for both InfiniBand and Ethernet (RoCE) NICs. Management NICs on separate NUMA nodes are excluded before this check runs (see Section 4.1). For the mode to be incorrect, more than half of the NICs would need to be independently failed at startup — a probability of ~0.00003% for an 8-NIC system. When there is no majority at all (a tie, e.g., one card up and its only peer down), the group is treated as indecisive and nothing is flagged — see the singleton/tied-group discussion in Section 4.3.4.

### 7.3 Event Classification

| Condition                                                  | Severity  | Recommended Action               |
|------------------------------------------------------------|-----------|----------------------------------|
| Device absent for 3 complete runtime polls                 | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Card active ports below peer mode (startup first poll)     | **FATAL** | **RecommendedAction_REPLACE_VM** |

> **Design Note**: Confirmed device disappearances are treated as FATAL because unexpected device loss indicates a hardware issue requiring investigation and VM replacement. The debounce distinguishes disappearance from transient enumeration gaps; it does not distinguish a clean driver removal from a hardware crash.

---

## 8. SR-IOV Virtual Function Handling

### 8.1 Background: Why VFs Being DOWN is Expected

**SR-IOV (Single Root I/O Virtualization)** is a technology that allows a single physical NIC to appear as multiple virtual NICs. Understanding this is critical for correct alerting behavior.

> **Note**: Clusters with the **NVIDIA Network Operator** installed will have SR-IOV enabled by default. This applies to both **VM-based** and **baremetal container** environments. In baremetal Kubernetes with SR-IOV, unassigned VFs will still appear as DOWN — the filtering logic applies equally to both deployment types.

**The Problem Without Understanding SR-IOV:**
```
Monitor starts → Sees 34 devices → 16 are DOWN → Generates 16 FATAL alerts!
But... those 16 devices are supposed to be DOWN. False alarm storm!
```

**Why VFs are DOWN by default:**
When SR-IOV is enabled, Virtual Functions are pre-created by the driver but remain in DOWN state until they are:
1. Assigned to a VM or container via passthrough/device allocation
2. Administratively enabled (for InfiniBand, also requires Subnet Manager configuration)

Unassigned VFs are essentially "empty slots" waiting for workloads. A DOWN VF is not a hardware failure—it's normal SR-IOV behavior.

### 8.2 Key Terminology

| Term   | Full Name         | Description                                                           |
|--------|-------------------|-----------------------------------------------------------------------|
| **PF** | Physical Function | The "real" NIC controlled by the host OS. It should ALWAYS be ACTIVE. |
| **VF** | Virtual Function  | A "virtual clone" of the PF. Created for VMs/containers to use.       |

### 8.3 VF Lifecycle

```
STAGE 1: System Boot (SR-IOV Enabled)
├── PF created: mlx5_0 → ACTIVE (host uses it)
├── VFs created: mlx5_18, mlx5_19, ... → DOWN (waiting for assignment)
└── This is NORMAL - VFs are pre-provisioned resources. Their ports remain DOWN 
    until assigned to a workload (VM or container) and configured by the Subnet 
    Manager. This is standard SR-IOV behavior, not a hardware failure.

STAGE 2: VM Starts
├── Orchestrator assigns VF to VM: mlx5_18 → VM1
├── mlx5_18 state changes: DOWN → ACTIVE
└── VM now has dedicated NIC hardware

STAGE 3: VM Shuts Down
├── VF released back to pool: mlx5_18
├── mlx5_18 state changes: ACTIVE → DOWN
└── Ready for next VM - back to "parking spot" state
```

### 8.4 Alerting Decision Matrix

| Device Type | State    | Should Alert? | Reason                                |
|-------------|----------|---------------|---------------------------------------|
| PF          | ACTIVE   | No            | Normal operation                      |
| PF          | **DOWN** | **YES!**      | Real problem - host lost connectivity |
| VF          | DOWN     | **No**        | Normal - VF not assigned to any VM    |
| VF          | ACTIVE   | No            | Normal - VF assigned and in use       |

### 8.5 Auto-Detection: PF vs VF

The Linux kernel provides clear indicators in sysfs:

| Indicator                    | PF (Physical Function)      | VF (Virtual Function)        |
|------------------------------|-----------------------------|------------------------------|
| `device/physfn` symlink      | Does NOT exist              | EXISTS (points to parent PF) |
| `device/sriov_totalvfs` file | EXISTS (shows max VF count) | Does NOT exist               |

```
# PF Example (mlx5_0):
/sys/class/infiniband/mlx5_0/device/
├── sriov_totalvfs    ← EXISTS (value: 16 = can create 16 VFs)
└── (no physfn)       ← Doesn't exist

# VF Example (mlx5_18):
/sys/class/infiniband/mlx5_18/device/
├── (no sriov_totalvfs)  ← Doesn't exist
└── physfn → ../0000:93:00.1  ← EXISTS (points to parent PF)
```

### 8.6 Real Example from Field Validation (34-NIC System)

```
┌────────────────────────────────────────────────────────────────────────┐
│  Device      State    Type   Alert if DOWN?   Reason                   │
├────────────────────────────────────────────────────────────────────────┤
│  mlx5_0      ACTIVE   PF     YES              Host management NIC      │
│  mlx5_1      ACTIVE   PF     YES              RDMA data path           │
│  ...                                                                    │
│  mlx5_17     ACTIVE   PF     YES              RDMA data path           │
│  ─────────────────────────────────────────────────────────────────────  │
│  mlx5_18     DOWN     VF     NO               Unassigned, waiting      │
│  mlx5_19     DOWN     VF     NO               Unassigned, waiting      │
│  ...                                                                    │
│  mlx5_33     DOWN     VF     NO               Unassigned, waiting      │
└────────────────────────────────────────────────────────────────────────┘
```

### 8.7 Implementation

To determine if a `DOWN` state is expected, the monitor detects if a device is an SR-IOV Virtual Function (VF) or Physical Function (PF).

- **Method 1 (Primary)**: Check for `physfn` symlink in the device directory. If present, it's a VF.
- **Method 2 (Secondary)**: Check for `sriov_totalvfs` file. If present, it's a PF.

VFs are expected to be `DOWN` when unassigned. PFs are expected to be `ACTIVE`.

---

## 9. RoCE State Monitoring

RoCE (RDMA over Converged Ethernet) devices appear in **both** `/sys/class/net` and `/sys/class/infiniband`. The monitor accesses RoCE devices via the InfiniBand interface (`/sys/class/infiniband/`). The following monitoring applies to RoCE:

- **State monitoring**: `state`, `phys_state` (via InfiniBand sysfs interface)
- **Device identification**: Check `link_layer` file for "Ethernet"

### 9.1 GID Table Information (RoCE Routing Diagnostics)

The GID (Global Identifier) table is critical for RoCE routing. Each device exposes GIDs at:
- `/sys/class/infiniband/<dev>/ports/<port>/gids/<index>`
- `/sys/class/infiniband/<dev>/ports/<port>/gid_attrs/types/<index>`

**GID Types** ([Linux Kernel sysfs ABI](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)):
- `IB/RoCE v1` = InfiniBand and RoCE v1 (GRH-based, layer 2)
- `RoCE v2` = RoCE v2 (UDP-encapsulated, layer 3, firewall-friendly)

At the API level (`ibv_gid_type`), there are three distinct types:
- `IBV_GID_TYPE_IB` (InfiniBand)
- `IBV_GID_TYPE_ROCE_V1` (RoCE v1)
- `IBV_GID_TYPE_ROCE_V2` (RoCE v2)

**Example GID table from 34-NIC system:**
```
DEV     PORT  INDEX  GID                                      IPv4           VER   DEV
mlx5_0  1     0      fe80:0000:0000:0000:ba3f:d2ff:fec3:65c4               v1    eth0
mlx5_0  1     1      fe80:0000:0000:0000:ba3f:d2ff:fec3:65c4               v2    eth0
mlx5_0  1     2      0000:0000:0000:0000:0000:ffff:0a33:ba20  10.51.186.32 v1    eth0
mlx5_0  1     3      0000:0000:0000:0000:0000:ffff:0a33:ba20  10.51.186.32 v2    eth0
mlx5_1  1     0      fe80:0000:0000:0000:ba3f:d2ff:fe7c:7570               v1    rdma4
mlx5_1  1     2      0000:0000:0000:0000:0000:ffff:ac10:0120  172.16.1.32  v1    rdma4
```

**Diagnostic value:**
- Empty GID table → `Error 61 (ENODATA)` during QP setup
- Missing IPv4 GIDs → routing issues for RoCE v2
- GID type mismatch between peers → connection failures

**Helper Functions:**
- `getGIDCount`: Enumerates `/sys/class/infiniband/<dev>/ports/<port>/gids/` to count valid GIDs.
- `getNetDevForIBDevice`: Discovers the network interface (e.g., `eth0`, `rdma4`) associated with an IB device by reading `/sys/class/infiniband/<dev>/device/net/`. This is critical for reading Ethernet statistics on RoCE devices.

---

## 10. Supported Hardware

> **Current Scope**: This initial implementation focuses on **Mellanox/NVIDIA InfiniBand and RoCE** devices only. The architecture is designed to be extensible for future support of additional NIC vendors.

| Vendor                 | Detection                              | State Monitoring  | Fatal Detection    |
|------------------------|----------------------------------------|-------------------|--------------------|
| **Mellanox (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes - state files | State + PCI checks |

### 10.1 Future Work

- **AWS EFA Support**: Device names matching `rdmap\d+s\d+`
- **Plain Ethernet**: `operstate = down` detection via `/sys/class/net/<interface>/operstate`
- **TCPXO Support**: TCP Express Offload support

---

## 11. Data Structures

### 11.1 State Monitoring Structures

```go
// IBPort is the per-poll snapshot the state check reads from sysfs.
// Counter fields (e.g., link_downed) live in the degradation check and
// are documented in link-counter-detection.md.
type IBPort struct {
    Device        string `json:"device,omitempty"`         // e.g., "mlx5_0"
    Port          uint   `json:"port,omitempty"`           // Port number
    State         string `json:"state,omitempty"`          // raw sysfs value, e.g., "4: ACTIVE", "1: DOWN"
    PhysicalState string `json:"physical_state,omitempty"` // raw sysfs value, e.g., "5: LinkUp", "3: Disabled"
    LinkLayer     string `json:"link_layer,omitempty"`     // "InfiniBand" or "Ethernet"
}

// Device is the discovered NIC record. HCAType / FWVer are purely
// informational today; they are surfaced to the event message when
// useful but do not drive any classification.
type Device struct {
    Name      string   `json:"name"`       // e.g., "mlx5_0"
    Vendor    string   `json:"vendor"`     // e.g., "mellanox"
    HCAType   string   `json:"hca_type"`   // e.g., "MT4123"
    FWVersion string   `json:"fw_ver"`
    Ports     []IBPort `json:"ports"`
    IsVF      bool     `json:"is_vf"`      // true for SR-IOV Virtual Functions; these are skipped
    NetDev    string   `json:"net_dev,omitempty"` // associated net device for RoCE
}
```

### 11.2 Entity Model

NICs and Ports are modeled as separate entity types to enable precise fault localization:

| Entity Type | Entity Value Format | Example  | Use Case                                       |
|-------------|---------------------|----------|------------------------------------------------|
| `NIC`       | `<device_name>`     | `mlx5_0` | Device-level failures (disappeared, PCI error) |
| `NICPort`   | `<port_number>`     | `1`      | Port-level failures (DOWN, uncabled anomaly)   |

**Rationale**: A single NIC (e.g., `mlx5_0`) can have multiple ports. Port-level events include **both** the NIC and Port entities in `EntitiesImpacted`, enabling:
- Precise fault localization (NIC + Port together identify the exact failing component)
- Precise cable replacement (which port's cable is faulty)
- Targeted firmware diagnostics
- Accurate capacity planning (one failed port vs entire NIC)

---

## 12. Configuration

### 12.1 State Monitoring Configuration

Configuration is split between a ConfigMap mounted at
`/etc/nic-health-monitor/config.toml` (rendered TOML, sourced from the
Helm `values.yaml` shown below) and command-line flags that govern
runtime paths and polling cadence. Both surfaces are documented below.

**Helm values (YAML)** — covers sysfs mount points and device filtering:

```yaml
# Comma-separated regex patterns for NICs to exclude from normal discovery.
# Names matching any pattern are dropped unless nicInclusionRegexOverride is set.
nicExclusionRegex: "^veth.*,^docker.*,^br-.*,^lo$"

# OPTIONAL. When non-empty, bypasses automatic NIC discovery and monitors
# only IB device names matching these comma-separated regex patterns.
# All automatic filters, including vendor, VF, management/NUMA, and
# NicExclusionRegex, are skipped — intended as an emergency override for
# operators who need to hand-pin a device list. Leave empty for the normal flow.
nicInclusionRegexOverride: ""

# sysfs mount points as seen inside the container. The monitor runs with
# the host's /sys bind-mounted under /nvsentinel/sys in the DaemonSet.
sysClassInfinibandPath: "/nvsentinel/sys/class/infiniband"
sysClassNetPath:        "/nvsentinel/sys/class/net"

# Counter detection settings live under `counterDetection:` — they are
# consumed by the counter PR and ignored by the state check.
counterDetection:
  enabled: true
  counters: [] # see link-counter-detection.md Section 10
```

**Command-line flags** — cover runtime wiring that changes per deployment:

| Flag                          | Default                                                                                       | Purpose                                                                                                                                                                                                            |
|-------------------------------|-----------------------------------------------------------------------------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `--checks`                    | `InfiniBandStateCheck,InfiniBandDegradationCheck,EthernetStateCheck,EthernetDegradationCheck` | Comma-separated list of enabled checks. Unknown names are logged and skipped.                                                                                                                                      |
| `--config`                    | `/etc/nic-health-monitor/config.toml`                                                         | Path to the TOML ConfigMap shown above.                                                                                                                                                                            |
| `--metadata-path`             | `/var/lib/nvsentinel/gpu_metadata.json`                                                       | Path to the GPU metadata file produced by the metadata collector (see Section 12.2).                                                                                                                               |
| `--state-polling-interval`    | `1s`                                                                                          | Polling interval for state checks. Counter checks run on a fixed 1-second cadence regardless of this setting.                                                                                                      |
| `--platform-connector-socket` | `unix:///var/run/nvsentinel.sock`                                                             | gRPC target for the platform connector that receives health events.                                                                                                                                                |
| `--metrics-port`              | `2112`                                                                                        | HTTP port that exposes `/metrics` (Prometheus) and `/healthz`.                                                                                                                                                     |
| `--state-file`                | `/var/run/nic_health_monitor/state.json`                                                      | Path to the persistent state file (hostPath-backed JSON). Seeds previous-poll port state across pod restarts and emits healthy baselines after host reboots. Missing or corrupt files are treated as a fresh boot. |
| `--boot-id-path`              | `/nvsentinel/proc/sys/kernel/random/boot_id`                                                  | Path to the kernel boot ID file. Detects host reboots; state is cleared and healthy baselines emitted when the ID changes.                                                                                         |
| `--processing-strategy`       | `EXECUTE_REMEDIATION`                                                                         | Event processing strategy (`EXECUTE_REMEDIATION` or `STORE_ONLY`).                                                                                                                                                 |
| `--node-name`                 | `${NODE_NAME}`                                                                                | Node name stamped on every event. Falls back to the `NODE_NAME` env var; startup fails if unset.                                                                                                                   |

**GPU metadata** is a hard startup dependency — see [Section 4](#4-management-nic-exclusion-and-uncabled-port-detection) for the fail-fast conditions and [Section 12.2](#122-metadata-collector-requirements) for the required fields.

**SR-IOV Virtual Function handling**

VFs are detected via the `device/physfn` sysfs symlink and skipped
unconditionally. There is no configuration knob — unassigned VFs are
expected to stay DOWN by design and monitoring them would produce false
positives.

> **Note**: The previous `gpu_port_config` and `MonitorNetworkType` configuration options have been removed. Management NIC exclusion is automatic via NUMA detection (Section 4.1). NIC role classification uses the topo matrix published by the metadata collector (Section 4.2). Uncabled port detection uses the card homogeneity check (Section 4.3). Both InfiniBand and Ethernet (RoCE) NICs are monitored equally — no link layer filtering is required.

### 12.2 Metadata Collector Requirements

The NIC Health Monitor is a **hard consumer** of topology data produced by the NVSentinel metadata collector. The collector must run on every node before (or alongside) the NIC Health Monitor DaemonSet and must populate the following fields in `/var/lib/nvsentinel/gpu_metadata.json`:

| Field                | Type                   | Source                                                    | Used by                                                  |
|----------------------|------------------------|-----------------------------------------------------------|----------------------------------------------------------|
| `gpus[].pci_address` | string                 | NVML                                                      | Card grouping (PCI bus:device)                           |
| `gpus[].numa_node`   | int                    | `nvidia-smi topo -m` NUMA Affinity column (-1 if unknown) | Section 4.1 Management exclusion (builds `gpu_numa_set`) |
| `nic_topology`       | map\<string,string[]\> | `nvidia-smi topo -m` relationship matrix                  | Section 4.2 topo-based classification                    |

**`nic_topology` format**: Keys are InfiniBand device names (e.g., `mlx5_0`, `ibp3s0`). Values are a slice of topology-level strings — one entry per GPU listed in `gpus[]`, in the same order. Each entry is one of `"X"`, `"PIX"`, `"PXB"`, `"PHB"`, `"NODE"`, `"SYS"`, or `"NV<n>"` (an NVLink bond count). The collector publishes this matrix verbatim; interpretation is the NIC Health Monitor's responsibility.

**Example `gpu_metadata.json` excerpt**:

```json
{
  "version": "1.0",
  "node_name": "gpu-node-42",
  "gpus": [
    {"gpu_id": 0, "pci_address": "0000:0f:00.0", "numa_node": 0, "uuid": "...", "serial_number": "..."},
    {"gpu_id": 1, "pci_address": "0000:15:00.0", "numa_node": 1, "uuid": "...", "serial_number": "..."}
  ],
  "nic_topology": {
    "mlx5_0": ["SYS", "SYS"],
    "mlx5_1": ["PIX", "SYS"],
    "mlx5_2": ["SYS", "PIX"],
    "mlx5_8": ["NODE", "NODE"]
  }
}
```

**Ordering guarantee**: The NIC Health Monitor DaemonSet pod manifest must declare a dependency (via init container, readiness gate, or pod startup ordering) such that the metadata collector completes its write before the NIC monitor starts. If this ordering is violated, the NIC monitor will fail at startup with a clear error pointing at the missing file.

---

## 13. Event Management

### 13.1 State Event Construction

Events are emitted on **health boundary crossings and fatal severity escalations** — one consolidated event per port per reportable transition. Logical state and physical state are combined into a single message.

**Example Event Fields (Fatal - IB Port DOWN):**

| Field             | Value                                                                                     |
|-------------------|-------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                      |
| CheckName         | `InfiniBandStateCheck`                                                                    |
| ComponentClass    | `NIC`                                                                                     |
| Message           | "Port mlx5_0 port 1: state DOWN, phys_state Disabled"                                     |
| IsFatal           | `true`                                                                                    |
| IsHealthy         | `false`                                                                                   |
| RecommendedAction | `REPLACE_VM`                                                                              |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}, {EntityType: "NICPort", EntityValue: "1"}]` |

**Example Event Fields (Fatal - RoCE Port DOWN):**

| Field             | Value                                                                                     |
|-------------------|-------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                      |
| CheckName         | `EthernetStateCheck`                                                                      |
| ComponentClass    | `NIC`                                                                                     |
| Message           | "RoCE port mlx5_0 port 1: state DOWN, phys_state Disabled, operstate down"                |
| IsFatal           | `true`                                                                                    |
| IsHealthy         | `false`                                                                                   |
| RecommendedAction | `REPLACE_VM`                                                                              |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}, {EntityType: "NICPort", EntityValue: "1"}]` |

**Example Event Fields (Healthy - Recovery):**

| Field             | Value                                                                                     |
|-------------------|-------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                      |
| CheckName         | `InfiniBandStateCheck`                                                                    |
| ComponentClass    | `NIC`                                                                                     |
| Message           | "Port mlx5_0 port 1: healthy (ACTIVE, LinkUp)"                                            |
| IsFatal           | `false`                                                                                   |
| IsHealthy         | `true`                                                                                    |
| RecommendedAction | `NONE`                                                                                    |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}, {EntityType: "NICPort", EntityValue: "1"}]` |

**Example Event Fields (Fatal - Device Disappeared):**

| Field             | Value                                                                   |
|-------------------|-------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                    |
| CheckName         | `InfiniBandStateCheck`                                                  |
| ComponentClass    | `NIC`                                                                   |
| Message           | "NIC mlx5_0 disappeared from /sys/class/infiniband/ - hardware failure" |
| IsFatal           | `true`                                                                  |
| IsHealthy         | `false`                                                                 |
| RecommendedAction | `REPLACE_VM`                                                            |
| EntitiesImpacted  | `[{EntityType: "NIC", EntityValue: "mlx5_0"}]`                          |

---

## Appendix A: Quick Reference - Fatal Condition Classification

The key question: **"Will the workload fail because of this?"**

### Fatal State Conditions (IsFatal = true)

| Condition                                              | Recommended Action               | Rationale                                                              |
|--------------------------------------------------------|----------------------------------|------------------------------------------------------------------------|
| **NIC state = DOWN** (runtime transition from healthy) | **RecommendedAction_REPLACE_VM** | Port was provably in use and lost connectivity; workloads will timeout |
| **Device disappeared**                                 | **RecommendedAction_REPLACE_VM** | Hardware failure, immediate job failure                                |
| **phys_state = Disabled** (runtime transition)         | **RecommendedAction_REPLACE_VM** | Port disabled while in use, no communication possible                  |
| **First-poll DOWN on an anomalous card**               | **RecommendedAction_REPLACE_VM** | Card below its role group's decisive mode — peer evidence of failure   |
| **Uncabled port anomaly**                              | **RecommendedAction_REPLACE_VM** | Card has fewer active ports than peers (homogeneity check)             |

### Non-Fatal State Conditions (IsFatal = false)

| Condition                                            | Recommended Action         | Rationale                                                                                                                                                                                     |
|------------------------------------------------------|----------------------------|------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| **phys_state = LinkErrorRecovery**                   | **RecommendedAction_NONE** | HCA firmware actively retrying; first-poll card-level homogeneity can still emit a separate fatal anomaly when the card is below its peers                                                    |
| **phys_state = Polling**                             | **RecommendedAction_NONE** | Transient link training; first-poll card-level homogeneity can still emit a separate fatal anomaly when the card is below its peers                                                           |
| **First-poll DOWN without peer evidence**            | N/A — no external event    | Never-seen-healthy port on a card in a singleton role group, an indecisive (tied/zero-mode) group, or at/above its role mode — indistinguishable from an uncabled or unprovisioned-by-design port (e.g., OCI H100 Aux frontend port) |

### Fatal Counters (IsFatal = true)

| Counter                           | Threshold           | Recommended Action               |
|-----------------------------------|---------------------|----------------------------------|
| `link_downed`                     | Delta > 0 (runtime) | **RecommendedAction_REPLACE_VM** |
| `excessive_buffer_overrun_errors` | > 0 (any)           | **RecommendedAction_REPLACE_VM** |
| `local_link_integrity_errors`     | > 0 (any)           | **RecommendedAction_REPLACE_VM** |
| `rnr_nak_retry_err`               | > 0 (any)           | **RecommendedAction_REPLACE_VM** |

### Driver/Firmware Logs

For kernel log pattern details (fatal and non-fatal classifications, regex patterns, log line examples, and kernel source references), see [Syslog Detection & Correlation](./syslog-detection-correlation.md). This document focuses on link state detection; syslog monitoring is covered in its own dedicated document to keep each document focused on a single problem.

### Repeated Non-Fatal Analyzer Escalation

Repeated non-fatal NIC degradation and selected non-fatal NIC driver syslog signals are escalated by the Health Events Analyzer with `CONTACT_SUPPORT`, not immediate `REPLACE_VM`. Use `RepeatedNICDegradation` for repeated degradation on the same `NIC` + `NICPort`, and `RepeatedNICDriverError` for repeated selected syslog patterns on the same node.

### State Detection Paths

| Condition                        | Recommended Action               | Path/Source                                                                                                     |
|----------------------------------|----------------------------------|-----------------------------------------------------------------------------------------------------------------|
| `state = DOWN` (runtime, or first-poll on an anomalous card) | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/state`; first-poll severity gated by peer evidence (Section 4.3.2) |
| `phys_state = Disabled`          | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/phys_state`                                                           |
| `phys_state = LinkErrorRecovery` | **RecommendedAction_NONE**       | `/sys/class/infiniband/<dev>/ports/<port>/phys_state` (non-fatal; first-poll homogeneity may emit a separate card-level fatal anomaly) |
| Uncabled port anomaly            | **RecommendedAction_REPLACE_VM** | Card homogeneity check (PCI card grouping + mode)                                                               |
| Device disappeared               | **RecommendedAction_REPLACE_VM** | Device enumeration in `/sys/class/infiniband/`                                                                  |

---

## References

1. [Linux Kernel sysfs-class-infiniband documentation](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
2. [DGX A100 User Guide](https://docs.nvidia.com/dgx/dgxa100-user-guide/introduction-to-dgxa100.html)
3. [DGX H100 User Guide](https://docs.nvidia.com/dgx/dgxh100-user-guide/introduction-to-dgxh100.html)
4. [DGX B200 User Guide](https://docs.nvidia.com/dgx/dgxb200-user-guide/introduction-to-dgxb200.html)
5. [GB200 NVL2](https://www.nvidia.com/en-us/data-center/gb200-nvl2/)

---
