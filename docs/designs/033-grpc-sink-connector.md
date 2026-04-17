# ADR-033: gRPC Sink Connector for Platform-Connectors

## Context

Platform-connectors receives health events from all NVSentinel health monitors
via a Unix domain socket and distributes them to registered connectors using a
ring buffer fanout pattern. Today there are two connectors:

- **Store connector** — persists events to MongoDB or PostgreSQL for downstream
  modules (fault-quarantine, node-drainer, health-events-analyzer) that watch
  change streams.
- **K8s connector** — updates Kubernetes node conditions so operators can see
  GPU health status via `kubectl describe node`.

Both connectors serve NVSentinel's internal architecture. There is no mechanism
to forward the full structured HealthEvent proto to components outside of
NVSentinel.

## Problem

External systems that want to consume NVSentinel health events have two options
today, both lossy:

1. **Watch Kubernetes node conditions** — node conditions flatten the rich
   HealthEvent proto (17 fields) into a condition type, boolean status, and a
   truncated message string. Entity-level detail (GPU UUID, GPC/TPC/SM,
   register values), the full error code array, recommended action enum,
   processing strategy, and metadata are lost or truncated at the 4KB
   Kubernetes message limit.

2. **Query the database** — requires access to the NVSentinel MongoDB or
   PostgreSQL instance, coupling the external system to NVSentinel's internal
   storage layer.

3. **Event exporter** — the existing event-exporter module can forward events
   to an external HTTP endpoint, but it reads from the database (MongoDB or
   PostgreSQL), so it still requires in-cluster state persistence.

## Solution

Add a third connector — the **gRPC sink connector** — that forwards the full
HealthEvent proto to an external gRPC server. The external server implements
the existing `PlatformConnector` service:

```protobuf
service PlatformConnector {
  rpc HealthEventOccurredV1(HealthEvents) returns (google.protobuf.Empty) {}
}
```

No new proto definitions are needed. The external server receives the same
structured data that the store and K8s connectors receive — full fidelity,
no truncation.

## Architecture

```text
Health Monitors (5+ types)
        |
    [UDS Socket]
        |
[Platform-Connectors gRPC Server]
        |
    [Pipeline: Transform Events]
        |
    [Ring Buffer Fanout]
        |--- K8s Ring Buffer ---> K8s Connector (node conditions)
        |--- Store Ring Buffer -> Store Connector (MongoDB/PostgreSQL)
        |--- gRPC Ring Buffer --> gRPC Sink Connector (external server)  <-- NEW
```

Each connector has its own ring buffer with independent retry logic. If the
gRPC sink target is unreachable, only its buffer backs up — the store and K8s
connectors are unaffected.

## Configuration

The connector is disabled by default and configured via Helm values:

```yaml
platformConnector:
  grpcSinkConnector:
    enabled: false
    target: ""         # gRPC server address, e.g. "my-service.example.com:50051"
    maxRetries: 3      # Retry attempts with exponential backoff before dropping
    tokenPath: ""      # Optional: SA token path for bearer auth (empty = disabled)
```

The per-RPC timeout is fixed at 10 seconds. If the target does not respond
within this window, the send is treated as a failure and retried via the ring
buffer's exponential backoff.

## Security

The gRPC sink target is expected to be a component within the same cluster,
outside of NVSentinel but still within the cluster's internal network. The
connector uses insecure credentials (`insecure.NewCredentials()`) by default,
consistent with all other internal NVSentinel gRPC connections (health monitors,
health-events-analyzer, metadata-collector).

For deployments that require authentication, the connector supports optional
Kubernetes ServiceAccount bearer token auth — the same pattern used by the
janitor → janitor-provider connection (ADR-030). When `tokenPath` is configured,
the connector reads a projected SA token and attaches it as a Bearer header on
every RPC. The receiving server can validate the token via the Kubernetes
TokenReview API.

```yaml
grpcSinkConnector:
  tokenPath: "/var/run/secrets/nvsentinel/grpcsink/token"  # enable auth
  # tokenPath: ""  # disable auth (default)
```

Network policies are also recommended to restrict which pods can connect
to the target service.

## Use Cases

- **Custom remediation pipelines** — receive structured fault data and trigger
  organization-specific remediation workflows without depending on NVSentinel's
  internal modules.
- **External analytics and dashboards** — stream GPU health events to a central
  analytics platform with full entity-level detail (GPU UUID, PCI address,
  error codes, recommended actions).
- **Multi-cluster aggregation** — forward events from multiple clusters to a
  central service for fleet-wide fault correlation and pattern detection.
- **Integration with existing infrastructure** — connect NVSentinel's detection
  layer to existing operational tooling without modifying NVSentinel's internal
  architecture.
