# ADR-004: Behavior — Workload Eviction Strategies

## Context

When a node is quarantined due to hardware failure, existing workloads must be handled appropriately. Different workload types have different requirements:

- **Training jobs**: May run for hours/days and can checkpoint their state. Abrupt termination wastes computation and loses progress. These workloads benefit from graceful completion or checkpointing before eviction.

- **Inference services**: Typically stateless and can be quickly rescheduled. Immediate eviction minimizes service degradation.

- **Batch jobs**: May be at different stages of completion. Some can finish quickly while others should be terminated immediately.

The challenge is providing a flexible eviction strategy that balances:
- User experience (minimizing wasted work)
- Cluster reliability (preventing workloads from running on faulty hardware)
- Fault severity (critical errors require immediate action)

Options considered:
1. Immediate eviction for all workloads
2. Wait indefinitely for all workloads to complete
3. Namespace-based configurable eviction strategies

## Decision

Implement a Node Drainer Module that supports two modes of operation configurable per namespace: **Immediate** eviction and **AllowCompletion**. Operators configure which namespaces use which mode, allowing different policies for different workload types.

## Implementation

The Node Drainer Module is deployed as a controller that:
- Watches MongoDB for fatal health events
- Consults configuration to determine eviction mode for each namespace
- Executes the appropriate eviction strategy
- Tracks drain status (in-progress, completed, failed)

Configuration format:
```toml
evictionTimeoutInSeconds = 40

[[userNamespaces]]
name = "training-*"
mode = "AllowCompletion"

[[userNamespaces]]
name = "inference-*"
mode = "Immediate"

[[userNamespaces]]
name = "batch-critical"
mode = "Immediate"
```

### Immediate Mode

1. Quarantine module cordons node
2. Node drainer receives fatal health event
3. For each pod in matching namespaces:
   a. Call Kubernetes eviction API
   b. Wait up to evictionTimeoutInSeconds
   c. If pod still exists, force delete
4. Mark node drain as successful

### AllowCompletion Mode

1. Quarantine module cordons node
2. Node drainer receives fatal health event  
3. For each pod in matching namespaces:
   a. Monitor pod status
   b. Wait for pod to reach Completed status
   c. Delete completed pod
4. Wait indefinitely (no timeout)
5. Mark node drain as successful when all pods removed

## Rationale

- **Workload-aware**: Different workload types get appropriate treatment
- **Namespace isolation**: Teams can configure their own policies
- **Wildcard support**: Pattern matching (e.g., `training-*`) simplifies configuration
- **Safe defaults**: Critical failures still protect the cluster

## Consequences

### Positive
- Training workloads can checkpoint and complete gracefully
- Inference services are quickly rescheduled, minimizing downtime
- Configuration is straightforward and declarative
- Status is observable through standard Kubernetes APIs
- Operators have fine-grained control over eviction behavior

### Negative
- AllowCompletion mode can leave nodes drained for extended periods
- Faulty hardware continues consuming resources during completion
- Workloads might not actually complete if hardware is degraded
- No mechanism to force eviction after a maximum wait time in AllowCompletion mode
- Configuration requires understanding namespace naming conventions

### Mitigations
- Document best practices for setting eviction timeouts
- Emit metrics on drain duration for monitoring
- Provide alerts when nodes are draining for unusually long periods
- Recommend that training jobs implement health checks to exit early on hardware issues
- Support manual override via node annotations to switch modes
- Consider future enhancement: hybrid mode with maximum wait time

## Alternatives Considered

### Immediate Eviction Only
**Rejected** because: This wastes computation for long-running training jobs. Modern ML training can run for days and accumulate significant cost. Forcing immediate termination loses checkpointing opportunities and degrades user experience. Users will resist enabling resilience features that waste their work.

### Wait Indefinitely for All Workloads
**Rejected** because: Faulty nodes consume cluster resources without providing value. Inference workloads and stateless services should be immediately moved to healthy nodes to maintain service availability. Long-running jobs on degraded hardware may never complete successfully, creating cluster deadlock.

### Per-Pod Annotation-Based Configuration
**Rejected** because: This pushes configuration burden to users who may not understand hardware failure modes. It's error-prone - forgetting annotations on pods could lead to data loss. Namespace-based configuration provides better defaults and centralized policy management.

### Time-Based Hybrid Mode
Considered but deferred: Allow completion for X minutes, then force eviction. This adds complexity around timeout management and notification. Workloads need clear signals about impending eviction. May be added in a future iteration based on operational experience.

## Notes

- The eviction timeout should be tuned based on cluster characteristics
- Pods with PodDisruptionBudgets are still respected during eviction
- System namespaces (kube-system, etc.) are never drained by this module
- Future enhancement: support for custom eviction signals (send SIGUSR1 to pod before evicting)

## References

- Kubernetes Eviction API: https://kubernetes.io/docs/concepts/scheduling-eviction/api-eviction/
