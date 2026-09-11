# ADR-056: `Reliability` — Remediation Retry Ownership and Failure Classification

> Status: Proposed

## Context

The OCI janitor provider can return transient errors when it sends a reboot request. Examples include request timeouts and conflicts while OCI modifies an instance. [Issue #1805](https://github.com/NVIDIA/NVSentinel/issues/1805) reports that these errors currently terminate a `RebootNode` operation.

The remediation path has three retry layers:

1. A CSP SDK can retry one API request.
2. Janitor can [requeue the same maintenance CR after a transient CSP plugin error](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/janitor/pkg/controller/rebootnode_controller.go#L518-L544).
3. Fault Remediation can create another maintenance CR as a new remediation attempt.

These layers do not currently share a failure contract. The janitor-provider converts all CSP failures to gRPC `Internal`. Janitor treats only `Unavailable` and `DeadlineExceeded` as transient. Fault Remediation checks one configured completion condition and cannot distinguish a transient failure from a permanent failure.

Fault Remediation already persists [`AttemptCount`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/fault-remediation/pkg/annotation/annotation_interface.go#L52-L55) in the node remediation-state annotation. [`RecordRemediationAttempt`](https://github.com/NVIDIA/NVSentinel/blob/67240a6c7754feda59488850b5360982e81839ab/fault-remediation/pkg/annotation/annotation.go#L147-L166) increments the count before maintenance CR creation. The count belongs to one equivalence group in one quarantine session. The current global `maxRemediationAttempts` value limits the count when it is greater than zero. Its default value of zero preserves unlimited legacy retries.

Setting `RebootNode.status.conditions[NodeReady]` to `False` for every signal failure does not solve the problem safely. Janitor stops after it sets `completionTime`, but Fault Remediation interprets the false completion condition as permission to create another CR. With an unlimited attempt policy, this can create maintenance CRs indefinitely. A timeout can also have an ambiguous outcome: the provider might have accepted the reboot even when the client did not receive a response.

Fault Remediation currently marks the health event remediated after it creates a maintenance CR. It does not watch maintenance CR status changes. A terminal Janitor status update therefore does not start a new Fault Remediation reconciliation.

The current counter increments before it checks the limit. A denied fourth attempt can therefore leave `AttemptCount=4` when only three attempts ran. Failed CR removal preserves the count, but malformed annotation JSON returns an empty state and silently resets the safety budget.

Maintenance CR names are deterministic from the node and health event. Repeating CR creation after a failure can resolve to the same failed object through `AlreadyExists`. TTL cleanup can later remove that object, after which Fault Remediation treats the missing CR as permission to retry.

The system needs to persist both facts:

- How many remediation attempts have occurred in the current quarantine session.
- Whether the latest terminal failure is transient or permanent for automatic retry.

## Decision

Separate request retry from remediation retry. The CSP plugin owns bounded retries for one idempotent CSP request. Janitor records the terminal result of one maintenance CR. Fault Remediation owns cross-CR retry decisions and the remediation attempt budget.

Persist the workflow state in a versioned node annotation. Persist each attempt result on its maintenance CR while that CR exists.

### Maintenance CR outcome

Add an `AttemptComplete` condition to built-in Janitor maintenance CRs. The condition becomes `True` only when the attempt is terminal. Its reason is the machine-readable outcome:

- `Succeeded`: the requested maintenance operation succeeded.
- `TransientFailure`: the operation was not accepted, and another attempt is safe.
- `PermanentFailure`: automatic retry is not safe or cannot succeed without an external change.
- `Superseded`: another maintenance CR owns the same operation.
- `Cancelled`: the target or quarantine session no longer exists.

Use the condition as follows:

```yaml
status:
  completionTime: "2026-09-10T23:00:00Z"
  conditions:
    - type: AttemptComplete
      status: "True"
      observedGeneration: 1
      reason: TransientFailure
      message: The provider reported that the instance is busy
```

Do not append one `AttemptComplete` condition for each retry. Kubernetes conditions represent the current state of one object, not an attempt history. Each maintenance CR represents one attempt.

For example, the first CR records a transient failure:

```yaml
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: maintenance-node-a-event-b-attempt-1
  annotations:
    nvsentinel.dgxc.nvidia.com/remediation-session-id: session-123
    nvsentinel.dgxc.nvidia.com/remediation-operation-id: operation-456
    nvsentinel.dgxc.nvidia.com/remediation-attempt: "1"
spec:
  force: false
  nodeName: node-a
status:
  completionTime: "2026-09-10T23:00:00Z"
  conditions:
    - type: AttemptComplete
      status: "True"
      observedGeneration: 1
      reason: TransientFailure
      message: The provider reported that the instance is busy
```

Fault Remediation then creates a second CR for the next attempt:

```yaml
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: maintenance-node-a-event-b-attempt-2
  annotations:
    nvsentinel.dgxc.nvidia.com/remediation-session-id: session-123
    nvsentinel.dgxc.nvidia.com/remediation-operation-id: operation-456
    nvsentinel.dgxc.nvidia.com/remediation-attempt: "2"
spec:
  force: false
  nodeName: node-a
status:
  completionTime: "2026-09-10T23:02:00Z"
  conditions:
    - type: AttemptComplete
      status: "True"
      observedGeneration: 1
      reason: Succeeded
      message: The node became ready after the reboot
```

Both CRs use the same session ID and operation ID. The v2 node annotation stores `AttemptCount=2` and `LastOutcome=Succeeded`. It does not store an unbounded history array.

Keep `SignalSent` and `NodeReady` as step conditions. They describe what happened inside the attempt. They do not control cross-CR retries.

For a signal failure, Janitor leaves `NodeReady=Unknown` and sets `AttemptComplete=True`. This prevents an older Fault Remediation instance from interpreting the new status as permission to retry.

Janitor sets `completionTime` only with `AttemptComplete=True`. Janitor includes `observedGeneration` on every terminal condition. The admission webhook rejects maintenance CR spec changes after processing starts.

### Failure classification contract

The CSP plugin classifies the original error before gRPC removes provider-specific information. It returns a canonical gRPC status with a structured `google.rpc.ErrorInfo` detail.

Use this versioned contract:

- `ErrorInfo.domain`: `csp.nvsentinel.nvidia.com`
- `ErrorInfo.reason`: a stable failure reason
- `ErrorInfo.metadata["contract_version"]`: `1`
- `ErrorInfo.metadata["failure_class"]`: `TRANSIENT` or `PERMANENT`
- `ErrorInfo.metadata["operation"]`: `reboot` or `terminate`
- `ErrorInfo.metadata["provider"]`: the configured provider name
- `ErrorInfo.metadata["provider_code"]`: the provider error code, when available
- `ErrorInfo.metadata["http_status_code"]`: the HTTP status code, when available

The initial reason vocabulary contains only the required failure modes:

- `RESOURCE_BUSY`: the provider cannot accept the operation because it is modifying the resource.
- `REQUEST_TIMEOUT`: the provider request exceeded its deadline.
- `PROVIDER_ERROR`: no more specific stable reason exists.

Add a new reason only when a real failure needs distinct operator diagnostics. Do not add reasons to predict future provider behavior.

For example:

```yaml
domain: csp.nvsentinel.nvidia.com
reason: RESOURCE_BUSY
metadata:
  contract_version: "1"
  failure_class: TRANSIENT
  operation: reboot
  provider: example-csp
  provider_code: ResourceBusy
  http_status_code: "409"
```

A valid `failure_class` controls automatic retry. `ErrorInfo.reason` describes the failure and must not control retry. Consumers accept new reason values without changing retry behavior.

A missing detail, malformed detail, unsupported version, or invalid `failure_class` produces `PermanentFailure`. A Go type in janitor-provider can implement this contract, but it is not the public contract.

Each provider plugin selects the canonical gRPC code and `failure_class` from its provider-specific semantics. Consumers must not infer `failure_class` from the gRPC code alone.

A plugin marks an error transient only when repetition is safe. It uses stable SDK classifiers and provider error codes when available. It can use a narrow message predicate only when no stable field exists. All other errors are permanent for automatic retry. No downstream component parses provider messages.

For example, a `RebootNode` readiness success produces `Succeeded`. A safe transient rejection produces `TransientFailure`. Permanent or ambiguous submission errors produce `PermanentFailure`. A readiness timeout also produces `PermanentFailure` by default. A duplicate lock produces `Superseded`, and a missing target node produces `Cancelled`.

### Idempotency

Create one stable operation ID for an equivalence group in one quarantine session. Persist it in the node remediation-state annotation and copy it to every maintenance CR created for that operation.

Add an optional `operation_id` field to the CSP gRPC requests. This protobuf change is additive. Janitor passes the persisted operation ID to the plugin. A provider with idempotency support derives its provider token from this ID and reuses the token across RPC and CR retries.

A provider without durable idempotency must classify an ambiguous outcome as permanent. Fault Remediation does not retry it automatically.

### Retry ownership

The CSP plugin retries transient provider errors within one RPC. These retries are bounded, context-aware, and use one idempotency token.

Janitor can retry communication with the plugin on the same CR. A configurable signal retry deadline bounds this loop. Same-CR retries do not increment the remediation attempt count.

When the same-CR deadline expires, Janitor records one terminal attempt outcome. Explicit safe rejection becomes `TransientFailure`. Ambiguous dispatch becomes `PermanentFailure`.

Fault Remediation watches maintenance CR status through shared informers. It does not mark the health event remediated when it only creates a CR. It updates the event and retry state after it observes `AttemptComplete=True`.

Fault Remediation applies this state machine:

```text
AttemptComplete is absent or not True
  -> keep waiting

AttemptComplete=True, reason=Succeeded
  -> finish the remediation session successfully

AttemptComplete=True, reason=TransientFailure, attempt count below the limit
  -> reserve and create one new maintenance CR

AttemptComplete=True, reason=TransientFailure, attempt count at the limit
  -> declare remediation failed

AttemptComplete=True, reason=PermanentFailure
  -> declare remediation failed

AttemptComplete=True, reason=Superseded
  -> follow the active maintenance CR without consuming another attempt

AttemptComplete=True, reason=Cancelled
  -> close the remediation session without retry
```

An attempt limit includes the first maintenance CR. A limit of three permits the first attempt and two later attempts.

### Retry execution policy

The failure classification decides whether Fault Remediation can retry. The retry policy decides when it retries and when it stops.

The first attempt starts immediately. After attempt `k` returns `TransientFailure`, Fault Remediation calculates this backoff cap:

```text
min(maxBackoff, initialBackoff * 2^(k-1))
```

Fault Remediation uses full jitter and selects the actual delay between zero and the cap. It persists the resulting `NextAttemptAt` value before it schedules another attempt. A controller restart must not reset the delay.

The CSP plugin can use its provider SDK retry policy inside one RPC. That policy must be bounded by the RPC context, reuse the operation ID, and honor a provider retry delay when the SDK supports one.

Fault Remediation checks `maxAttempts` before it reserves a new CR. When `AttemptCount` equals `maxAttempts`, it:

1. Does not create another maintenance CR.
2. Persists `TerminalReason=AttemptsExhausted` in the v2 state.
3. Marks the health event terminal with remediation unsuccessful.
4. Marks the node remediation state failed and keeps the node quarantined.
5. Emits a structured log, metric, and Kubernetes event.

No automatic retry occurs again in the same quarantine session. An operator must resolve the node condition before a new session can start.

For example, this policy allows three attempts:

```yaml
maintenance:
  retryPolicies:
    restart:
      maxAttempts: 3
      initialBackoffSeconds: 30
      maxBackoffSeconds: 600
```

Attempt 1 starts immediately. A transient failure waits up to 30 seconds before attempt 2. Another transient failure waits up to 60 seconds before attempt 3. A transient failure from attempt 3 produces `AttemptsExhausted`; attempt 4 is not created.

### Persisted attempt state and configuration

Create a versioned `remediation-state-v2` node annotation. Do not add safety fields to the existing typed annotation because an old writer drops unknown JSON fields.

Store these fields in each v2 equivalence-group entry:

- `SessionID`: identifies the quarantine session.
- `SessionStartedAt`: rejects stale cancellation events.
- `OperationID`: supplies the cross-attempt idempotency key.
- `AttemptCount`: counts attempts that were reserved.
- `PendingAttempt`: records the reserved attempt number and deterministic CR name.
- `NextAttemptAt`: preserves the backoff deadline across restarts.
- `LastOutcome`: preserves terminal outcome after CR deletion.
- `TerminalReason`: records why the session stopped.

Treat malformed or unsupported annotation state as an error. Fault Remediation must stop instead of returning an empty state.

Reserve an attempt atomically before CR creation. The reservation checks the limit before it increments the counter. A rejected reservation does not increment `AttemptCount`. A failed CR creation consumes the reserved attempt. A reconciliation after a crash reuses `PendingAttempt` instead of reserving another number.

Use a unique deterministic name for each attempt. Include a bounded hash and the attempt number. `AlreadyExists` is success only when the existing CR carries the same session ID, operation ID, and attempt number.

Copy the health event ID, equivalence group, session ID, operation ID, and attempt number to maintenance CR annotations. Fault Remediation uses these values to map informer events back to persisted workflow state.

Define retry limits by equivalence group because the persisted counter has that scope:

```yaml
maintenance:
  retryPolicies:
    restart:
      maxAttempts: 3
      initialBackoffSeconds: 30
      maxBackoffSeconds: 600
  actions:
    RESTART_VM:
      completeConditionType: NodeReady
      attemptOutcomeConditionType: AttemptComplete
      equivalenceGroup: restart
```

The configured policy name must match `equivalenceGroup`. All actions in one group therefore share one limit. `maxAttempts` must be greater than zero.

Keep `completeConditionType` unchanged for legacy status handling. Add the optional `attemptOutcomeConditionType` and `retryPolicies` keys. Keep the existing global `maxRemediationAttempts` key and its zero-value semantics for actions without the new contract.

Persist `LastOutcome` before removing a CR reference. A missing or TTL-deleted CR uses the persisted outcome. If neither the CR nor a persisted outcome exists, classify the result as permanent and do not retry.

A cancellation event clears state only when it belongs to the current session. An older cancellation event cannot clear a newer session budget.

### Compatibility and rollout

The new condition, protobuf field, CR annotations, and configuration keys are additive. Existing objects continue to deserialize.

New Fault Remediation reads `AttemptComplete` when the configured condition exists. It falls back to the unchanged `completeConditionType` for CRs written by an older Janitor. Legacy `NodeReady=True` means success. Legacy `NodeReady=False` becomes `PermanentFailure` and cannot start an automatic retry. Legacy `NodeReady=Unknown` remains in progress.

An older Fault Remediation instance continues to read `NodeReady`. A new Janitor leaves `NodeReady=Unknown` for every non-success terminal outcome, including signal failure, readiness timeout, readiness-check failure, and superseded work. The older instance therefore fails closed.

Use a two-release rollout because an already released Fault Remediation binary does not understand the v2 annotation:

1. Release a compatibility Fault Remediation version that detects `remediation-state-v2` and stops legacy processing for that node.
2. Deploy the compatibility version to all Fault Remediation replicas.
3. Release and enable v2 Fault Remediation with the new informer, state format, outcome handling, and legacy fallback.
4. Release Janitor with `AttemptComplete`.
5. Release CSP plugins with the operation ID and structured error details.

Do not roll back Fault Remediation below the compatibility version while any v2 annotation exists. The upgrade and rollback runbooks must check this condition.

An old plugin returns no valid failure detail. Janitor records `PermanentFailure`, which stops automatic retries.

## Implementation

### Janitor-provider

- Implement the versioned `google.rpc.ErrorInfo` contract in shared gRPC server code.
- Map provider SDK errors to transient or permanent.
- Add the optional `operation_id` protobuf request field and regenerate bindings.
- Keep provider SDK retries bounded and context-aware.
- Reuse the operation ID for provider idempotency where the provider supports it.
- Treat ambiguous timeouts as permanent when the provider cannot prove idempotency.

Each provider implementation uses its SDK retry classifier where available. Provider-specific exceptions stay inside that provider package.

### Janitor

- Decode the structured gRPC failure detail.
- Pass the operation ID to the plugin.
- Bound same-CR signal retries with a persisted deadline.
- Set `AttemptComplete` and `completionTime` when the attempt ends.
- Keep `SignalSent` and `NodeReady` for step-level observability, but leave `NodeReady=Unknown` for non-success terminal outcomes.
- Record `PermanentFailure` when the plugin returns an unstructured, malformed, or unsupported error detail.
- Set `observedGeneration` and reject spec mutation after processing starts.

### Fault Remediation

- Add shared informers for every configured maintenance GVK and enqueue status changes.
- Stop marking a health event remediated immediately after CR creation.
- Extend the CR status checker to return the attempt outcome.
- Keep compatibility with `completeConditionType` and map an unclassified legacy failure to permanent.
- Add `attemptOutcomeConditionType` and equivalence-group retry policies.
- Store and validate retry safety state in the separate v2 node annotation.
- Add the v2 guard in a compatibility release before enabling v2 writers.
- Reserve each attempt atomically and reuse pending reservations after a crash.
- Create another CR only for `TransientFailure` and only below the configured limit.
- Use a unique deterministic CR name for each attempt.
- Persist terminal outcomes before a CR can be deleted.
- Set the node state to `remediation-failed` for permanent or exhausted failures.
- Mark the health event terminal when no more attempts are allowed.

### Documentation and observability

- Document the equivalence-group retry policy in the Helm values and configuration guide.
- Emit metrics for transient, permanent, and exhausted outcomes.
- Include the session ID, operation ID, attempt number, and outcome in structured logs and traces.
- Document how operators manually start a new session after they verify a permanent outcome.

### Testing

- Test that a healthy node does not receive an additional reboot request.
- Test that one operation reuses its idempotency token across RPC and CR retries.
- Test that a permanent failure creates no additional maintenance CR.
- Test that an unclassified or ambiguous failure creates no additional maintenance CR.
- Test that a transient failure creates no more than the configured number of CRs.
- Test that a denied attempt does not increment the count.
- Test that pending attempt reservations survive crashes.
- Test that attempt counts and outcomes survive controller restarts, CR removal, and TTL cleanup.
- Test that malformed annotation state blocks remediation.
- Test that a stale cancellation cannot clear a newer session.
- Test every terminal `RebootNode` path.
- Test rolling combinations of old and new Fault Remediation, Janitor, and janitor-provider versions.

## Rationale

- A CSP plugin has the provider-specific information needed to classify failures.
- Janitor owns the maintenance CR and must publish an accurate attempt result.
- Fault Remediation owns the remediation workflow and must control cross-CR attempts.
- An explicit outcome condition is observable and does not require message parsing.
- A finite equivalence-group limit prevents infinite destructive remediation loops.
- Ambiguous and unclassified outcomes become permanent and fail closed.
- Legacy fallback supports rolling upgrades and existing CRs.

## Consequences

### Positive

- Transient CSP failures can recover without losing the remediation request.
- Permanent failures stop without creating repeated CRs.
- Ambiguous outcomes do not cause an automatic duplicate reboot.
- Operators can inspect the session, attempt count, and failure class.
- Retry ownership is explicit across all three modules.
- CR deletion cannot erase the retry decision.

### Negative

- The status contract becomes more complex.
- Custom maintenance controllers must publish the configured conditions to use classified retries.
- Ambiguous failures require operator verification before a new session.
- The rollout spans Fault Remediation, Janitor, and CSP plugins.
- Fault Remediation must watch dynamically configured maintenance CRDs.
- Equivalence-group retry policy adds Helm and TOML configuration.

### Mitigations

- Keep the legacy completion key and add an optional outcome key.
- Treat missing classification as permanent.
- Keep the existing global attempt setting unchanged.
- Provide tested built-in defaults for Janitor CRs.
- Persist workflow state before maintenance CR cleanup.
- Add runbook steps for ambiguous outcomes and manual recovery.

## Alternatives Considered

### Keep all retries in the provider or Janitor

**Rejected** because: Neither component owns the quarantine-session attempt budget.

### Retry every failed maintenance CR

**Rejected** because: Permanent and ambiguous failures can cause repeated destructive operations.

### Store retry state only on maintenance CRs

**Rejected** because: Each attempt creates a new CR, and TTL cleanup removes old CRs.

### Infer retryability from gRPC codes or messages

**Rejected** because: Codes do not show whether the provider accepted a request, and messages are not stable interfaces.

### Reuse the unlimited legacy attempt policy

**Rejected** because: Destructive retries need a finite limit. Changing the meaning of the existing zero value would break compatibility.

## Notes

- This decision does not require direct calls between Fault Remediation and Janitor.
- Coordination continues through maintenance CR status and versioned node annotations.
- The first implementation target is `RebootNode`.
- Other built-in maintenance CRs can adopt the same contract in later changes.
- An action that uses this retry contract must set `maxAttempts` to a value greater than zero. It cannot use unlimited automatic retries.
- Provider-specific request retries are separate from cross-CR retries and this status contract.
- A maintainer must accept this proposed ADR before implementation starts.

## References

- [Issue #1805: OCI janitor provider does not retry](https://github.com/NVIDIA/NVSentinel/issues/1805)
- [Pull request #1806: OCI transient failure retry](https://github.com/NVIDIA/NVSentinel/pull/1806)
- [ADR-005: Kubernetes-Native Maintenance API](005-maintenance-api-design.md)
- [ADR-009: Fault Remediation Triggering](009-fault-remediation-triggering.md)
- [ADR-017: Remediation Plugins](017-remediation-plugins.md)
- [ADR-037: Janitor CR TTL Cleanup](037-janitor-cr-ttl-cleanup.md)
- [Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md)
- [gRPC status codes](https://grpc.io/docs/guides/status-codes/)
