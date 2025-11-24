# ADR-012: Observability — Health Events Exporter

## Context

NVSentinel stores health events in per-cluster MongoDB. This works well for local operations (fault quarantine, node draining) but creates fleet-wide visibility challenges: isolated data cannot be queried across clusters; Prometheus handles aggregate metrics but not high-cardinality event data (198B+ time series); no centralized event store for detailed search and analysis.

---

## Problem Statement

Health events are trapped in per-cluster MongoDB instances (100s clusters). Operations teams need centralized access for fleet-wide analytics: querying events by GPU serial number, analyzing failure patterns across clusters, investigating timelines before node failures.

**Current:** Each cluster's MongoDB is isolated → no cross-cluster visibility

**Needed:** Export events to centralized event store → enable fleet-wide search and analysis

**Note:** Existing Prometheus/Grafana handle aggregate metrics and alerting. This exporter addresses a different use case: detailed event-level queries that would cause cardinality explosion in Prometheus.

---

## Goals and Non-Goals

### Goals

- Export health events from MongoDB to HTTP sinks in CloudEvents format
- Support OIDC authentication for secure sink communication
- Automatically backfill historical events on first deployment
- Provide operational observability (metrics, logging, health checks)

### Non-Goals

- Building centralized storage or analytics (users provide sink)
- Supporting multiple sinks per exporter instance

---

## Design

### Decision

Implement a new component - health event exporter that continuously exports health events from the in-cluster MongoDB data store to external event stores for centralized analytics and detailed querying. The exporter transforms MongoDB documents into standardized [CloudEvents](https://cloudevents.io/) format and publishes them to HTTP-based sinks with OIDC authentication support.

The exporter is deployed as a single-replica Deployment in the `nvsentinel` namespace, using the existing NVSentinel service account. Configuration is loaded from ConfigMap; sensitive values come from Secrets.

### Health Events

This design focuses on exporting **Health Events** - hardware and cluster health status changes from NVSentinel's health monitors (GPU, Syslog, CSP, K8s Object monitors). Events stored in MongoDB `healthevents` collection; delivery guarantee is at-least-once; use case is centralized analytics and pattern detection.

### Architecture Overview

```
┌─────────────────────────────────────────────────────────────────────────┐
│                     Health Events Exporter Architecture                 │
│                                                                         │
│  ┌────────────────────────────────────────────────────────────────┐     │
│  │                    Exporter Core                               │     │
│  │                                                                │     │
│  │   On First Start: Automatic Backfill (historical events)       │     │
│  │   Then: Streaming (real-time events)                           │     │
│  └────────────────────────────────────────────────────────────────┘     │
│                                                                         │
│  ┌────────────────────────────────────────────────────────────────┐     │
│  │              ChangeStreamWatcher                               │     │
│  │              (Database Agnostic)                               │     │
│  │                                                                │     │
│  │    Start(ctx)                                                  │     │
│  │    Events() <-chan Event                                       │     │
│  │    MarkProcessed(ctx, token) error                             │     │
│  │    Close(ctx) error                                            │     │
│  └────────────────────────────────────────────────────────────────┘     │
│           │                      │                      │               │
│           ↓                      ↓                      ↓               │
│  ┌──────────────┐      ╔══════════════╗      ╔══════════════╗           │
│  │   MongoDB    │      ║  PostgreSQL  ║      ║    Kafka     ║           |
│  │ ChangeStream │      ║    NOTIFY    ║      ║   Consumer   ║           │
│  │ (IMPLEMENTED)│      ║ (extensible) ║      ║ (extensible) ║           │
│  └──────────────┘      ╚══════════════╝      ╚══════════════╝           │
│                                                                         │
│  ┌────────────────────────────────────────────────────────────────┐     │
│  │          EventTransformer (CloudEvents 1.0)                    │     │
│  │                                                                │     │
│  │    Transform(Event) -> CloudEvent                              │     │
│  └────────────────────────────────────────────────────────────────┘     │
│                                                                         │
│  ┌────────────────────────────────────────────────────────────────┐     │
│  │                 EventSink Interface                            │     │
│  │                 (Pluggable Destinations)                       │     │
│  │                                                                │     │
│  │    Publish(event) error                                        │     │
│  └────────────────────────────────────────────────────────────────┘     │
│           │                      │                      │               │
│           ↓                      ↓                      ↓               │
│  ┌──────────────┐      ╔══════════════╗      ╔══════════════╗           │
│  │  HTTP Sink   │      ║  Kafka Sink  ║      ║  gRPC Sink   ║           │
│  │  + OIDC      │      ║  + ACLs      ║      ║  + mTLS      ║           │
│  │(IMPLEMENTED) │      ║ (extensible) ║      ║ (extensible) ║           │
│  └──────────────┘      ╚══════════════╝      ╚══════════════╝           │
│           │                      │                      │               │
│           └──────────────────────┴──────────────────────┘               │
│                                  │                                      │
└──────────────────────────────────┼──────────────────────────────────────┘
                                   │
                                   ↓
         ┌─────────────────────────────────────────────────┐
         │       External Destinations                     │
         │  (Elasticsearch, Kafka, SIEM, Custom APIs, ...) │
         └─────────────────────────────────────────────────┘

Legend:
  Solid boxes (─) = Implemented in this design
  Double boxes (═) = Extensible via interfaces (not implemented)
```

### Component Responsibilities

- **ChangeStream Watcher:** Watches MongoDB `healthevents` collection; reuses existing `store-client` infrastructure and resume token pattern from fault-quarantine
- **CloudEvents Transformer:** Converts health events to CloudEvents 1.0 format; uses cluster name as `source` field
- **HTTP Publisher:** Publishes to HTTP sink with retry logic and OIDC authentication
- **Resume Token Manager:** Tracks last processed position for at-least-once delivery

---

## Implementation Details

### 1. Core Interfaces

The exporter uses database-agnostic interfaces, enabling future migration to different datastores (PostgreSQL, CockroachDB, etc.) without code changes.

#### ChangeStreamWatcher

Database-agnostic interface for streaming events. Currently implemented for MongoDB; extensible to PostgreSQL LISTEN/NOTIFY, Kafka, etc.

```go
type ChangeStreamWatcher interface {
    Start(ctx context.Context)
    Events() <-chan Event
    MarkProcessed(ctx context.Context, token []byte) error
    Close(ctx context.Context) error
}

type Event interface {
    GetDocumentID() (string, error)
    GetNodeName() (string, error)
    GetResumeToken() []byte
    UnmarshalDocument(v interface{}) error
}
```

#### Sink Interface

```go
// EventSink abstracts the destination for exported events
// Implementations: HTTPSink, KafkaSink, GRPCSink, etc.
type EventSink interface {
    // Publish sends a single event to the sink
    Publish(ctx context.Context, event *CloudEvent) error
    
    // Close flushes pending events and releases resources
    Close(ctx context.Context) error
}
```

#### Transformer Interface

```go
// EventTransformer converts datastore events to CloudEvents
type EventTransformer interface {
    // Transform converts a database event to CloudEvents format
    Transform(event Event) (*CloudEvent, error)
}
```

#### Exporter Architecture

```go
// HealthEventsExporter is the main component
// Completely decoupled from specific database or sink implementations
type HealthEventsExporter struct {
    source      ChangeStreamWatcher  // Database-agnostic event source
    transformer EventTransformer     // Format converter
    sink        EventSink            // Destination (HTTP, Kafka, etc.)
    config      ExporterConfig
}

func NewHealthEventsExporter(
    source ChangeStreamWatcher,
    transformer EventTransformer,
    sink EventSink,
    config ExporterConfig,
) *HealthEventsExporter {
    return &HealthEventsExporter{
        source:      source,
        transformer: transformer,
        sink:        sink,
        config:      config,
    }
}
```

### 2. CloudEvents Schema

Following the [CloudEvents 1.0 specification](https://github.com/cloudevents/spec/blob/v1.0/spec.md), health events are transformed into the following format:

```json
{
  "specversion": "1.0",
  "id": "fe5302cf-1887-48a9-8e6e-117d366a7344",
  "time": "2024-11-18T10:30:00.123456Z",
  "source": "nvsentinel://us-west-1-prod-cluster/healthevents",
  "type": "com.nvidia.nvsentinel.health.v1",
  "data": {
    "metadata": {
      "cluster": "us-west-1-prod-cluster",
      "environment": "production"
    },
    "healthEvent": {
      "version": "1",
      "agent": "syslog-health-monitor",
      "componentClass": "GPU",
      "checkName": "XID_ERROR_48",
      "isFatal": true,
      "isHealthy": false,
      "message": "GPU 3 reported XID 48: Double Bit ECC Error",
      "recommendedAction": "REPLACE_GPU",
      "errorCode": ["XID_48"],
      "entitiesImpacted": [
        {
          "entityType": "PCI",
          "entityValue": "0000:17:00.0"
        },
        {
          "entityType": "GPU_UUID",
          "entityValue": "GPU-a1b2c3d4-e5f6-7890-abcd-ef1234567890"
        }
      ],
      "metadata": {
        "chassis_serial": "SN123456789",
        "providerID": "aws:///us-west-2a/i-1234567890abcdef0",
        "topology.kubernetes.io/zone": "us-west-2a",
        "topology.kubernetes.io/region": "us-west-2"
      },
      "generatedTimestamp": "2024-11-18T10:30:00.123456Z",
      "nodeName": "gpu-node-42",
      "quarantineOverrides": {
        "force": true,
        "skip": false
      },
      "drainOverrides": {
        "force": false,
        "skip": false
      }
    }
  }
}
```

**CloudEvents Top-Level Attributes:**

| Attribute | Type | Required | Description |
|-----------|------|----------|-------------|
| `specversion` | String | Yes | CloudEvents version (always "1.0") |
| `id` | String (UUID) | Yes | Unique event ID; from MongoDB `_id` |
| `time` | Timestamp (RFC3339) | Yes | Event generation time |
| `source` | URI | Yes | Event source in format `nvsentinel://<cluster>/healthevents` |
| `type` | String | Yes | Event type: `com.nvidia.nvsentinel.health.v1` |

**Data Payload Structure:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `metadata.cluster` | String | Yes | Kubernetes cluster name |
| `metadata.environment` | String | Yes | Environment label (production, staging, dev, etc.) |
| `healthEvent` | Object | Yes | Complete health event from NVSentinel (see fields below) |

**Health Event Fields (`data.healthEvent`):**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `version` | Number | Yes | Schema version |
| `agent` | String | Yes | NVSentinel health monitor that generated the event (`gpu-health-monitor`, `syslog-health-monitor`, `csp-health-monitor`, `kubernetes-object-monitor`) |
| `componentClass` | String | Yes | Component type (GPU, CPU, Memory, CSP, etc.) |
| `checkName` | String | Yes | Name of the health check (e.g., `XID_ERROR_48`, `SXID_ERROR_12`) |
| `isFatal` | Boolean | Yes | Whether error is fatal |
| `isHealthy` | Boolean | Yes | Overall health status |
| `message` | String | No | Human-readable description |
| `recommendedAction` | String | Yes | Suggested remediation (`NONE`, `COMPONENT_RESET`, `CONTACT_SUPPORT`, `RESTART_VM`, `RESTART_BM`, `REPLACE_VM`) |
| `errorCode` | Array[String] | No | Specific error codes (e.g., `["XID_48"]`) |
| `entitiesImpacted` | Array[Entity] | Yes | Affected entities (e.g., GPU, PCI, NVSWITCH) |
| `entitiesImpacted[].entityType` | String | Yes | Entity type (`GPU`, `PCI`, `GPU_UUID`, `NVSWITCH`, `NVLINK`, etc.) |
| `entitiesImpacted[].entityValue` | String | Yes | Entity identifier (GPU index, PCI address, UUID, etc.) |
| `metadata` | Map[String, String] | No | Key-value metadata (enriched by platform connector); common keys: `chassis_serial`, `providerID`, `topology.kubernetes.io/zone`, `topology.kubernetes.io/region` |
| `generatedTimestamp` | Timestamp (RFC3339) | Yes | Event generation timestamp in UTC |
| `nodeName` | String | Yes | Kubernetes node name |
| `quarantineOverrides.force` | Boolean | No | Force node cordoning regardless of rules |
| `quarantineOverrides.skip` | Boolean | No | Skip node cordoning |
| `drainOverrides.force` | Boolean | No | Force pod eviction regardless of rules |
| `drainOverrides.skip` | Boolean | No | Skip pod eviction |

**Field Mapping from Datastore:**

The exporter transforms NVSentinel's `HealthEvent` protobuf (already enriched by platform connector) to CloudEvents format:

```go
// Mapping from protobuf HealthEvent to CloudEvents
CloudEvent := map[string]interface{}{
    "specversion": "1.0",
    "id":          uuid.New().String(),  // Generate new UUID for CloudEvents ID
    "time":        healthEvent.GeneratedTimestamp.AsTime().Format(time.RFC3339Nano),
    "source":      fmt.Sprintf("nvsentinel://%s/healthevents", config.Metadata.Cluster),
    "type":        "com.nvidia.nvsentinel.health.v1",
    
    "data": map[string]interface{}{
        // Operational metadata from config
        "metadata": map[string]string{
            "cluster":     config.Metadata.Cluster,
            "environment": config.Metadata.Environment,
        },
        
        // Health event from MongoDB
        "healthEvent": map[string]interface{}{
            "version":           healthEvent.Version,
            "agent":             healthEvent.Agent,
            "componentClass":    healthEvent.ComponentClass,
            "checkName":         healthEvent.CheckName,
            "isFatal":           healthEvent.IsFatal,
            "isHealthy":         healthEvent.IsHealthy,
            "message":           healthEvent.Message,
            "recommendedAction": healthEvent.RecommendedAction.String(),
            "errorCode":         healthEvent.ErrorCode,  // Already an array
            "entitiesImpacted":  transformEntities(healthEvent.EntitiesImpacted),
            "metadata":          healthEvent.Metadata,  // Already enriched by platform connector (providerID, topology labels, chassis_serial)
            "generatedTimestamp": healthEvent.GeneratedTimestamp.AsTime().Format(time.RFC3339Nano),
            "nodeName":          healthEvent.NodeName,
            "quarantineOverrides": map[string]interface{}{
                "force": healthEvent.QuarantineOverrides.Force,
                "skip":  healthEvent.QuarantineOverrides.Skip,
            },
            "drainOverrides": map[string]interface{}{
                "force": healthEvent.DrainOverrides.Force,
                "skip":  healthEvent.DrainOverrides.Skip,
            },
        },
    },
}

// transformEntities converts protobuf entities to JSON-friendly format
func transformEntities(entities []*pb.Entity) []map[string]string {
    result := make([]map[string]string, len(entities))
    for i, entity := range entities {
        result[i] = map[string]string{
            "entityType":  entity.EntityType,
            "entityValue": entity.EntityValue,
        }
    }
    return result
}
```


### 3. Event Stream Pipeline

The exporter watches all insert operations on the health events collection:

```go
// BuildHealthEventsExportPipeline creates a pipeline for health events
// Works with any datastore provider (MongoDB, PostgreSQL, etc.)
func BuildHealthEventsExportPipeline() interface{} {
    return EventFilter{
        OperationType: "insert",  // Watch all new health event inserts
    }
}

// Provider-specific implementation:
// MongoDB: db.healthevents.watch([{$match: {operationType: "insert"}}])
// PostgreSQL: LISTEN health_events WHERE operation = 'INSERT'
// Kafka: Consume from health-events topic
```

### 4. Sink Implementations

#### HTTP Sink

```go
type HTTPPublisher interface {
    // Publish single event
    Publish(ctx context.Context, event CloudEvent) error
    
    // Close publisher and flush pending events
    Close(ctx context.Context) error
}

type HTTPPublisherConfig struct {
    // Sink configuration
    Endpoint            string        // HTTP endpoint URL
    MaxRetries          int           // Number of retries on failure
    RetryBackoff        time.Duration // Initial backoff duration (exponential)
    MaxRetryBackoff     time.Duration // Maximum backoff duration cap
    Timeout             time.Duration // Request timeout
    
    // Authentication
    OIDCTokenProvider   TokenProvider // OIDC token provider
    
    // TLS configuration
    TLSCABundle         string        // Path to CA certificate bundle (optional)
    TLSInsecureSkipVerify bool        // Skip TLS verification (dev/testing only)
    
    // HTTP client settings
    MaxIdleConns        int
    MaxIdleConnsPerHost int
    IdleConnTimeout     time.Duration
}
```

#### Authentication & TLS

**OAuth2 Client Credentials Flow (OIDC):** Client credentials configured via secrets, tokens auto-refreshed and cached, included in requests as `Authorization: Bearer <token>`.

**TLS:** Custom CA bundle support; defaults to system CA bundle. Server certificate verification enforced in production.

#### Failure Handling

**Retry Policy:**
- Per-request timeout: 30s
- Exponential backoff: 1s → 2s → 4s → 8s → ... → 5m (capped)
- Max retries: 17 (~30 minutes total)

**Persistent Failures:**
- After max retries exhausted → exporter exits
- Kubernetes restarts the pod
- Resumes from last checkpoint (resume token)

### 5. Event Processing Flow

Pipeline: Receive → Transform → Publish → Update resume token → Repeat

**Error Handling:** Publish failures retried with exponential backoff; resume token update failures trigger restart.

### 6. Automatic Backfill (Bootstrap Phase)

**Trigger:** Backfill runs when **no resume token exists** in the `resumetokens` collection for this exporter's `client_name`. This occurs on:
- First deployment (clean install)
- After resume token is manually deleted
- After datastore wipe/restore

**Behavior:**
1. Record timestamp T before backfill starts
2. Query and export historical events (where `timestamp < T`)
3. Start change stream from timestamp T
4. Update resume token after each event; subsequent startups resume from last token

Network failures during backfill or streaming are handled via retry mechanism and resume tokens (see Failure Handling above).

To skip backfill and only export forward-looking events, set `enabled = false`.

```go
type BackfillConfig struct {
    Enabled        bool          // Enable/disable backfill (default: true)
    MaxAge         time.Duration // How far back to backfill (e.g., 24h, 7d, 30d); empty = all history
    MaxEvents      int           // Optional: safety limit to prevent overwhelming sink
    RateLimit      int           // Events per second during backfill
}
```

---

## Configuration

Configuration is managed via **ConfigMap** (structured TOML) and **Secrets** (sensitive values only). This follows existing NVSentinel patterns and makes config easier to review and maintain.

### ConfigMap: `health-events-exporter-config`

```toml
[exporter]
enabled = true

[exporter.metadata]
cluster = "nvsentinel-prod"      # Required: Kubernetes cluster name
environment = "production"       # Required: Environment label (production, staging, dev, etc.)

[exporter.sink]
endpoint = "https://events.example.com/api/v1/events"
timeout = "30s"
max_retries = 17                 # Standard CDC pattern: ~30 minutes of retries
retry_backoff = "1s"             # Exponential backoff base
max_retry_backoff = "5m"         # Cap backoff at 5 minutes

[exporter.sink.tls]
# ca_bundle = "/etc/ssl/certs/ca-bundle.crt"  # Optional: Custom CA bundle for enterprise CAs
# insecure_skip_verify = false                 # NEVER set to true in production

[exporter.oidc]
token_url = "https://auth.example.com/oauth2/token"
client_id = "nvsentinel-exporter"
scopes = ["events:write"]
# client_secret comes from Kubernetes Secret (not in ConfigMap)

[exporter.backfill]
# Backfill runs automatically when no resume token exists (first deployment, token deleted, etc.)
enabled = true           # Set to false to skip backfill and only export forward-looking events
# max_age = "720h"       # Optional: limit how far back to backfill (24h, 168h=7d, 720h=30d); empty = all history
# max_events = 1000000   # Optional: safety limit to prevent overwhelming sink
rate_limit = 1000        # Events per second during backfill

[exporter.resume_token]
client_name = "health-events-exporter"

# Note: Datastore configuration is reused from existing NVSentinel ConfigMap
# The exporter mounts the same ConfigMap as other NVSentinel components
```

### Secret: health-events-exporter-secret

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: health-events-exporter-secret
  namespace: nvsentinel
type: Opaque
stringData:
  oidc-client-secret: "<your-secret-here>"
```

---

## Rationale

### Key Design Decisions

- **CloudEvents format:** Industry standard
- **HTTP sink:** Direct HTTP POST (Issue #128)
- **At-least-once delivery:** Idempotent sinks handle duplicates
- **Retry pattern:** Exponential backoff, fail-fast on persistent errors, resume from checkpoint
- **Database-agnostic interfaces:** MongoDB, PostgreSQL, Kafka support
- **ConfigMap configuration:** TOML-based, Secrets for credentials
- **Operational metadata:** Cluster/environment labels in `data.metadata`
- **Single sink per instance:** Multiple instances for multi-sink scenarios

---

## Observability

### Metrics

```go
// Events processed
health_events_exporter_events_received_total{cluster="..."}
health_events_exporter_events_published_total{cluster="...",status="success|failure"}

// Latency
health_events_exporter_publish_duration_seconds{cluster="...",quantile="0.5|0.9|0.99"}

// Errors
health_events_exporter_transform_errors_total{cluster="..."}
health_events_exporter_publish_errors_total{cluster="...",error_type="..."}
health_events_exporter_token_refresh_errors_total{cluster="..."}

// Queue/Backlog
health_events_exporter_event_backlog_size{cluster="..."}

// Resume token
health_events_exporter_resume_token_update_timestamp{cluster="..."}

// Backfill
health_events_exporter_backfill_enabled{cluster="..."}              # Config: 1 if enabled, 0 if disabled
health_events_exporter_backfill_in_progress{cluster="..."}          # 1 during backfill, 0 otherwise
health_events_exporter_backfill_events_processed_total{cluster="..."}
health_events_exporter_backfill_duration_seconds{cluster="..."}
```

## References

- [CloudEvents Specification v1.0](https://github.com/cloudevents/spec/blob/v1.0/spec.md)
- [CloudEvents HTTP Protocol Binding](https://github.com/cloudevents/spec/blob/v1.0/http-protocol-binding.md)
- [OAuth 2.0 Client Credentials Grant](https://datatracker.ietf.org/doc/html/rfc6749#section-4.4)
- [GitHub Issue #128](https://github.com/NVIDIA/NVSentinel/issues/128)

