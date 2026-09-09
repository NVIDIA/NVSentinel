# ADR-056: Janitor — GPU Reset Provider Delegation

## Context

Janitor executes three remediation actions, each driven by its own CR: `RebootNode`, `TerminateNode` and `GPUReset`. Two of the three are pluggable. The third is not.

`RebootNode` and `TerminateNode` delegate the work to a gRPC service. The controller dials `CSPProviderService` and calls `SendRebootSignal`, `IsNodeReady` or `SendTerminateSignal`. Janitor keeps the CR lifecycle, the node lock, the metrics and the timeout. The provider decides how to reboot or terminate the machine. [ADR-017](017-remediation-plugins.md) made this decision, and stated the goal plainly: end users must be able to supply their own remediation backend without forking janitor.

`GPUReset` never received the same treatment. It was designed separately in [ADR-019](019-janitor-gpu-reset.md) and [ADR-020](020-nvsentinel-gpu-reset.md) as a Kubernetes Job, and the Job is built in Go:

```go
// janitor/pkg/config/default.go
func getDefaultGPUResetJobTemplate(namespace string, image string, secrets []ImagePullSecret,
	resources ResourceRequirements, runtimeClassName string, writeSyslogEvent bool,
	uploadURL string) (*batchv1.JobTemplateSpec, error)
```

The signature is the whole configuration surface. Everything else in the pod spec is a compile-time constant, including the host paths:

```go
HostDevPath    = "/dev"
DriverRootPath = "/run/nvidia/driver"
HostSysPath    = "/sys"
```

This produces two gaps. An operator cannot replace the reset procedure, and an operator cannot adjust the Job that performs it. There is no hook before the reset, no hook after it, and no way to add a condition to the CR.

Evidence shows the seam was planned and then not built. `GPUResetControllerConfig` carries a `CSPProviderHost` field, and the janitor chart renders `gpuResetController.cspProviderHost` into the ConfigMap. No code reads either one.

### Observed cost

One downstream deployment runs GPU reset on Blackwell bare metal across four infrastructure providers. It cannot use the built-in reset, so it maintains a patched build of janitor. The patches divide cleanly along the four missing extension points:

| Downstream patch | Missing extension point |
|---|---|
| Change the reset Job template signature to accept per-provider driver roots and host services | The Job pod spec is not configurable |
| Add a reconciler that gates provider-owned DaemonSets before a reset | There is no pre-reset hook |
| Add a client barrier, a timeout latch and durable host-service restoration to the reconciler | There is no post-reset hook, and the condition and reason enums are closed |
| Extend the Helm chart and RBAC to carry the above | Follows from the three above |

The specific policy in that fork is deployment-specific and does not belong upstream. The absence of any seam to hold it is the defect this record addresses.

## Decision

Make GPU reset pluggable through two additive changes.

1. **Add `SendGPUResetSignal` and `IsGPUResetComplete` to the existing `CSPProviderService`.** When `gpuResetController.cspProviderHost` is set, the GPUReset controller delegates the reset to the provider instead of creating a Job. This completes ADR-017 for the third action.
2. **Add `gpuResetController.resetJob.podTemplateOverride`.** This is a strategic merge patch applied to the built-in Job pod template. It serves deployments that keep the built-in reset but need a different host layout.

Both paths are opt-in. When neither value is set, behaviour does not change.

## Implementation

### Protobuf

Add two RPCs and four messages to `api/proto/csp/v1alpha1/provider.proto`:

```protobuf
service CSPProviderService {
  rpc SendRebootSignal(SendRebootSignalRequest) returns (SendRebootSignalResponse) {}
  rpc IsNodeReady(IsNodeReadyRequest) returns (IsNodeReadyResponse) {}
  rpc SendTerminateSignal(SendTerminateSignalRequest) returns (SendTerminateSignalResponse) {}

  rpc SendGPUResetSignal(SendGPUResetSignalRequest) returns (SendGPUResetSignalResponse) {}
  rpc IsGPUResetComplete(IsGPUResetCompleteRequest) returns (IsGPUResetCompleteResponse) {}
}

message SendGPUResetSignalRequest {
  string node_name = 1;
  string cr_name = 2;
  // Empty selectors mean every GPU on the node, which matches the optional
  // selector on GPUResetSpec.
  repeated string gpu_uuids = 3;
  repeated string gpu_pci_bus_ids = 4;
}

message SendGPUResetSignalResponse {
  string request_id = 1;
}

message IsGPUResetCompleteRequest {
  string node_name = 1;
  string request_id = 2;
}

message IsGPUResetCompleteResponse {
  bool is_complete = 1;
  bool succeeded = 2;
  string message = 3;
}
```

`IsGPUResetCompleteResponse` carries two booleans, where `IsNodeReadyResponse` carries one. The reason is that the two operations fail differently. A rebooting node cannot report its own failure, so janitor waits for the timeout. A GPU reset can run to completion and fail. The provider knows that outcome, so it reports it, and janitor records a real reason instead of waiting out a 25 minute timeout.

Adding RPCs to an existing service is wire-compatible in both directions. An old janitor never calls the new methods. A new janitor against an old provider receives `codes.Unimplemented`, which is a clear signal rather than an error.

### Controller state machine

`reconcileHelper` advances through seven steps, each gated on a status condition. Delegation replaces two of them:

| Step | Condition | Built-in path | Delegated path |
|---|---|---|---|
| 1 | — | `initialize` | unchanged |
| 2 | `Ready` | `isReady` | unchanged |
| 3 | `ServicesTornDown` | `tearDownServices` | unchanged |
| 4 | `ResetJobCreated` | `createJob` | `SendGPUResetSignal` |
| 5 | `ResetJobCompleted` | `checkJobStatus` | `IsGPUResetComplete` |
| 6 | `ServicesRestored` | `restoreServices` | unchanged |
| 7 | `Complete` | `reconcileCompletion` | unchanged |

Janitor keeps the CR lifecycle, the distributed node lock, maintenance contention, the finalizer, metrics and tracing. The provider owns only the reset itself. This is the same division of labour that reboot and terminate already use.

The condition type names stay as they are. `ResetJobCreated` and `ResetJobCompleted` are read by external automation, so renaming them would break it. Only the work behind each condition changes.

`GPUResetReconciler` gains a `dialProviderFunc` field. Copy the pattern from `RebootNodeReconciler.dialProvider`, including the fresh connection per reconcile that picks up rotated CA bundles and ServiceAccount tokens from disk.

### Fallback

The controller selects a path from configuration, not from probing:

- `cspProviderHost` empty: create the Job. This is the default.
- `cspProviderHost` set: call the provider.
- `cspProviderHost` set and the provider returns `codes.Unimplemented`: log a warning, then create the Job.

The third case covers a rolling upgrade where a new janitor runs against an old provider. Treat every other gRPC error the way `RebootNodeReconciler` does. Requeue on `Unavailable` and `DeadlineExceeded`, and fail the CR on anything else.

### Service teardown when delegating

A provider that performs its own teardown does not need janitor's. The existing configuration already covers this. `tearDownServices` skips its work and sets `ServicesTornDown=True` with `ReasonSkipped` when the manager has no apps, so an operator sets `serviceManager.name: ""` and the provider takes over.

No new configuration is needed. Document the interaction, because running both teardowns is a real misconfiguration.

### Job pod template override

Add one Helm value:

```yaml
gpuReset:
  resetJob:
    # Strategic merge patch applied over the built-in reset Job pod template.
    # Use it to change host paths, volumes, tolerations or the security context
    # for a specific hardware or driver layout. Janitor still injects the
    # container name, the GPU selector environment variable and the node name.
    podTemplateOverride: {}
```

Apply the patch with `strategicpatch.StrategicMergePatch` against the rendered default, after `getDefaultGPUResetJobTemplate` returns and before the template is stored on `ResolvedJobTemplate`. Validate at startup, so a malformed patch fails the pod rather than the first reset.

A merge patch is deliberate. A full template replacement would let the pod spec drift from the fields janitor must control, and would stop the deployment from inheriting later upstream fixes.

### Configuration reference

Both values already have a home. `gpuResetController.cspProviderHost` renders today and needs no chart change, only a controller that reads it. Document both in `docs/configuration/janitor.md` and describe the provider side in `docs/configuration/janitor-provider.md`.

### RBAC

Janitor needs no new permissions. It creates fewer resources when it delegates, not more.

A provider that implements GPU reset by creating its own Job needs `batch/jobs` in its own namespace. `janitor-provider` already carries that Role for the `generic` reboot provider, so the pattern exists. An out-of-tree provider declares whatever its own implementation requires.

### Reference implementation

Add a `SendGPUResetSignal` and `IsGPUResetComplete` pair to `janitor-provider` behind the existing `model.CSPClient` interface, with the `kind` provider returning a simulated success. This keeps the E2E suite able to exercise the delegated path without real hardware. The cloud providers return `Unimplemented`, because none of them exposes a device-level GPU reset API today.

### Observability

Reuse the existing `metrics.ActionTypeGPUReset` counters and MTTR histogram, so dashboards keep working across both paths. Add one label, `path`, with values `job` and `provider`, so an operator can confirm which path a cluster is running.

## Rationale

- **It completes a decision already taken.** ADR-017 chose gRPC delegation for remediation backends. Reset was omitted because it was designed on a separate track, not because delegation was judged wrong for it.
- **It matches the sibling controllers exactly.** An engineer who has read `rebootnode_controller.go` can read the delegated reset path without learning a new mechanism.
- **Safety-critical orchestration stays in janitor.** The node lock, the contention check and the finalizer are the parts that protect healthy nodes. Those stay under test upstream. The provider supplies only the hardware-specific step.
- **The override is the cheap majority of the value.** Many deployments need a different driver root and nothing else. They should not have to run a gRPC service to get one.
- **It removes a fork rather than adding a feature.** At least one deployment maintains patched janitor builds today. Patched builds do not receive upstream fixes, which is a safety problem in code that reboots and resets production GPUs.

## Consequences

### Positive

- Users implement GPU reset for hardware and layouts that upstream does not model, without forking.
- Deployments with an unusual driver root need one Helm value instead of a patched build.
- Providers can add gates and barriers before a reset, and restoration after one, because they own the whole step.
- Reset reaches the same extensibility level as reboot and terminate, so the three actions are consistent.

### Negative

- One more surface in `CSPProviderService` to keep compatible.
- A provider that implements reset now holds a safety-critical responsibility. A bad implementation can reset a GPU that carries live work.
- Two code paths for GPU reset, so tests must cover both.
- The strategic merge patch lets an operator produce a Job that cannot work.

### Mitigations

- The proto change is purely additive, and `Unimplemented` gives a defined fallback, so no rollout ordering is required between janitor and a provider.
- Janitor keeps the node lock, the contention check and the drain precondition. A provider cannot reset a node that janitor has not already released for maintenance.
- Add envtest coverage for both paths, and a test proving the delegated path does not fire on a healthy node, as remediation code requires.
- Validate the merge patch at startup, and document the fields janitor injects.

## Alternatives Considered

### A separate `GPUResetProviderService`

**Rejected** because: it needs a second endpoint, with its own TLS certificate, audience, Service and discovery, for no benefit. The provider already receives the node identity it needs from the existing service. A separate service also allows a split-brain deployment where reboot and reset point at different backends for the same node.

### Pre-reset and post-reset hook RPCs around the existing Job

**Rejected** because: it leaves the Job template problem unsolved, so a deployment with a different driver root still cannot use the built-in reset. It also adds more RPCs than delegation while giving less, because janitor still owns the reset procedure and the provider can only observe it.

### The pod template override alone

**Rejected as a complete answer**, and adopted as half of the decision. It fixes host layout, which is the most common need. It cannot express a gate before the reset, a barrier on external device clients, or restoration after a failure. Those need the provider to own the step.

### Route GPU reset through `ExternalRemediationRequest`

**Rejected** because: ERR ([ADR-040](040-external-remediation-request.md)) transfers ownership of a whole node. The release taint and the `managed=false` label take the node out of service and evict the monitors. That discards the property ADR-019 exists to provide, which is resetting one GPU while the other GPUs on the node keep running. ERR stays the right mechanism for out-of-cluster work such as an RMA.

### A full `jobTemplate` replacement instead of a merge patch

**Rejected** because: the deployment inherits no later upstream change to the Job, and janitor loses control of the container name, the GPU selector environment variable and the node name that its own code depends on.

## Notes

### Non-goals

- No new remediation action, and no change to how `fault-remediation` maps a recommended action to a CR.
- No change to the `GPUReset` CRD schema. Delegation reuses the existing conditions.
- No upstream implementation of any specific vendor or provider reset. Upstream ships the seam and a simulated `kind` implementation.
- No change to reboot or terminate.

### Backwards compatibility

| Surface | Effect |
|---|---|
| Protobuf | Two RPCs and four messages added. No field is renumbered, retyped or removed |
| CRD | No change. Condition types and reasons are unchanged |
| Helm values | One key added, `resetJob.podTemplateOverride`. One existing key, `cspProviderHost`, starts being read |
| Metrics | One label added, `path`. No metric is renamed |
| Event store | No change |

`gpuResetController.cspProviderHost` needs care during review. The key renders today and does nothing, so a cluster that already sets it would begin delegating after an upgrade. Reviewers should decide whether to read that key or introduce a separate one. This record proposes reading the existing key, because the field was clearly added for this purpose, but the risk is real and the maintainers own the call.

### Responsibility

When a provider implements GPU reset, it becomes responsible for not resetting a GPU that carries live work. Janitor still guarantees that the node reached the maintenance state before the call. Everything after that is the provider's. Say this plainly in the provider documentation.

## References

- [ADR-017: Architecture — Remediation Plugins](017-remediation-plugins.md) — chose gRPC delegation for remediation backends. This record completes it for the third action
- [ADR-019: Janitor Support for GPU Reset](019-janitor-gpu-reset.md) — the Job-based reset design this record makes optional
- [ADR-020: NVSentinel Support for GPU Reset](020-nvsentinel-gpu-reset.md) — the end-to-end reset flow
- [ADR-028: Janitor — Generic Bare-Metal Reboot Provider](028-generic-baremetal-reboot-provider.md) — a provider that creates its own Job, and the closest existing pattern
- [ADR-040: API — External Remediation Request (ERR)](040-external-remediation-request.md) — node-level ownership transfer, considered and rejected for this purpose
- [`api/proto/csp/v1alpha1/provider.proto`](../../api/proto/csp/v1alpha1/provider.proto) — the service this record extends
