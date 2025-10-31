# ADR-006: Reliability — Platform Connector Event Buffering

## Context

Health monitors report events at variable rates. Under normal conditions, events are rare. During failures (cascading hardware issues, network problems), events can arrive in bursts. Platform connectors process these events by writing to MongoDB, updating Kubernetes conditions, or forwarding to other systems.

If platform connectors process events slower than health monitors produce them, events accumulate in memory. The challenge is handling this gracefully:
- **Dropping events**: Simple but unacceptable - missing fatal hardware failures degrades user experience
- **Unbounded queues**: Lead to memory exhaustion and pod crashes
- **Blocking producers**: Health monitors stall, unable to report new failures

The system needs a buffering mechanism that prevents event loss while bounding memory consumption.

## Decision

Use a rate-limiting workqueue (ring buffer) for each connector to buffer health events. Each connector (Kubernetes, MongoDB) has its own dedicated queue to prevent head-of-line blocking.

## Implementation

Architecture:
```
Health Monitor
      |
      v
  gRPC Server (platform-connector)
      |
      v
Common Server Infrastructure
      |
      +----> Ring Buffer 1 (workqueue) ----> Connector 1 (Kubernetes)
      |
      +----> Ring Buffer 2 (workqueue) ----> Connector 2 (MongoDB)
```

Implementation uses Kubernetes `client-go` workqueue with rate limiting:
- Each connector has dedicated `TypedRateLimitingInterface` queue
- Built-in retry logic with exponential backoff
- Bounded memory through queue depth limits
- Prometheus metrics integration via workqueue provider

Ring buffer operations:
- `Enqueue(data)` - Add health events to queue
- `Dequeue()` - Get next event for processing
- `HealthMetricEleProcessingCompleted(data)` - Mark event as successfully processed
- `HealthMetricEleProcessingFailed(data)` - Trigger retry with backoff

Metrics automatically exposed:
- `platform_connector_workqueue_depth_{name}` - current queue depth
- `platform_connector_workqueue_adds_total_{name}` - total events added
- `platform_connector_workqueue_latency_seconds_{name}` - time in queue

## Rationale

- **Bounded memory**: Workqueue implementation provides fixed queue depth
- **Fairness**: Each connector processes at its own rate without blocking others
- **Built-in reliability**: Workqueue handles retries with exponential backoff
- **Observable**: Automatic Prometheus metrics for queue depth and latency
- **Battle-tested**: Uses Kubernetes client-go workqueue (used in all controllers)
- **Rate limiting**: Prevents overwhelming downstream systems

## Consequences

### Positive
- Memory consumption is bounded and predictable via workqueue
- Slow connectors don't block fast connectors
- Built-in retry logic handles transient failures
- No external dependencies
- Automatic observability through Prometheus metrics
- Proven reliability (used in all Kubernetes controllers)

### Negative
- Events may be retried multiple times if processing repeatedly fails
- Queue can fill up if connector is permanently blocked
- No persistent buffering across pod restarts

### Mitigations
- Monitor queue depth metrics and alert on sustained high values
- Implement circuit breakers in connectors for permanent failures
- Use rate limiting to prevent overwhelming downstream systems
- Configure appropriate queue sizes based on cluster scale

## Alternatives Considered

### Unbounded In-Memory Queues
**Rejected** because: Under sustained load or slow connector processing, memory usage grows without bound. This can trigger OOMKills, crashing the entire platform connector pod and losing all in-flight events. No mechanism for backpressure to health monitors.

### Single Shared Buffer for All Connectors
**Rejected** because: Creates head-of-line blocking. If one connector is slow (e.g., MongoDB connection issue), all connectors are blocked. Reduces fault isolation - a problem in one connector affects all event processing.

### External Message Queue (Kafka, RabbitMQ, NATS)
**Rejected** because: Adds operational complexity and external dependencies. Requires additional infrastructure (message broker deployment, monitoring, upgrades). Increases latency vs local buffering. Overkill for node-local communication. Introduces new failure modes.

### Database-Backed Queue
**Rejected** because: Writing to disk on every event is high latency. Creates dependency on database availability for event ingestion. Requires garbage collection of processed events. Much slower than in-memory buffering for normal operation.

### Drop Events Under Extreme Load
**Rejected** because: Could result in missing critical failures. Degrades the primary value proposition of the resilience system. Workqueue with backoff provides better handling of transient overload.

## Notes

- Workqueue implementation uses `k8s.io/client-go/util/workqueue`
- Default rate limiter uses exponential backoff (base delay increases on repeated failures)
- Queue metrics follow Kubernetes controller conventions
- Failed events are automatically retried with increasing delays
- Queue shutdown is graceful - processes remaining items before exit

## References

- Kubernetes Workqueue: https://pkg.go.dev/k8s.io/client-go/util/workqueue
