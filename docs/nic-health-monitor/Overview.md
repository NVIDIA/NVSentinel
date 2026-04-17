# NIC Health Monitor Design

---

## Overview

The NIC Health Monitor is a comprehensive monitoring solution for detecting and reporting network interface failures in GPU clusters. It addresses the challenge of **Grey Failures**—subtle degradations where a single degraded link can throttle thousands of GPUs in distributed training workloads.

This documentation is organized into three focused areas:

| Document                                                                | Focus                                       | Key Capabilities                                                                                    |
|-------------------------------------------------------------------------|---------------------------------------------|-----------------------------------------------------------------------------------------------------|
| [**Link State Detection**](./link-state-detection.md)                   | UP/DOWN monitoring, device disappearance    | Binary state changes, NIC role classification (compute/storage/management), uncabled port anomalies |
| [**Link Counter Detection**](./link-counter-detection.md)               | Error rate monitoring, threshold violations | BER tracking, FEC exhaustion prediction, congestion detection                                       |
| [**Syslog Detection & Correlation**](./syslog-detection-correlation.md) | Kernel log monitoring, repeat failures      | Driver/firmware errors, correlated failure patterns                                                 |

---

## Architecture Overview

```text
┌────────────────────────────────────────────────────────────────────────────────┐
│                    NVSentinel NIC MONITORING ARCHITECTURE                      │
├────────────────────────────────────────────────────────────────────────────────┤
│                                                                                │
│  ┌──────────────────────────────────────────────────────────────────────────┐  │
│  │                 PER-NODE DAEMONSETS (Raw Event Reporters)                │  │
│  ├──────────────────────────────────────────────────────────────────────────┤  │
│  │                                                                          │  │
│  │  ┌─────────────────────────────┐  ┌─────────────────────────────────┐   │  │
│  │  │    NIC HEALTH MONITOR       │  │    SYSLOG HEALTH MONITOR        │   │  │
│  │  │    ══════════════════       │  │    ════════════════════         │   │  │
│  │  │                             │  │                                 │   │  │
│  │  │  DATA SOURCES:              │  │  DATA SOURCE:                   │   │  │
│  │  │  • /sys/class/infiniband/   │  │  • journald / /var/log/journal  │   │  │
│  │  │  • /sys/class/net/          │  │                                 │   │  │
│  │  │                             │  │  CHECK:                         │   │  │
│  │  │  CHECKS:                    │  │  • SysLogsNICDriverError        │   │  │
│  │  │  • InfiniBandStateCheck     │  │    (mlx5_core cmd_exec timeout, │   │  │
│  │  │  • InfiniBandDegradationChk │  │     health poll failed,         │   │  │
│  │  │  • EthernetStateCheck       │  │     unrecoverable, PCIe fatal)  │   │  │
│  │  │  • EthernetDegradationCheck │  │                                 │   │  │
│  │  │                             │  │  BEHAVIOR:                      │   │  │
│  │  │  BEHAVIOR:                  │  │  • Reports RAW events as-is     │   │  │
│  │  │  • Reports RAW events as-is │  │  • Persistent local state       │   │  │
│  │  │  • Persistent local state   │  │    (journal cursors, boot ID)   │   │  │
│  │  │    (port states, counters,  │  │  • Correlation centralized      │   │  │
│  │  │    boot ID, breach flags,   │  │                                 │   │  │
│  │  │    known devices)           │  │                                 │   │  │
│  │  │  • Correlation centralized  │  │                                 │   │  │
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
│  │             CENTRALIZED DEPLOYMENT (Correlation & Intelligence)          │ │
│  ├──────────────────────────────────────────────────────────────────────────┤ │
│  │                                                                          │ │
│  │  ┌────────────────────────────────────────────────────────────────────┐  │ │
│  │  │                    HEALTH EVENTS ANALYZER                          │  │ │
│  │  │                    ══════════════════════                          │  │ │
│  │  │                                                                    │  │ │
│  │  │  OPTIONAL CORRELATION (MongoDB Aggregation Pipelines):             │  │ │
│  │  │                                                                    │  │ │
│  │  │  • Correlate kernel log warnings with port state events            │  │ │
│  │  │  • Provide diagnostic context for operator investigation           │  │ │
│  │  │                                                                    │  │ │
│  │  │  NOTE: Deterministic kernel logs (cmd_exec timeout, etc.) = Fatal  │  │ │
│  │  │        Diagnostic logs = Non-Fatal                                  │  │ │
│  │  │                                                                    │  │ │
│  │  │  OUTPUT:                                                           │  │ │
│  │  │  • Deterministic failure detection and remediation triggering      │  │ │
│  │  │  • Diagnostic correlation for operator investigation               │  │ │
│  │  └────────────────────────────────────────────────────────────────────┘  │ │
│  │                                                                          │ │
│  └──────────────────────────────────────────────────────────────────────────┘ │
│                                                                                │
└────────────────────────────────────────────────────────────────────────────────┘
```

---

## Design Principles

### "Report Raw, Correlate Centrally" Pattern

The NIC Health Monitor follows NVSentinel's established architectural pattern:

| Component                  | Function                                                                                                                                         | Data Source                   | Event Flow                                 |
|----------------------------|--------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------|--------------------------------------------|
| **NIC Health Monitor**     | Detect UP/DOWN transitions, counter threshold violations, emit recovery events on admin counter resets, emit healthy baselines after host reboot | `sysfs` state files, counters | Raw + recovery events → Platform Connector |
| **Syslog Health Monitor**  | Detect driver/firmware events from kernel logs                                                                                                   | `journald`/`dmesg`            | Raw events → Platform Connector            |
| **Health Events Analyzer** | Correlate events, detect patterns, escalate to fatal                                                                                             | MongoDB                       | Correlated events → Platform Connector     |

> **Design Principle**: Health monitors report **raw events as-is**. All aggregation, correlation, link flap detection, and stabilization window logic is handled centrally by the **Health Events Analyzer** using configurable MongoDB aggregation rules. Each monitor maintains **minimal persistent local state** on the node (via hostPath-backed state files) to survive pod restarts — port state snapshots, counter snapshots, breach flags, known device lists, and boot ID for the NIC monitor; journal cursors and boot ID for the syslog monitor. This local state is strictly for delta/velocity calculation, health boundary transition detection, recovery event emission, and resumption; it is not used for correlation or pattern detection.

### Binary Severity Model

This monitor uses a binary severity model based on **workload impact**:

| Severity      | Meaning                                  | Example                                                    |
|---------------|------------------------------------------|------------------------------------------------------------|
| **Fatal**     | Workload WILL fail or HAS failed         | NIC DOWN, `cmd_exec timeout`, unrecoverable hardware error |
| **Non-Fatal** | Degradation detected, workload continues | Symbol errors, congestion, insufficient power              |

**Key Design Principle**: The only question that matters is **"Will the running workload fail because of this?"**

---

## Detection Methods Summary

### Three-Layer Detection Approach

```text
┌────────────────────────────────────────────────────────────────────────────┐
│                        THREE-LAYER NIC MONITORING                          │
├────────────────────────────────────────────────────────────────────────────┤
│                                                                            │
│  LAYER 1: LINK STATE DETECTION                                             │
│  ═══════════════════════════════                                           │
│  • Polling interval: 1 second                                              │
│  • Data source: /sys/class/infiniband/<dev>/ports/<port>/state, phys_state │
│  • Detects: Hard DOWN, device disappearance, uncabled port anomalies       │
│  • Documentation: link-state-detection.md                                  │
│                                                                            │
│  ───────────────────────────────────────────────────────────────────────── │
│                                                                            │
│  LAYER 2: LINK COUNTER DETECTION                                           │
│  ═══════════════════════════════                                           │
│  • Polling interval: 5 seconds                                             │
│  • Data source: /sys/class/infiniband/<dev>/ports/<port>/counters/         │
│  • Detects: Symbol errors, link flaps, buffer overruns, transport errors   │
│  • Documentation: link-counter-detection.md                                │
│                                                                            │
│  ───────────────────────────────────────────────────────────────────────── │
│                                                                            │
│  LAYER 3: SYSLOG DETECTION                                                 │
│  ═══════════════════════════                                               │
│  • Trigger: Event-driven (journald watch)                                  │
│  • Data source: Kernel ring buffer (dmesg), journald                       │
│  • Detects: cmd_exec timeout, health poll failed, PCIe errors              │
│  • Documentation: syslog-detection-correlation.md                          │
│                                                                            │
└────────────────────────────────────────────────────────────────────────────┘
```

### Coverage Map

```text
┌─────────────────────────────────────────────────────────────────┐
│                     Coverage Map                                 │
├─────────────────────────────────────────────────────────────────┤
│                                                                  │
│  PACKET PATH (Data Moving Through NIC):                          │
│  ├── Link State Detection: Binary UP/DOWN                        │
│  └── Link Counter Detection: Error rates, degradation            │
│                                                                  │
│  DRIVER/FIRMWARE INTERFACE (OS <--> NIC Communication):          │
│  └── Syslog Detection: mlx5_core errors, PCIe AER events        │
│                                                                  │
│  Driver/firmware failures can occur while packet path appears    │
│  healthy. All three layers are needed for complete coverage.     │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## Key Capabilities

1. **Deterministic failure thresholds** from IBTA specifications, cloud provider heuristics (Azure, AWS), and vendor documentation
2. **Fully configurable counters and thresholds** - operators can define which counters to monitor, set custom thresholds (delta or velocity-based), and configure fatal/non-fatal severity per counter
3. **Rate-based degradation detection** via centralized Health Events Analyzer rules
4. **Pre-failure prediction** by detecting BER climbing before FEC exhaustion (IBTA 10E-12 BER threshold: 120 errors/hour)
5. **Kernel log monitoring** integrated into the existing syslog-health-monitor with NIC-specific check patterns
6. **Centralized event correlation** via Health Events Analyzer MongoDB aggregation pipelines
7. **Link flap detection** via Health Events Analyzer rules (e.g., "link_downed 3+ times in 10 minutes")
8. **Persistent local state** shared across state and counter checks — port state snapshots and known device list (state recovery and device disappearance detection across restarts), counter snapshots with per-counter timestamps (precise velocity calculation), breach flags (recovery event emission after admin counter resets), and boot ID (clear all state and emit healthy baselines on host reboot, since NICs may have been replaced)

---

## Event Sources Summary

### State Detection (Fatal)

| Source            | Fatal Conditions                                                               | Recommended Action               |
|-------------------|--------------------------------------------------------------------------------|----------------------------------|
| **State Monitor** | `state=DOWN`, `phys_state=Disabled`, device disappeared, uncabled port anomaly | **RecommendedAction_REPLACE_VM** |

### Counter Detection (Fatal - Defaults)

| Source                  | Default Fatal Conditions                                                                                                           | Recommended Action               |
|-------------------------|------------------------------------------------------------------------------------------------------------------------------------|----------------------------------|
| **Degradation Monitor** | `link_downed` (Delta > 0), `excessive_buffer_overrun_errors` (any), `local_link_integrity_errors` (any), `rnr_nak_retry_err` (any) | **RecommendedAction_REPLACE_VM** |

> **Note**: All counter thresholds and severity levels are configurable. See [Link Counter Detection](./link-counter-detection.md#10-configuration) for customization options.

### Syslog Detection (Fatal & Non-Fatal)

| Source          | Conditions                                                                         | Severity      | Purpose                               |
|-----------------|------------------------------------------------------------------------------------|---------------|---------------------------------------|
| **Log Watcher** | `mlx5_core.*cmd_exec timeout`, `health poll failed`, `unrecoverable`, `PCIe Fatal` | **Fatal**     | Deterministic hardware/driver failure |
| **Log Watcher** | `insufficient power`, `module absent`, `ACCESS_REG failed`                         | **Non-Fatal** | Diagnostic context for correlation    |

> **Design Note**: Deterministically fatal events in logs trigger `REPLACE_VM` (emitted as `IsFatal=true`). Diagnostic logs are published as non-fatal events (`IsFatal=false`) for correlation and do not directly trigger automated remediation.

### Non-Fatal Event Sources (Degradation Monitoring)

| Source                  | Non-Fatal Conditions                                                                        | Action                 |
|-------------------------|---------------------------------------------------------------------------------------------|------------------------|
| **Degradation Monitor** | `symbol_error` (elevated rate), `link_error_recovery` (>5/min), `port_rcv_errors` (>10/sec) | Monitor for escalation |
| **Degradation Monitor** | `carrier_changes` (>2/interval), `rx_missed_errors` (host bottleneck)                       | Monitor for escalation |
| **Degradation Monitor** | `roce_slow_restart` (>10/sec), `local_ack_timeout_err` (>1/sec)                             | Monitor for escalation |

### Recovery and Healthy Baseline Events (IsHealthy = true)

The NIC Health Monitor emits healthy events (`IsHealthy=true`) in two scenarios to clear stale unhealthy conditions on the platform:

| Trigger                 | Source              | Behavior                                                                                                                                                                                                                                                                                                      | Purpose                                                                                         |
|-------------------------|---------------------|---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|-------------------------------------------------------------------------------------------------|
| **Admin counter reset** | Degradation Monitor | When a previously-breached counter returns below threshold (typically after CSP clears counters via `perfquery -x`), emit recovery event per counter                                                                                                                                                          | Clears stale counter breach conditions                                                          |
| **Port recovery**       | State Monitor       | When a previously-DOWN port transitions to `ACTIVE/LinkUp`, emit recovery event per port                                                                                                                                                                                                                      | Clears stale port FATAL conditions                                                              |
| **Host reboot**         | Both                | On boot ID change, clear all persisted state and emit baseline events: healthy events for all `ACTIVE/LinkUp` ports and all counters (at 0 after reboot); fatal/non-fatal events for any ports that are unhealthy post-reboot. Counter unhealthy events follow on the second poll if errors are accumulating. | Clears all stale conditions from previous boot — NICs may have been replaced during maintenance |

> **Design Note**: Without recovery events, a node that was marked FATAL on the platform would remain stuck in that state indefinitely after the issue is resolved. Persistent state (see [Link Counter Detection, Section 6.6](./link-counter-detection.md#66-persistent-state-file)) ensures recovery events survive pod restarts. On host reboot, all state is cleared and baseline events are emitted for every port and counter — healthy for those currently healthy, unhealthy for those currently unhealthy — because the node may have entirely new hardware and the platform needs a complete picture of the current state.

---

## Supported Hardware

> **Current Scope**: This initial implementation focuses on **Mellanox/NVIDIA InfiniBand and RoCE** devices only. The architecture is designed to be extensible for future support of additional NIC vendors.

| Vendor                 | Detection                              | Counter Monitoring   | Fatal Detection                |
|------------------------|----------------------------------------|----------------------|--------------------------------|
| **Mellanox (IB/RoCE)** | Device name `mlx5_*` or driver symlink | Yes - fatal counters | Counters + State + Kernel Logs |

### Future Work

- **AWS EFA Support**: Device names matching `rdmap\d+s\d+`
- **Plain Ethernet**: `operstate = down` detection via `/sys/class/net/<interface>/operstate`
- **TCPXO Support**: TCP Express Offload support

---

## Quick Navigation

| Topic                             | Document                                                            | Section     |
|-----------------------------------|---------------------------------------------------------------------|-------------|
| UP/DOWN state monitoring          | [Link State Detection](./link-state-detection.md)                   | Section 3   |
| Device disappearance / PCI checks | [Link State Detection](./link-state-detection.md)                   | Section 7   |
| Management NIC exclusion (NUMA)   | [Link State Detection](./link-state-detection.md)                   | Section 4.1 |
| NIC role classification (PCIe)    | [Link State Detection](./link-state-detection.md)                   | Section 4.2 |
| Uncabled port detection           | [Link State Detection](./link-state-detection.md)                   | Section 4.3 |
| SR-IOV VF handling                | [Link State Detection](./link-state-detection.md)                   | Section 8   |
| BER/FEC theory                    | [Link Counter Detection](./link-counter-detection.md)               | Section 2   |
| Counter thresholds                | [Link Counter Detection](./link-counter-detection.md)               | Section 4   |
| Counter reset handling            | [Link Counter Detection](./link-counter-detection.md)               | Section 6   |
| Admin reset recovery events       | [Link Counter Detection](./link-counter-detection.md)               | Section 6.4 |
| Persistent state file             | [Link Counter Detection](./link-counter-detection.md)               | Section 6.6 |
| Boot ID handling                  | [Link Counter Detection](./link-counter-detection.md)               | Section 6.5 |
| Driver error patterns             | [Syslog Detection & Correlation](./syslog-detection-correlation.md) | Section 5   |
| Repeat failure detection          | [Syslog Detection & Correlation](./syslog-detection-correlation.md) | Section 7   |
| Health Events Analyzer rules      | [Syslog Detection & Correlation](./syslog-detection-correlation.md) | Appendix B  |

---

## References

### PHY & Signal Integrity
1. [PAM4 Error Correction Challenges in 400GbE (EDN)](https://www.edn.com/pam4-error-correction-bring-400gbe-test-challenges/)
2. [Determine Which Links Are Experiencing Significant Errors - Sun/Oracle (citing IBTA BER Threshold)](https://docs.oracle.com/cd/E19654-01/820-7751-12/z40004881932077.html)

### Linux Kernel & Driver
3. [sysfs-class-infiniband (Linux Kernel)](https://www.kernel.org/doc/Documentation/ABI/stable/sysfs-class-infiniband)
4. [RHEL8 mlx5_core Stack Overflow (Red Hat)](https://access.redhat.com/solutions/6955682)

### Fabric Diagnostics
5. [ibdiagnet User Manual (NVIDIA)](https://docs.nvidia.com/networking/display/ibdiagnet-infiniband-fabric-diagnostic-tool-user-manual-v2-21.21.pdf)
6. [Black Hole Detection (sFlow)](https://blog.sflow.com/2016/05/black-hole-detection.html)
7. [InfiniBand™ Architecture Specification (IBTA)](https://www.infinibandta.org/ibta-specification/)

### Vendor Monitoring Guides
8. [InfiniBand Errors Dashboard - HPE ClusterStor](https://support.hpe.com/hpesc/public/docDisplay?docId=sd00001143en_us&page=GUID-35D4C04D-E65E-45A7-A870-72F9659DE565.html&docLocale=en_US)
9. [HPC Clusters Using InfiniBand on IBM Power Systems - IBM Redbooks](https://www.redbooks.ibm.com/redbooks/pdfs/sg247767.pdf)
10. [NVIDIA UFM InfiniBand Port Counters](https://docs.nvidia.com/networking/display/ufmsdnappumv4184/InfiniBand+Port+Counters)
11. [NVIDIA DOCA Telemetry Service Guide](https://docs.nvidia.com/doca/archive/2-9-3/doca+telemetry+service+guide/index.html)
12. [NVIDIA NCCL Environment Variables (NCCL_IB_RETRY_CNT)](https://docs.nvidia.com/deeplearning/nccl/user-guide/docs/env.html#nccl-ib-retry-cnt)

### GPU System References
13. [DGX A100 User Guide](https://docs.nvidia.com/dgx/dgxa100-user-guide/introduction-to-dgxa100.html)
14. [DGX H100 User Guide](https://docs.nvidia.com/dgx/dgxh100-user-guide/introduction-to-dgxh100.html)
15. [DGX B200 User Guide](https://docs.nvidia.com/dgx/dgxb200-user-guide/introduction-to-dgxb200.html)
16. [GB200 NVL2](https://www.nvidia.com/en-us/data-center/gb200-nvl2/)

---
