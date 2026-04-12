# ADR-030: Fabric Manager Monitoring — Scope and DCGM Integration

- **Status:** Proposed
- **Date:** 2026-03-20
- **Author:** dmvevents
- **Reviewers:** XRFXLP, lalitadithya
- **Related:** PR #891, Issue #883, Issue #889, Issue #890, ADR-029

## Context

PR #891 adds a `fabric-manager-monitor` with 6 check categories: FM service health,
PCIe link downtraining, NVLink fabric degradation, GPU clock throttling, CUDA context
validity, and GPU service lifecycle. The implementation uses `nsenter` + `nvidia-smi`
and DCGM exporter HTTP scraping to collect telemetry.

During review, XRFXLP raised two architectural questions:

1. **PCIe and NVLink checks**: The existing `gpu-health-monitor` already polls DCGM
   directly via `pydcgm` and has health watches for `DCGM_HEALTH_WATCH_PCIE` and
   `DCGM_HEALTH_WATCH_NVLINK`. Should these be extended there instead of
   reimplementing via DCGM exporter HTTP scraping?

2. **Monitor scope**: Should `fabric-manager-monitor` be focused on what DCGM
   genuinely cannot see — FM service health, GPU service lifecycle?

This ADR resolves both questions and defines the architectural boundary between
hardware telemetry monitoring and service-level health monitoring.

## Problem Statement

The current PR #891 mixes two categories of health detection in a single monitor:

1. **DCGM-visible device health** — PCIe link state, NVLink bandwidth, clock
   throttle reasons. These signals are available through DCGM field IDs that
   `gpu-health-monitor` already has access to via `pydcgm`.

2. **Non-DCGM service health** — Fabric Manager process state, FM responsiveness,
   CUDA context validity, GPU service lifecycle. These are not exposed through
   DCGM health watches.

Reimplementing DCGM-visible signals via HTTP scraping of the DCGM exporter creates
a second collection path for the same underlying data, with different polling
intervals, thresholds, and event semantics. This duplicates logic already in
`gpu-health-monitor` and introduces inconsistent source-of-truth for GPU health.

## Options Evaluated

### Option A: Keep all checks in `fabric-manager-monitor` (current PR #891)

All 6 check categories remain in a single new monitor. PCIe and NVLink telemetry
collected via DCGM exporter HTTP scraping.

| Advantage | Disadvantage |
|-----------|-------------|
| Single component for all fabric-related checks | Reimplements DCGM polling already in `gpu-health-monitor` |
| No changes to `gpu-health-monitor` | HTTP scraping is derived; `pydcgm` is authoritative |
| | Divergent thresholds and event semantics |
| | +4,432 LOC includes redundant client definitions |

**Assessment:** Not recommended. Creates duplication and source-of-truth conflict.

### Option B: Scope split — extend `gpu-health-monitor` + focused `fabric-manager-monitor`

Move DCGM-visible checks into `gpu-health-monitor`. Keep `fabric-manager-monitor`
focused on non-DCGM service health.

| Advantage | Disadvantage |
|-----------|-------------|
| Aligns hardware checks with existing DCGM-based monitor | Requires refactoring PR #891 into two change sets |
| Avoids HTTP scraping duplication | Needs clear event taxonomy between monitors |
| Keeps new monitor focused on what DCGM cannot see | |
| Reuses existing polling, state caching, gRPC transport | |
| Directly addresses reviewer feedback | |

**Assessment:** Recommended.

### Option C: Fold all checks into `gpu-health-monitor`

Add FM service checks and CUDA context probes into `gpu-health-monitor`.

| Advantage | Disadvantage |
|-----------|-------------|
| Single binary for all node health | Active probes (CUDA context, systemd) risk stalling DCGM polling loop |
| | Mixes passive telemetry with active service probing |
| | `gpu-health-monitor` is not designed for host-level daemon state |

**Assessment:** Not recommended. Architectural mismatch.

## Recommendation

Adopt **Option B: Scope split**.

### Answer to review question 1

**Yes. PCIe and NVLink checks should extend `gpu-health-monitor`, not be
reimplemented in `fabric-manager-monitor` via DCGM exporter HTTP scraping.**

`gpu-health-monitor` already polls DCGM via `pydcgm`. The DCGM field IDs for
degradation detection are:

| Signal | DCGM Field ID | Current Watch |
|--------|---------------|---------------|
| PCIe link generation | `DCGM_FI_DEV_PCIE_LINK_GEN` | `DCGM_HEALTH_WATCH_PCIE` (fatal only) |
| PCIe link width | `DCGM_FI_DEV_PCIE_LINK_WIDTH` | `DCGM_HEALTH_WATCH_PCIE` (fatal only) |
| NVLink bandwidth | `DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL` | `DCGM_HEALTH_WATCH_NVLINK` (fatal only) |
| NVLink error counters | `DCGM_FI_DEV_NVLINK_*_ERRORS` | `DCGM_HEALTH_WATCH_NVLINK` (fatal only) |
| Clock throttle reasons | `DCGM_FI_DEV_CLOCK_THROTTLE_REASONS` | None |

The existing health watches catch fatal errors. Extending the `pydcgm` polling loop
to evaluate these field IDs against baselines enables degradation detection without
a second collection path.

### Answer to review question 2

**Yes. `fabric-manager-monitor` should be scoped to what DCGM genuinely cannot
see: FM service health and related service lifecycle state.**

DCGM monitors per-GPU device metrics. It does not monitor:
- Whether the Fabric Manager process is running
- Whether FM has hung or is in a crashloop
- Whether the fabric orchestration state is stuck ("In Progress" indefinitely)
- Whether a CUDA context can be created on a given GPU (driver/runtime wedge)

These require active probing (systemd queries, CUDA allocation tests) that do not
belong in a passive DCGM polling loop.

## Signal Ownership

| Signal | Source | Owner |
|--------|--------|-------|
| PCIe link downtraining | DCGM field IDs via `pydcgm` | `gpu-health-monitor` |
| NVLink degradation | DCGM field IDs via `pydcgm` | `gpu-health-monitor` |
| GPU clock throttling | DCGM field IDs via `pydcgm` | `gpu-health-monitor` |
| FM service up/down | `nsenter` + systemd | `fabric-manager-monitor` |
| FM responsiveness | FM-specific probe | `fabric-manager-monitor` |
| FM crashloop/flapping | Restart count + time window | `fabric-manager-monitor` |
| Fabric state stuck | `nvidia-smi` fabric.state | `fabric-manager-monitor` |
| CUDA context validity | Active `cuInit`/`cudaMalloc` probe | `fabric-manager-monitor` |
| GPU service lifecycle | `nsenter` + systemd | `fabric-manager-monitor` |

## Implementation

### Architecture

```text
┌─────────────────────────────────────────────────────────────┐
│  Node                                                       │
│                                                             │
│  ┌──────────────────────┐    ┌──────────────────────────┐   │
│  │  gpu-health-monitor  │    │  fabric-manager-monitor  │   │
│  │                      │    │                          │   │
│  │  DCGM via pydcgm:    │    │  Non-DCGM active probes: │   │
│  │  - PCIe link gen/wid │    │  - FM systemd state      │   │
│  │  - NVLink bandwidth  │    │  - FM restart frequency  │   │
│  │  - NVLink errors     │    │  - Fabric state query    │   │
│  │  - Clock throttle    │    │  - CUDA context probe    │   │
│  │  - XID errors        │    │  - GPU svc lifecycle     │   │
│  │                      │    │                          │   │
│  │  State caching ──────│──┐ │  State caching ──────────│─┐ │
│  └──────────┬───────────┘  │ └──────────┬───────────────┘ │ │
│             │              │            │                  │ │
│             ▼              ▼            ▼                  │ │
│  ┌──────────────────────────────────────────────────────┐  │ │
│  │  platform-connector                                  │  │ │
│  │  - Aggregates health events from both monitors       │  │ │
│  │  - gRPC transport (ADR-029 TLS)                      │  │ │
│  │  - Node health state reconciliation                  │  │ │
│  └──────────────────────────────────────────────────────┘  │ │
└─────────────────────────────────────────────────────────────┘ │
```

### Event Model

Reuse the existing health event envelope. No new transport model required.

**Device health events** from `gpu-health-monitor`:
- `PCIE_LINK_DEGRADED` — link gen or width below expected baseline
- `NVLINK_DEGRADED` — bandwidth below threshold or error count above threshold
- `CLOCK_THROTTLED` — non-idle throttle reason active (exclude idle bitmask `0x1`)

**Service health events** from `fabric-manager-monitor`:
- `FM_DOWN` — FM service not running
- `FM_UNRESPONSIVE` — FM running but fabric state stuck
- `FM_FLAPPING` — restart count exceeds threshold in time window
- `CUDA_CONTEXT_INVALID` — `cuInit`/`cudaMalloc` probe fails

Both monitors emit state transitions only (via state caching). Both use the
existing gRPC transport secured per ADR-029.

### False-Positive Mitigations (retained from PR #891)

- **Boot grace period** (default 300s): Suppress alerts during node startup
- **Flap detection**: Track FM restart frequency within configurable window
- **GPU idle filter**: Exclude idle throttle reason `0x0000000000000001`
- **NVLink correlation**: Bandwidth-zero flagged only when correlated with FM down

## Implementation Plan

### Phase 1: Refactor PR #891

Remove from `fabric-manager-monitor`:
- DCGM exporter HTTP scraping for PCIe, NVLink, clock throttling
- Any hardware telemetry logic already coverable by `pydcgm`

Retain:
- FM systemd health checks
- FM flap detection
- Fabric state query
- CUDA context validity probe
- GPU service lifecycle checks
- gRPC client, state caching, structlog, Click CLI

Target: ~500-800 LOC (down from 4,432).

### Phase 2: Extend `gpu-health-monitor`

Add DCGM field ID subscriptions for:
- `DCGM_FI_DEV_PCIE_LINK_GEN` / `DCGM_FI_DEV_PCIE_LINK_WIDTH`
- `DCGM_FI_DEV_NVLINK_BANDWIDTH_TOTAL` / NVLink error counters
- `DCGM_FI_DEV_CLOCK_THROTTLE_REASONS`

Implement threshold evaluation in the existing polling loop. Emit health events
through existing gRPC path with state caching.

### Phase 3: Integration testing

- Device-level faults handled only by `gpu-health-monitor`
- Service-level faults handled only by `fabric-manager-monitor`
- No overlap in event generation for the same underlying condition
- State caching deduplication verified for both monitors
- Event schema compatibility over gRPC

## Open Questions

1. **Clock throttling ownership**: If `DCGM_FI_DEV_CLOCK_THROTTLE_REASONS` is
   reliably available through the existing `pydcgm` path, clock throttling moves
   to `gpu-health-monitor`. If not, it stays in `fabric-manager-monitor` as an
   `nvidia-smi` query. Needs validation against current DCGM version in NVSentinel.

2. **CUDA context probe implementation**: The current PR #891 uses a subprocess
   `torch` test. If a lighter probe (`cuInit` via ctypes or `pynvml`) is preferred,
   the implementation can be adjusted. The probe mechanism is orthogonal to the
   scope split.

3. **FM readiness definition**: Is FM readiness determined from systemd active
   state alone, or should it include a local API/socket probe? Active + responsive
   is preferred over active-only if FM exposes a health endpoint.

## Consequences

### Positive
- Removes overlap between hardware and service monitoring
- Uses DCGM directly where `pydcgm` access already exists
- Keeps `fabric-manager-monitor` focused and small (~500-800 LOC)
- Preserves consistent event semantics across monitors
- Directly addresses reviewer feedback from XRFXLP

### Negative
- PR #891 must be refactored into two smaller changes
- Phase 2 (`gpu-health-monitor` extension) requires changes to existing code
- CUDA context probe placement may need further discussion
