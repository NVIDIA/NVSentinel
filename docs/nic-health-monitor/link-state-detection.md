# NIC Health Monitor: Link State Detection

---

## Table of Contents

1. [Overview](#1-overview)
2. [Architecture](#2-architecture)
3. [State Monitoring Specification](#3-state-monitoring-specification)
4. [Management NIC Exclusion, NIC Role Classification, and Uncabled Port Detection](#4-management-nic-exclusion-and-uncabled-port-detection)
5. [Device Discovery and Parsing](#5-device-discovery-and-parsing)
6. [State Change and Flap Detection](#6-state-change-and-flap-detection)
7. [PCI Configuration Space Health Check](#7-pci-configuration-space-health-check)
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
│  │  • Link Flap Detection: "link_downed 3+ times in 10 min"            │   │
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
| **Raw Event Reporting**     | Health boundary crossing → immediate event | One event per port per healthy↔fatal transition                           |
| **Centralized Correlation** | Health Events Analyzer MongoDB pipelines   | Flexible, configurable rules without monitor code changes                 |
| **Temporal Correlation**    | Analyzer rules with time windows           | Detects patterns like "3 link flaps in 10 minutes"                        |
| **Stabilization Windows**   | Analyzer rules with sticky XID-style logic | Prevents "Alert Blinking" where transient recoveries hide critical issues |

### 2.2 Component Responsibilities

| Component                            | Responsibility                                                                                                                              | What It Does NOT Do                                        |
|--------------------------------------|---------------------------------------------------------------------------------------------------------------------------------------------|------------------------------------------------------------|
| **NIC Health Monitor (State Check)** | Poll sysfs state files, detect UP/DOWN transitions, persist port state snapshots and known device list, emit raw events and recovery events | Aggregation, deduplication, correlation, pattern detection |
| **Health Events Analyzer**           | Correlate events, detect link flap patterns, escalate severity                                                                              | Direct hardware access                                     |

> **Local State Persistence**: The State Check persists port state snapshots (`state`, `phys_state` per port) and the known device list to the shared NIC health monitor state file (hostPath-backed, see [Link Counter Detection, Section 6.6](./link-counter-detection.md#66-persistent-state-file)). This enables the monitor to (1) emit **recovery events** (`IsHealthy=true`) after pod restart when a previously-DOWN port has been fixed, (2) detect **device disappearance** across pod restarts by comparing the current device list against the persisted known devices, and (3) on **host reboot** (boot ID change), clear all state and emit **healthy baseline events** for all currently-healthy ports to clear stale FATAL conditions on the platform — since the node may have had NICs replaced during maintenance (see [Link Counter Detection, Section 6.5](./link-counter-detection.md#65-boot-id-handling)).

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
└── Device not in sysfs → Hardware failure (FATAL)

Persists (to shared state file after each poll cycle):
├── Port state snapshots → state + phys_state per port (for recovery events)
└── Known device list    → Device names seen (for disappearance across restarts)

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
│  │                    (Link Flap Detection)                                 │  │
│  │                    ══════════════════════                                │  │
│  │                                                                          │  │
│  │  NIC STATE CORRELATION RULES:                                            │  │
│  │  • RepeatedNICLinkFlap: "link_downed 3+ times in 10 min → REPLACE_VM"    │  │
│  │  • NICStabilizationWindow: Prevent flapping (similar to sticky XID)      │  │
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

- **DOWN**: No connectivity (FATAL)
- **INIT**: Initializing — normal transient state during startup. Every port passes through INIT during boot and Subnet Manager configuration. For **InfiniBand ports**, classified as **Non-Fatal** (`IsFatal=false`) because INIT can persist while waiting for SM configuration. For **Ethernet/RoCE ports**, INIT is a brief sub-second transient during link training and is not reported (logged at DEBUG level only). If an IB port remains stuck in INIT, it won't satisfy the `ACTIVE/LinkUp` condition, causing the card's active port count to fall below its peers, which is caught as a Fatal condition by the card homogeneity check (see Section 4.2).
- **ARMED**: Waiting for Subnet Manager — same rationale as INIT. For **InfiniBand ports**, classified as **Non-Fatal** (`IsFatal=false`). For **Ethernet/RoCE ports**, this state is rare/transient and is not reported. Prolonged ARMED state on IB is caught by the card homogeneity check.
- **ACTIVE**: Normal operational state (HEALTHY)

**Physical State Substates**: `Sleep (1)`, `Polling (2)`, `Disabled (3)`, `Training (4)`, `LinkUp (5)`, `LinkErrorRecovery (6)`

- **Polling (2)**: Transient state during link training. Every port passes through Polling when establishing a connection. Classified as **Non-Fatal** (`IsFatal=false`). If a port remains in Polling, it won't count as active in the card homogeneity check, so the card's active port count will fall below the peer mode and be caught as a Fatal anomaly (see Section 4.2).
- **LinkErrorRecovery (6)**: Active error recovery in progress (FATAL)

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

4. **Emit event only on health boundary crossing:**
   - **First poll after host reboot (boot ID changed — state cleared)**:
     - All persisted state has been discarded (see [Link Counter Detection, Section 6.5](./link-counter-detection.md#65-boot-id-handling))
     - Healthy ports (`ACTIVE/LinkUp`): Emit **healthy event** (`IsHealthy=true`) — this clears any stale FATAL conditions on the platform from the previous boot (the node may have had NICs replaced, cables reseated, etc.)
     - Unhealthy ports on **anomalous cards**: Emit fatal event as usual
     - Unhealthy ports on **expected cards**: **Suppressed** (uncabled port, not a failure)
   - **First poll with no persisted state (fresh node, corrupt/missing state file)**:
     - Same behavior as the reboot case above
   - **First poll with persisted previous state (pod restart, same boot)**:
     - Compare current health against **persisted** previous state
     - Emit events on boundary crossings as with subsequent polls below (this is the key benefit of persistence — a port that was DOWN before restart and is now ACTIVE triggers a recovery event)
   - **Subsequent polls**: Only emit when `wasHealthy ≠ isHealthy`
     - Healthy → Unhealthy: **FATAL event** with consolidated message (e.g., "state DOWN, phys_state Disabled - no connectivity")
     - Unhealthy → Healthy: **HEALTHY event** (e.g., "healthy (ACTIVE, LinkUp)")
     - Unhealthy → Unhealthy (e.g., DOWN/Disabled → DOWN/Polling): **No event** — still unhealthy, intermediate transition suppressed
     - Healthy → Healthy: **No event** — still healthy

4. **One consolidated event per port per transition:**
   - Logical state and physical state are combined into a single message
   - For Ethernet/RoCE, the operstate is also included in the same event
   - `EntitiesImpacted` includes both NIC and Port entities
   - `RecommendedAction = REPLACE_VM` for fatal events

---

## 4. Management NIC Exclusion and Uncabled Port Detection

This section describes three zero-configuration mechanisms that replace the previous `gpu_port_config` / `AtLeastPorts` / `AtLeastRate` approach. These mechanisms require no per-GPU-type configuration and work automatically across DGX, HGX, OEM servers, and cloud VMs.

- **Section 4.1**: NUMA-based management NIC exclusion (exclude NICs on non-GPU NUMA nodes)
- **Section 4.2**: PCIe tree walk classification (distinguish compute NICs from storage NICs)
- **Section 4.3**: Role-based card homogeneity (detect uncabled ports and failures within each role group)

### 4.1 Management NIC Exclusion (NUMA-Based)

#### 4.1.1 The Problem

DGX systems (e.g., DGX A100) have Mellanox ConnectX management NICs that appear in `/sys/class/infiniband/` alongside compute fabric NICs. If monitored, a management NIC going DOWN would trigger `IsFatal=true` with `RecommendedAction_REPLACE_VM` — an incorrect remediation for a NIC that doesn't affect GPU workloads. The design doc's severity model (Fatal = "workload WILL fail") is specifically designed for compute and storage NICs, not management NICs.

#### 4.1.2 Detection Mechanism

Management NICs on DGX systems are placed on CPU sockets that have **no compute GPUs**. The monitor exploits this by checking whether each NIC's NUMA node has a compute GPU on it:

1. Read compute GPU PCI addresses from `/var/lib/nvsentinel/gpu_metadata.json` (provided by the metadata collector)
2. For each GPU PCI address, read `/sys/bus/pci/devices/<pci>/numa_node` → build `gpu_numa_set`
3. For each `mlx5_*` NIC, read `/sys/class/infiniband/<dev>/device/numa_node`
4. If `nic_numa ∉ gpu_numa_set` → **exclude** (management NIC on separate socket)

**Fallback**: If the metadata file is unavailable, the NUMA filter is skipped and all `mlx5_*` PF NICs are monitored. A warning is logged.

**Edge case**: If `numa_node = -1` (unknown, common in VMs), the NIC is **included** (safe direction — over-monitor rather than miss a failure).

#### 4.1.3 Field Validation

| Cluster                  | Management NICs                                                           | NUMA Check Result | Correct? |
|--------------------------|---------------------------------------------------------------------------|-------------------|----------|
| **A100 RoCE** (4-socket) | `mlx5_0` (NUMA 0), `mlx5_13` (NUMA 6) — no compute GPU on those NUMAs     | Excluded          | Yes      |
| **L40** (2-socket)       | None visible (BMC is non-Mellanox, invisible in `/sys/class/infiniband/`) | Nothing excluded  | Yes      |
| **H100 DGX** (2-socket)  | Storage/mgmt NICs share NUMA with GPUs — correctly kept for monitoring    | All monitored     | Yes      |

> **Design Note**: Storage NICs (e.g., H100 Slot1/Slot2 ConnectX-7 cards) share a NUMA node with compute GPUs. They are intentionally **not excluded** because storage NIC failures also impact workloads (I/O hangs, checkpoint failures). The NUMA check only excludes NICs on NUMA nodes with **zero** compute GPUs.

### 4.2 NIC Role Classification (PCIe Tree Walk)

#### 4.2.1 The Problem

DGX/HGX systems have both **compute fabric NICs** (OSFP ports on the GPU tray) and **storage NICs** (Slot1/Slot2 on the CPU motherboard). These are the same hardware (ConnectX-7) but serve different roles, may have different port counts, and run at different speeds. The card homogeneity check (Section 4.3) must compare NICs of the same role — compute against compute, storage against storage — to avoid false positives.

#### 4.2.2 Detection Mechanism: Shared PCIe Switch Ancestor

Compute NICs share a **PCIe switch** with their paired GPU (PXB/PIX in `nvidia-smi topo`). Storage NICs do not — they connect through the CPU root port only (NODE in `nvidia-smi topo`). This topological difference is detectable from sysfs by comparing the real device paths:

1. Resolve GPU real path: `/sys/bus/pci/devices/<gpu_pci>` → e.g., `/sys/devices/pci0000:07/.../0000:0f:00.0`
2. Resolve NIC real path: `/sys/class/infiniband/<dev>/device` → e.g., `/sys/devices/pci0000:07/.../0000:0c:00.0`
3. Find the **longest common path prefix** (number of matching `/`-delimited components)
4. If common depth > 4 → NIC shares a PCIe switch with a GPU → **Compute**
5. If common depth ≤ 4 → NIC only shares the PCI domain root → **Storage**

The threshold of 4 corresponds to the PCI host bridge level: `["", "sys", "devices", "pciXXXX:XX"]`. Anything deeper means the NIC and GPU share at least one PCIe switch or root port.

#### 4.2.3 Three-Tier Classification

Combined with the NUMA check (Section 4.1), the monitor classifies each NIC into one of three roles:

| Role           | Detection                                                          | Monitoring Behavior                                            |
|----------------|--------------------------------------------------------------------|----------------------------------------------------------------|
| **Management** | NIC NUMA has no compute GPU                                        | Excluded from monitoring entirely                              |
| **Compute**    | NIC shares PCIe switch ancestor with a GPU (common path depth > 4) | Monitored; compared against other compute NICs for homogeneity |
| **Storage**    | NIC on GPU NUMA but no shared PCIe switch (common path depth ≤ 4)  | Monitored; compared against other storage NICs for homogeneity |

#### 4.2.4 Field Validation (H100 OCI Node)

Verified against `nvidia-smi topo -m` output on an H100 OCI node with 8 GPUs, 18 PF NICs:

| NIC              | PCI Domain       | GPU in Same Domain? | Common Depth | Classification | nvidia-smi topo |
|------------------|------------------|---------------------|--------------|----------------|-----------------|
| mlx5_0, mlx5_1   | `pci0000:07`     | GPU0 (0000:0f:00.0) | 6            | **Compute**    | PXB to GPU0     |
| **mlx5_2**       | **`pci0000:1e`** | **None**            | **3**        | **Storage**    | NODE to GPUs    |
| mlx5_3, mlx5_4   | `pci0000:25`     | GPU1                | 6            | **Compute**    | PXB to GPU1     |
| mlx5_5, mlx5_6   | `pci0000:3c`     | GPU2                | 6            | **Compute**    | PXB to GPU2     |
| mlx5_7, mlx5_8   | `pci0000:53`     | GPU3                | 6            | **Compute**    | PXB to GPU3     |
| mlx5_9, mlx5_10  | `pci0000:81`     | GPU4                | 6            | **Compute**    | PXB to GPU4     |
| **mlx5_11**      | **`pci0000:99`** | **None**            | **3**        | **Storage**    | NODE to GPUs    |
| mlx5_12, mlx5_13 | `pci0000:a0`     | GPU5                | 6            | **Compute**    | PXB to GPU5     |
| mlx5_14, mlx5_15 | `pci0000:b8`     | GPU6                | 6            | **Compute**    | PXB to GPU6     |
| mlx5_16, mlx5_17 | `pci0000:d0`     | GPU7                | 6            | **Compute**    | PXB to GPU7     |

**18/18 PF NICs correctly classified. Perfect match with `nvidia-smi topo -m`.**

### 4.3 Uncabled Port Detection (Role-Based Card Homogeneity)

#### 4.3.1 The Problem

Some NIC cards have multiple ports, but not all ports are cabled. For example, dual-port ConnectX cards may have only port 1 cabled and port 2 unused. The monitor must distinguish between a genuinely failed port and an intentionally uncabled one — without requiring static configuration like `gpu_port_config`.

Additionally, compute and storage NICs may have different port counts (e.g., dual-port compute cards vs single-port storage cards). The homogeneity check must compare NICs within the same role group to avoid false positives.

#### 4.3.2 Detection Mechanism

NICs are grouped by **role** (Compute or Storage, from Section 4.2), then within each role group:

1. Group NICs by **physical card** (PCI `bus:device` address — e.g., `0000:47:00` groups `0000:47:00.0` and `0000:47:00.1`)
2. Count active (`ACTIVE` + `LinkUp`) ports per card
3. Calculate the **mode** (most common active-port-count) within the role group
4. Any card with fewer active ports than its role's mode → **FATAL event**

#### 4.3.3 Algorithm

```
For all monitored PF NICs:
  Classify each NIC as Compute or Storage (Section 4.2)
  Group by physical card (PCI bus:device)
  Assign each card's role from its NICs

  For each role group (Compute, Storage):
    Calculate mode_active = most common active-port-count in this group
    For each card in this group:
      If card_active_count < mode_active:
        FATAL event: "Card <pci> (<role>) has <n> active ports, expected <mode>"
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

## 6. State Change and Flap Detection

The NIC Health Monitor reports **health boundary events** — one event per port when the port transitions between healthy and unhealthy states. Intermediate transitions (e.g., DOWN/Disabled → DOWN/Polling) are suppressed. The **Health Events Analyzer** performs pattern detection to distinguish between persistent drops and transient flapping.

### 6.1 Architecture

1. NIC Health Monitor reports health boundary crossings (healthy→fatal, fatal→healthy)
2. Events flow to MongoDB via Platform Connector
3. Health Events Analyzer applies correlation rules to detect patterns

### 6.2 Port Drop Detection (Analyzer Rule: `NICPortDrop`)

An InfiniBand port is marked as "Dropped" when the Analyzer detects:
- The port has been reporting `state=DOWN` for at least 4 minutes
- No `link_downed` delta events during this period (indicating no recovery attempts)

### 6.3 Port Flap Detection (Analyzer Rule: `RepeatedNICLinkFlap`)

An InfiniBand port is marked as "Flapping" (**Severity: FATAL**) when the Analyzer detects:
- 3+ `link_downed` events within 10 minutes on the same NICPort entity
- This indicates repeated DOWN→ACTIVE transitions (unstable hardware)

### 6.4 Link Flap Detection Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                         LINK FLAP DETECTION FLOW                                 │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  TIME        NIC STATE           RAW EVENTS SENT                                 │
│  ────        ─────────           ───────────────                                 │
│                                                                                  │
│  T+0:00      ACTIVE              (baseline)                                      │
│  T+1:30      DOWN ────────────►  Event: link_downed, mlx5_0_port1               │
│  T+1:45      ACTIVE              (recovered)                                     │
│  T+4:20      DOWN ────────────►  Event: link_downed, mlx5_0_port1               │
│  T+4:35      ACTIVE              (recovered)                                     │
│  T+7:10      DOWN ────────────►  Event: link_downed, mlx5_0_port1               │
│              │                                                                   │
│              │   ┌─────────────────────────────────────────────────────────┐    │
│              └──►│        HEALTH EVENTS ANALYZER                           │    │
│                  │                                                          │    │
│                  │  Query: SELECT COUNT(*) FROM health_events              │    │
│                  │         WHERE agent = 'nic-health-monitor'              │    │
│                  │         AND message LIKE '%link_downed%'                │    │
│                  │         AND entity = 'mlx5_0_port1'                     │    │
│                  │         AND timestamp > NOW() - 10 minutes              │    │
│                  │                                                          │    │
│                  │  Result: 3 events                                        │    │
│                  │                                                          │    │
│                  │  Rule: RepeatedNICLinkFlap                               │    │
│                  │        IF count >= 3 THEN FATAL                         │    │
│                  │                                                          │    │
│                  │  ┌─────────────────────────────────────────────────┐    │    │
│                  │  │  OUTPUT: FATAL EVENT                            │    │    │
│                  │  │  Message: "NIC port flapping detected"          │    │    │
│                  │  │  RecommendedAction: REPLACE_VM                  │    │    │
│                  │  │  Entity: mlx5_0_port1                           │    │    │
│                  │  └─────────────────────────────────────────────────┘    │    │
│                  └─────────────────────────────────────────────────────────┘    │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

> **Effect**: The Analyzer emits a new fatal event with `RecommendedAction_REPLACE_VM`. The stabilization window logic (similar to sticky XID) can be implemented as an Analyzer rule to prevent rapid re-alerting.

---

## 7. Device Disappearance Handling

### 7.1 Purpose

When the State Monitor detects a device has disappeared from `/sys/class/infiniband/`, this is treated as a **FATAL** condition requiring VM replacement.

### 7.2 Detection

Device disappearance is detected through three complementary mechanisms:

**Case 1: Runtime disappearance (monitor has in-memory or persisted state, same boot)**

The monitor tracks devices across polling cycles via an in-memory device set and a persisted `KnownDevices` list (see [Link Counter Detection, Section 6.6](./link-counter-detection.md#66-persistent-state-file)). If a previously-seen device is no longer present in `/sys/class/infiniband/`, a FATAL event is generated immediately with the exact device name.

This works both during normal operation (in-memory state from prior poll) and **after pod restart on the same boot** (persisted `KnownDevices` loaded from the state file). Without persistence, a device that disappeared while the pod was restarting would go undetected — the new pod would have no knowledge the device ever existed.

- Example: `mlx5_3` was in the persisted `KnownDevices`, but is absent from sysfs on startup → `EntityType: "NIC", EntityValue: "mlx5_3"`

**Case 2: Device missing after host reboot (boot ID changed — state cleared)**

On host reboot, all persisted state (including `KnownDevices`) is cleared because the node may have had NICs replaced (see [Link Counter Detection, Section 6.5](./link-counter-detection.md#65-boot-id-handling)). The monitor cannot compare against prior devices because they may be entirely different hardware. Device disappearance detection after reboot falls through to Case 3 (homogeneity check).

**Case 3: Device missing on startup (no persisted state — fresh node, post-reboot, or corrupt state file)**

On the **first poll cycle after startup with no persisted state**, the monitor uses the **card homogeneity check** (see Section 4.2) to detect anomalies without requiring prior state or static configuration. This covers fresh nodes, post-reboot startups (where state was cleared), and corrupt state files. After the first poll, all runtime state changes (cable pulls, link failures, recoveries) are handled by the per-port boundary-crossing transition detection, making repeated homogeneity checks unnecessary:

1. Group all monitored PF NICs by physical card (PCI `bus:device`)
2. Count active (`ACTIVE/LinkUp`) ports per card
3. Calculate the mode (most common active-port-count) across all cards
4. Any card with fewer active ports than the mode → FATAL event

This startup homogeneity check requires no persisted state and works immediately as a fallback. It detects missing ports by comparing against **peer NICs on the same node** rather than against a static expected count.

- Example: 8 single-port NIC cards, 7 are ACTIVE, 1 is DOWN → mode is 1, the DOWN card has 0 active → FATAL
- Message: "Card 0000:XX:00 has 0 active ports, expected 1 (peer mode)"

> **Why the homogeneity assumption is safe**: Compute fabric NICs are all the same model on GPU cluster nodes (DGX, HGX, or OEM). This approach works for both InfiniBand and Ethernet (RoCE) NICs. Management NICs on separate NUMA nodes are excluded before this check runs (see Section 4.1). For the mode to be incorrect, more than half of the NICs would need to be independently failed at startup — a probability of ~0.00003% for an 8-NIC system.

### 7.3 Event Classification

| Condition                                                  | Severity  | Recommended Action               |
|------------------------------------------------------------|-----------|----------------------------------|
| Device disappeared from `/sys/class/infiniband/` (runtime) | **FATAL** | **RecommendedAction_REPLACE_VM** |
| Card active ports below peer mode (startup/runtime)        | **FATAL** | **RecommendedAction_REPLACE_VM** |

> **Design Note**: All device disappearances are treated as FATAL because in production environments, unexpected device loss indicates a hardware issue requiring investigation and VM replacement. The monitor does not differentiate between "clean" removals (driver unload) and "dirty" removals (hardware crash).

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
// Represents a single port's state
type IBPort struct {
    Device           string `json:"device,omitempty"`           // e.g., "mlx5_0"
    Port             uint   `json:"port,omitempty"`             // Port number
    State            string `json:"state,omitempty"`            // e.g., "Active", "Down"
    PhysicalState    string `json:"physical_state,omitempty"`   // e.g., "LinkUp", "Disabled"
    LinkLayer        string `json:"link_layer,omitempty"`       // e.g., "Infiniband", "Ethernet"
    TotalLinkDowned  uint64 `json:"total_link_downed"`         // From sysfs link_downed counter
}

// Represents a NIC device
type Device struct {
    Name     string    `json:"name"`      // e.g., "mlx5_0"
    HcaType  string    `json:"hca_type"`  // e.g., "MT4123"
    FWVer    string    `json:"fw_ver"`
    Ports    []IBPort  `json:"ports"`
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

```ini
#------------------------------------------------------------------------------
# General Settings
#------------------------------------------------------------------------------
[General]
# Polling interval for state monitoring
PollingIntervalInMilliseconds = 1000

# Maximum time to retry before confirming NIC is down
MaxRetryDurationForDownDetectedNICInMilliseconds = 500
RetryIntervalForDownDetectedNICInMilliseconds = 100

# Regex patterns for NICs to exclude (comma-separated)
NicExclusionRegex = ^veth.*,^docker.*,^br-.*,^lo$

# Regex patterns for RoCE interface filtering
RoCEInterfaceRegex = ^rdma\d+$,^eth\d+$,^nic\d+\.\d+$

# sysfs paths (for containerized deployments with host mounts)
SysClassNetPath = /sys/class/net
SysClassInfinibandPath = /sys/class/infiniband

#------------------------------------------------------------------------------
# Management NIC Auto-Exclusion (NUMA-Based)
#------------------------------------------------------------------------------
# GPU metadata path for NUMA-based management NIC exclusion.
# The monitor reads compute GPU PCI addresses from this file to determine
# which NUMA nodes have GPUs. NICs on NUMA nodes with no compute GPU are
# automatically excluded. If unavailable, all mlx5_* PFs are monitored.
GpuMetadataPath = /var/lib/nvsentinel/gpu_metadata.json

#------------------------------------------------------------------------------
# SR-IOV Virtual Function Auto-Detection
#------------------------------------------------------------------------------
# VFs are expected to be DOWN when not assigned to a VM/container.
AutoDetectSRIOVVFs = true

# Manual override (OPTIONAL - only for non-SRIOV edge cases)
# ExpectedDownDevices = mlx5_disabled_port
# ExpectedDownDevicesRegex = ^mlx5_maintenance_.*$
```

> **Note**: The previous `gpu_port_config` and `MonitorNetworkType` configuration options have been removed. Management NIC exclusion is now automatic via NUMA detection (see Section 4.1). Uncabled port detection uses the card homogeneity check (see Section 4.2). Both InfiniBand and Ethernet (RoCE) NICs are monitored equally — no link layer filtering is required.

---

## 13. Event Management

### 13.1 State Event Construction

Events are emitted only on **health boundary crossings** — one consolidated event per port per transition. Logical state and physical state are combined into a single message.

**Example Event Fields (Fatal - IB Port DOWN):**

| Field             | Value                                                                                     |
|-------------------|-------------------------------------------------------------------------------------------|
| Agent             | `nic-health-monitor`                                                                      |
| CheckName         | `InfiniBandStateCheck`                                                                    |
| ComponentClass    | `NIC`                                                                                     |
| Message           | "Port mlx5_0 port 1: state DOWN, phys_state Disabled - no connectivity"                   |
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
| Message           | "RoCE port mlx5_0 port 1: state DOWN, operstate down - no connectivity"                   |
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

| Condition                          | Recommended Action               | Rationale                                                  |
|------------------------------------|----------------------------------|------------------------------------------------------------|
| **NIC state = DOWN**               | **RecommendedAction_REPLACE_VM** | No network connectivity, workloads will timeout            |
| **Device disappeared**             | **RecommendedAction_REPLACE_VM** | Hardware failure, immediate job failure                    |
| **phys_state = Disabled**          | **RecommendedAction_REPLACE_VM** | Port disabled, no communication possible                   |
| **phys_state = LinkErrorRecovery** | **RecommendedAction_REPLACE_VM** | Active link problems                                       |
| **Uncabled port anomaly**          | **RecommendedAction_REPLACE_VM** | Card has fewer active ports than peers (homogeneity check) |
| **Port flapping (3+ cycles)**      | **RecommendedAction_REPLACE_VM** | Intermittent hardware/cable instability                    |

### Fatal Counters (IsFatal = true)

| Counter                           | Threshold           | Recommended Action               |
|-----------------------------------|---------------------|----------------------------------|
| `link_downed`                     | Delta > 0 (runtime) | **RecommendedAction_REPLACE_VM** |
| `excessive_buffer_overrun_errors` | > 0 (any)           | **RecommendedAction_REPLACE_VM** |
| `local_link_integrity_errors`     | > 0 (any)           | **RecommendedAction_REPLACE_VM** |
| `rnr_nak_retry_err`               | > 0 (any)           | **RecommendedAction_REPLACE_VM** |

### Driver/Firmware Logs

For kernel log pattern details (fatal and non-fatal classifications, regex patterns, log line examples, and kernel source references), see [Syslog Detection & Correlation](./syslog-detection-correlation.md). This document focuses on link state detection; syslog monitoring is covered in its own dedicated document to keep each document focused on a single problem.

### State Detection Paths

| Condition                        | Recommended Action               | Path/Source                                           |
|----------------------------------|----------------------------------|-------------------------------------------------------|
| `state = DOWN`                   | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/state`      |
| `phys_state = Disabled`          | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/phys_state` |
| `phys_state = LinkErrorRecovery` | **RecommendedAction_REPLACE_VM** | `/sys/class/infiniband/<dev>/ports/<port>/phys_state` |
| Uncabled port anomaly            | **RecommendedAction_REPLACE_VM** | Card homogeneity check (PCI card grouping + mode)     |
| Device disappeared               | **RecommendedAction_REPLACE_VM** | Device enumeration in `/sys/class/infiniband/`        |

---

## References

1. [Linux Kernel sysfs-class-infiniband documentation](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
2. [DGX A100 User Guide](https://docs.nvidia.com/dgx/dgxa100-user-guide/introduction-to-dgxa100.html)
3. [DGX H100 User Guide](https://docs.nvidia.com/dgx/dgxh100-user-guide/introduction-to-dgxh100.html)
4. [DGX B200 User Guide](https://docs.nvidia.com/dgx/dgxb200-user-guide/introduction-to-dgxb200.html)
5. [GB200 NVL2](https://www.nvidia.com/en-us/data-center/gb200-nvl2/)

---
