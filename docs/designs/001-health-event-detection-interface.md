# ADR-001: Architecture — Health Event Detection Interface

## Context

Hardware failures in accelerated computing clusters need to be detected quickly and acted upon to maintain system reliability. The system consists of multiple health monitoring components (GPU monitors, network monitors, switch monitors, etc.) that need to report failures to a central processing system.

The primary challenge is creating a clean separation between the detection logic (which may be vendor-specific or hardware-specific) and the platform-specific handling logic (which understands Kubernetes, cloud providers, etc.). This separation allows:
- Health monitors to be developed and maintained independently
- Easy addition of new types of health monitors
- Platform-agnostic health monitor binaries

Three architectural options were considered:
1. Direct Kubernetes API writes from monitors
2. Shared database for communication
3. gRPC-based interface with platform connectors

## Decision

Use a gRPC-based interface where health monitors report events to platform connectors over Unix Domain Sockets (UDS). Health monitors are standalone daemons that detect issues and encode events using Protocol Buffers, then send them via gRPC to platform connectors that translate events into platform-specific actions.

## Implementation

- Health monitors run as DaemonSet pods on every node
- Each monitor implements a gRPC client that connects to a Unix Domain Socket
- Platform connectors expose a gRPC server listening on the UDS at `/var/run/nvsentinel/platform-connector.sock`
- The interface uses the `HealthEventOccuredV1` RPC with `HealthEvents` message type
- Events include: agent name, component class, check name, fatality flag, error codes, impacted entities, and recommended actions
- All communication happens locally on the node - no network calls required

Key interface fields:
```
message HealthEvent {
  string agent                      // monitor name (e.g., "GPUHealthMonitor")
  string componentClass             // component type (e.g., "GPU", "NIC")
  string checkName                  // specific check executed
  bool isFatal                      // requires immediate action
  bool isHealthy                    // current health status
  string message                    // human-readable description
  RecommendedAction recommendedAction  // suggested remediation
  repeated string errorCode         // machine-readable codes
  repeated Entity entitiesImpacted  // affected hardware (GPU UUIDs, etc.)
  map<string, string> metadata      // additional context
  google.protobuf.Timestamp generatedTimestamp
  string nodeName
}
```

## Rationale

- **Loose coupling**: Health monitors don't need Kubernetes client libraries or cloud provider SDKs
- **Language agnostic**: Protocol Buffers and gRPC support many languages
- **Simple deployment**: Unix Domain Sockets don't require network configuration or service discovery
- **Security**: Local socket communication eliminates network attack surface
- **Performance**: UDS provides high throughput with low latency for local IPC

## Consequences

### Positive
- Health monitors can be written in any language (Python for GPU monitoring, C++ for low-level hardware)
- New health monitors can be added without modifying platform connectors
- Testing is simplified - monitors can be tested independently
- Binary portability - same monitor binary works across different platforms
- No authentication needed for local socket communication

### Negative
- Requires Unix Domain Socket volume mounts in pod specifications
- Additional abstraction layer adds complexity
- Health monitors and platform connectors must both be running
- Protocol Buffer schema changes require coordination

### Mitigations
- Use semantic versioning in the `HealthEvents.version` field
- Platform connectors maintain in-memory cache until health monitors connect
- Include retry logic in health monitors with exponential backoff
- Monitor pod anti-affinity rules prevent scheduling issues

## Alternatives Considered

### Direct Kubernetes API Integration
**Rejected** because: Health monitors would require Kubernetes client libraries, making them platform-dependent. This would increase binary size, add complexity, and make monitors harder to test independently. Additionally, it would require managing service account tokens and RBAC policies for every monitor.

### Shared Database Communication
**Rejected** because: Introducing a database dependency for every health monitor adds operational complexity. Monitors would need database drivers, connection management, and retry logic. It also creates a single point of failure and requires network connectivity for local communication.

## Notes

- Health monitors should implement graceful shutdown to drain pending events
- The gRPC interface is versioned to support future extensions
- Events are fire-and-forget from the monitor's perspective - the platform connector handles persistence and retries
- For testing, a mock platform connector can record events to files
