# ADR-002: Infrastructure — Storage Layer Selection

## Context

The system needs persistent storage for health events that multiple components can read and react to in real-time. The Fault Quarantine, Node Drainer, and Fault Remediation modules all need to watch for new health events and take action accordingly.

Key requirements:
1. **Strong consistency**: All components must see the same view of events at any point in time
2. **High availability**: No single point of failure; survive node failures
3. **Watch/notification mechanism**: Components need to be notified when new events arrive
4. **Robust ecosystem**: Well-maintained with good documentation and client libraries

Storage candidates evaluated: etcd, MongoDB, Redis, CouchDB, and Cassandra.

## Decision

Use MongoDB with replica sets as the storage layer for health events. MongoDB provides strong consistency through configurable write concerns, high availability through automatic failover, and real-time notifications through Change Streams.

## Implementation

- Deploy MongoDB as a StatefulSet with 3 replicas for high availability
- Use majority write concern (`w: majority`) to ensure data durability
- Use majority read concern to prevent reading stale data
- Configure Change Streams for real-time event notifications to downstream modules
- Use transaction-based operations for atomic multi-event insertions
- Store health events as documents with indexes on: nodeName, timestamp, isFatal, componentClass
- Implement in-memory caching in the MongoDB connector to reduce duplicate writes

Deployment in the cluster:
```
mongodb-0   [Primary]
mongodb-1   [Secondary]  
mongodb-2   [Secondary]
```

Key operations:
- Platform connectors insert health events with majority write concern
- Quarantine/Drainer/Remediation modules establish Change Stream watches
- Aggregation pipelines used for complex event correlation

## Rationale

- **Strong consistency**: Write concern `majority` ensures data is persisted to most replicas before acknowledging
- **Automatic failover**: Replica sets automatically promote a secondary to primary on failure
- **Change Streams**: Provide resumable, real-time notifications of document changes without polling
- **Rich querying**: Aggregation pipelines enable complex event correlation (e.g., counting repeated non-fatal events)
- **Mature Go support**: Official MongoDB Go driver is well-maintained and feature-complete
- **Operational experience**: MongoDB is widely deployed and understood by operations teams

## Consequences

### Positive
- Components receive events in real-time without polling
- Replica sets provide automatic recovery from node failures
- Aggregation framework enables sophisticated event analysis
- Change Streams are resumable - clients can recover from disconnections without missing events
- Official Go driver simplifies development

### Negative
- MongoDB requires more resources than simpler key-value stores
- Stateful deployment requires persistent volumes
- Operators need MongoDB operational knowledge
- Replica set coordination adds latency vs single-node writes

### Mitigations
- Set resource limits appropriate for cluster size
- Use local SSDs for persistent volumes to reduce latency
- Provide monitoring dashboards and runbooks for common operations
- Implement connection pooling in clients to reduce overhead
- Use in-memory caching to minimize database writes

## Alternatives Considered

### etcd
**Rejected** because: While etcd provides strong consistency via Raft and excellent watch capabilities, it's optimized for small key-value data (typically < 1.5MB total). Health events include metadata, stack traces, and diagnostic information that can be large. etcd's performance degrades with larger values. Additionally, complex queries (like aggregating repeated events) would require client-side logic.

### Redis
**Rejected** because: Redis provides only eventual consistency through asynchronous replication. The `WAIT` command can enforce synchronous replication but doesn't provide the same consistency guarantees as consensus algorithms. More critically, Redis Pub/Sub is fire-and-forget - if a client disconnects, all events during disconnection are lost. This violates the requirement that no health events should be missed.

### CouchDB
**Rejected** because: CouchDB's changes feed can provide notifications, but the setup is complex and resuming after disconnections requires manual state management. The Go ecosystem for CouchDB is immature - there's no widely-adopted, production-ready client library. CouchDB's multi-master replication model provides eventual consistency by default, requiring additional configuration for stronger guarantees.

### Cassandra
**Rejected** because: Cassandra lacks built-in watch/notification mechanisms. Implementing event notifications would require external systems like Kafka, adding significant complexity. While Cassandra excels at write-heavy workloads, our read patterns (watching for events, running aggregations) don't align with Cassandra's strengths.

## Notes

- Change Streams require MongoDB replica sets (not standalone instances)
- For very large clusters (>10k nodes), consider sharding based on nodeName
- The health event schema should include TTL indexes to automatically clean up old events
- Non-goals include using MongoDB as a general-purpose application database
