# ADR-049: API — ExternalFault (EF)

## Context

[ADR-040](040-external-remediation-request.md) introduced the `ExternalRemediationRequest` (ERR) — the **exit door** from NVSentinel node ownership. When NVSentinel detects a fault it cannot remediate itself, it creates an ERR, releases the node to an external system, and waits for that system to signal completion.

ADR-040 covers the NVSentinel-initiated direction. It does not cover the reverse: an external system that *proactively* needs a node drained before it can begin work. In that case NVSentinel does not generate a fault; the external system has received a signal from outside the cluster — a CSP maintenance notification, a hardware-monitoring alert, an external event source — and needs NVSentinel to prepare the node on its behalf.

Without a formal entry point for this direction, external automation must side-step NVSentinel (cordoning nodes directly, fighting over taints) or rely on custom integrations that duplicate NVSentinel's quarantine and drain logic. Both outcomes undermine the ownership model established by ADR-040.

## Decision

Introduce a new CRD, `ExternalFault` (EF), in the existing `nvsentinel.dgxc.nvidia.com` API group. EF is the **entry door** into NVSentinel ownership transfer from an external system:

- Created by an external system or human operator when it determines a node must be drained and released before external repair work can begin.
- The EF reconciler emits a `CUSTOM` health event to the platform-connector. Provided the pipeline is configured to act on that event (see [Pipeline dependencies](#pipeline-dependencies)), this drives the normal NVSentinel flow: quarantine (cordon), drain, and ERR creation by `fault-remediation`.
- The ERR created from an EF-triggered health event carries an `OwnerReference` pointing to the parent EF, linking the two coordination objects for the lifetime of the repair.
- The EF reconciler watches its child ERR (when one exists) and uses the ERR reporting its remediation complete as the internal trigger to clear the fault — it does **not** surface the ERR's state as an EF condition. The external system never clears faults in NVSentinel: a node is only un-quarantined when the source that raised the fatal health event submits a matching `isHealthy=true` event. EF *is* that source for an external-origin fault, so when the repair completes it emits a second health event — a plain `isHealthy=true` event for the same node/check — to retract the fault it raised, and sets `FaultCleared=True` once that clearing event is submitted.
- `CompletionTime` is set on the EF when `FaultCleared=True` (the clearing event was submitted) or on operator-delete (force close).

EF and ERR together form the complete bilateral handoff API. ERR is the outbound signal ("NVSentinel is releasing this node"); EF is the inbound signal ("an external system needs this node released").

Human operators follow the same path as automated systems: creating an EF CR is the sanctioned mechanism for requesting a node drain before manual intervention.

## ExternalFault Reconciler

Where the EF reconciler is hosted is a distinct, open decision. It is unusual among NVSentinel controllers because it needs three capabilities that no single existing component provides together:

1. **A controller-runtime manager + validating webhook** — to reconcile the EF CRD (finalizers, status, `Owns(&ExternalRemediationRequest{})`) and to enforce admission checks (duplicate-node, `nodeName` immutability, node existence).
2. **A health-event emitter** — a `healthpub.Publisher` over the platform-connector's node-local socket, to submit both the opening `CUSTOM` event and the closing `isHealthy=true` event.
3. **Proximity to the ERR** it watches — the `Owns`-watch reacts to the child ERR's `ExternalRemediationComplete` status, which is set by the ERR reconciler.

No component has all three, so each option grafts on what it lacks. The [Implementation](#implementation) section below is written against **Option 1**.

### Option 1 — janitor (reference implementation)

Host the EF reconciler in `janitor`, alongside the ERR reconciler and the shared validating-webhook server.

- **Pros:** janitor is already a controller-runtime manager with a webhook server (RebootNode/TerminateNode/GPUReset/ERR), and the ERR reconciler — whose status the EF watches — lives here, so capabilities (1) and (3) come for free. Smallest change.
- **Cons:** janitor emits no health events today; EF makes it an emitter, adding the platform-connector socket mount and a `healthpub` client (see [Health-event emission](#health-event-emission-janitor--platform-connector)). That is a new outbound dependency for a component that previously spoke only to the Kubernetes API.

### Option 2 — csp-health-monitor

Host the EF reconciler in `csp-health-monitor` (or another health monitor).

- **Pros:** health monitors are native emitters — `csp-health-monitor` already mounts the platform-connector socket and holds a `healthpub.Publisher`, so capability (2) is free. It also already detects CSP maintenance signals (AWS/GCP), the canonical EF trigger, so the component that observes external faults would also own their lifecycle — a good fit with EF's "synthetic monitor" framing.
- **Cons:** `csp-health-monitor` is a poll/emit loop, **not** a controller-runtime app — no manager, no CRD reconcilers, no webhook server, and no leader election (it runs as a multi-replica Deployment). Hosting EF here means grafting on the entire controller-runtime + webhook stack (capability 1). It also splits the EF↔ERR pair across components: `csp-health-monitor` would import janitor's CRD types and `Owns`-watch a janitor-owned CRD, and the EF webhook would either need a new server here or stay behind in janitor.

### Option 3 — standalone component

Introduce a dedicated component (e.g. an `external-fault-controller`) that owns the EF reconciler, its webhook, and its emitter — optionally owning the ERR reconciler as well, making it the single home for the bilateral handoff API.

- **Pros:** cleanest bounded context. EF and ERR are the two halves of one handoff; owning both in one binary makes the `Owns`-watch and the shared webhook trivial and keeps the handoff logic in one place. No capability is grafted onto a component whose role it doesn't fit.
- **Cons:** the largest lift — a new component (image, chart, RBAC, webhook certs, leader election, deployment) to build and operate. Realizing the full benefit means relocating the ERR reconciler out of janitor (ADR-040 already ships it there), a migration in its own right. If EF alone moves and ERR stays, this collapses toward Option 1's split without Option 1's low cost.

Option 1 is the least-change path and the shape the rest of this ADR describes; Option 3 is the cleanest long-term structure but the largest investment. This choice is deferred to team review.

## Implementation

### Module layout

```text
data-models/
├── protobufs/
│   └── external_fault.proto               (new)
└── pkg/protos/
    └── external_fault.pb.go               (generated)

distros/kubernetes/nvsentinel/charts/janitor/
├── crds/
│   └── external_fault.crd.yaml            (generated by protoc-gen-crd)
└── templates/
    ├── deployment.yaml                    (extended — platform-connector socket mount + flag)
    ├── clusterrole.yaml                   (extended — externalfaults RBAC)
    └── webhook.yaml                       (extended — EF validating webhook)

janitor/
├── api/v1alpha1/
│   ├── external_fault_register.go         (new — scheme registration + wrapper type)
│   ├── external_fault_json.go             (new — protojson marshal for spec/status)
│   └── external_remediation_register.go   (extended — registers EF into the shared group scheme)
├── pkg/controller/
│   └── externalfault_controller.go        (new — EF reconciler)
├── pkg/webhook/v1alpha1/
│   └── janitor_webhook.go                 (extended — EF validator added)
└── main.go                                (extended — EF controller + platform-connector emitter + EF TTL)
```

### CRD schema

The CRD is proto-generated, matching the ERR pattern:

```proto
message ExternalFaultSpec {
  // healthEvent describes the fault that caused the external system to
  // request node ownership. The EF reconciler re-emits this event into the
  // NVSentinel pipeline (with recommendedAction overridden to CUSTOM) so
  // the normal quarantine → drain → ERR flow fires.
  HealthEvent healthEvent = 1;
}

message ExternalFaultStatus {
  repeated Condition conditions = 1;

  // completionTime is set when the EF reconciler is done — FaultCleared=True
  // (successful repair and node return) or on operator-delete. Matches the
  // CompletionTime semantics used by ERR and the other janitor CRDs.
  google.protobuf.Timestamp completionTime = 2;
}

message ExternalFault {
  option (protoc_gen_crd.k8s_crd) = {
    api_group: "nvsentinel.dgxc.nvidia.com",
    kind: "ExternalFault",
    plural: "externalfaults",
    singular: "externalfault",
    short_names: ["ef"],
    categories: ["nvsentinel"],
    scope: ST_CLUSTER
  };
  ExternalFaultSpec spec = 1;
  ExternalFaultStatus status = 2;
}
```

### Example

```yaml
apiVersion: nvsentinel.dgxc.nvidia.com/v1
kind: ExternalFault
metadata:
  name: csp-maintenance-ip-10-0-31-7
spec:
  healthEvent:
    agent: external-system
    checkName: csp-scheduled-maintenance
    componentClass: node
    customRecommendedAction: external-remediation
    entitiesImpacted:
      - entityType: Node
        entityValue: ip-10-0-31-7.us-west-2.compute.internal
    errorCode:
      - CSP-MAINT-AWS-EBS-RETIRE
    generatedTimestamp: "2026-05-13T02:00:00Z"
    id: he-mst-c6d92aa1-2f6e-4e8b-9e3d-b75f86b1aaaa
    isFatal: false
    isHealthy: false
    message: "AWS scheduled maintenance 2026-05-13T03:00Z; node must be drained."
    metadata:
      cluster: nvcf-dgxc-k8s-aws-usw2-prod
      cspEventId: evt-0a9bc8e74e2c2c10c
      source: aws-health
    nodeName: ip-10-0-31-7.us-west-2.compute.internal
    recommendedAction: CUSTOM
    version: 1
status:
  conditions:
    - lastTransitionTime: "2026-05-13T02:00:01Z"
      message: >-
        Submitted CUSTOM health event he-mst-c6d92aa1 to platform-connector
        on node ip-10-0-31-7.us-west-2.compute.internal.
      observedGeneration: 1
      reason: HealthEventEmitted
      status: "True"
      type: FaultReported
    - lastTransitionTime: "2026-05-13T02:00:01Z"
      message: Fault active; clearing health event not yet emitted.
      observedGeneration: 1
      reason: FaultActive
      status: "False"
      type: FaultCleared
```

### Status conditions

| Condition | Initial | Terminal values | Meaning |
|---|---|---|---|
| `FaultReported` | `Unknown (Initializing)` | `True (HealthEventEmitted)` | Health event successfully submitted to platform-connector. |
| `FaultCleared` | `False (FaultActive)` | `True` | The EF reconciler submitted the clearing (`isHealthy=true`) health event that retracts the fault it raised, letting the normal quarantine-recovery path un-cordon the node. Emitted once the child ERR reports its remediation complete. |

The EF surfaces only these two conditions. It deliberately does **not** mirror the ERR's `ExternalRemediationComplete` — that state lives on the ERR, and not every EF necessarily produces an ERR. Until `FaultCleared=True`, the EF is simply "open," regardless of whether an ERR is in progress, has failed, or was never created.

`CompletionTime` is stamped by the EF reconciler when `FaultCleared=True` or on the operator-delete path.

### EF reconciler state machine

**Init** (first reconcile — neither finalizer nor initial conditions present):
1. Add cleanup finalizer.
2. Seed initial conditions: `FaultReported=Unknown`, `FaultCleared=False (FaultActive)`.
3. Emit the health event from `spec.healthEvent` to the platform-connector, with `recommendedAction` set to `CUSTOM` and `customRecommendedAction` set to `external-remediation`. Include the EF's name in `healthEvent.metadata["externalFaultName"]` so `fault-remediation` can link the child ERR back to this EF.
4. On success: set `FaultReported=True (HealthEventEmitted)`. Reconcile returns; next trigger is the child ERR appearing.
5. On failure: return error; controller-runtime requeues with backoff.

**Open / awaiting clear** (`FaultReported=True`, `FaultCleared=False`):
- Idle. The controller watches owned ERRs via `Owns(&ExternalRemediationRequest{})` in `SetupWithManager`; when `fault-remediation` creates the ERR with an `OwnerReference` to this EF, a reconcile is triggered automatically. If no ERR is ever created — e.g. the emitted event doesn't produce external remediation — the EF simply stays open here until an operator deletes it.
- Child ERR reports `ExternalRemediationComplete=True` → emit the clearing health event: a plain `isHealthy=true` event carrying the same `agent`/`checkName`/`nodeName` as the original with `recommendedAction=NONE` (not `CUSTOM` — it must clear the check, not trigger a second remediation). On successful emit → set `FaultCleared=True` → stamp `CompletionTime`. Reconciler is done. On emit failure → return error; controller-runtime requeues and retries, exactly like the opening emit — `FaultCleared` stays unset until it succeeds.
- Child ERR reports `ExternalRemediationComplete=False` (repair failed) → no clearing event; the EF stays open. A failed repair means the fault stands. The failure is visible on the ERR, not the EF; the EF closes only on a subsequent ERR success or an operator delete.

**Operator delete** (DeletionTimestamp set):
1. If child ERR exists and `FaultCleared` has not been set to `True`, record an operator-delete close metric.
2. Stamp `CompletionTime`.
3. Remove the cleanup finalizer. Kubernetes GC cascades the OwnerReference deletion to the child ERR (see *OwnerReference design* below).

### OwnerReference design

`fault-remediation` creates the ERR when it processes a `CUSTOM` health event. When the health event carries `metadata["externalFaultName"]`, `fault-remediation`:

1. Looks up the named EF by name.
2. Adds an `OwnerReference` on the new ERR pointing to the EF:

```yaml
ownerReferences:
  - apiVersion: nvsentinel.dgxc.nvidia.com/v1
    kind: ExternalFault
    name: <ef-name>
    uid: <ef-uid>
    controller: true
    blockOwnerDeletion: false   # ERR handles its own node cleanup via finalizer
```

`blockOwnerDeletion: false` means deleting an EF does not block on the ERR being deleted first. Kubernetes GC enqueues the ERR for deletion independently, and the ERR's own cleanup finalizer (established in ADR-040) ensures the release taint and `managed=false` label are removed before the ERR is garbage-collected, regardless of EF lifecycle.

The EF reconciler uses `Owns(&ExternalRemediationRequest{})` in `SetupWithManager`, which installs a field indexer on `metadata.ownerReferences` so EF reconciles fire when the paired ERR changes state.

The link between EF and ERR rides on `metadata["externalFaultName"]`, **not** on the health-event `id`. The platform-connector/datastore assigns its own identifier to the event as it is persisted, so the `id` on the EF's `spec.healthEvent` is not stable downstream — `fault-remediation` sees the datastore's id, not the one the EF submitted. `externalFaultName` is a stable, EF-controlled key that survives the round-trip and is therefore the correct linkage.

### No node lock

The EF reconciler does not acquire the cross-controller node-level lock (`NodeLock` / coordination Lease). The ERR reconciler holds the lock for the node's active remediation lifetime (established by ADR-040). The EF reconciler's role ends at health-event emission; the resulting ERR takes ownership of the lock from that point.

### Health-event emission (janitor ↔ platform-connector)

The janitor process does not emit health events today; EF adds this capability. The EF reconciler publishes health events to the platform-connector over the node-local Unix domain socket (`healthpub.Publisher` → `PlatformConnector.HealthEventOccurredV1`) — the same ingress path the health monitors use. EF emits twice over the fault's lifetime: the opening `CUSTOM` event that raises the fault (`FaultReported`), and the `isHealthy=true` clearing event that retracts it (`FaultCleared`). It acts as the synthetic health monitor for external-origin faults, which no real monitor observes. Emission requires additions to the janitor deployment that the reconciler alone does not provide:

- **Socket mount.** The janitor pod mounts the platform-connector's node-local socket via `hostPath` (`/var/run/nvsentinel`), exactly as the Deployment-type monitors (e.g. `csp-health-monitor`) do. The platform-connector runs as a per-node DaemonSet, so a socket is present on whatever node the (single) janitor pod is scheduled to — co-location is guaranteed.
- **gRPC client.** A lazy `PlatformConnectorClient` (`grpc.NewClient`) wrapped by `healthpub.Publisher`. Lazy dialing means janitor start-up never blocks on the socket; if the platform-connector is briefly unavailable, `Emit` fails, `FaultReported` stays `Unknown`, and the reconciler requeues.
- **RBAC.** No new RBAC for emission itself (the gRPC call is not a Kubernetes API call), but the reconciler needs `externalfaults` (+ `/status`, `/finalizers`) and `get;list;watch` on `externalremediationrequests`.

### Pipeline dependencies

EF only *emits* a health event; whether that event flows through quarantine → drain → ERR depends on the rest of the pipeline being configured to act on it. Two pieces must be in place for the happy path to fire, and neither is implied by the EF reconciler itself:

1. **A `fault-quarantine` ruleset that matches EF-emitted events.** `fault-quarantine` cordons only events matching one of its rulesets; a generic external-remediation event (e.g. a CSP-maintenance `CUSTOM` event) matches none of the default agent/check-specific rulesets and is skipped, so no cordon/drain occurs. The EF-emitted event must carry fields that match an existing ruleset, or a dedicated ruleset for EF-originated events must be added.
2. **A `fault-remediation` action that produces the ERR.** `fault-remediation` maps `customRecommendedAction` to a configured action template; an `external-remediation` action that renders an `ExternalRemediationRequest` (and stamps the `OwnerReference` from `metadata["externalFaultName"]`) must exist. This is the ERR-production path — a prerequisite for EF, not part of EF itself.

These are sequencing dependencies: EF is the entry door, but the entry door is only useful once the quarantine ruleset and the ERR-producing action behind it are wired.

### Validating admission webhook

Added to the existing `janitor_webhook.go` validator chain:

| Check | On create | On update |
|---|---|---|
| `spec.healthEvent.nodeName` is non-empty | ✓ | ✓ |
| Node named by `nodeName` exists in the cluster | ✓ | if `nodeName` changed |
| `spec.healthEvent.nodeName` is immutable | — | ✓ |
| No other active EF for the same node (`CompletionTime == nil`) | ✓ | — |

The duplicate-node check prevents race conditions where two EF CRs race to drain the same node. The webhook queries EFs by `spec.healthEvent.nodeName` using an informer-backed lister (the same pattern as the ERR webhook for node-existence checks). If an active EF already exists, the webhook rejects with a descriptive message directing the caller to the existing EF's name.

### RBAC

EF reconciler (in `janitor` binary):

```
# kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalfaults,verbs=get;list;watch;update;patch;delete
# kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalfaults/status,verbs=get;update;patch
# kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalfaults/finalizers,verbs=update
# kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalremediationrequests,verbs=get;list;watch
```

`fault-remediation` (new permissions required for EF integration):

```
# kubebuilder:rbac:groups=nvsentinel.dgxc.nvidia.com,resources=externalfaults,verbs=get;list;watch
```

No new node-level RBAC is needed; EF does not touch nodes directly.

### Sequence: EF-initiated fault (happy path)

```
External System         NVSentinel                     Node
──────────────          ──────────────────────────     ────
Create ExternalFault ──▶
                        EF reconciler:
                          emit CUSTOM health event
                          (metadata: externalFaultName)
                          FaultReported=True
                        ↓
                        fault-quarantine: cordon node ──▶ unschedulable=true
                        node-drainer: drain workloads ──▶ workloads evicted
                        fault-remediation:
                          create ERR
                          (OwnerReference → EF)
                        ↓
                        ERR reconciler:
                          apply release taint ──────────▶ taint applied
                          managed=false ────────────────▶ label applied
                          NVSentinelOwnershipReleased=True
                        ↓
External System watches ERR
  performs repair ───────────────────────────────────────▶ (external work)
  patches ERR ExternalRemediationComplete=True
                        ↓
                        ERR reconciler:
                          remove taint ─────────────────▶ taint removed
                          remove managed label ──────────▶ label removed
                          ERR CompletionTime set
                        ↓
                        EF reconciler (Owns watch fires):
                          child ERR reports complete
                          emit isHealthy=true event ────▶ platform-connector
                          FaultCleared=True
                          EF CompletionTime set
                        ↓
                        fault-quarantine: check recovered ──▶ node un-cordoned
```

### Sequence: EF operator delete (cancellation)

```
Operator             Kubernetes GC        ERR reconciler (finalizer)
────────             ─────────────        ──────────────────────────
kubectl delete ef ──▶
                     EF finalizer runs:
                       CompletionTime set
                       finalizer removed
                     EF deleted
                     GC cascades ────────▶ ERR deletion enqueued
                                           ERR finalizer runs:
                                             remove taint
                                             remove managed label
                                           ERR finalizer removed
                                           ERR GC'd
```

## Rationale

**Single entry point for external-initiated handoff.** Without EF, an external system either creates its own node drain logic (duplicating NVSentinel's quarantine/drain/managed-label chain) or side-steps NVSentinel entirely. EF reuses the entire existing NVSentinel pipeline with a single health event emission.

**Reusing the CUSTOM health event flow.** EF emits the same event payload that NVSentinel-detected faults produce on the CUSTOM path, so it reuses the existing quarantine/drain/ERR machinery rather than duplicating it. It does *not* follow that the pipeline reacts to the event out of the box: `fault-quarantine` only cordons events matching one of its rulesets, and `fault-remediation` only produces an ERR for a configured action. Both must be wired for the EF path (see [Pipeline dependencies](#pipeline-dependencies)); the ERR-production path additionally stamps the `OwnerReference` from `metadata["externalFaultName"]`.

**OwnerReference with `blockOwnerDeletion: false`.** ERR's cleanup finalizer already guarantees node cleanup regardless of how the ERR lifecycle ends (ADR-040). Cascading EF deletion to ERR via GC preserves that guarantee without requiring the EF reconciler to orchestrate ERR deletion itself.

**No node lock on EF.** The EF reconciler's sole in-cluster side effect is emitting a health event. All node mutations are performed by downstream reconcilers (fault-quarantine, ERR reconciler) that already participate in the cross-controller node lock protocol. Adding a lock to EF would be redundant and would block on nodes that are not yet claimed.

**Webhook rejects duplicate active EFs.** A second EF for the same node would create a second health event, a second quarantine session, and potentially a second ERR — all racing over the same node. Rejecting at admission is cleaner than detecting and resolving the race at runtime.

## Consequences

**Positive:**
- External systems and human operators have a single, well-defined entry point for requesting node drain and ownership transfer.
- The full NVSentinel quarantine/drain pipeline fires for EF-initiated faults, ensuring workloads are safely evicted before external repair begins.
- EF and ERR together form a symmetric, observable API: both exist as Kubernetes objects with `CompletionTime` and structured conditions, auditable via `kubectl` and standard Kubernetes tooling.
- The existing TTL reconciler (ADR-037) can manage EF object lifecycle after completion, the same as it does for RebootNode, TerminateNode, GPUReset, and ERR.

**Negative / tradeoffs:**
- The EF path depends on pipeline wiring that does not exist purely for EF: a `fault-quarantine` ruleset that matches EF-emitted events, and a `fault-remediation` `external-remediation` action that produces the ERR (and reads `healthEvent.metadata["externalFaultName"]` to set the OwnerReference). These must land with, or before, EF (see [Pipeline dependencies](#pipeline-dependencies)).
- The janitor gains a new outbound dependency — it must reach the platform-connector's node-local socket to emit — where before it only spoke to the Kubernetes API. See [Health-event emission](#health-event-emission-janitor--platform-connector).
- The webhook's duplicate-node check requires an informer-backed lister at admission time. This is the same approach the ERR webhook uses for node-existence checks, so no new infrastructure is needed.
- An EF whose repair fails, stalls, or never produces an ERR simply stays open (`FaultReported=True`, not cleared) until an operator deletes it; NVSentinel does not auto-recover. The remediation's success/failure detail lives on the ERR, not the EF — the EF is intentionally binary (submitted / cleared).

## Alternatives Considered

**EF reconciler directly creates the ERR (skipping the health event).** This is simpler in the short term but bypasses the quarantine/drain pipeline entirely. The external system would be responsible for ensuring the node is drained before the ERR is created. This violates the ADR-040 principle that every ERR's node has passed through NVSentinel's quarantine and drain before ownership is transferred.

**EF as a label or annotation on an existing object.** Placing the entry signal on the Node object itself is simpler but not observable as a first-class Kubernetes resource, not auditable via standard tools, and not compatible with the TTL cleanup pattern.

**ERR created directly by the external system (no EF).** The external system could create an ERR directly. This would skip quarantine/drain entirely, violating the invariant that every ERR node has been drained. The EF → health event → quarantine → drain → ERR chain is the only path that enforces this invariant.

## Testing

- Unit tests for EF reconciler covering all condition transitions: FaultReported=Unknown → True (opening emit), clearing-event emission triggered by the child ERR reporting complete (including retry when the clearing emit fails), FaultCleared=True, no-clear when the ERR fails or never appears, CompletionTime stamping, deletion path.
- Webhook unit tests: empty nodeName rejected, non-existent nodeName rejected, nodeName immutable on update, duplicate active EF for same node rejected.
- E2E (`tests/external_fault_test.go`, build tag `arm64_group`):
  - Happy path: EF created → monitors suppress on node → ERR appears with OwnerReference → ERR ExternalRemediationComplete=True → EF emits the `isHealthy=true` clearing event → EF FaultCleared=True → node un-cordoned.
  - Failure path: child ERR reports ExternalRemediationComplete=False → EF emits no clearing event and stays open, node stays cordoned; operator delete closes the EF.
  - Cancellation: operator deletes EF → ERR GC'd → node cleanup via ERR finalizer confirmed.
  - Duplicate rejection: second EF for same active node is rejected at admission.

## References

- [ADR-040: External Remediation Request](040-external-remediation-request.md) — the outbound handoff API this ADR complements.
- [ADR-036: Custom Remediation Actions](036-custom-remediation-actions.md) — defines the `CUSTOM` recommended action path EF uses.
- [ADR-009: Fault Remediation Triggering](009-fault-remediation-triggering.md) — the fault-remediation pipeline EF's health event flows through.
- [ADR-037: Janitor CR TTL Cleanup](037-janitor-cr-ttl-cleanup.md) — TTL cleanup applies to EF after `CompletionTime` is set.
