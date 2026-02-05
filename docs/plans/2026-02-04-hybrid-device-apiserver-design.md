# Hybrid Device API Server Design

**Date:** 2026-02-04
**Status:** Approved for implementation
**Base:** PR #718 (merged) + PR #720 (to cherry-pick)
**Authors:** @pteranodan (PR #718), @ArangoGutierrez (PR #720)

---

## Executive Summary

This design combines PR #718's architectural foundation with PR #720's runtime components, replacing Kine/SQLite persistent storage with an in-memory cache. GPU state is ephemeral by design—providers are the source of truth.

**Key decisions:**
1. **In-memory cache** - No persistence. On restart, providers re-register GPUs.
2. **Readiness gate** - Server blocks consumer requests until providers register (Option A).
3. **Memory bounds** - Sized for Vera Rubin Ultra NVL576 (576 GPUs, 2027).
4. **Preserve authorship** - All commits include `Co-authored-by` for both authors.

---

## Why In-Memory (Not Persistent)

Persistent storage is harmful for GPU status because:

1. **Stale data is dangerous** - After restart, serving cached "Ready: True" when GPU is actually bad could cause workload failures.
2. **Providers are source of truth** - NVML, DCGM, NVSentinel health monitors own GPU state.
3. **Cleaner recovery** - Restart → empty → providers re-register → fresh state.
4. **Simpler failure modes** - No SQLite corruption, no compaction, no "database locked."

---

## Architecture

```
┌─────────────────────────────────────────────────────────────────┐
│                    Device API Server                             │
│                                                                  │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │                    gRPC Server                              ││
│  │  ┌─────────────┐  ┌─────────────┐  ┌──────────────────┐    ││
│  │  │ GpuService  │  │HealthProbe │  │ Metrics/Reflect  │    ││
│  │  └──────┬──────┘  └──────┬──────┘  └──────────────────┘    ││
│  └─────────┼────────────────┼──────────────────────────────────┘│
│            │                │                                    │
│            ▼                ▼                                    │
│  ┌─────────────────────────────────────────────────────────────┐│
│  │                  In-Memory Cache                            ││
│  │  ┌──────────────┐  ┌──────────────┐  ┌─────────────────┐   ││
│  │  │ GPU Registry │  │  Broadcaster │  │ Readiness Gate  │   ││
│  │  │  (RWMutex)   │  │   (Watch)    │  │ (provider count)│   ││
│  │  └──────────────┘  └──────────────┘  └─────────────────┘   ││
│  └─────────────────────────────────────────────────────────────┘│
│                              ▲                                   │
│              ┌───────────────┼───────────────┐                  │
│              │               │               │                  │
│  ┌───────────┴───┐  ┌───────┴───────┐  ┌────┴────────┐        │
│  │ NVML Provider │  │ External Prov │  │ Future Prov │        │
│  │  (built-in)   │  │ (NVSentinel)  │  │   (DCGM)    │        │
│  └───────────────┘  └───────────────┘  └─────────────┘        │
└─────────────────────────────────────────────────────────────────┘
```

---

## Memory Safety Bounds

Based on research of NVIDIA architectures:

| System | Year | GPUs per Unit |
|--------|------|---------------|
| DGX H100 | 2022 | 8 |
| DGX B200 | 2024 | 8 |
| GB200 NVL72 | 2024 | 72 |
| Vera Rubin NVL72 | 2026 | 72 |
| Vera Rubin NVL144 | 2026 | 144 |
| Vera Rubin Ultra NVL576 | 2027 | **576** |

### CacheLimits Configuration

```go
type CacheLimits struct {
    // GPU registration limits
    MaxGPUs              int  // Default: 1024 (covers NVL576 + headroom)
    MaxConditionsPerGPU  int  // Default: 32
    MaxLabelsPerGPU      int  // Default: 64
    MaxGPUObjectBytes    int  // Default: 64KB per GPU object

    // Watch subscriber limits
    MaxSubscribers       int           // Default: 256
    SubscriberRateLimit  rate.Limit    // Default: 10 subscribes/sec
    SubscriberBurst      int           // Default: 20

    // Per-subscriber limits
    EventBufferSize      int           // Default: 256 events
    SlowSubscriberTimeout time.Duration // Default: 30s, then disconnect
}

func DefaultCacheLimits() CacheLimits {
    return CacheLimits{
        MaxGPUs:              1024,
        MaxConditionsPerGPU:  32,
        MaxLabelsPerGPU:      64,
        MaxGPUObjectBytes:    64 * 1024,
        MaxSubscribers:       256,
        SubscriberRateLimit:  rate.Limit(10),
        SubscriberBurst:      20,
        EventBufferSize:      256,
        SlowSubscriberTimeout: 30 * time.Second,
    }
}
```

### Enforcement Points

| Limit | Enforced At | Behavior on Exceed |
|-------|-------------|-------------------|
| `MaxGPUs` | `CreateGpu()` | Return `ResourceExhausted` |
| `MaxConditionsPerGPU` | `UpdateGpuStatus()` | Truncate + log warning |
| `MaxGPUObjectBytes` | All writes | Return `InvalidArgument` |
| `MaxSubscribers` | `Subscribe()` | Return `ResourceExhausted` |
| `SubscriberRateLimit` | `Subscribe()` | Return `ResourceExhausted` with retry-after |
| `SlowSubscriberTimeout` | Broadcaster goroutine | Disconnect + log |

### Memory Budget at Max Scale (NVL576)

| Component | Calculation | Max Memory |
|-----------|-------------|------------|
| GPU objects | 576 GPUs × 64KB | ~37 MB |
| Watch buffers | 256 subscribers × 256 events × ~1KB | ~65 MB |
| Overhead | ~20% | ~20 MB |
| **Total** | | **~122 MB** |

Container limit of 256MB provides 2x headroom.

---

## Readiness Gate

Server blocks consumer requests until providers register GPUs.

### Startup Sequence

```
STARTING → WAITING_FOR_PROVIDERS → READY → SERVING
    │              │                  │         │
  gRPC up      Health:            Health:   Health:
  Cache empty  NOT_SERVING        SERVING   SERVING
```

### Readiness Conditions

```go
type ReadinessGate struct {
    mu sync.RWMutex

    minGPUs          int           // Default: 1
    providerTimeout  time.Duration // Default: 30s

    ready            bool
    readySince       time.Time
    registeredGPUs   int
    activeProviders  map[string]providerState
}

func (r *ReadinessGate) checkReadiness() {
    // Condition 1: At least minGPUs registered
    if r.registeredGPUs >= r.minGPUs {
        r.markReady("gpu_threshold_met")
        return
    }

    // Condition 2: Timeout expired (node might have no GPUs)
    if time.Since(r.startTime) > r.providerTimeout {
        r.markReady("provider_timeout")
        return
    }
}
```

### Health Probe Behavior

| Endpoint | Before Ready | After Ready |
|----------|--------------|-------------|
| `/healthz` (liveness) | `200 OK` | `200 OK` |
| `/readyz` (readiness) | `503 Service Unavailable` | `200 OK` |
| gRPC `Health/Check` | `NOT_SERVING` | `SERVING` |

### Interceptor

```go
func (s *Server) readinessInterceptor(ctx context.Context, req interface{},
    info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {

    // Allow provider writes always
    if isProviderMethod(info.FullMethod) {
        return handler(ctx, req)
    }

    // Block consumer reads until ready
    if !s.readinessGate.IsReady() {
        return nil, status.Error(codes.Unavailable,
            "server initializing, waiting for GPU providers")
    }

    return handler(ctx, req)
}
```

---

## Watch Broadcaster

Non-blocking fan-out to multiple subscribers with slow subscriber eviction.

```go
type Broadcaster struct {
    mu          sync.RWMutex
    subscribers map[string]*subscriber
    limits      CacheLimits
    logger      klog.Logger
    metrics     *BroadcasterMetrics
    subscribeLimiter *rate.Limiter
}

type subscriber struct {
    id        string
    channel   chan WatchEvent
    createdAt time.Time
    lastSend  time.Time
    dropCount int64
}

// Non-blocking broadcast - drops events for slow subscribers
func (b *Broadcaster) Broadcast(event WatchEvent) {
    b.mu.RLock()
    defer b.mu.RUnlock()

    for id, sub := range b.subscribers {
        select {
        case sub.channel <- event:
            sub.lastSend = time.Now()
        default:
            atomic.AddInt64(&sub.dropCount, 1)
            b.metrics.EventsDropped.WithLabelValues(id).Inc()
        }
    }
}

// Periodic eviction of stuck subscribers
func (b *Broadcaster) evictSlowSubscribers(ctx context.Context) {
    ticker := time.NewTicker(10 * time.Second)
    defer ticker.Stop()

    for {
        select {
        case <-ctx.Done():
            return
        case <-ticker.C:
            b.mu.Lock()
            now := time.Now()
            for id, sub := range b.subscribers {
                if now.Sub(sub.lastSend) > b.limits.SlowSubscriberTimeout {
                    close(sub.channel)
                    delete(b.subscribers, id)
                    b.metrics.SubscribersEvicted.Inc()
                }
            }
            b.mu.Unlock()
        }
    }
}
```

---

## What to Take from Each PR

### From PR #718 (pteranodan) ✅

- Single unified Go module
- `internal/generated/` proto path
- Options → Config → CompletedConfig pattern
- `pkg/client-go/` generated clients
- Service registry pattern
- `errgroup` startup/shutdown
- Examples

### From PR #720 (ArangoGutierrez) ✅

- In-memory cache with RWMutex
- Watch Broadcaster
- NVML Provider
- XID health monitoring
- Helm chart
- Dockerfile (multi-stage)

### Remove ❌

- Kine/SQLite storage (from #718)
- Multi-module layout (from #720)
- `api/gen/go/` proto path (from #720)

### New Components (Neither PR)

- CacheLimits with memory bounds
- ReadinessGate
- Slow subscriber eviction
- Provider heartbeat (optional)

---

## Package Structure

```
github.com/nvidia/nvsentinel/
├── api/
│   ├── device/v1alpha1/        # Go types
│   └── proto/                  # Proto definitions
├── cmd/
│   ├── device-apiserver/       # Main binary
│   └── nvml-provider/          # NVML sidecar
├── internal/
│   └── generated/              # Proto output
├── pkg/
│   ├── apiserver/
│   │   ├── cache/              # In-memory cache
│   │   ├── broadcaster/        # Watch fan-out
│   │   ├── config/             # Options pattern
│   │   ├── metrics/            # Prometheus
│   │   ├── registry/           # Service registry
│   │   └── server.go           # Main server
│   ├── client-go/              # Generated clients
│   ├── grpc/client/            # Client helpers
│   └── nvml/                   # NVML provider
├── deployments/
│   ├── helm/                   # Helm chart
│   └── container/              # Dockerfile
└── examples/
```

---

## Metrics

```
# Cache operations
device_api_cache_gpus_current
device_api_cache_bytes_current
device_api_cache_rejected_total{reason="max_gpus_exceeded|object_too_large"}
device_api_cache_truncations_total{field="conditions"}

# Broadcaster
device_api_broadcaster_subscribers_current
device_api_broadcaster_subscribers_evicted_total
device_api_broadcaster_subscriptions_rejected_total{reason="rate_limited|max_exceeded"}
device_api_broadcaster_events_sent_total
device_api_broadcaster_events_dropped_total{subscriber="..."}

# Readiness
device_api_readiness_state{state="waiting|ready"}
device_api_readiness_transition_timestamp
```

---

## Implementation Phases

### Phase 1: Storage Swap
- Remove Kine/SQLite dependencies from #718
- Add in-memory cache (from #720)
- Add CacheLimits
- Add Broadcaster
- Wire cache into server

### Phase 2: Readiness Gate
- Add ReadinessGate
- Add readiness interceptor
- Update health probes
- Add provider timeout fallback

### Phase 3: NVML Provider
- Port `pkg/nvml/` from #720
- Port XID health monitoring
- Integrate with cache
- Add build tags for CGO

### Phase 4: Deployment
- Port Helm chart from #720
- Port Dockerfile
- Update CI workflow
- Add integration tests

### Phase 5: Documentation
- Update README
- Add architecture doc
- Add operations guide

---

## Authorship Requirements

**CRITICAL:** Every commit MUST include both authors:

```
feat: implement in-memory GPU cache with memory bounds

Co-authored-by: Carlos Eduardo Arango Gutierrez <carangog@redhat.com>
Co-authored-by: Dan Stone <danstone@nvidia.com>
```

### PR Description Template

```markdown
## Summary

Hybrid Device API Server combining #718 and #720.

**Key changes from original PRs:**
- Replaced Kine/SQLite with in-memory cache
- Added memory safety bounds (MaxGPUs: 1024 for Rubin Ultra NVL576)
- Added readiness gate (blocks until providers register)

## Attribution

This work is based on contributions from:
- @pteranodan - PR #718: Core architecture, client generation, options pattern
- @ArangoGutierrez - PR #720: In-memory cache, NVML provider, Helm chart

All commits include `Co-authored-by` trailers.

## References
- Closes #720 (PR #718 already merged)
```

---

## Quick Reference for Next Session

1. **PR #718 is merged** - Base is ready
2. **Rebase #720 on main** - Will have conflicts
3. **Remove from #720:** `api/go.mod`, `client-go/` changes, `api/gen/go/` references
4. **Update imports:** `api/gen/go/` → `internal/generated/`
5. **Add new files:** CacheLimits, ReadinessGate, slow subscriber eviction
6. **Port from #720:** `pkg/deviceapiserver/cache/`, `pkg/deviceapiserver/nvml/`, Helm, Dockerfile
7. **Reconcile protos:** Merge both sets of changes, regenerate
8. **Every commit:** Include both `Co-authored-by` trailers

---

## References

- [NVIDIA GB200 NVL72](https://www.nvidia.com/en-us/data-center/gb200-nvl72/)
- [Vera Rubin Platform - Tom's Hardware](https://www.tomshardware.com/pc-components/gpus/nvidias-vera-rubin-platform-in-depth-inside-nvidias-most-complex-ai-and-hpc-platform-to-date)
- [Rubin Ultra 576 GPUs - TweakTown](https://www.tweaktown.com/news/108238/nvidia-teases-next-gen-kyber-rack-scale-tech-up-to-576-nvidia-rubin-ultra-gpus-in-2027/index.html)
