# ADR-040: API — External Remediations (EF / ERR)

## Context

NVSentinel is the primary owner of nodes in the clusters where it operates: when a node joins, NVSentinel owns it, and any cordon/drain/reboot/terminate flows through NVSentinel's pipeline (health-monitor → fault-quarantine → node-drainer → fault-remediation).

External breakfix systems — automated orchestrators driving CSP-side repair, RMA workflows, hardware swaps, or human operators performing manual maintenance — also have authority to perform destructive actions on the same nodes. Today these systems coordinate with NVSentinel ad-hoc, through side channels and operator knowledge.

A node must be owned by **exactly one** system at any point in time. Without a formal protocol, NVSentinel and an external system can both decide to act on the same node and race — duplicating remediation work, fighting over taints, or interleaving operations that corrupt cluster state.

The two crossing points needed are:

1. **NVSentinel-detected fault that an external system must fix.** NVSentinel's triage maps a known failure mode to a remediation that NVSentinel cannot perform itself (e.g. CSP-side repair, RMA). NVSentinel needs to signal "this node is yours" and stop touching it.
2. **External-detected fault that NVSentinel must prepare for.** An external system observes a fault NVSentinel can't see (e.g. a CSP-scheduled maintenance window, an out-of-band monitoring signal, an operator-driven request). The external system needs NVSentinel to cordon and drain the node, then transfer ownership.

The same protocol must work for human operators preparing nodes for manual remediation. Multiple bespoke entry points (one per external system) would not. The design must be agnostic to which external system is on the other side — NVSentinel cannot make assumptions about how, when, or whether an external system completes its work.

## Decision

Introduce two namespaced CRDs in the existing `nvsentinel.nvidia.com/v1alpha1` API group, plus two new reconcilers and one integration point in `fault-remediation`:

| CRD | Created by | Role |
| --- | --- | --- |
| `ExternalRemediationRequest` (ERR) | NVSentinel (`fault-remediation` or `ExternalFault` reconciler) | **Exit door.** Taints the node and signals "NVSentinel has released the node." External system reads this to know it can begin work, and writes a completion condition back when done. |
| `ExternalFault` (EF) | External system | **Entry door.** External system declares "this node has a fault I need to take ownership of." Triggers the standard NVSentinel pipeline (quarantine → drain → fault-remediation), which produces an ERR. |

Both CRD specs are a `datamodels.HealthEvent` (the same proto already emitted by every health-monitor and consumed by every downstream stage — see `data-models/protobufs/health_event.proto`). All coordination state lives in `status.conditions`. There is exactly **one** entry point into external ownership (EF) and exactly **one** exit point (ERR); every external system speaks the same protocol.

EF creation always results in ERR creation by routing through the normal pipeline with `recommendedAction = CUSTOM` (per [ADR-036](036-custom-remediation-actions.md)). The EF reconciler does not create the ERR directly — that keeps a single, well-tested code path responsible for materialising release requests.

## Implementation

### Module layout

The two reconcilers and their CRD types live alongside the existing maintenance reconcilers in `janitor/`:

```
janitor/
├── api/v1alpha1/
│   ├── externalfault_types.go            (new)
│   ├── externalremediationrequest_types.go (new)
│   ├── gpureset_types.go                  (existing)
│   ├── rebootnode_types.go                (existing)
│   ├── terminatenode_types.go             (existing)
│   └── groupversion_info.go               (existing — already nvsentinel.nvidia.com/v1alpha1)
└── pkg/controller/
    ├── externalfault_controller.go        (new)
    ├── externalremediationrequest_controller.go (new)
    └── ...
```

Generated CRD YAML is committed to the existing `distros/kubernetes/nvsentinel/charts/janitor/crds/` directory.

### API: `ExternalRemediationRequest`

```go
// ERR condition types
const (
    // NVSentinelOwnershipReleased indicates the ERR reconciler has tainted the node and
    // released it to the external system. Set by NVSentinel.
    ERROwnershipReleasedCondition = "NVSentinelOwnershipReleased"

    // ExternalRemediationComplete indicates the external system has finished its work
    // and is returning the node to NVSentinel. Set by the external system.
    ERRRemediationCompleteCondition = "ExternalRemediationComplete"
)

type ExternalRemediationRequestSpec struct {
    // HealthEvent that triggered this remediation request. Carried through from
    // fault-remediation (NVSentinel-detected path) or from the EF spec (external-
    // detected path).
    // +kubebuilder:validation:Required
    *HealthEvent `json:",inline"`
}

type ExternalRemediationRequestStatus struct {
    // Conditions represent the latest available observations of the object's state.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=err,categories=nvsentinel
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="OwnershipReleased",type="string",JSONPath=".status.conditions[?(@.type=='NVSentinelOwnershipReleased')].status"
// +kubebuilder:printcolumn:name="RemediationComplete",type="string",JSONPath=".status.conditions[?(@.type=='ExternalRemediationComplete')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ExternalRemediationRequest struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   ExternalRemediationRequestSpec   `json:"spec,omitempty"`
    Status ExternalRemediationRequestStatus `json:"status,omitempty"`
}
```

`HealthEvent` in the spec is the CRD-friendly mirror of `datamodels.HealthEvent`. Because the existing proto generator does not emit kubebuilder markers, the mirror struct lives in `janitor/api/v1alpha1/healthevent.go` and is round-tripped to/from the proto type by a thin conversion helper in `commons/pkg/healthevent`.

Concrete shape from `kubectl get externalremediationrequest -o yaml`:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ExternalRemediationRequest
metadata:
  creationTimestamp: "2026-05-11T21:16:02Z"
  generation: 1
  labels:
    nvsentinel.nvidia.com/component-class: gpu
    nvsentinel.nvidia.com/node: node-01.example-cluster.internal
    nvsentinel.nvidia.com/recommended-action: external-remediation
  name: gpu0-xid79-node-01
  namespace: nvsentinel
spec:
  agent: gpu-health-monitor
  checkName: nvml-xid-79
  componentClass: gpu
  customRecommendedAction: external-remediation
  entitiesImpacted:
    - {entityType: GPU,  entityValue: "0"}
    - {entityType: PCIE, entityValue: "0000:18:00.0"}
  errorCode: [XID-79]
  generatedTimestamp: "2026-05-11T20:14:07Z"
  id: he-7f0b3e2c-1cab-4d22-9a96-2d5b3a8ee2f1
  isFatal: true
  isHealthy: false
  message: GPU fell off the bus (XID 79) on /dev/nvidia0; node requires hardware-level repair.
  metadata:
    cluster: example-cluster-01
    cudaVersion: "12.4"
    driverVersion: 550.144.03
    serialNumber: "1320820063748"
  nodeName: node-01.example-cluster.internal
  processingStrategy: EXECUTE_REMEDIATION
  recommendedAction: CUSTOM
  version: 1
status:
  conditions:
    - type: NVSentinelOwnershipReleased
      status: "True"
      observedGeneration: 1
      lastTransitionTime: "2026-05-11T20:14:09Z"
      reason: ReleaseTaintApplied
      message: Applied taint nvsentinel.nvidia.com/external-remediation=gpu0-xid79-node-01:NoSchedule to node-01.example-cluster.internal; node released to external system.
    - type: ExternalRemediationComplete
      status: "Unknown"
      observedGeneration: 1
      lastTransitionTime: "2026-05-11T20:14:09Z"
      reason: AwaitingExternalSystem
      message: Waiting for external system to set ExternalRemediationComplete=True (success) or False (failure).
```

### API: `ExternalFault`

```go
// EF condition types
const (
    // FaultReported indicates the EF reconciler has emitted a CUSTOM health event into
    // the NVSentinel pipeline. Set by NVSentinel.
    EFFaultReportedCondition = "FaultReported"

    // ExternalRemediationComplete is propagated up from the owning ERR. Set by NVSentinel.
    EFRemediationCompleteCondition = "ExternalRemediationComplete"

    // FaultCleared indicates the EF reconciler has emitted a healthy event that will
    // unquarantine the node. Set by NVSentinel.
    EFFaultClearedCondition = "FaultCleared"
)

type ExternalFaultSpec struct {
    // HealthEvent describing the externally-detected fault. Recommended action is
    // typically CUSTOM with customRecommendedAction=external-remediation; the EF
    // reconciler does not validate this and treats any HealthEvent as a request to
    // run the node through the standard pipeline.
    // +kubebuilder:validation:Required
    *HealthEvent `json:",inline"`
}

type ExternalFaultStatus struct {
    Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Namespaced,shortName=ef,categories=nvsentinel
// +kubebuilder:printcolumn:name="Node",type="string",JSONPath=".spec.nodeName"
// +kubebuilder:printcolumn:name="FaultReported",type="string",JSONPath=".status.conditions[?(@.type=='FaultReported')].status"
// +kubebuilder:printcolumn:name="RemediationComplete",type="string",JSONPath=".status.conditions[?(@.type=='ExternalRemediationComplete')].status"
// +kubebuilder:printcolumn:name="FaultCleared",type="string",JSONPath=".status.conditions[?(@.type=='FaultCleared')].status"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
type ExternalFault struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   ExternalFaultSpec   `json:"spec,omitempty"`
    Status ExternalFaultStatus `json:"status,omitempty"`
}
```

`kubectl get externalfault -o yaml`:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: ExternalFault
metadata:
  creationTimestamp: "2026-05-11T21:16:02Z"
  generation: 1
  labels:
    nvsentinel.nvidia.com/external-source: csp-events
    nvsentinel.nvidia.com/fault-class: csp-maintenance
    nvsentinel.nvidia.com/node: node-02.example-cluster.internal
  name: csp-maintenance-node-02
  namespace: nvsentinel
spec:
  agent: external-orchestrator
  checkName: csp-scheduled-maintenance
  componentClass: node
  customRecommendedAction: external-remediation
  entitiesImpacted:
    - {entityType: Node, entityValue: node-02.example-cluster.internal}
  errorCode: [CSP-MAINT-EBS-RETIRE]
  generatedTimestamp: "2026-05-11T20:14:07Z"
  id: he-ext-c6d92aa1-2f6e-4e8b-9e3d-b75f86b1aaaa
  isFatal: false
  isHealthy: false
  message: CSP scheduled maintenance window 2026-05-13T03:00Z to 2026-05-13T07:00Z; node must be drained before window.
  metadata:
    cluster: example-cluster-01
    cspEventId: evt-0a9bc8e74e2c2c10c
    maintenanceWindowEnd: "2026-05-13T07:00:00Z"
    maintenanceWindowStart: "2026-05-13T03:00:00Z"
    source: csp-events
  nodeName: node-02.example-cluster.internal
  processingStrategy: EXECUTE_REMEDIATION
  recommendedAction: CUSTOM
  version: 1
status:
  conditions:
    - type: FaultReported
      status: "True"
      observedGeneration: 1
      lastTransitionTime: "2026-05-11T20:14:08Z"
      reason: HealthEventEmitted
      message: Submitted CUSTOM health event he-ext-c6d92aa1-2f6e-4e8b-9e3d-b75f86b1aaaa to platform-connector on node node-02.example-cluster.internal.
    - type: ExternalRemediationComplete
      status: "Unknown"
      observedGeneration: 1
      lastTransitionTime: "2026-05-11T20:14:08Z"
      reason: AwaitingExternalRemediation
      message: Waiting for owning ExternalRemediationRequest to resolve.
    - type: FaultCleared
      status: "Unknown"
      observedGeneration: 1
      lastTransitionTime: "2026-05-11T20:14:08Z"
      reason: FaultActive
      message: Fault is active; node has not been returned to NVSentinel ownership.
```

### Condition state machines

`True` and `False` are **terminal** states for every condition on these CRDs. Conditions in flight are represented by `Unknown`; once a condition lands on `True` (the named state has been achieved) or `False` (it cannot be achieved), it does not transition again for the same generation. This matches the convention already established by `RebootNode`, `GPUReset`, and `TerminateNode`.

The reconcilers call `SetInitialConditions` on first reconcile, writing every condition as `Unknown` with `reason: Initializing` (modelled after `janitor/api/v1alpha1/rebootnode_types.go`).

**ExternalRemediationRequest:**

| Condition | Initial | Terminal `True` | Terminal `False` | Set by |
| --- | --- | --- | --- | --- |
| `NVSentinelOwnershipReleased` | `Unknown` (`Initializing`) | Release taint applied to `spec.nodeName` | Permanent failure to apply taint (retries exhausted, e.g. invalid configuration) | ERR reconciler |
| `ExternalRemediationComplete` | `Unknown` (`AwaitingExternalSystem`) | External system reports remediation succeeded | External system reports remediation failed (e.g. RMA declined, repair unsuccessful, external system gave up) | **External system** |

Both `True` and `False` on `ExternalRemediationComplete` are meaningful, terminal outcomes — they trigger the same ERR reconciler behaviour: remove the release taint and propagate the value (unchanged) to any owning EF. If the underlying fault is still present after a `False` outcome, NVSentinel's existing health-monitors will re-detect it and re-trigger the standard pipeline, producing a fresh ERR. This is the desired self-healing path: the external system's failure does not strand the node in tainted limbo.

**ExternalFault:**

| Condition | Initial | Terminal `True` | Terminal `False` | Set by |
| --- | --- | --- | --- | --- |
| `FaultReported` | `Unknown` (`Initializing`) | Health event submitted to platform-connector | Submission failed after retry budget exhausted | EF reconciler |
| `ExternalRemediationComplete` | `Unknown` | Owning ERR resolved with `True` | Owning ERR resolved with `False` | ERR reconciler (propagation) |
| `FaultCleared` | `Unknown` (`FaultActive`) | Healthy event submitted to platform-connector | Submission failed after retry budget exhausted | EF reconciler |

All conditions are append-or-update in place via the `SetCondition` helper, which short-circuits no-op updates so reconcile storms don't flap `lastTransitionTime`.

### ERR reconciler

**Watches:** `ExternalRemediationRequest` (primary); `Node` (secondary, to detect taint drift).

**Reconcile loop:**

1. On first reconcile, call `SetInitialConditions`: write `NVSentinelOwnershipReleased=Unknown (Initializing)` and `ExternalRemediationComplete=Unknown (AwaitingExternalSystem)`.
2. If `NVSentinelOwnershipReleased` is `Unknown`:
   - Apply the configured release taint to `spec.nodeName` (idempotent).
   - On success: set `NVSentinelOwnershipReleased=True` with `reason=ReleaseTaintApplied`.
   - On permanent failure (retry budget exhausted): set `NVSentinelOwnershipReleased=False` with `reason=ReleaseTaintFailed`. The ERR is now in a terminal failed state; no further reconciliation work, but the object remains so the failure is visible to operators.
3. If `ExternalRemediationComplete` has resolved to `True` or `False` (either terminal):
   - Remove the release taint from `spec.nodeName` (idempotent; tolerate missing taint).
   - If this ERR has an `ownerReference` of kind `ExternalFault`, propagate the same value (`True` or `False`) to the EF's `ExternalRemediationComplete` condition.
   - Done; no further work.
4. Otherwise (`ExternalRemediationComplete` is still `Unknown`): no-op until the external system writes a terminal value.

**Release taint** (key configurable via Helm values):

```
nvsentinel.nvidia.com/external-remediation=<err-name>:NoSchedule
```

The taint **value** is the `metadata.name` of the owning `ExternalRemediationRequest` — the same deterministic hash used by `fault-remediation` when constructing the ERR. This is the only piece of node-side metadata the design adds; no Node labels or annotations are created. The taint carries both the operational guard ("don't act on this node") and the correlation key ("which ERR owns the release") in one place.

The taint is the *single source of truth* for "this node is not owned by NVSentinel right now." Any other NVSentinel component that takes destructive action MUST refuse to act on a node carrying any taint with this key, regardless of value. `node-drainer` and `fault-quarantine` get a one-line check; `fault-remediation` already keys off node selection logic and inherits the same guard.

**Single active ERR per node.** Because the `(key, effect)` taint tuple is unique on a Node, only one ERR can have its release taint applied at a time. This invariant is enforced by `fault-remediation`'s existing equivalence-group machinery (see the *`fault-remediation` integration* section): ERRs declare their own equivalence group and a status checker, and the existing "skip CR creation when a CR for a matching group is in progress" logic prevents a second concurrent ERR from ever being created. No new dedup code is required in the ERR reconciler.

**Idempotency:** taint application uses a server-side patch. The ERR reconciler verifies both key AND value match its own ERR name before treating the taint as "its own" — protecting against drift from a previous, no-longer-existing ERR.

**Node deletion:** if `spec.nodeName` no longer exists at reconcile time, the reconciler logs and treats the taint operation as complete. The ERR object remains until the external system writes a terminal value to `ExternalRemediationComplete`; if the external system never acknowledges, the ERR persists with `ExternalRemediationComplete=Unknown` (visible via age metric).

### Discovery from the Node side

An operator who notices a tainted node finds the originating ERR by reading the taint value. With no Node labels or annotations, all discovery flows through the taint:

```bash
# 1. See the ERR name embedded in the taint value
kubectl describe node <node-name>
# Taints: nvsentinel.nvidia.com/external-remediation=gpu0-xid79-node-01:NoSchedule

# Or extract just that value:
kubectl get node <node-name> -o jsonpath=\
  '{.spec.taints[?(@.key=="nvsentinel.nvidia.com/external-remediation")].value}'

# 2. Fetch the ERR (default namespace: nvsentinel; if customised, search with -A)
kubectl get externalremediationrequest -A | grep <err-name>
kubectl get externalremediationrequest -n nvsentinel <err-name> -o yaml
```

To enumerate every node currently under external remediation:

```bash
kubectl get nodes -o json | jq -r '
  .items[]
  | select(.spec.taints[]?.key == "nvsentinel.nvidia.com/external-remediation")
  | [.metadata.name,
     (.spec.taints[] | select(.key=="nvsentinel.nvidia.com/external-remediation") | .value)]
  | @tsv'
```

The trade-off vs. duplicating the ERR name to a Node label is explicit: filtering nodes via `kubectl get nodes -l …` is unavailable; operators use the jq query above. Given how rarely this enumeration is needed in practice, the cost of maintaining a parallel Node label was judged not worth it.

### EF reconciler

**Watches:** `ExternalFault` (primary).

**Reconcile loop:**

1. On first reconcile, call `SetInitialConditions`: write `FaultReported=Unknown (Initializing)`, `ExternalRemediationComplete=Unknown`, `FaultCleared=Unknown (FaultActive)`.
2. If `FaultReported` is `Unknown`:
   - Submit `spec.HealthEvent` to the platform-connector running on `spec.nodeName` via the existing gRPC client (`platform-connectors/pkg/client`).
   - On success: set `FaultReported=True` with `reason=HealthEventEmitted`. Include the event ID in the message.
   - On permanent failure (retry budget exhausted): set `FaultReported=False` with `reason=HealthEventEmitFailed`. Terminal failed state.
3. If `ExternalRemediationComplete` is terminal (`True` or `False`) and `FaultCleared` is `Unknown`:
   - Submit a healthy companion event to the platform-connector — same `id` family, same `nodeName`, `isHealthy=true`, `recommendedAction=NONE`. (Emit the healthy event regardless of whether `ExternalRemediationComplete` was `True` or `False`; existing health-monitors will re-detect any still-present fault.)
   - On success: set `FaultCleared=True` with `reason=NodeReturnedToNVSentinel`.
   - On permanent failure: set `FaultCleared=False` with `reason=HealthEventEmitFailed`.
4. Otherwise, no-op.

The EF reconciler does **not** create the ERR directly. Instead, the `CUSTOM` recommendedAction in the emitted health event flows through the standard pipeline; `fault-remediation` produces the ERR. This keeps one well-tested code path responsible for materialising release requests.

When `fault-remediation` creates an ERR triggered by an EF-emitted health event, it sets an `ownerReference` on the ERR pointing to the EF. The ERR reconciler uses this owner reference to propagate `ExternalRemediationComplete` back.

### `fault-remediation` integration

`fault-remediation` already uses `GetEffectiveActionName(he)` (per ADR-036) to resolve the action name for routing. To produce ERRs:

- **`fault-remediation/pkg/common/equivalence_groups.go`** — extend the equivalence-group config schema so a group can declare `produces: ExternalRemediationRequest` in addition to (or instead of) a maintenance-CR template.
- **`fault-remediation/pkg/remediation/remediation.go`** — `CreateMaintenanceResource` branches on the produces type; for `ExternalRemediationRequest`, build the ERR struct from the triaged HealthEvent and create it via the dynamic client.

ERR construction details:

- `metadata.name` is a deterministic hash of `(nodeName, healthEvent.id)` so repeated reconciles of the same event do not create duplicate ERRs. The name **also** becomes the value of the release taint applied by the ERR reconciler — so it MUST be a valid Kubernetes taint value (≤ 63 chars, alphanum + `.`, `-`, `_`).
- `metadata.namespace` is the configured ERR namespace (default: `nvsentinel`).
- `metadata.labels` include `nvsentinel.nvidia.com/node`, `…/component-class`, and `…/recommended-action` for observability inside the ERR API (these do NOT propagate to the Node — see "Discovery from the Node side" in the ERR reconciler section for the rationale).
- `metadata.ownerReferences` is set to the originating EF if and only if the source health event carries a label `nvsentinel.nvidia.com/source-ef` (added by the EF reconciler when emitting); otherwise the ERR is standalone.
- `spec.HealthEvent` is a full copy of the triaged event.

**Equivalence-group integration.** ERR creation goes through the same `latestFaultRemediationState`-annotation + `ShouldSkipCRCreation` machinery as existing maintenance CRs (`RebootNode`, `GPUReset`, etc.) — see `fault-remediation/pkg/reconciler/reconciler.go` `shouldCreateCRForGroup`. Two pieces are added:

1. **Equivalence group declared in the TOML config** for the external-remediation action, e.g. `equivalenceGroup: "external-remediation"`. Cross-kind supersedence (e.g. `external-remediation` superseded by `restart`, or vice versa) can be declared the same way as today, so a node with an in-flight RebootNode does not get a second concurrent ERR, and a node with an in-flight ERR does not get a concurrent maintenance CR.
2. **A status checker for `ExternalRemediationRequest`** plugging into the existing `StatusChecker` interface: returns `ShouldSkip=true` when the ERR's `ExternalRemediationComplete` condition is `Unknown` (in-flight); `false` once it is terminal (`True` or `False`), at which point the entry is pruned from the annotation as usual.

This gets the "single active ERR per node" invariant for free — it falls out of the existing skip logic, no bespoke deduplication code in the ERR reconciler.

`fault-remediation` does **not** create both an ERR and a maintenance CR for the same event — the equivalence group declares one or the other.

### Sequence diagrams

**NVSentinel-detected fault → external remediation:**

```mermaid
sequenceDiagram
    participant HM as health-monitor
    participant FQ as fault-quarantine
    participant ND as node-drainer
    participant FR as fault-remediation
    participant ERR as ExternalRemediationRequest
    participant RC as ERR Reconciler
    participant N as Node
    participant EXT as External System

    HM->>FQ: HealthEvent (e.g. XID 79)
    Note over FQ: recommendedAction CUSTOM
    FQ->>N: cordon + apply fault-quarantine taint
    FQ->>ND: drained event
    ND->>N: evict workload
    ND->>FR: drained event
    FR->>ERR: create (spec = HealthEvent)
    RC->>N: apply release taint
    RC->>ERR: NVSentinelOwnershipReleased=True
    Note over EXT: external system watches ERR; sees OwnershipReleased
    EXT->>N: perform remediation (RMA, manual fix, …)
    EXT->>ERR: ExternalRemediationComplete=True
    RC->>N: remove release taint
    Note over HM,N: existing health-monitors emit healthy events
    HM->>FQ: healthy HealthEvent
    FQ->>N: remove fault-quarantine taint, uncordon
```

**Externally-detected fault → NVSentinel pipeline → external remediation:**

```mermaid
sequenceDiagram
    participant EXT as External System
    participant EF as ExternalFault
    participant EFR as EF Reconciler
    participant PC as platform-connector
    participant FQ as fault-quarantine
    participant ND as node-drainer
    participant FR as fault-remediation
    participant ERR as ExternalRemediationRequest
    participant ERRR as ERR Reconciler
    participant N as Node

    EXT->>EF: create (spec = HealthEvent)
    EFR->>PC: HealthEventOccurredV1 (CUSTOM)
    EFR->>EF: FaultReported=True
    PC->>FQ: HealthEvent
    FQ->>N: cordon + fault-quarantine taint
    FQ->>ND: drained event
    ND->>N: evict workload
    ND->>FR: drained event
    FR->>ERR: create (ownerRef=EF, spec=HealthEvent)
    ERRR->>N: apply release taint
    ERRR->>ERR: NVSentinelOwnershipReleased=True
    Note over EXT: external system watches ERR; sees OwnershipReleased
    EXT->>N: perform remediation
    EXT->>ERR: ExternalRemediationComplete=True
    ERRR->>N: remove release taint
    ERRR->>EF: propagate ExternalRemediationComplete=True
    EFR->>PC: healthy HealthEvent
    EFR->>EF: FaultCleared=True
    PC->>FQ: healthy event → remove fault-quarantine taint, uncordon
```

### RBAC

**ERR reconciler ServiceAccount:**

- `externalremediationrequests` — get, list, watch, update (status only)
- `externalfaults` — get, list, watch, patch (status only; for propagation)
- `nodes` — get, list, watch, patch (for taint apply/remove)
- `events` — create

**EF reconciler ServiceAccount:**

- `externalfaults` — get, list, watch, update (status only)
- gRPC client credentials to reach the platform-connector (already provisioned for other NVSentinel components)
- `events` — create

**External system ServiceAccount:**

- `externalfaults` — create, get, list, watch
- `externalremediationrequests` — get, list, watch, patch (status only)

External systems must NOT be granted write access to `nodes` through the external-remediation flow — node operations are mediated by the ERR reconciler via the release taint. External systems may carry separate node-level access if they choose to create in-cluster remediation CRs (e.g. `GPUReset`) as part of their workflow, but that is a separate authorisation surface.

### Observability

**Metrics** (Prometheus, namespace `nvsentinel_external_remediation`):

| Metric | Type | Labels | Meaning |
| --- | --- | --- | --- |
| `err_total` | Counter | `phase` | ERRs entered each phase (`created`, `released`, `completed`) |
| `ef_total` | Counter | `phase` | EFs entered each phase (`created`, `reported`, `cleared`) |
| `err_open` | Gauge | `node`, `recommended_action` | Open ERRs with `ExternalRemediationComplete=False` |
| `err_age_seconds` | Histogram | `recommended_action` | Time from ERR creation to `ExternalRemediationComplete=True` |
| `taint_apply_latency_seconds` | Histogram | — | Time from ERR creation to release taint applied |
| `health_event_emit_failures_total` | Counter | `source` (`ef-reconciler`) | gRPC failures emitting to platform-connector |

**Events** (Kubernetes events on the EF / ERR object):

- `ReleaseTaintApplied`, `ReleaseTaintRemoved`, `OwnershipPropagated` (ERR)
- `HealthEventEmitted`, `HealthyEventEmitted`, `EmitFailed` (EF)

**Tracing:** EF reconciler propagates an OTEL span ID into the emitted health event's `metadata.spanIds` so the full externally-originated lifecycle is observable end-to-end alongside health-monitor-originated events.

## Rationale

- **Single coordination surface.** Two CRDs, two reconcilers, and one integration point in `fault-remediation`. Any external system — automated orchestrator, human operator, future integration — speaks the same protocol. No bespoke per-system entry/exit.
- **Reuses the existing pipeline.** EFs do not bypass `fault-quarantine` or `node-drainer`; the same quarantine/drain that protects NVSentinel-detected faults protects external-detected ones. Adding a new external system requires zero changes to those stages.
- **CRDs are debuggable.** Operators can `kubectl get err,ef -A` to see every node currently outside NVSentinel ownership and the reason. Status conditions surface the exact step the handoff is on.
- **Plugs into ADR-036.** The `CUSTOM` recommended action and `GetEffectiveActionName` resolution already exist; this ADR layers the ERR-producing branch on top of that machinery rather than introducing a parallel routing path.
- **Asynchronous by design.** Remediation can take hours to weeks (RMA, replacement parts, scheduled CSP windows). The protocol does not require either system to be online for the other to progress.

## Consequences

### Positive

- Clear, enforceable ownership invariant: **a node is owned by NVSentinel iff it does not carry the release taint.**
- One protocol for all external integrations, present and future.
- Standard Kubernetes patterns throughout (CRDs, conditions, ownerReferences, RBAC) — no custom RPCs, no shared databases.
- Cascade delete: deleting an EF cleans up its owned ERR automatically.

### Negative

- Two new CRDs and two new reconcilers to operate and monitor.
- Asynchronous handshake adds latency vs. direct RPC (typically seconds, but bounded by reconcile poll intervals).
- External systems must be granted CRD access in the cluster — this is a new authorisation surface for cluster admins to reason about.

### Mitigations

- The asynchronous latency is dominated by quarantine + drain (already in the pipeline), so the additional ERR/EF reconciliation contributes a small fraction of total handoff time.
- RBAC is intentionally minimal: external systems get write access to EFs (their object) and status-only patch access to ERRs (the system's response). No node-level access is granted via this flow.
- Observability (metrics, events, tracing) gives operators the same level of insight as the existing NVSentinel pipeline.

## Alternatives Considered

### Direct gRPC API between an external system and NVSentinel

**Rejected** because: bespoke per-external-system surface; not debuggable with `kubectl`; doesn't extend to human operators without building a CLI; would require parallel infrastructure (TLS, service accounts, load balancing) that the CRD path inherits from Kubernetes for free.

### Reuse existing maintenance CRs (RebootNode, TerminateNode, GPUReset)

**Rejected** because: those CRs encode specific in-cluster actions performed by the janitor, not generic ownership transfer. An external system performing an RMA isn't doing "reboot" or "terminate" — it's doing arbitrary work that NVSentinel doesn't model. Forcing this through existing CRs would muddy their semantics. External systems remain free to create those CRs directly when in-cluster remediation is part of their workflow.

### Just use a taint, no CRDs

**Rejected** because: a taint by itself has no acknowledgment signal. The external system has no way to communicate "I'm done" back to NVSentinel except by removing the taint (which it shouldn't be authorised to do directly on the node API), or via metadata on the node object (which has the same authorisation problem). CRDs give a purpose-built object for the external system to write to.

### Single CRD with a `direction` field instead of EF and ERR

**Rejected** because: the two directions have asymmetric responsibilities and condition lifecycles. Conflating them would force every reconciler and every consumer to branch on the direction field, and the asymmetric RBAC (external system creates EFs but only patches ERR status) would be expressed via field-level admission rather than separate kinds. Two kinds are clearer and idiomatic.

## Notes

### Ownership definition

A node is **owned by NVSentinel** if and only if it does *not* carry any taint with key `nvsentinel.nvidia.com/external-remediation` and effect `NoSchedule`. The taint's *value* identifies the owning `ExternalRemediationRequest` and is used for operator-side discovery; the ownership invariant itself is keyed on the taint's existence, regardless of value. NVSentinel components MUST refuse to take destructive action on a tainted node.

### Failure modes

- **External system never sets `ExternalRemediationComplete`.** No timeout. NVSentinel makes no assumptions about how, when, or whether an external system completes its work. The ERR persists with `ExternalRemediationComplete=Unknown`, the node stays tainted, `err_age_seconds` grows, and operators are notified via existing alert rules on ERR age.
- **External system sets `ExternalRemediationComplete=False`.** A legitimate terminal outcome meaning "I tried, couldn't fix it." Treated symmetrically with `True` for taint removal and propagation. If the underlying fault is still present, the existing health-monitors will re-detect it and re-trigger the standard pipeline (producing a fresh ERR). This is the desired self-healing path when external remediation fails.
- **Node deleted while ERR is open.** ERR reconciler logs and treats taint operations as no-ops. The ERR remains so the external system can still acknowledge completion (which is then a no-op against the missing node). Cascade delete via the owning EF (if any) cleans up.
- **Duplicate EFs for the same node.** Permitted. Each EF independently emits its own health event and produces its own ERR (deterministic name = hash of `node + event.id` deduplicates only same-event resubmissions, not distinct events).
- **ERR created without a matching EF.** Normal — this is the NVSentinel-detected fault path. ERR has no `ownerReference` of kind `ExternalFault`; reconciler skips the propagation step.
- **Race between EF reconciler emitting the healthy event and the existing fault-quarantine unquarantine path.** Fault-quarantine's normal handling of healthy events is idempotent; double-clearing is a no-op.

### Non-goals

- **External-system implementation.** External systems create EFs and patch ERR conditions; their internal logic is out of scope for this ADR.
- **New remediation actions.** This ADR uses the existing `CUSTOM` action from ADR-036; it does not extend the action set.
- **In-cluster remediation CRs.** External systems that need to trigger in-cluster operations (GPUReset, RebootNode, TerminateNode) create those CRs directly as part of their remediation. This ADR does not change those flows.
- **Standalone-ERR garbage collection.** Standalone ERRs (no EF owner) are not auto-collected by this design; a follow-up ADR will cover TTL/GC policy if it becomes necessary in operation.

### Migration

No migration required — this is a pure addition. Existing flows that do not declare `produces: ExternalRemediationRequest` in their equivalence-group config continue to behave exactly as today.

## References

- Tracking issue: [#1276](https://github.com/NVIDIA/NVSentinel/issues/1276) — the capability gap this ADR addresses.
- [ADR-036: Custom Remediation Actions](036-custom-remediation-actions.md) — the `CUSTOM` recommendedAction this ADR builds on.
- [HealthEvent proto](../../data-models/protobufs/health_event.proto) — the source-of-truth schema for the CRD spec.
- [RebootNode CRD](../../janitor/api/v1alpha1/rebootnode_types.go) — pattern reference for condition-driven CRDs in this repo.
