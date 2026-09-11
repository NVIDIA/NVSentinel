# ADR-056: `Reliability` — Bounded Provider Signal Retries

> Status: Proposed

## Context

[Issue #1805](https://github.com/NVIDIA/NVSentinel/issues/1805) reports transient failures when the OCI janitor provider submits a reboot. The provider can reject a request temporarily while another instance operation is active.

### Current behavior

The current retry behavior is split across three components:

1. A provider implementation calls its SDK. SDK retry behavior varies by provider. The OCI implementation makes one [`InstanceAction`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor-provider/pkg/csp/oci/oci.go#L122-L129) call without an explicit retry policy.
2. For `SendRebootSignal`, Janitor-provider converts a provider error to gRPC [`Internal`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor-provider/main.go#L89-L97).
3. Janitor treats context deadline errors and gRPC `Unavailable` or `DeadlineExceeded` as [transient](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor/pkg/controller/rebootnode_controller.go#L334-L350).

Janitor [requeues the same `RebootNode` after 30 seconds](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor/pkg/controller/rebootnode_controller.go#L519-L544) for a transient signal error. The CR does not persist a retry count or next retry time. This loop has no signal-submission attempt limit. A non-transient error [sets `SignalSent=False` and `completionTime`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor/pkg/controller/rebootnode_controller.go#L547-L563).

Fault Remediation does not watch maintenance CR status. It watches only event-store and cold-start channels ([code](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/fault-remediation/pkg/reconciler/reconciler.go#L1947-L1968)). Another maintenance CR is possible only when another event is reconciled and the recorded CR is failed or missing ([code](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/fault-remediation/pkg/reconciler/reconciler.go#L1793-L1818)).

Fault Remediation's `maxRemediationAttempts` limits maintenance CR creation for an equivalence group. It does not limit provider calls on one CR. [`AttemptCount`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/fault-remediation/pkg/annotation/annotation_interface.go#L52-L55) is persisted only when the configured limit is greater than zero ([code](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/fault-remediation/pkg/reconciler/reconciler.go#L1544-L1553)).

These retry paths use different meanings of "attempt":

- A signal attempt is one Janitor call to the provider plugin.
- A remediation attempt is one maintenance CR created by Fault Remediation.
- Provider SDK retries can make multiple HTTP calls inside one signal attempt.

A destructive submission is safe to repeat only when the provider confirms that repetition is safe. A lost response can mean that the provider accepted the first request. Retrying that request without provider evidence can reboot the node twice.

## Decision

A maintenance CR represents one requested maintenance operation. Janitor performs bounded signal retries on that CR. Fault Remediation does not create another CR to retry a provider submission error.

### Retry ownership

- The provider plugin decides whether repeating a provider operation is safe.
- Janitor owns signal retry count, backoff, and terminal state for one maintenance CR.
- Fault Remediation continues to own event-to-CR creation, equivalence-group deduplication, and its existing remediation CR limit.

Provider implementations disable SDK retries for destructive submissions by default. A provider can enable a bounded SDK retry policy only when every repeated HTTP request uses provider-supported idempotency. The Janitor attempt limit bounds signal RPCs; it does not count SDK HTTP retries.

A provider can derive its idempotency token from the existing [`SendRebootSignalRequest.cr_name`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/api/proto/csp/v1alpha1/provider.proto#L26-L29). This decision does not add another operation identifier.

### Provider retry contract

Janitor-provider returns gRPC `Aborted` with a standard `google.rpc.RetryInfo` detail only when the provider confirms that it rejected the operation and another signal attempt is safe. Janitor requires both `Aborted` and valid `RetryInfo` to retry.

A bare gRPC code does not authorize retry. A missing or malformed `RetryInfo`, a lost response, and a transport timeout are permanent for automatic retry.

The provider can also attach `google.rpc.ErrorInfo` for diagnostics. `ErrorInfo.reason` records a stable provider-independent reason. The initial reasons are:

- `RESOURCE_BUSY`: the provider cannot accept the operation because it is modifying the resource.
- `REQUEST_TIMEOUT`: the provider request exceeded its deadline.
- `PROVIDER_ERROR`: no more specific stable reason exists.

Add another reason only when a real failure needs distinct operator diagnostics. Janitor does not derive retryability from `ErrorInfo.reason`, provider messages, HTTP status, or gRPC code.

For example, a provider can return both details:

```yaml
retryInfo:
  retryDelay: 30s
errorInfo:
  domain: csp.nvsentinel.nvidia.com
  reason: RESOURCE_BUSY
  metadata:
    provider: example-csp
    provider_code: ResourceBusy
```

Each provider owns the mapping from its SDK errors to this contract. It can use stable SDK classifiers and provider error codes. Provider-specific message matching stays inside the provider package.

### Persisted retry state

Add `signalRetry` to the maintenance CR status:

```yaml
status:
  signalRetry:
    attempts: 1
    nextAttemptTime: "2026-09-10T23:00:30Z"
    lastFailure:
      class: Transient
      reason: RESOURCE_BUSY
  conditions:
    - type: SignalSent
      status: "Unknown"
      observedGeneration: 1
      reason: RetryScheduled
      message: The provider reported that the resource is busy
```

The fields have these meanings:

- `attempts`: the number of provider signal calls that Janitor started.
- `nextAttemptTime`: the absolute time of the scheduled retry. Omit it when no retry is scheduled.
- `lastFailure.class`: `Transient` when `Aborted` and valid `RetryInfo` permit another call. Otherwise, use `Permanent`.
- `lastFailure.reason`: the stable diagnostic reason from `ErrorInfo.reason`.

No retry field belongs in `spec`. The spec contains the requested operation, not controller progress. No new retry annotation or attempt-history array is required.

Janitor sets `observedGeneration` on every `SignalSent` and `NodeReady` condition. The admission webhook rejects all `RebootNode.spec` changes after `signalRetry` or `startTime` is present. Retry state never carries across a spec generation change.

Multiple signal attempts remain on one CR. For example, after the second signal attempt succeeds:

```yaml
status:
  signalRetry:
    attempts: 2
  conditions:
    - type: SignalSent
      status: "True"
      observedGeneration: 1
      reason: Succeeded
      message: request-123
```

Conditions show the current CR state. Logs, metrics, traces, and Kubernetes Events contain per-attempt history.

### State transitions

Janitor persists retry state before each external call:

```text
New CR
  -> set attempts=1 and SignalSent=Unknown/Dispatching
  -> call the provider

Provider accepts the signal
  -> set SignalSent=True
  -> continue the existing readiness loop

Provider returns Aborted with valid RetryInfo and attempts < maxAttempts
  -> set lastFailure.class=Transient
  -> persist nextAttemptTime
  -> requeue the same CR

Provider does not authorize retry
  -> set lastFailure.class=Permanent
  -> set SignalSent=False and completionTime

Provider authorizes retry after the final allowed call
  -> set SignalSent=False/AttemptsExhausted and completionTime
```

Janitor clears `nextAttemptTime` when it starts the next call or reaches a terminal state.

If Janitor restarts while `SignalSent` has reason `Dispatching`, the provider outcome is ambiguous. Janitor fails closed and does not call the provider again.

Readiness checks do not consume signal attempts. The existing operation timeout continues to bound readiness observation after the provider accepts the signal.

### Retry execution policy

Configure signal retry independently from Fault Remediation's `maxRemediationAttempts`:

```yaml
rebootNodeController:
  signalRetry:
    maxAttempts: 3
    initialBackoffSeconds: 30
    maxBackoffSeconds: 600
```

`maxAttempts` includes the first provider call. It must be greater than zero. `initialBackoffSeconds` must be greater than zero. `maxBackoffSeconds` must be greater than or equal to the initial value.

The default policy uses three attempts, a 30-second initial backoff, and a 600-second maximum backoff. Setting `maxAttempts` to one disables signal retry.

To disable signal retry while keeping the initial provider call:

```yaml
rebootNodeController:
  signalRetry:
    maxAttempts: 1
```

Zero is invalid because `maxAttempts` includes the initial call. With a value of one, Janitor makes the initial call but never schedules another call. Provider SDK retries are separate and remain disabled by default for destructive submissions.

After failed attempt `n`, Janitor calculates the backoff cap:

```text
min(maxBackoff, initialBackoff * 2^(n-1))
```

Janitor uses full jitter and selects a delay between zero and the cap. If `RetryInfo.retry_delay` is longer, Janitor uses the provider delay. Janitor persists the resulting absolute `nextAttemptTime` before it returns `RequeueAfter`.

A controller restart reads `attempts` and `nextAttemptTime` from the CR. It does not reset the count or backoff.

### Retry exhaustion

Before a call, Janitor checks the persisted count. If `attempts` is greater than or equal to `maxAttempts`, it does not start another call. Otherwise, it increments and persists `attempts`, sets `SignalSent=Unknown` with reason `Dispatching`, and starts the call. A call that increments `attempts` to `maxAttempts` is allowed.

If the final allowed call returns a retry-safe rejection, Janitor:

1. Sets `SignalSent=False` with reason `AttemptsExhausted`.
2. Clears `nextAttemptTime` and sets `completionTime`.
3. Adds the [`nvsentinel.nvidia.com/preserve`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor/pkg/ttl/ttl.go#L42-L44) annotation with value `"true"`, which [prevents TTL cleanup](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor/pkg/ttl/ttl.go#L134-L138).
4. Emits a metric and Kubernetes Event with the attempt count and last failure reason.
5. Leaves the node quarantined for operator review.

Janitor persists the `preserve` annotation before it writes terminal status. If the metadata update fails, Janitor returns an error and does not set `completionTime`. It writes `SignalSent=False` and `completionTime` only after preservation succeeds.

Janitor uses the same terminal handling for a permanent or ambiguous signal error. No automatic signal retry occurs after terminal failure. An operator removes `preserve` only after verifying provider state and ending the quarantine session. Removing it while the session remains active can let TTL deletion make Fault Remediation treat the CR as missing.

### Compatibility and rollout

The new CR status fields are optional and additive. Existing CRs continue to deserialize.

Before upgrading Janitor, stop new `RebootNode` creation and resolve every nonterminal existing CR. The object alone cannot distinguish an unprocessed new CR from a legacy CR whose provider response was lost. Resume CR creation only after all Janitor replicas run the new version.

Deploy Janitor before or with the provider change. Old Janitor treats `Aborted` as terminal, and new Janitor treats an old provider error as terminal. Mixed versions therefore fail closed. The retry contract does not constrain rollback order.

This decision does not change the meaning of `FaultRemediated`, add a Fault Remediation CR informer, add a node-state annotation, or add a new operation-ID protobuf field.

## Implementation

### Janitor-provider

- Map each provider's retry-safe errors to `Aborted` with `RetryInfo`.
- Add optional `ErrorInfo` diagnostics.
- Return no `RetryInfo` for ambiguous or permanent errors.
- Keep provider SDK retries disabled for destructive submissions unless the provider guarantees idempotency.

### Janitor

- Add `signalRetry` to `RebootNodeStatus` and the CRD.
- Add retry configuration and validation.
- Persist `Dispatching`, attempt count, and retry deadline before external calls.
- Implement bounded exponential backoff with full jitter.
- Reject spec changes after retry processing starts.
- Persist `preserve` before terminal status.
- Preserve terminal signal failures from TTL cleanup.
- Emit retry and exhaustion metrics and Kubernetes Events.

### Fault Remediation

- Keep the current event-to-CR architecture.
- Keep `maxRemediationAttempts` as the separate limit for maintenance CR creation.
- Do not use it to limit Janitor provider calls.

### Testing

- Test that a retry-safe failure persists its schedule and succeeds after restart.
- Test that Janitor waits until `nextAttemptTime`.
- Test that `maxAttempts` includes the final call and prevents later calls.
- Test that exhaustion and permanent failure produce preserved terminal CRs.
- Test that ambiguous timeout, `Unavailable`, and restart from `Dispatching` fail closed.
- Test that a spec change after processing starts is rejected.
- Test that terminal status is not written when preservation fails.
- Test that a healthy node receives no additional reboot signal.
- Test that mixed Janitor-provider versions fail closed in both directions.

## Rationale

- The provider has the information required to authorize a safe retry.
- Janitor owns the maintenance CR and can persist signal retry progress on that CR.
- One CR continues to represent one requested maintenance operation.
- Fault Remediation keeps its current event-to-CR responsibility.
- Standard gRPC details avoid a custom cross-language retry protocol.
- A finite persisted budget prevents unbounded destructive calls.

## Consequences

### Positive

- Bounded signal retries survive Janitor restarts.
- Ambiguous outcomes fail closed without another workflow store.

### Negative

- Providers must classify retry-safe errors.
- Terminal signal failures require operator verification and cleanup.

### Mitigations

- Start with a small provider-specific classifier.
- Document how operators verify provider state and release preserved CRs.

## Alternatives Considered

### Provider-only retry

**Rejected** because: Janitor restart would lose the retry budget and deadline.

### New maintenance CR for each signal retry

**Rejected** because: One requested operation could produce multiple CRs and duplicate destructive calls.

### Infer retryability from gRPC codes or messages

**Rejected** because: Transport errors can have ambiguous outcomes, and messages are unstable.

### Unbounded Janitor requeue

**Rejected** because: Destructive provider calls require a finite retry budget.

## Notes

- The first implementation applies to `RebootNode.SendRebootSignal`.
- The same contract can support other destructive provider submissions after separate review.
- [Pull request #1806](https://github.com/NVIDIA/NVSentinel/pull/1806) implements provider SDK retries only. It does not persist a Janitor retry budget.
- A maintainer must accept this proposed ADR before implementation starts.

## References

- [Issue #1805: OCI janitor provider does not retry](https://github.com/NVIDIA/NVSentinel/issues/1805)
- [Pull request #1806: OCI transient failure retry](https://github.com/NVIDIA/NVSentinel/pull/1806)
- [ADR-005: Kubernetes-Native Maintenance API](005-maintenance-api-design.md)
- [ADR-009: Fault Remediation Triggering](009-fault-remediation-triggering.md)
- [ADR-017: Remediation Plugins](017-remediation-plugins.md)
- [ADR-037: Janitor CR TTL Cleanup](037-janitor-cr-ttl-cleanup.md)
- [gRPC richer error model](https://grpc.io/docs/guides/error/)
