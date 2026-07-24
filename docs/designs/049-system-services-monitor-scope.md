# ADR-049: System Services Monitor — Scope and Event Taxonomy

- **Status:** Proposed
- **Date:** 2026-03-20 (revised 2026-07-24)
- **Author:** dmvevents
- **Reviewers:** XRFXLP, lalitadithya

## Context

NVSentinel has no service-level monitoring of the host daemons that keep a GPU
node's fabric healthy. The existing monitors observe different layers:

- `gpu-health-monitor` polls DCGM via `pydcgm` for per-GPU device telemetry
  (PCIe, NVLink, thermal). It does not observe host process state.
- `syslog-health-monitor` parses kernel logs for XID/SXID and fallen-off-bus
  events. It does not observe systemd unit state.

Neither monitor can tell whether `nvidia-fabricmanager` is running, has hung, or
is crash-looping, whether `nvidia-persistenced` is up, or whether NVSwitch fabric
registration is stuck "In Progress" indefinitely. On NVSwitch platforms a dead or
stuck Fabric Manager silently breaks multi-GPU workloads while every device-level
and log-level check still reports healthy — the failure is in the *service layer*,
which nothing currently watches.

`system-services-monitor` fills that gap: a node-local monitor for non-DCGM,
non-syslog service health. This ADR defines its scope, the boundary against
`gpu-health-monitor`, and the health-event taxonomy it emits.

## Problem Statement

Service-level health signals fall into two buckets relative to what NVSentinel
already collects:

1. **Already covered** — per-GPU device health (PCIe, NVLink, clock throttling)
   is owned by `gpu-health-monitor` through DCGM health watches; kernel XID/SXID
   events are owned by `syslog-health-monitor`.

2. **Not covered by any monitor** — Fabric Manager process state, FM
   responsiveness, NVSwitch fabric registration state, and GPU-support service
   lifecycle (e.g. `nvidia-persistenced`). These require active host probing
   (systemd queries, `nvidia-smi` fabric-state) that does not fit a passive DCGM
   polling loop or a syslog tail.

The design goal is to add the second bucket **without** re-collecting the first.
Re-scraping DCGM-visible signals would create a second collection path for the
same underlying data, with divergent polling intervals, thresholds, and event
semantics, and would split the source of truth for GPU device health.

## Options Evaluated

### Option A: One monitor collects everything (DCGM re-scrape)

A single new monitor collects FM service health *and* re-derives PCIe/NVLink/clock
telemetry by scraping the DCGM exporter over HTTP.

| Advantage | Disadvantage |
|-----------|-------------|
| Single component for all fabric-related checks | Re-implements DCGM polling already in `gpu-health-monitor` |
| No changes to `gpu-health-monitor` | HTTP-scraped metrics are derived; `pydcgm` is authoritative |
| | Divergent thresholds and event semantics |
| | Redundant DCGM client code |

**Assessment:** Not recommended. Creates duplication and a source-of-truth conflict.

### Option B: Scope split — device health stays in DCGM path, services get a focused monitor

Keep DCGM-visible checks in `gpu-health-monitor` via `pydcgm`. Add
`system-services-monitor` scoped only to non-DCGM service health.

| Advantage | Disadvantage |
|-----------|-------------|
| Aligns device checks with the existing DCGM-based monitor | Needs a clear event taxonomy across monitors |
| Avoids HTTP-scraping duplication | |
| Keeps the new monitor focused on what DCGM cannot see | |
| Reuses the existing health-event envelope and gRPC transport | |

**Assessment:** Recommended.

### Option C: Fold service checks into `gpu-health-monitor`

Add FM systemd checks and fabric-state probes into `gpu-health-monitor`.

| Advantage | Disadvantage |
|-----------|-------------|
| Single binary for all node health | Active probes (`systemctl`, `nvidia-smi`) risk stalling the DCGM polling loop |
| | Mixes passive telemetry with active service probing |
| | `gpu-health-monitor` is not designed for host-level daemon state |

**Assessment:** Not recommended. Architectural mismatch.

## Recommendation

Adopt **Option B: Scope split**.

### Device-health signals stay in `gpu-health-monitor`

DCGM-visible signals — PCIe link health (`DCGM_HEALTH_WATCH_PCIE`), NVLink fabric
(`DCGM_HEALTH_WATCH_NVLINK`), and thermal/clock throttling
(`DCGM_HEALTH_WATCH_THERMAL`) — remain owned by `gpu-health-monitor` via the
existing `pydcgm` path and are explicitly **out of scope** for
`system-services-monitor`. "Future degradation-detection work" on these signals
means additional policy or processing built *on top of* the existing DCGM health
watches, tracked in `gpu-health-monitor`; it does **not** mean re-implementing or
re-scraping those watches here. `system-services-monitor` adds no DCGM watches and
opens no DCGM client. DCGM service health itself (the `nv-hostengine` process) is
also excluded, because `gpu-health-monitor` already reports it as
`GpuDcgmConnectivityFailure`.

### Service-health signals go to `system-services-monitor`

`system-services-monitor` is scoped to what DCGM genuinely cannot see:

- Whether `nvidia-fabricmanager` is running, hung, or crash-looping
- Whether NVSwitch fabric registration is stuck or errored per GPU
- Whether GPU-support services (e.g. `nvidia-persistenced`) are up

These require active systemd and `nvidia-smi` probing that does not belong in a
passive DCGM polling loop.

## Signal Ownership

| Signal | Source | Owner |
|--------|--------|-------|
| PCIe link downtraining | `DCGM_HEALTH_WATCH_PCIE` via `pydcgm` | `gpu-health-monitor` |
| NVLink degradation | `DCGM_HEALTH_WATCH_NVLINK` via `pydcgm` | `gpu-health-monitor` |
| GPU clock / thermal throttling | `DCGM_HEALTH_WATCH_THERMAL` via `pydcgm` | `gpu-health-monitor` |
| DCGM host-engine connectivity | `pydcgm` connect | `gpu-health-monitor` |
| XID / SXID errors | kernel log parsing | `syslog-health-monitor` |
| Fabric Manager up/down | `nsenter` → `systemctl show` (`ActiveState`/`SubState`) | `system-services-monitor` |
| Fabric Manager flapping | `systemctl show NRestarts` + restart-window tracking | `system-services-monitor` |
| Fabric Manager journal errors | `journalctl -u nvidia-fabricmanager` pattern scan | `system-services-monitor` |
| NVSwitch fabric registration state | `nvidia-smi --query-gpu=fabric.state,fabric.status` | `system-services-monitor` |
| GPU service lifecycle (`nvidia-persistenced`) | `nsenter` → `systemctl show` | `system-services-monitor` |

## Detection Mechanism

`system-services-monitor` runs a polling loop (`--poll-interval`, default 30s) that
executes host probes via `nsenter -t 1 -m --` into PID 1's mount namespace. There
is **no HTTP health endpoint** — `nvidia-fabricmanager` does not expose one — so
detection is unit-state and query based:

- **Fabric Manager service state:** `systemctl show nvidia-fabricmanager
  --property=ActiveState,SubState,MainPID,ExecMainStartTimestamp`. The service is
  considered up when `ActiveState == "active"`. `NRestarts` is queried in a
  separate `systemctl show` call for compatibility with older systemd that omits
  it from combined property lists.
- **Flap detection:** each new restart (delta in `NRestarts`) is timestamped into a
  per-service deque; the service is "flapping" when the number of restarts inside
  the window (`--flap-window`, default 600s) reaches the threshold
  (`--flap-threshold`, default 3).
- **Journal errors:** `journalctl -u nvidia-fabricmanager --since "5 minutes ago"`
  is scanned for known patterns and classified as `nvswitch_error`,
  `initialization_failed`, `timeout`, or `general_error`.
- **Fabric registration state:** `nvidia-smi --query-gpu=index,fabric.state,fabric.status
  --format=csv,noheader` per GPU. A GPU is healthy when `fabric.state == "Completed"`
  and `fabric.status == "Success"`; `fabric.state == "N/A"` (non-NVSwitch topology)
  is skipped.
- **GPU service lifecycle:** the same `systemctl show` probe for each service in the
  configured list (default `nvidia-persistenced`).

A boot grace period (`--boot-grace-period`, default 300s) suppresses unhealthy
alerts during node startup.

### FM readiness (Phase 1)

Fabric Manager is considered ready when it is **systemd `active`** *and* the
per-GPU fabric-state query shows FM is responsive/progressing — i.e. not in a
stuck or errored condition (`FM_REGISTRATION_STUCK` / `FM_FABRIC_ERROR`). An
explicit local API/socket probe is deferred as a future refinement, not a Phase-1
prerequisite, because the systemd unit exposes no health endpoint.

## Architecture

```text
┌────────────────────────────────────────────────────────────────────┐
│  Node                                                                │
│                                                                      │
│  ┌────────────────────┐ ┌────────────────────┐ ┌──────────────────┐ │
│  │ gpu-health-monitor │ │ syslog-health-     │ │ system-services- │ │
│  │                    │ │ monitor            │ │ monitor          │ │
│  │ DCGM via pydcgm:   │ │                    │ │ Non-DCGM probes: │ │
│  │ - PCIe / NVLink    │ │ Kernel log parse:  │ │ - FM systemd     │ │
│  │ - thermal / clock  │ │ - XID / SXID       │ │ - FM flap / jrnl │ │
│  │ - DCGM connectivity│ │ - fallen-off-bus   │ │ - fabric state   │ │
│  │                    │ │                    │ │ - GPU svc up/down│ │
│  │                    │ │                    │ │ + state cache    │ │
│  └─────────┬──────────┘ └─────────┬──────────┘ └────────┬─────────┘ │
│            │                      │                      │           │
│            └──────────────────────┼──────────────────────┘           │
│                                   ▼                                  │
│  ┌────────────────────────────────────────────────────────────────┐ │
│  │ platform-connector                                             │ │
│  │ - Aggregates HealthEvents from all node monitors               │ │
│  │ - gRPC over a node-local Unix domain socket (unix://…)         │ │
│  │ - Node health state reconciliation                             │ │
│  └────────────────────────────────────────────────────────────────┘ │
└────────────────────────────────────────────────────────────────────┘
```

Transport from a monitor to `platform-connector` is gRPC over a **node-local Unix
domain socket** (`grpc.insecure_channel("unix://<socket>")`), not a network
connection, so no TLS is required on this hop. (The gRPC-TLS decision in
ADR-030 applies to the janitor-controller ↔ janitor-provider path, which is a
different, potentially cross-namespace connection, and is out of scope here.)

## Event Model

`system-services-monitor` reuses the existing `HealthEvent` envelope — no new
transport model is required. Every event sets a coarse `checkName` for the
category and carries the specific condition in `errorCode`.

| Condition | `checkName` | `errorCode` | `isFatal` | `recommendedAction` |
|-----------|-------------|-------------|-----------|---------------------|
| FM not running | `FabricManagerServiceDown` | `FABRIC_MANAGER_NOT_RUNNING` | true | `RESTART_BM` |
| FM flapping | `FabricManagerServiceDown` | `FABRIC_MANAGER_FLAPPING` | true | `RESTART_BM` |
| FM journal errors | `FabricManagerServiceDown` | `JOURNAL_NVSWITCH_ERROR` / `JOURNAL_INITIALIZATION_FAILED` / `JOURNAL_TIMEOUT` / `JOURNAL_GENERAL_ERROR` | true | `RESTART_BM` |
| Fabric registration not started | `FabricStateUnhealthy` | `FM_NOT_STARTED` | true | `RESTART_BM` |
| Fabric registration stuck | `FabricStateUnhealthy` | `FM_REGISTRATION_STUCK` | true | `RESTART_BM` |
| Fabric error | `FabricStateUnhealthy` | `FM_FABRIC_ERROR` | true | `RESTART_BM` |
| GPU support service down | `GpuServiceDown` | `GPU_SERVICE_NOT_RUNNING` | false | `CONTACT_SUPPORT` |

Multiple `errorCode` values combine on a single event (e.g. an FM-down event that
is also flapping carries `[FABRIC_MANAGER_NOT_RUNNING, FABRIC_MANAGER_FLAPPING]`).

### HealthEvent schema

Events populate the shared `HealthEvent` protobuf. Fields set by this monitor:

| Field | Type | Value |
|-------|------|-------|
| `version` | int | `1` |
| `agent` | string | `"system-services-monitor"` |
| `componentClass` | string | `"INFRASTRUCTURE"` |
| `checkName` | string | category (table above) |
| `isFatal` | bool | per condition (table above) |
| `isHealthy` | bool | `false` on failure, `true` on recovery |
| `message` | string | human-readable detail |
| `recommendedAction` | enum `RecommendedAction` | `RESTART_BM` (fatal) / `CONTACT_SUPPORT` (non-fatal) / `NONE` (healthy) |
| `errorCode` | repeated string | condition codes (table above) |
| `entitiesImpacted` | repeated `Entity` | `{entityType, entityValue}` — `NODE:<node>` for service checks, `GPU:<index>` for fabric-state checks |
| `metadata` | map<string,string> | check-specific context |
| `generatedTimestamp` | Timestamp | event time |
| `nodeName` | string | node the monitor runs on |
| `processingStrategy` | enum `ProcessingStrategy` | `EXECUTE_REMEDIATION` (default) or `STORE_ONLY` |

Example — Fabric Manager down (fatal, node-scoped):

```json
{
  "version": 1,
  "agent": "system-services-monitor",
  "componentClass": "INFRASTRUCTURE",
  "checkName": "FabricManagerServiceDown",
  "isFatal": true,
  "isHealthy": false,
  "message": "Fabric Manager is failed on gpu-node-07",
  "recommendedAction": "RESTART_BM",
  "errorCode": ["FABRIC_MANAGER_NOT_RUNNING"],
  "entitiesImpacted": [{"entityType": "NODE", "entityValue": "gpu-node-07"}],
  "metadata": {"sub_state": "failed", "n_restarts": "4", "flapping": "true"},
  "nodeName": "gpu-node-07",
  "processingStrategy": "EXECUTE_REMEDIATION"
}
```

Example — fabric registration stuck (fatal, GPU-scoped):

```json
{
  "version": 1,
  "agent": "system-services-monitor",
  "componentClass": "INFRASTRUCTURE",
  "checkName": "FabricStateUnhealthy",
  "isFatal": true,
  "isHealthy": false,
  "message": "FM_REGISTRATION_STUCK on gpu-node-07 GPU 7: state=In Progress, status=In Progress",
  "recommendedAction": "RESTART_BM",
  "errorCode": ["FM_REGISTRATION_STUCK"],
  "entitiesImpacted": [{"entityType": "GPU", "entityValue": "7"}],
  "metadata": {"gpu_index": "7", "fabric_state": "In Progress", "fabric_status": "In Progress", "failure_class": "FM_REGISTRATION_STUCK"},
  "nodeName": "gpu-node-07",
  "processingStrategy": "EXECUTE_REMEDIATION"
}
```

Example — GPU support service down (non-fatal, node-scoped):

```json
{
  "version": 1,
  "agent": "system-services-monitor",
  "componentClass": "INFRASTRUCTURE",
  "checkName": "GpuServiceDown",
  "isFatal": false,
  "isHealthy": false,
  "message": "Service nvidia-persistenced is dead on gpu-node-07",
  "recommendedAction": "CONTACT_SUPPORT",
  "errorCode": ["GPU_SERVICE_NOT_RUNNING"],
  "entitiesImpacted": [{"entityType": "NODE", "entityValue": "gpu-node-07"}],
  "metadata": {"service_name": "nvidia-persistenced", "sub_state": "dead"},
  "nodeName": "gpu-node-07",
  "processingStrategy": "EXECUTE_REMEDIATION"
}
```

## Cached State

`system-services-monitor` emits **state transitions only**, not per-cycle events.
The event processor keeps an in-memory `entity_cache` so an unchanged condition is
not re-sent every poll interval.

- **Key:** `"<checkName>|<entityType>:<entityValue>"`, built from the impacted
  entities sorted by `(entityType, entityValue)` so key construction is
  order-independent (e.g. `FabricManagerServiceDown|NODE:gpu-node-07`).
- **Value:** `CachedEntityState(is_fatal, is_healthy)` — the last emitted state for
  that key. An event is sent only when the key is unseen or either `is_fatal` or
  `is_healthy` differs from the cached value.
- **Concurrency:** the read-decide-write sequence is guarded by a
  `threading.Lock`. Callbacks fire on a `ThreadPoolExecutor`, so without the lock
  two overlapping callbacks could observe the same stale entry and both emit while
  one blocks in the gRPC send. Under the lock, the new state is *reserved* into the
  cache immediately (before the send) so a concurrent callback skips the duplicate.
- **Rollback:** the gRPC send retries with backoff (up to 5 attempts, ~26 s worst
  case). If it ultimately fails or raises, the reserved cache entries are rolled
  back (popped) so the next poll cycle re-attempts those events. The cache is only
  committed as the authoritative last-sent state after a successful send.
- **Lifetime:** in-memory for the life of the process; there is no persistence, so
  the cache is empty on restart and the first post-restart cycle re-establishes
  current state.

### False-positive mitigations

- **Boot grace period** (default 300s): suppress unhealthy alerts during node
  startup.
- **Flap detection**: only flag Fabric Manager as flapping when restarts within the
  configurable window exceed the threshold, rather than on any single restart.

## Scope

In scope for `system-services-monitor`:

- Fabric Manager systemd health (`ActiveState`/`SubState`)
- Fabric Manager flap detection and journal-error classification
- Per-GPU NVSwitch fabric-registration state
- GPU support-service lifecycle (`nvidia-persistenced`)
- gRPC client (Unix domain socket), state caching, structured logging, CLI

Out of scope (owned elsewhere):

- PCIe, NVLink, thermal/clock telemetry — `gpu-health-monitor` (DCGM watches)
- DCGM host-engine connectivity — `gpu-health-monitor` (`GpuDcgmConnectivityFailure`)
- XID / SXID kernel events — `syslog-health-monitor`

### Integration testing

- Device-level faults handled only by `gpu-health-monitor`
- Kernel XID/SXID handled only by `syslog-health-monitor`
- Service-level faults handled only by `system-services-monitor`
- No overlap in event generation for the same underlying condition
- State-cache deduplication verified (transition-only emission)
- HealthEvent schema compatibility over gRPC

## Consequences

### Positive
- Adds service-level monitoring that no existing monitor provides
- No overlap or source-of-truth conflict with DCGM device health
- Keeps `system-services-monitor` focused and small
- Preserves consistent event semantics across monitors
- Explicit, testable ownership boundary against `gpu-health-monitor` and
  `syslog-health-monitor`

### Negative
- Adds a third node-local monitor DaemonSet to operate
- Requires the event taxonomy above to stay in sync with remediation policy in
  `platform-connector`
