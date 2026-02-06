# ADR-027: Node Management — Automated Node Labeling for Scale Operations

## Context

NVSentinel's circuit breaker is designed to prevent too many nodes from being cordoned simultaneously when widespread health issues are detected. It monitors the percentage of nodes that become unhealthy within a time window (default: 50% within 5 minutes) and trips when the threshold is exceeded.

However, during legitimate cluster scale operations (initial GPU node bringup, autoscaling, capacity expansion), newly provisioned nodes go through an initialization phase where GPU components (drivers, GPU Operator, DCGM) are not yet ready. From NVSentinel's perspective, these transitioning nodes appear unhealthy, triggering the circuit breaker despite no actual failures occurring.

### Current Challenges

1. **Services inside the cluster cannot distinguish between scale-up and failure**: A controller running in-cluster sees nodes coming online but doesn't know if they're new nodes or existing nodes recovering from maintenance/failure.

2. **Circuit breaker trips during legitimate operations**:
   - Small clusters (1-2 nodes): Adding even 1 node represents 50%+ of cluster
   - Initial bringup: CPU-only cluster → add GPU nodes exceeds threshold
   - Rapid autoscaling: Inference workloads triggering Karpenter to provision many nodes
   - Multi-zone expansion: Multiple nodes coming online simultaneously

3. **Manual workaround is error-prone**: Current solution requires operators to manually label nodes with `k8saas.nvidia.com/ManagedByNVSentinel=false` before scale operations, then remove the label after nodes are healthy. This is:
   - Forgotten during time-pressured operations
   - Not integrated into automated workflows
   - Inconsistently applied across teams
   - Similar to the manual labeling required during OSMO maintenance upgrades

4. **Similar to previously solved GPU Operator timing issue**: This mirrors the GPU Operator race condition where the runtime controller checked for cluster-policy readiness before GPU resources were available. That was solved with preflight checks at the orchestration layer.

### Options Considered

**Option A: Higher-order orchestration** (e.g., runtime controller, GitOps)
- Pros: Can see cluster spec and expected node count, knows scale operations are happening
- Cons: Only works for centrally managed clusters, not for user-managed Karpenter/autoscaling

**Option B: Webhook-based automatic labeling** (Kubernetes admission controller)
- Pros: Automatic, works for all provisioning methods
- Cons: Requires additional component, adds complexity

**Option C: Node initialization controller** (DaemonSet + operator pattern)
- Pros: Self-contained, works across provisioning methods
- Cons: Adds another component, timing-dependent

**Option D: Enhanced circuit breaker logic** (node age awareness)
- Pros: No external changes needed
- Cons: Circuit breaker still can't distinguish new vs. recovered nodes

**Option E: Manual labeling with better documentation** (current state + docs)
- Pros: Simple, no code changes, works today
- Cons: Requires manual intervention, error-prone

## Decision

Implement a **phased approach** combining multiple solutions:

### Phase 1: Enhanced Documentation (Immediate)
Create comprehensive runbooks documenting:
- When and why circuit breaker trips during scale operations
- Step-by-step procedures for manual labeling
- Examples for common scenarios (initial bringup, autoscaling, multi-zone)
- Integration with infrastructure-as-code tools (Terraform, Pulumi)
- Reference to similar maintenance procedures (OSMO upgrades, driver upgrades)

### Phase 2: Orchestration Layer Integration (Medium-term)
For centrally managed clusters using runtime controllers or GitOps:
- Implement scale-operation awareness in higher-order controllers
- Automatically apply `k8saas.nvidia.com/ManagedByNVSentinel=false` label during orchestrated operations
- Monitor node health and automatically remove label when ready
- Similar to how the runtime controller solved GPU Operator timing issues

### Phase 3: Admission Controller (Long-term - Optional)
For clusters where users directly provision nodes (Karpenter, manual kubectl):
- Develop validating/mutating webhook admission controller
- Automatically inject `k8saas.nvidia.com/ManagedByNVSentinel=false` label on new GPU nodes
- Use node initialization signals to determine when to remove label
- Make this optional/configurable per cluster

## Implementation

### Phase 1: Documentation (This ADR's Scope)

#### New Runbook: `docs/runbooks/cluster-scale-operations.md`
- Overview of the problem
- Common scenarios (initial bringup, autoscaling, expansion)
- Step-by-step procedures for:
  - Manual labeling workflow
  - Troubleshooting circuit breaker trips during scale operations
  - Resetting circuit breaker with appropriate cursor mode
- Integration examples:
  - Terraform/Pulumi scripts
  - Karpenter/Cluster Autoscaler configurations
  - GitOps workflows
- Automation approaches for different platforms

#### Update Existing Documentation
- `docs/runbooks/circuit-breaker.md`: Add section on scale operations
- `docs/runbooks/driver-upgrades.md`: Cross-reference scale operations runbook
- `README.md`: Link to scale operations guidance

### Phase 2: Orchestration Integration (Future Implementation)

#### For Runtime Controllers
```go
// Conceptual example - not actual implementation
func (r *Reconciler) ScaleGPUNodePool(ctx context.Context, desired int) error {
    // 1. Read expected node count from cluster spec
    expectedNodes := desired
    
    // 2. Label existing nodes before scale operation
    if err := r.labelNodesForScaleOperation(ctx); err != nil {
        return err
    }
    
    // 3. Trigger node pool scale-up
    if err := r.scaleNodePool(ctx, desired); err != nil {
        return err
    }
    
    // 4. Wait for GPU Operator to be ready on new nodes
    if err := r.waitForGPUOperator(ctx, expectedNodes); err != nil {
        return err
    }
    
    // 5. Wait for DCGM health on all nodes
    if err := r.waitForDCGMHealthy(ctx, expectedNodes); err != nil {
        return err
    }
    
    // 6. Remove NVSentinel exclusion labels
    if err := r.unlabelNodesAfterScaleOperation(ctx); err != nil {
        return err
    }
    
    return nil
}
```

**Integration Points:**
- Runtime controller's node pool management logic
- Maintenance API (similar to OSMO upgrades)
- Cluster lifecycle hooks

### Phase 3: Admission Controller (Future - Optional)

#### Architecture
```
┌─────────────────────────────────────────────────────────────┐
│ Kubernetes API Server                                       │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Node Creation Request                                 │  │
│  └────────────────────┬─────────────────────────────────┘  │
│                       │                                     │
│                       ▼                                     │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Mutating Webhook: nvsentinel-node-labeler            │  │
│  │ - Check if node has GPU labels                       │  │
│  │ - Inject: k8saas.nvidia.com/ManagedByNVSentinel=false│  │
│  │ - Inject: node-initializing=true                     │  │
│  └────────────────────┬─────────────────────────────────┘  │
│                       │                                     │
│                       ▼                                     │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Node Created with Labels                             │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
                       │
                       ▼
┌─────────────────────────────────────────────────────────────┐
│ Node Initialization Controller (DaemonSet)                  │
│  ┌──────────────────────────────────────────────────────┐  │
│  │ Watch for nodes with node-initializing=true          │  │
│  │ - Wait for GPU Operator pods Ready                   │  │
│  │ - Wait for DCGM connectivity                         │  │
│  │ - Wait for driver loaded                             │  │
│  │ - Remove both labels when healthy                    │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────┘
```

**Components:**
1. **Mutating Webhook**: Injects labels on GPU node creation
2. **Initialization Controller**: Monitors node health and removes labels
3. **Configuration**: Per-cluster opt-in via Helm values

## Rationale

### Why Phased Approach?

1. **Immediate Value**: Phase 1 provides immediate relief through better documentation - users know how to handle this issue today

2. **Targeted Automation**: Phase 2 automates for the most common use case (centrally managed clusters) without adding complexity for all users

3. **Optional Complexity**: Phase 3 handles the remaining cases (user-managed autoscaling) but as an opt-in feature

4. **Proven Pattern**: Follows the same pattern used to solve GPU Operator timing issues - orchestration layer awareness

### Why Not Automatic in All Cases?

The core issue is **information asymmetry**: services inside the cluster don't have context about higher-order operations. There's no universal way for NVSentinel (running in-cluster) to know:
- If a node is new (scale-up) vs. existing (break-fix recovery)
- How many total nodes are expected
- Whether a scale operation is in progress

The best solution depends on **who controls the provisioning**:
- **Centrally managed**: Higher-order controller has the context → automate there (Phase 2)
- **User-managed**: Users control provisioning → provide tools/docs (Phase 1, Phase 3)

## Consequences

### Positive

1. **Immediate improvement**: Documentation helps users today without code changes
2. **Reduces operational burden**: Automation in orchestration layer eliminates manual steps for most users
3. **Prevents circuit breaker trips**: Proper labeling prevents false-positive trips during legitimate operations
4. **Follows established patterns**: Similar to GPU Operator timing fix and OSMO maintenance procedures
5. **Flexible**: Users can choose manual, orchestrated, or automatic approaches based on their setup

### Negative

1. **Not fully automatic**: Still requires user awareness for ad-hoc scale operations
2. **Multiple solutions**: Different approaches for different scenarios increases documentation complexity
3. **Phase 2/3 require development**: Orchestration integration and admission controller need implementation effort
4. **Admission controller overhead**: Phase 3 adds webhook latency to node creation

### Mitigations

1. **Clear documentation**: Comprehensive runbooks explain when and how to use each approach
2. **Sensible defaults**: Most common case (orchestrated) gets automated first
3. **Opt-in complexity**: Admission controller is optional, only for users who need it
4. **Cross-references**: Link related docs (circuit breaker, driver upgrades, OSMO maintenance)

## Alternatives Considered

### Make Circuit Breaker "Smarter" (Node Age Awareness)

**Approach**: Modify circuit breaker to ignore nodes younger than N minutes.

**Rejected because**:
- Still can't distinguish "new node being provisioned" from "existing node that just rebooted"
- Would require storing node creation timestamps and tracking state
- Could mask real issues with newly rebooted nodes
- Adds complexity to circuit breaker logic that should remain simple

### Disable Circuit Breaker by Default

**Approach**: Make circuit breaker opt-in rather than opt-out.

**Rejected because**:
- Circuit breaker is a critical safety mechanism for production
- Would increase risk of widespread cordoning during legitimate issues
- Removes important protection for users who don't understand the implications
- Better to solve the scale-up problem than remove the safety mechanism

### Taint-Based Handoff (Similar to Skyhook → GPU Operator → NVSentinel)

**Approach**: Use multi-layer taints where each system removes its taint when ready.

**Rejected because**:
- Requires changes to multiple systems (node provisioner, GPU Operator, NVSentinel)
- More complex to debug when issues occur
- Still doesn't solve the circuit breaker threshold issue (nodes are still "unhealthy")
- Taint handoff is for scheduling, not health monitoring

### Separate Circuit Breakers for New vs. Existing Nodes

**Approach**: Track new vs. existing nodes separately with different thresholds.

**Rejected because**:
- No reliable way to determine "new" vs. "existing" from inside the cluster
- Would require persistent state about all nodes
- Adds significant complexity to circuit breaker logic
- Doesn't solve the fundamental information asymmetry problem

## Notes

### Related Issues and Discussions

- HIPPO-5214: Circuit breaker tripped during cluster bringup (EKS CPU → GPU flow)
- Similar to GPU Operator cluster-policy timing bug (solved via runtime controller preflight checks)
- OSMO maintenance process already uses this label-based approach successfully

### Non-Goals

- **Automatic detection of scale operations**: Not attempting to make NVSentinel "know" when scale operations happen - this requires higher-order context
- **Replacing circuit breaker**: Circuit breaker remains essential safety mechanism
- **Universal automation**: Different provisioning methods (Terraform, Karpenter, manual) may require different approaches

### Future Considerations

1. **Cloud provider integrations**: Could leverage cloud provider APIs (AWS ASG events, GCP MIG operations) to detect scale operations
2. **Maintenance API enhancement**: Extend existing maintenance API to cover scale operations explicitly
3. **Standardized node lifecycle hooks**: Kubernetes SIG-Node discussions around standardized init/ready signals
4. **Metrics and alerting**: Add metrics for nodes with exclusion labels, alert if labels remain too long

## References

- [Runbook: Cluster Bringup and Node Scale Operations](../runbooks/cluster-scale-operations.md)
- [Runbook: Circuit Breaker](../runbooks/circuit-breaker.md)
- [Runbook: Driver Upgrades](../runbooks/driver-upgrades.md)
- [ADR-022: Circuit Breaker Reset Mechanism](022-circuit-breaker-reset-mechanism.md)
- HIPPO-5214: Circuit breaker tripped during EKS cluster bringup
- GPU Operator timing issue discussion (internal)
- OSMO maintenance procedures (internal)

