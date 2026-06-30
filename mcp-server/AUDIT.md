# NVSentinel MCP Server — Monitor-Surface Audit

| | |
|---|---|
| **Date** | 2026-05-14 |
| **Auditor** | Claude Code session, working on `feat/mcp-server-merge` |
| **Donation source** | `ArangoGutierrez/k8s-gpu-mcp-server@80ac33d89ede70aa3f967088f8716d94b8e692e4` (pinned in `.agents/plans/donation-source.md`) |
| **Design spec** | `/Users/eduardoa/src/github/ArangoGutierrez/k8s-gpu-mcp-server/.worktrees/merge-into-nvsentinel/docs/superpowers/specs/2026-05-13-merge-gpu-mcp-into-nvsentinel-design.md` |
| **Status** | Drives downstream Task 2-22 decisions. Per-tool table is authoritative. |

---

## 1. Summary

The donation spec assumed a hybrid read surface — store reads **plus** per-monitor gRPC fan-out. The audit finds NVSentinel's actual surface is **store-only** for any centralized component: the monitors expose **no read-side gRPC servers**, and the single proto `service PlatformConnector.HealthEventOccurredV1` is the **write** ingress used by all monitors (and the analyzer) to push events into the store. Sibling components (`event-exporter`, `fault-quarantine`, `fault-remediation`, `health-events-analyzer`) all consume the store exclusively via `store-client/pkg/datastore.HealthEventStore`.

This simplifies the donation: `mcp-server` becomes a pure store-reader with optional K8s API access. The `pkg/monitors/` package planned in the original Task 3 is **dropped**. All `Config`-level monitor wiring in the ported `pkg/mcp/server.go` (Task 4) is removed; only `Store` and (where needed) the K8s clientset remain.

Of the ten tools from `gpu-mcp-server`, **nine ship as Working** against the store + K8s API in this PR. **One ships as Stub** (`get_nvlink_topology`) because topology data is currently read from a per-node JSON file and is not persisted into the store; a companion issue is drafted for the monitor extension that would close the gap.

---

## 2. Spec divergences (load-bearing)

| Original spec assumption | Actual NVSentinel state | Adaptation |
|---|---|---|
| Module path `github.com/NVIDIA/NVSentinel/...` | Module path `github.com/nvidia/nvsentinel/...` (lowercase) | Use lowercase everywhere in new code, replace directives, ported file rewrites |
| Go 1.22 | Go 1.26.0 with `toolchain go1.26.2` (per `event-exporter/go.mod`) | `mcp-server/go.mod` declares `go 1.26.0`, `toolchain go1.26.2` |
| `HealthSnapshot` proto with per-GPU snapshot rows | No snapshot schema. Only `HealthEvent` (event stream) in `data-models/protobufs/health_event.proto` | All "snapshot" tools reinterpret as "latest events filtered by node/entity" |
| `HealthMonitor` gRPC service(s) on each monitor | Only `PlatformConnector.HealthEventOccurredV1` (events flow IN, not out). Monitors are pure producers. | Drop `pkg/monitors/` from Task 3. `Config` carries `Store` only. |
| Mongo-only event store | Pluggable backend: `store-client/pkg/datastore/providers/{mongodb,postgresql}`. `HealthEventStore` interface is DB-agnostic. | Depend on the abstract `HealthEventStore` interface, never on Mongo specifically. Provider chosen at startup via env (`store-client/pkg/config/env_loader.go`). |
| XID errors live in `gpu-health-monitor` | `gpu-health-monitor` is **Python** (DCGM bindings). **`syslog-health-monitor`** (Go) is the actual XID source — `pkg/xid/xid_handler.go` emits `HealthEvent{ErrorCode: []string{xidResp.Result.DecodedXIDStr}}`. `DecodedXIDStr` is the numeric XID as a string (`"13"`, `"74"`). | `analyze_xid` tool filters on `errorCode` field containing pure numeric strings; documented as such. |
| `pkg/incidents` "incident objects" stored in a dedicated collection | "Incidents" are not a stored type. `health-events-analyzer` (`pkg/analyzer/xid_burst_detector.go`) detects patterns and **re-publishes synthesized events** via `PlatformConnector.HealthEventOccurredV1` — these land in the same `HealthEvents` collection, identifiable by `agent=health-events-analyzer`. | `get_incident_report` consumes analyzer-sourced events; the ported `pkg/incidents` pattern matcher (Task 11) runs at query time over related raw events to enrich the response narrative. |
| Helm subchart at `mcp-server/deploy/helm/`, dependency from top-level | Charts live at `distros/kubernetes/nvsentinel/charts/<component>/` (one per component). Top-level chart at `distros/kubernetes/nvsentinel/Chart.yaml` declares dependencies. | Task 18 puts the chart at `distros/kubernetes/nvsentinel/charts/mcp-server/`, not under `mcp-server/`. |
| Sibling components roll their own server/logger/metrics | All siblings use `commons/pkg/{logger,server,metrics,tracing}` (see `event-exporter/main.go`). OpenTelemetry tracing is first-class (e.g. `tracing.StartSpan(ctx, "health_events_analyzer.grpc.publish")` in `health-events-analyzer/pkg/publisher/publisher.go:53`). | `mcp-server/main.go` mirrors `event-exporter/main.go`: `commons/pkg/logger` for `slog`, `commons/pkg/server` for HTTP servers, `commons/pkg/metrics` for Prometheus, `commons/pkg/tracing` for OTel. |

---

## 3. Per-tool data source matrix

| MCP tool | Data source | Status | Gap (if Stub) |
|---|---|---|---|
| `gpu_inventory` | `HealthEventStore.FindHealthEventsByNode(ctx, node)` — return latest events filtered by `componentClass==GPU` (or `entitiesImpacted` entries with `entityType==GPU`). | Working | n/a |
| `gpu_health` | `HealthEventStore.FindHealthEventsByNode(ctx, node)` + optional filter by GPU UUID via `entitiesImpacted[].entityType==GPU && entityValue==<uuid>`. | Working | n/a |
| `describe_gpu_node` | Store: `FindLatestEventForNode(ctx, node)` + K8s API `corev1.Nodes().Get(ctx, node, ...)` for spec/status/labels/taints. | Working | n/a |
| `pod_gpu_allocation` | Pure K8s API: `corev1.Pods(ns).List(ctx, ...)` filtered to pods with `nvidia.com/gpu` resource requests > 0; GPU UUIDs resolved from `NVIDIA_VISIBLE_DEVICES` env var when present. | Working | n/a |
| `pod_failure` | Store: `FindHealthEventsByQuery(ctx, query.New().Build(...))` filtered by `entitiesImpacted` carrying the pod identity + K8s API `Pods().Get(...)`, `Events().List(...)`. | Working | n/a |
| `analyze_xid` | Store: `FindHealthEventsByQuery(...)` filtered by `errorCode` containing the requested numeric XID code (e.g., `"13"`, `"74"`). XID emission confirmed at `health-monitors/syslog-health-monitor/pkg/xid/xid_handler.go` (assignment `ErrorCode: []string{xidResp.Result.DecodedXIDStr}`). | Working | n/a |
| `get_nvlink_topology` | Topology lives in `data-models/pkg/model/gpu_metadata.go` `NVLink struct {LinkID, RemotePCIAddress, RemoteLinkID}`, loaded by `syslog-health-monitor` from a per-node JSON file at runtime (`os.ReadFile(r.path)` in `health-monitors/syslog-health-monitor/pkg/metadata/reader.go:69`). **Not persisted to the store.** A centralized read-only `mcp-server` cannot reach per-node files. | **Stub** | See § 6 — needs a monitor extension to persist topology into the store. No tracking GitHub issue filed (donor direction); stub envelope's `needed_monitor_extension` explains the gap inline. |
| `explain_failure` | Store: `FindHealthEventsByNode(ctx, node)` time-windowed + ported `pkg/tools/incidents.go` `Match([]HealthEvent) []Incident` pattern matcher → narrative + severity. | Working | n/a |
| `get_incident_report` | Store: `FindHealthEventsByQuery(...)` filtered by `agent==health-events-analyzer` for the incident id (carried in `HealthEvent.id` or `metadata` per analyzer convention) + related raw events for `RelatedEvents` enrichment. | Working | n/a |
| `get_gpu_timeline` | Store: `FindHealthEventsByQuery(...)` with node filter + `generatedTimestamp` range (`google.protobuf.Timestamp`, nanosecond precision in proto3). Result sorted ascending by timestamp. | Working | n/a |

**Summary:** 9 Working, 1 Stub. Down from the design's hedged 3-tool "Working OR Stub" range.

---

## 4. NVSentinel read surface consumed

### 4.1 `store-client/pkg/datastore.HealthEventStore` (`store-client/pkg/datastore/interfaces.go`)

```go
type HealthEventStore interface {
    FindHealthEventsByNode(ctx context.Context, nodeName string) ([]HealthEventWithStatus, error)
    FindHealthEventsByFilter(ctx context.Context, filter map[string]interface{}) ([]HealthEventWithStatus, error)
    FindHealthEventsByStatus(ctx context.Context, status Status) ([]HealthEventWithStatus, error)
    FindHealthEventsByQuery(ctx context.Context, builder QueryBuilder) ([]HealthEventWithStatus, error)
    FindLatestEventForNode(ctx context.Context, nodeName string) (*HealthEventWithStatus, error)
    FindHealthEventsByQueryBatched(
        ctx context.Context, builder QueryBuilder, batchSize int,
        fn func([]HealthEventWithStatus) error,
    ) error
    // Plus write methods (UpdateHealthEventStatus, ...) — mcp-server WILL NOT USE these.
}
```

`mcp-server/pkg/store.StoreReader` (Task 3) will be a **narrowed** interface containing only the read methods, with the concrete `MongoStore`/Postgres-aware implementation delegating to the underlying `HealthEventStore`.

### 4.2 `store-client/pkg/query.Builder` (`store-client/pkg/query/builder.go`)

```go
func New() *Builder
func (b *Builder) Build(cond Condition) *Builder
func (b *Builder) ToMongo() map[string]interface{}
func (b *Builder) ToSQL() (string, []interface{})
func (b *Builder) ToSQLWithOffset(startParam int) (string, []interface{})
```

Conditions composed via the `Condition` type (file under `store-client/pkg/query/`). MCP tools use `query.New().Build(condition)` and pass to `FindHealthEventsByQuery`.

### 4.3 `data-models/protobufs/health_event.proto` — canonical event schema

```proto
message HealthEvent {
  uint32 version = 1;
  string agent = 2;
  string componentClass = 3;
  string checkName = 4;
  bool isFatal = 5;
  bool isHealthy = 6;
  string message = 7;
  RecommendedAction recommendedAction = 8;
  repeated string errorCode = 9;
  repeated Entity entitiesImpacted = 10;
  map<string, string> metadata = 11;
  google.protobuf.Timestamp generatedTimestamp = 12;
  string nodeName = 13;
  BehaviourOverrides quarantineOverrides = 14;
  BehaviourOverrides drainOverrides = 15;
  ProcessingStrategy processingStrategy = 16;
  string id = 17;
  string customRecommendedAction = 18;
}

message Entity {
  string entityType = 1;
  string entityValue = 2;
}
```

`HealthEventWithStatus` wraps `HealthEvent` with `HealthEventStatus` (quarantine/drain/remediation lifecycle fields).

### 4.4 Commons primitives consumed (mirroring `event-exporter/main.go`)

| Package | Purpose |
|---|---|
| `commons/pkg/logger` | `slog`-based structured logger with trace correlation |
| `commons/pkg/server` | HTTP server helpers (graceful shutdown, common middleware) |
| `commons/pkg/metrics` | Prometheus registration/exposition pattern |
| `commons/pkg/tracing` | OpenTelemetry init/shutdown (`tracing.InitTracing(tracing.ServiceMCPServer)` — new service constant to add) and span helpers |

### 4.5 K8s clientset (only for `pod_gpu_allocation`, `pod_failure`, `describe_gpu_node`)

`k8s.io/client-go/kubernetes.Interface`, dialed via in-cluster config; tests use `k8s.io/client-go/kubernetes/fake.NewSimpleClientset`.

---

## 5. Monitor surface NOT consumed

These were assumed in the spec but the audit confirms they do not exist in NVSentinel as read interfaces:

| Spec-assumed surface | Reality | File:line |
|---|---|---|
| `gpu-health-monitor` gRPC `GetSnapshot`/`GetHealth` | gpu-health-monitor is Python; no Go gRPC server | `health-monitors/gpu-health-monitor/gpu_health_monitor/` (Python tree) |
| `kubernetes-object-monitor` events stream | The monitor writes K8s events into the store as `HealthEvent`s; mcp-server reads them like any other event | `health-monitors/kubernetes-object-monitor/` |
| Cross-node aggregation gRPC | Aggregation = store query (`FindHealthEventsByQuery` with no node filter) | n/a |
| `XIDWatcher` for live XID events | XID events are persisted as `HealthEvent`s by `syslog-health-monitor`; mcp-server queries the store | `health-monitors/syslog-health-monitor/pkg/xid/xid_handler.go` |

---

## 6. Companion issue draft (filing deferred)

Per donor direction (2026-05-14), **no GitHub issue is filed against `NVIDIA/NVSentinel` as part of this PR**. The draft below remains in the audit as supporting material — if NVSentinel maintainers ask for a tracking issue during PR review, this content can be used directly. The stub tool (`mcp-server/pkg/tools/get_nvlink_topology.go`, Task 15) leaves `tracking_issue` empty and explains the gap inline in `needed_monitor_extension`, referencing this AUDIT.md section by relative file path.

### 6.1 Issue draft — Surface NVLink topology in the HealthEvents stream

**Title:** `feat(monitors): persist NVLink topology into HealthEvents (or a dedicated metadata collection)`

**Labels:** `mcp,enhancement`

**Body:**

> The new `mcp-server` component (donation from `ArangoGutierrez/k8s-gpu-mcp-server`) exposes `get_nvlink_topology` as an MCP tool. The tool needs to return per-GPU NVLink connectivity: `{local GPU UUID, remote GPU UUID, link ID, status}`.
>
> Today, NVLink topology is read from a per-node JSON metadata file at `gpu_metadata.json` (loaded by `health-monitors/syslog-health-monitor/pkg/metadata/reader.go:69` via `os.ReadFile(r.path)`). The structure is `data-models/pkg/model/gpu_metadata.go`'s `NVLink {LinkID int, RemotePCIAddress string, RemoteLinkID int}`. This metadata is local to each node and is **not persisted into the NVSentinel store**, so a centralized read-only deployment (like `mcp-server`) cannot access it.
>
> **Scope of the requested extension:**
>
> - **Option A (preferred):** Have one of the monitors (likely `syslog-health-monitor`, since it already loads the metadata reader, or a new `metadata-collector` step) emit a periodic `HealthEvent` of `componentClass=NVLINK_TOPOLOGY` carrying the per-GPU NVLink list in `metadata` (serialized JSON or repeated `Entity`). `mcp-server` would then read the latest such event per node.
> - **Option B:** Add a `TopologyMetadata` collection alongside `HealthEvents` / `MaintenanceEvents` and define a small `TopologyMetadataStore` interface in `store-client/pkg/datastore`. Per-node snapshot push from each monitor at startup + periodic refresh.
>
> **Acceptance criteria:**
>
> - `mcp-server`'s `get_nvlink_topology` tool returns real topology data for a node deployed in `nvsentinel/charts/` integration tests.
> - The audit query
>   `rg -i 'nvlink|NvLink|NVLink' data-models/protobufs/ store-client/pkg/datastore/` returns at least one non-test hit in a persisted-data path.
>
> **Caller in this donation PR:** `mcp-server/pkg/tools/get_nvlink_topology.go` ships as a stub returning the `NVSENTINEL_DATA_GAP` envelope (per design spec § 6.3) with this issue URL in `tracking_issue`. The stub will be swapped for the real implementation in a follow-up PR once one of the options above lands.

**Resolution:** No issue filed in this PR (donor direction). If maintainers request one during review, this body can be filed verbatim via `gh issue create --repo NVIDIA/NVSentinel --label "mcp,enhancement" --title "feat(monitors): persist NVLink topology into HealthEvents (or a dedicated metadata collection)" --body-file mcp-server/AUDIT.md` (after a quick excerpt of just § 6.1). The resulting URL would then be substituted into `mcp-server/pkg/tools/get_nvlink_topology.go`'s `tracking_issue` constant in a follow-up commit.

---

## 7. Architectural simplifications adopted (vs spec)

1. **Drop `pkg/monitors/`.** No monitor gRPC client. Task 3 collapses to `pkg/store/` only.
2. **`Config` struct** in `pkg/mcp/server.go` (Task 4) drops `Monitors` field. Only `Store store.StoreReader` and (for K8s-touching tools) a `K8sClient kubernetes.Interface`.
3. **Module path** is `github.com/nvidia/nvsentinel/mcp-server` (lowercase).
4. **Go 1.26.0 toolchain go1.26.2.**
5. **`commons/pkg/{logger,server,metrics,tracing}`** used throughout `main.go` and tool handlers (matches `event-exporter/main.go` pattern).
6. **Helm subchart at `distros/kubernetes/nvsentinel/charts/mcp-server/`** (not `mcp-server/deploy/helm/`). Top-level chart at `distros/kubernetes/nvsentinel/Chart.yaml` adds the dependency with `condition: mcpServer.enabled`.
7. **Default deployment is opt-in** (`mcpServer.enabled: false` in top-level `distros/kubernetes/nvsentinel/values.yaml`).
8. **Tracing service constant:** add `tracing.ServiceMCPServer` in `commons/pkg/tracing/` (touching commons is a documented integration point; verify in Task 20 whether maintainers prefer this or a free-form string).

---

## 8. Open items the receiving Tasks 2-22 must verify

- **commons/pkg/tracing service constants:** confirm by reading `commons/pkg/tracing/` whether the API takes an enum or a string. If string, no commons modification needed.
- **K8s client dial helper:** read `event-exporter/pkg/initializer/` or equivalent to see how siblings dial in-cluster vs out-of-cluster, and lift the helper.
- **MCP transport choice in the kind environment:** confirm whether siblings expose ports via Service ClusterIP only (no NodePort) and whether cert-manager is already present in the Tilt setup (a topic for Task 17/18).
- **CodeRabbit config:** check `.coderabbit.yaml` for path-based review opt-ins.
- **golangci-lint config:** read `.golangci.yml` for any module-specific carve-outs.

---

## 9. Where to find things (for the receiving session)

| Asset | Path |
|---|---|
| Canonical event proto | `data-models/protobufs/health_event.proto` |
| Generated Go bindings | `data-models/pkg/protos/` (referenced as `protos.HealthEvent`, `protos.HealthEvents`, `protos.PlatformConnectorClient` in callers) |
| `HealthEventStore` interface | `store-client/pkg/datastore/interfaces.go` |
| QueryBuilder | `store-client/pkg/query/builder.go` |
| GPU metadata model (NVLink struct) | `data-models/pkg/model/gpu_metadata.go` |
| Closest sibling for layout | `event-exporter/` (single Deployment, store reader, OTel-traced, ko-built) |
| Top-level Helm chart | `distros/kubernetes/nvsentinel/Chart.yaml` |
| Sibling Helm subchart example | `distros/kubernetes/nvsentinel/charts/event-exporter/` |
| Root ko config | `.ko.yaml` |
| Root Makefile | `Makefile` |
| Donation source pin | `.agents/plans/donation-source.md` |
