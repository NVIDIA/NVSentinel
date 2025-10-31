# ADR-005: API — Kubernetes-Native Maintenance API

## Context

After detecting and quarantining faulty hardware, the system needs to trigger remediation actions. Cloud service providers expose APIs to reset GPUs, reboot instances, terminate instances, and report hardware failures. The system needs a way to invoke these CSP-specific operations.

Key requirements:
1. Support multiple remediation types (GPU reset, node reboot, node termination, fault reporting)
2. Track operation status and completion
3. Work across different cloud providers
4. Follow Kubernetes patterns and conventions
5. Allow both automated and manual triggering

Two architectural approaches were considered:
1. **Non-Kubernetes native**: Webhook-based integration with existing tools (Argo Workflows, Shoreline)
2. **Kubernetes native**: Custom Resource Definitions with controller pattern

Existing non-native solutions have gaps - none provides complete coverage across all CSPs, workload types, and remediation operations.

## Decision

Design a Kubernetes-native Maintenance API using Custom Resource Definitions (CRDs) to represent maintenance operations. Define separate CRDs for each operation type: `ResetGpu`, `RebootNode`, `TerminateNode`, `NodeFault`, `NodeDiagnosticReport`, and `SupportRequestMonitor`.

## Implementation

The API consists of six CRDs following standard Kubernetes conventions:

### ResetGpu
Resets one or more GPUs on a node without rebooting.
```yaml
apiVersion: maintenance.nvidia.com/v1alpha1
kind: ResetGpu
metadata:
  name: reset-node-1-gpu-0
spec:
  nodeName: node-1
  indices: [0, 1]  # Empty list means all GPUs
status:
  startTime: "2025-10-31T10:00:00Z"
  completionTime: "2025-10-31T10:01:30Z"
  conditions:
    - type: Complete
      status: "True"
      reason: ResetSucceeded
```

### RebootNode
Power cycles a node.
```yaml
apiVersion: maintenance.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: reboot-node-1
spec:
  nodeName: node-1
  force: false
status:
  conditions:
    - type: InProgress
      status: "True"
      reason: RebootInitiated
```

### TerminateNode
Terminates and optionally replaces a node.
```yaml
apiVersion: maintenance.nvidia.com/v1alpha1
kind: TerminateNode
metadata:
  name: terminate-node-1
spec:
  nodeName: node-1
  force: false
status:
  completionTime: "2025-10-31T10:05:00Z"
  conditions:
    - type: Complete
      status: "True"
```

### NodeFault
Reports a hardware fault to the cloud provider.
```yaml
apiVersion: maintenance.nvidia.com/v1alpha1
kind: NodeFault
metadata:
  name: node-1-gpu-memory-fault
spec:
  nodeName: node-1
  faultBehavior: UNRECOVERABLE_GPU_ERROR
  faultStartTime: "2025-10-31T09:00:00Z"
  entitiesImpacted:
    - type: GPU
      value: "GPU-abc123"
  description: "XID 64 with remapping failure"
  nodeDiagnosticReportRef:
    name: node-1-diagnostics
status:
  supportRequestRef:
    name: ticket-12345
  conditions:
    - type: Reported
      status: "True"
```

### NodeDiagnosticReport
Collects diagnostic information from a node.
```yaml
apiVersion: maintenance.nvidia.com/v1alpha1
kind: NodeDiagnosticReport
metadata:
  name: node-1-diagnostics
spec:
  nodeName: node-1
status:
  completionTime: "2025-10-31T09:30:00Z"
  reportPath: "s3://diagnostics/node-1-20251031.tgz"
  conditions:
    - type: Complete
      status: "True"
```

### SupportRequestMonitor
Tracks CSP support ticket status.
```yaml
apiVersion: maintenance.nvidia.com/v1alpha1
kind: SupportRequestMonitor
metadata:
  name: ticket-12345
spec:
  supportRequestId: "CASE-12345-ABC"
status:
  ticketStatus: "IN_PROGRESS"
  recommendedAction: "TERMINATE"
  conditions:
    - type: Active
      status: "True"
```

A controller (dgxc-janitor or equivalent) watches these CRDs and invokes CSP-specific APIs to perform the operations, updating the status fields.

## Rationale

- **Kubernetes-native**: Uses familiar patterns (CRDs, controllers, status conditions)
- **Declarative**: Users declare desired state; controller handles execution
- **Observable**: Status can be queried with `kubectl get`, `kubectl describe`
- **Composable**: Resources reference each other (NodeFault → NodeDiagnosticReport)
- **Auditable**: Operations are recorded as Kubernetes resources in etcd
- **Extensible**: New maintenance types can be added as new CRDs
- **Standardized**: Follows Kubernetes API conventions for broader ecosystem compatibility

## Consequences

### Positive
- Works with standard Kubernetes tooling (kubectl, RBAC, admission controllers)
- Operations are visible in cluster history and audit logs
- Easy to trigger manually for testing or emergency intervention
- Status conditions follow Kubernetes conventions
- Multiple controllers can watch the same CRDs for different CSPs
- Clear separation of concerns (spec = intent, status = reality)

### Negative
- Requires developing and maintaining CRD controllers
- More code than webhook-based integration
- CRDs add to cluster resource consumption
- Operators need to understand Kubernetes resource model
- Status updates require write access to Kubernetes API

### Mitigations
- Provide comprehensive documentation and examples
- Implement controllers using controller-runtime framework for reliability
- Use status subresources to prevent spec/status conflicts
- Include extensive validation in CRD schemas
- Provide CLI tool/plugin for common operations
- Emit metrics on operation success/failure rates

## Alternatives Considered

### Webhook-Based Integration with Existing Tools
**Rejected** because: No single existing tool provides complete coverage across all CSPs and operation types. Webhook-based systems require additional infrastructure (API server, authentication, networking). Status tracking is external to Kubernetes, reducing observability. Integration with Kubernetes RBAC is more complex. The ecosystem benefits from standardized APIs rather than vendor-specific webhooks.

### Single Unified Maintenance CRD
**Rejected** because: A single CRD with a "type" field (Reset/Reboot/Terminate) violates Kubernetes design principles. Different operations have different schemas and validation requirements. RBAC becomes coarse-grained - can't give users permission for resets but not terminations. Separate CRDs provide better type safety and clear intent.

### Job/CronJob Based Execution
**Rejected** because: Jobs are for workload execution, not resource lifecycle management. Job status model doesn't map well to maintenance operations (jobs complete, maintenance may be ongoing). Jobs create pods, which is unnecessary overhead for API calls. CRDs better represent the intent of the operation.

## Notes

- CRD schemas should include extensive validation rules
- Consider admission webhooks for complex validation (e.g., node must be cordoned before termination)
- Status conditions should follow the Kubernetes standard set: InProgress, Complete, Failed
- Owner references can link resources (NodeFault owns NodeDiagnosticReport)
- Future: consider standardizing these APIs through a Kubernetes SIG
- The API is versioned as v1alpha1, allowing evolution before v1beta1/v1

## References

- Kubernetes API Conventions: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md
