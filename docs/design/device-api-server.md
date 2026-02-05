# Device API Server - Design & Implementation Plan

> **Status**: Draft  
> **Author**: NVSentinel Team  
> **Created**: 2026-01-21  

## Table of Contents

- [Executive Summary](#executive-summary)
- [Architecture Overview](#architecture-overview)
- [Design Decisions](#design-decisions)
- [Implementation Phases](#implementation-phases)
- [Directory Structure](#directory-structure)
- [API Design](#api-design)
- [Observability](#observability)
- [Deployment](#deployment)

## Related Documents

- [Implementation Tasks](./device-api-server-tasks.md) - Detailed task breakdown
- [NVML Fallback Provider](./nvml-fallback-provider.md) - Built-in NVML health provider design

---

## Executive Summary

The Device API Server is a **node-local gRPC cache server** deployed as a Kubernetes DaemonSet. It acts as an intermediary between:

- **Providers** (e.g., NVSentinel health monitors) that update GPU device states
- **Consumers** (e.g., Device Plugins, DRA Drivers) that read device states for scheduling decisions

### Key Requirements

| Requirement | Description |
|-------------|-------------|
| Node-local | DaemonSet running on each GPU node |
| Read-blocking semantics | MUST block reads during provider updates to prevent stale data |
| Multiple providers | Support multiple health monitors updating different conditions |
| Multiple consumers | Support multiple readers (device-plugin, DRA driver, etc.) |
| Kubernetes patterns | klog/v2, structured logging, health probes |
| Helm-only deployment | No kustomize, pure Helm chart |
| Observability | Prometheus metrics, alerting rules |

---

## Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                              Kubernetes Node                                     │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  ┌──────────────────────┐                     ┌──────────────────────────────┐  │
│  │     NVSentinel       │                     │    Device Plugin / DRA       │  │
│  │   (Health Monitor)   │                     │         Driver               │  │
│  │      [Provider]      │                     │        [Consumer]            │  │
│  └──────────┬───────────┘                     └──────────────┬───────────────┘  │
│             │                                                 │                  │
│             │ UpdateGpuStatus()                               │ GetGpu()         │
│             │ (gRPC)                                          │ ListGpus()       │
│             │                                                 │ WatchGpus()      │
│             ▼                                                 ▼                  │
│  ┌──────────────────────────────────────────────────────────────────────────┐   │
│  │                        Device API Server (DaemonSet)                      │   │
│  │  ┌────────────────────────────────────────────────────────────────────┐  │   │
│  │  │                         gRPC Server                                 │  │   │
│  │  │  ┌────────────────────────────────────────────────────────────┐   │  │   │
│  │  │  │                  GpuService (Unified)                      │   │  │   │
│  │  │  │   Write: CreateGpu, UpdateGpu, UpdateGpuStatus, DeleteGpu  │   │  │   │
│  │  │  │   Read:  GetGpu, ListGpus, WatchGpus                       │   │  │   │
│  │  │  └────────────────────────────────┬───────────────────────────┘   │  │   │
│  │  │                                    │                               │  │   │
│  │  │                                    ▼                               │  │   │
│  │  │  ┌─────────────────────────────────────────────────────────────┐  │  │   │
│  │  │  │                    Cache Layer                               │  │  │   │
│  │  │  │  ┌───────────────────────────────────────────────────────┐  │  │  │   │
│  │  │  │  │              sync.RWMutex (Writer-Preference)         │  │  │  │   │
│  │  │  │  │                                                       │  │  │   │   │
│  │  │  │  │   Write Lock() ──────────► Blocks ALL new RLock()     │  │  │  │   │
│  │  │  │  │                            until write completes      │  │  │  │   │
│  │  │  │  │                                                       │  │  │  │   │
│  │  │  │  │   This ensures consumers NEVER read stale data when   │  │  │  │   │
│  │  │  │  │   a provider is updating (healthy → unhealthy)        │  │  │  │   │
│  │  │  │  └───────────────────────────────────────────────────────┘  │  │  │   │
│  │  │  │                                                              │  │  │   │
│  │  │  │  ┌───────────────────────────────────────────────────────┐  │  │  │   │
│  │  │  │  │              map[string]*Gpu (In-Memory Store)        │  │  │  │   │
│  │  │  │  └───────────────────────────────────────────────────────┘  │  │  │   │
│  │  │  └─────────────────────────────────────────────────────────────┘  │  │   │
│  │  │                                                                    │  │   │
│  │  │  ┌─────────────────────────────────────────────────────────────┐  │  │   │
│  │  │  │                    Watch Broadcaster                         │  │  │   │
│  │  │  │  Notifies all WatchGpus() streams on state changes          │  │  │   │
│  │  │  └─────────────────────────────────────────────────────────────┘  │  │   │
│  │  └────────────────────────────────────────────────────────────────────┘  │   │
│  │                                                                           │   │
│  │  ┌─────────────┐  ┌─────────────┐  ┌─────────────────────────────────┐   │   │
│  │  │ Health      │  │ Metrics     │  │ Unix Socket                     │   │   │
│  │  │ :8081       │  │ :9090       │  │ /var/run/device-api/device.sock │   │   │
│  │  │ /healthz    │  │ /metrics    │  │ (node-local gRPC)               │   │   │
│  │  │ /readyz     │  │             │  │                                 │   │   │
│  │  └─────────────┘  └─────────────┘  └─────────────────────────────────┘   │   │
│  └──────────────────────────────────────────────────────────────────────────┘   │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

### Data Flow: Read-Blocking Semantics

```
Timeline ──────────────────────────────────────────────────────────────────────────►

Provider (NVSentinel)           Cache (RWMutex)              Consumer (Device Plugin)
        │                              │                              │
        │                              │◄──── RLock() ────────────────┤ GetGpu()
        │                              │      (allowed)               │
        │                              │──────────────────────────────►│ Returns data
        │                              │      RUnlock()               │
        │                              │                              │
        │──── UpdateGpuStatus() ──────►│                              │
        │     Lock() requested         │                              │
        │                              │                              │
        │                              │◄──── RLock() ────────────────┤ GetGpu()
        │                              │      BLOCKED ⛔               │ (waits)
        │                              │                              │
        │◄──── Lock() acquired ────────│                              │
        │      (write in progress)     │                              │
        │                              │                              │
        │──── Update complete ────────►│                              │
        │      Unlock()                │                              │
        │                              │                              │
        │                              │──── RLock() allowed ─────────►│
        │                              │     (fresh data)             │
        │                              │                              │

⚠️  CRITICAL: Consumer NEVER reads stale "healthy" state when provider
    is updating to "unhealthy". The RWMutex writer-preference ensures
    new readers block once a write is pending.
```

---

## Design Decisions

### D1: Read-Blocking vs Eventually Consistent

| Option | Pros | Cons | Decision |
|--------|------|------|----------|
| **sync.RWMutex (writer-preference)** | Prevents stale reads; simple; Go-native | Readers blocked during writes | ✅ **Selected** |
| atomic.Value + copy-on-write | Never blocks readers | Readers may see stale data during update | ❌ Rejected |
| sync.Map | Good for read-heavy | No blocking semantics; may read stale | ❌ Rejected |

**Rationale**: The requirement explicitly states "MUST block reads, preventing false positives when a node 'was' healthy, and the next state is unhealthy." This mandates write-blocking reads.

### D2: Transport Protocol

| Option | Pros | Cons | Decision |
|--------|------|------|----------|
| **Unix Socket** | Node-local only; no network exposure; fast | Pod must mount socket path | ✅ **Primary** |
| TCP localhost | Easy client setup | Requires port allocation | ✅ **Secondary** |
| hostNetwork + TCP | Accessible from host | Security risk | ❌ Rejected |

**Rationale**: Unix socket provides security isolation and performance for node-local communication. TCP fallback for flexibility.

### D3: Provider Registration Model

| Option | Pros | Cons | Decision |
|--------|------|------|----------|
| **Implicit (any caller can update)** | Simple; stateless server | No provider identity tracking | ✅ **Phase 1** |
| Explicit registration | Track providers; detect failures | More complexity | 🔮 **Phase 2** |

### D4: Logging Framework

| Option | Pros | Cons | Decision |
|--------|------|------|----------|
| **klog/v2** | Kubernetes native; contextual logging; JSON format | Slightly verbose API | ✅ **Selected** |
| zap | Fast; popular | Not Kubernetes native | ❌ Rejected |
| logr | Interface-based | Needs backend anyway | Used via klog |

---

## Implementation Phases

### Phase 1: Core Server Foundation

**Goal**: Minimal viable gRPC server with cache and blocking semantics.

| Task ID | Task | Description | Estimate |
|---------|------|-------------|----------|
| P1.1 | Project scaffolding | Create `cmd/device-api-server/`, `internal/` structure | S |
| P1.2 | Proto extensions | Add provider-side RPCs (UpdateGpuStatus, RegisterGpu, UnregisterGpu) | M |
| P1.3 | Cache implementation | Thread-safe cache with RWMutex, writer-preference blocking | M |
| P1.4 | Consumer gRPC service | Implement GetGpu, ListGpus, WatchGpus (read path) | M |
| P1.5 | Provider gRPC service | Implement UpdateGpuStatus, RegisterGpu, UnregisterGpu (write path) | M |
| P1.6 | Watch broadcaster | Fan-out changes to all active WatchGpus streams | M |
| P1.7 | Graceful shutdown | SIGTERM handling, drain connections, health status | S |
| P1.8 | Unit tests | Cache tests, service tests, blocking behavior tests | L |

**Deliverables**:
- Working gRPC server binary
- Consumer and Provider services
- Basic health endpoint

---

### Phase 2: Kubernetes Integration

**Goal**: Production-ready DaemonSet with proper k8s integration.

| Task ID | Task | Description | Estimate |
|---------|------|-------------|----------|
| P2.1 | klog/v2 integration | Structured logging, contextual loggers, log levels | M |
| P2.2 | Health probes | gRPC health protocol, HTTP /healthz /readyz endpoints | M |
| P2.3 | Configuration | Flags, environment variables, config validation | S |
| P2.4 | Unix socket support | Listen on configurable socket path | S |
| P2.5 | Signal handling | Proper SIGTERM/SIGINT handling per k8s lifecycle | S |
| P2.6 | Integration tests | Test with mock providers/consumers | L |

**Deliverables**:
- Kubernetes-ready binary
- Health endpoints
- Configurable via flags/env

---

### Phase 3: Observability

**Goal**: Full observability stack with metrics and alerts.

| Task ID | Task | Description | Estimate |
|---------|------|-------------|----------|
| P3.1 | Prometheus metrics | Request counts, latencies, cache stats, connection counts | M |
| P3.2 | gRPC interceptors | grpc-prometheus interceptors for all RPCs | M |
| P3.3 | Custom metrics | `device_api_server_gpus_total`, `_unhealthy`, `_cache_*` | M |
| P3.4 | Metrics endpoint | HTTP /metrics on separate port | S |
| P3.5 | Alerting rules | PrometheusRule CRD for critical alerts | M |
| P3.6 | Grafana dashboard | JSON dashboard for visualization | M |

**Metrics to implement**:

```
# Server metrics
device_api_server_info{version="...", go_version="..."}
device_api_server_up

# Cache metrics  
device_api_server_cache_gpus_total
device_api_server_cache_gpus_healthy
device_api_server_cache_gpus_unhealthy
device_api_server_cache_updates_total{provider="..."}
device_api_server_cache_lock_wait_seconds_bucket

# gRPC metrics (via interceptor)
grpc_server_started_total{grpc_service, grpc_method}
grpc_server_handled_total{grpc_service, grpc_method, grpc_code}
grpc_server_handling_seconds_bucket{grpc_service, grpc_method}

# Watch metrics
device_api_server_watch_streams_active
device_api_server_watch_events_total{type="ADDED|MODIFIED|DELETED"}
```

**Alerts**:

```yaml
- alert: DeviceAPIServerDown
  expr: up{job="device-api-server"} == 0
  for: 5m
  
- alert: DeviceAPIServerHighLatency  
  expr: histogram_quantile(0.99, grpc_server_handling_seconds_bucket) > 0.5
  for: 5m
  
- alert: DeviceAPIServerUnhealthyGPUs
  expr: device_api_server_cache_gpus_unhealthy > 0
  for: 1m
```

---

### Phase 4: Helm Chart

**Goal**: Production-ready Helm chart with all configurations.

| Task ID | Task | Description | Estimate |
|---------|------|-------------|----------|
| P4.1 | Chart scaffolding | `charts/device-api-server/` structure | S |
| P4.2 | DaemonSet template | Node selector, tolerations, resource limits | M |
| P4.3 | RBAC templates | ServiceAccount, Role, RoleBinding | M |
| P4.4 | ConfigMap/Secret | Server configuration, TLS certs | M |
| P4.5 | Service templates | Headless service, metrics service | S |
| P4.6 | PrometheusRule | Alerting rules as k8s resource | M |
| P4.7 | ServiceMonitor | Prometheus scrape configuration | S |
| P4.8 | Values schema | JSON schema for values validation | M |
| P4.9 | Chart tests | Helm test hooks | M |
| P4.10 | Documentation | README, NOTES.txt, examples | M |

**Chart Structure**:

```
charts/device-api-server/
├── Chart.yaml
├── values.yaml
├── values.schema.json
├── README.md
├── templates/
│   ├── _helpers.tpl
│   ├── daemonset.yaml
│   ├── serviceaccount.yaml
│   ├── role.yaml
│   ├── rolebinding.yaml
│   ├── configmap.yaml
│   ├── service.yaml
│   ├── service-metrics.yaml
│   ├── servicemonitor.yaml
│   ├── prometheusrule.yaml
│   ├── poddisruptionbudget.yaml
│   └── NOTES.txt
└── tests/
    └── test-connection.yaml
```

---

### Phase 5: Documentation & Polish

**Goal**: Comprehensive documentation and production hardening.

| Task ID | Task | Description | Estimate |
|---------|------|-------------|----------|
| P5.1 | Architecture docs | Design document, diagrams | M |
| P5.2 | API reference | Proto documentation, examples | M |
| P5.3 | Operations guide | Deployment, troubleshooting, runbooks | L |
| P5.4 | Developer guide | Contributing, local development | M |
| P5.5 | Security hardening | TLS, authentication review | M |
| P5.6 | Performance testing | Benchmark under load | L |
| P5.7 | CI/CD pipeline | GitHub Actions for build, test, release | M |

---

## Directory Structure

Following the [kubernetes-sigs/node-feature-discovery](https://github.com/kubernetes-sigs/node-feature-discovery) pattern
where the `api/` is a standalone module and `pkg/` contains public library code:

```
NVSentinel/
├── api/                                   # STANDALONE API MODULE (own go.mod)
│   ├── gen/go/device/v1alpha1/            # Generated Go code
│   │   ├── gpu.pb.go
│   │   └── gpu_grpc.pb.go
│   ├── proto/device/v1alpha1/             # Proto definitions
│   │   └── gpu.proto                      # Unified GpuService (CRUD operations)
│   ├── go.mod                             # module github.com/nvidia/nvsentinel/api
│   ├── go.sum
│   └── Makefile
├── cmd/                                   # Command entry points (thin)
│   └── device-api-server/
│       └── main.go                        # Server entrypoint only
├── pkg/                                   # PUBLIC LIBRARY CODE (importable)
│   ├── deviceapiserver/                   # Device API Server implementation
│   │   ├── cache/                         # Thread-safe GPU cache
│   │   │   ├── cache.go
│   │   │   ├── cache_test.go
│   │   │   └── broadcaster.go
│   │   ├── service/                       # gRPC service implementation
│   │   │   └── gpu_service.go             # GpuService (unified read/write)
│   │   ├── nvml/                          # NVML provider (uses gRPC client)
│   │   │   ├── provider.go
│   │   │   ├── enumerator.go
│   │   │   └── health_monitor.go
│   │   ├── metrics/                       # Prometheus metrics
│   │   └── health/                        # Health check handlers
│   ├── version/                           # Version information
│   │   └── version.go
│   └── signals/                           # Signal handling utilities
├── charts/                                # Helm charts
│   └── device-api-server/
│       ├── Chart.yaml
│       ├── values.yaml
│       └── templates/
├── docs/
│   ├── design/
│   ├── api/
│   └── operations/
├── hack/                                  # Build/development scripts
├── test/                                  # E2E tests
├── go.mod                                 # Root module with replace directive
├── go.sum
└── Makefile
```

**Key Layout Decisions:**

| Directory | Purpose | Importable |
|-----------|---------|------------|
| `api/` | Standalone API module for versioning | Yes (own module) |
| `pkg/` | Public library code | Yes |
| `cmd/` | Thin entry points | No |
| `charts/` | Helm deployment | N/A |

Root `go.mod` uses: `replace github.com/nvidia/nvsentinel/api => ./api`

---

## API Design

### Unified GpuService

Following Kubernetes API conventions, the API is consolidated into a single `GpuService` with standard CRUD methods:

```protobuf
// GpuService provides a unified API for managing GPU resources.
//
// Read operations (Get, List, Watch) are intended for consumers.
// Write operations (Create, Update, UpdateStatus, Delete) are intended for providers.
service GpuService {
  // Read Operations
  rpc GetGpu(GetGpuRequest) returns (Gpu);
  rpc ListGpus(ListGpusRequest) returns (ListGpusResponse);
  rpc WatchGpus(WatchGpusRequest) returns (stream WatchGpusResponse);

  // Write Operations
  rpc CreateGpu(CreateGpuRequest) returns (CreateGpuResponse);
  rpc UpdateGpu(UpdateGpuRequest) returns (Gpu);
  rpc UpdateGpuStatus(UpdateGpuStatusRequest) returns (Gpu);
  rpc DeleteGpu(DeleteGpuRequest) returns (google.protobuf.Empty);
}

message CreateGpuRequest {
  Gpu gpu = 1;  // metadata.name and spec.uuid required
}

message CreateGpuResponse {
  Gpu gpu = 1;
  bool created = 2;  // true if new, false if already existed
}

message UpdateGpuRequest {
  Gpu gpu = 1;  // includes resource_version for optimistic concurrency
}

message UpdateGpuStatusRequest {
  string name = 1;
  GpuStatus status = 2;
  int64 resource_version = 3;  // optional, for conflict detection
}

message DeleteGpuRequest {
  string name = 1;
}
```

**Design Rationale**:
- Single service simplifies API surface and tooling compatibility
- Standard CRUD verbs enable better integration with Kubernetes patterns
- `UpdateGpuStatus` follows the Kubernetes subresource pattern
- Optimistic concurrency via `resource_version` prevents lost updates

---

## Observability

### Metrics Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Device API Server                             │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                   gRPC Interceptors                      │    │
│  │  grpc_server_started_total                               │    │
│  │  grpc_server_handled_total                               │    │
│  │  grpc_server_handling_seconds_bucket                     │    │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                   Custom Metrics                         │    │
│  │  device_api_server_cache_gpus_total                      │    │
│  │  device_api_server_cache_lock_contention_total           │    │
│  │  device_api_server_watch_streams_active                  │    │
│  └─────────────────────────────────────────────────────────┘    │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────┐    │
│  │                   Go Runtime Metrics                     │    │
│  │  go_goroutines                                           │    │
│  │  go_memstats_alloc_bytes                                 │    │
│  │  process_cpu_seconds_total                               │    │
│  └─────────────────────────────────────────────────────────┘    │
│                              │                                   │
│                              ▼                                   │
│                    :9090/metrics                                 │
│                              │                                   │
└──────────────────────────────┼───────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│                       Prometheus                                 │
│                                                                  │
│  ServiceMonitor ──► scrape_configs                               │
│                                                                  │
│  PrometheusRule ──► alerting_rules                               │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
                               │
                               ▼
┌─────────────────────────────────────────────────────────────────┐
│                        Grafana                                   │
│                                                                  │
│  Dashboard: Device API Server Overview                           │
│  - Request rate / error rate                                     │
│  - P50/P99 latency                                               │
│  - GPU health summary                                            │
│  - Cache statistics                                              │
│  - Active watch streams                                          │
│                                                                  │
└─────────────────────────────────────────────────────────────────┘
```

---

## Deployment

### Helm Values (Key Configuration)

```yaml
# values.yaml
replicaCount: 1  # DaemonSet ignores this, but kept for consistency

image:
  repository: ghcr.io/nvidia/device-api-server
  tag: ""  # Defaults to Chart appVersion
  pullPolicy: IfNotPresent

# Server configuration
server:
  # gRPC listen address (TCP) - localhost only by default for security
  # Set to ":50051" to bind to all interfaces (WARNING: unauthenticated API)
  grpcAddress: "127.0.0.1:50051"
  # Unix socket path (primary for node-local)
  unixSocket: /var/run/device-api/device.sock
  # Health probe port
  healthPort: 8081
  # Metrics port
  metricsPort: 9090

# Logging
logging:
  # Log level (0=info, higher=more verbose)
  verbosity: 0
  # Output format: text, json
  format: json

# Node selection
nodeSelector:
  nvidia.com/gpu.present: "true"

tolerations:
  - key: nvidia.com/gpu
    operator: Exists
    effect: NoSchedule

resources:
  requests:
    cpu: 50m
    memory: 64Mi
  limits:
    cpu: 200m
    memory: 256Mi

# Security
securityContext:
  runAsNonRoot: true
  runAsUser: 65534
  readOnlyRootFilesystem: true
  allowPrivilegeEscalation: false

# RBAC
serviceAccount:
  create: true
  name: ""
  automountServiceAccountToken: false

rbac:
  create: true

# Observability
metrics:
  enabled: true
  serviceMonitor:
    enabled: true
    interval: 30s
    scrapeTimeout: 10s
  prometheusRule:
    enabled: true

# Health probes
probes:
  liveness:
    initialDelaySeconds: 5
    periodSeconds: 10
  readiness:
    initialDelaySeconds: 5
    periodSeconds: 10
```

### DaemonSet Topology

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                           Kubernetes Cluster                                     │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                  │
│  ┌───────────────────────┐  ┌───────────────────────┐  ┌───────────────────────┐│
│  │      GPU Node 1       │  │      GPU Node 2       │  │      GPU Node 3       ││
│  │                       │  │                       │  │                       ││
│  │  ┌─────────────────┐  │  │  ┌─────────────────┐  │  │  ┌─────────────────┐  ││
│  │  │ device-api-     │  │  │  │ device-api-     │  │  │  │ device-api-     │  ││
│  │  │ server pod      │  │  │  │ server pod      │  │  │  │ server pod      │  ││
│  │  │                 │  │  │  │                 │  │  │  │                 │  ││
│  │  │ GPU-0: Healthy  │  │  │  │ GPU-0: Healthy  │  │  │  │ GPU-0: Unhealthy│  ││
│  │  │ GPU-1: Healthy  │  │  │  │ GPU-1: Healthy  │  │  │  │ GPU-1: Healthy  │  ││
│  │  │ GPU-2: Healthy  │  │  │  │                 │  │  │  │ GPU-2: Healthy  │  ││
│  │  │ GPU-3: Healthy  │  │  │  │                 │  │  │  │ GPU-3: Healthy  │  ││
│  │  └─────────────────┘  │  │  └─────────────────┘  │  │  └─────────────────┘  ││
│  │                       │  │                       │  │                       ││
│  │  /var/run/device-api/ │  │  /var/run/device-api/ │  │  /var/run/device-api/ ││
│  │    device.sock        │  │    device.sock        │  │    device.sock        ││
│  │                       │  │                       │  │                       ││
│  └───────────────────────┘  └───────────────────────┘  └───────────────────────┘│
│                                                                                  │
│  ┌───────────────────────┐                                                       │
│  │   Non-GPU Node        │  (DaemonSet does NOT schedule here due to            │
│  │   (No GPU)            │   nodeSelector: nvidia.com/gpu.present=true)         │
│  └───────────────────────┘                                                       │
│                                                                                  │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Risk Assessment

| Risk | Impact | Likelihood | Mitigation |
|------|--------|------------|------------|
| Cache corruption on concurrent writes | High | Low | RWMutex provides exclusivity |
| Watch stream memory leak | Medium | Medium | Bounded channels, timeouts |
| Provider not updating (stale data) | High | Medium | Health checks, provider heartbeat (Phase 2) |
| Socket permission issues | Medium | Medium | Init container for socket dir |
| High lock contention | Medium | Low | Metrics to detect, sharding if needed |

---

## Success Criteria

### Phase 1
- [ ] Server starts and accepts gRPC connections
- [ ] Provider can register/update/unregister GPUs
- [ ] Consumer can Get/List/Watch GPUs
- [ ] Read-blocking verified under concurrent load

### Phase 2
- [ ] Structured logs with klog/v2
- [ ] Health probes pass in Kubernetes
- [ ] Unix socket communication works

### Phase 3
- [ ] Prometheus metrics exposed
- [ ] Grafana dashboard visualizes key metrics
- [ ] Alerts fire correctly in test scenarios

### Phase 4
- [ ] `helm install` works out of box
- [ ] DaemonSet schedules on GPU nodes only
- [ ] RBAC properly scoped

### Phase 5
- [ ] Documentation complete
- [ ] CI/CD pipeline green
- [ ] Performance benchmarks pass

---

## Appendix: Research References

1. **Kubernetes DaemonSet gRPC Best Practices** - Health probes, graceful shutdown, load balancing
2. **Go sync.RWMutex** - Writer-preference semantics, blocking behavior
3. **klog/v2** - Structured logging, contextual logging, JSON format
4. **Helm Chart Best Practices** - RBAC, ServiceAccount, DaemonSet templates
5. **grpc-prometheus** - Metrics interceptors, histogram configuration

---

*Document version: 1.0*  
*Last updated: 2026-01-21*
