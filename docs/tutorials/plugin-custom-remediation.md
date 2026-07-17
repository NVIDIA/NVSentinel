# Tutorial: Plugin Custom Remediation

This tutorial walks you through extending NVSentinel's **fault-remediation** stage with your own
repair automation — for example, a custom controller.

By the end you will understand:

- Where custom remediation fits in the NVSentinel pipeline.
- How to register a **custom remediation action** with no NVSentinel code changes.
- The status-condition completion contract your controller must implement.

> **Who is this for?** Teams **already running NVSentinel** — who need a repair step
> NVSentinel does not ship out of the box (CSP RMA tickets, multi-step orchestration, …) instead of
> the built-in reboot / terminate / GPU-reset actions.

> **Just want the AI to do it?** Jump to [Appendix: One-shot AI prompt](#appendix-one-shot-ai-prompt).

---

## 1. Where custom remediation fits

NVSentinel remediates a faulty node in stages:

```text
health-monitor → fault-quarantine → node-drainer → fault-remediation → your system
```

**fault-remediation** is the extension point. After a node is cordoned and drained, fault-remediation:

1. Resolves the health event's **recommended action** (a built-in enum value or a custom string).
2. Looks up a matching entry in the `maintenance.actions` Helm config.
3. Renders a **Go template** into a Kubernetes Custom Resource.
4. **Creates** that CR in the cluster.
5. **Polls** the CR's status conditions (`completeConditionType`) to avoid duplicate repair requests.

Your controller watches the CR and performs the actual repair.

```text
  fault-remediation ──create CR──► Kubernetes API ◄──watch── Your controller
        ▲                              │
        └── poll completeConditionType ┘
                    (repair + set status condition)
```

### Built-in vs custom actions

| Action source | Example | Config key in `maintenance.actions` |
|---------------|---------|-------------------------------------|
| Built-in enum | `RESTART_VM`, `COMPONENT_RESET`, `REPLACE_VM` | `RESTART_VM`, `COMPONENT_RESET`, … |
| Custom string ([ADR-036](../designs/036-custom-remediation-actions.md)) | `recommendedAction: CUSTOM`, `customRecommendedAction: hardware-rma` | `hardware-rma` |

---

## 2. Integration approach

fault-remediation creates a Kubernetes CR for your action; your controller watches it, runs the
repair, and reports completion via a status condition.

### Why your CR must expose status conditions

fault-remediation's status checker only understands **Kubernetes-style conditions** — it looks for a
`status.conditions[]` entry whose `type` equals `completeConditionType` and reads its `status`
(`True` → done, `False` → failed/retry).

Many external orchestrators report completion differently (e.g. a phase field, a job exit code, or a
ticket status). Use a thin wrapper CRD whose controller translates external completion into
`status.conditions[Complete]=True`.

---

## 3. The completion contract

Whatever CR NVSentinel creates, fault-remediation decides "is this repair done?" by reading a status
condition:

```yaml
status:
  conditions:
    - type: <completeConditionType>   # e.g. "Complete"
      status: "True"                  # True = success, False = failed (retry allowed)
      reason: RepairSucceeded
      message: Node hardware repaired and validated
```

Behavior of the status checker:

- **Condition `True`** → repair succeeded; equivalent events are marked covered.
- **Condition `False`** → repair failed; NVSentinel drops the group and **creates a new CR to retry**.
  There is no "terminal failure" state — if a repair is unrecoverable, keep the condition off `True`
  (leave it `Unknown`/absent) rather than setting `False`, or stop requeueing after your own retry budget.
- **Condition missing / `Unknown`** → repair in progress; NVSentinel **skips** creating a duplicate CR.

fault-remediation derives an **effective equivalence group** from the matched action — and, when
`impactedEntityScope` is set, the impacted entity (e.g. `reset-GPU-123`). It skips creating a new CR
only while a CR in that effective group (or a superseding group) is in progress or already succeeded;
it does **not** deduplicate every repair on the node.

`supersedingEquivalenceGroups` lets a broader action cover a narrower one. The **subordinate** action
lists the group that supersedes it. For example, a node reboot has the same effect as an RMA, so the
`hardware-rma` action lists `restart`:

```yaml
hardware-rma:
  equivalenceGroup: external-rma
  supersedingEquivalenceGroups: [restart]   # a restart supersedes external-rma
RESTART_VM:
  equivalenceGroup: restart                 # must be a defined action with no impactedEntityScope
```

---

## 4. Walkthrough — custom CR plugin (step by step)

### Step 1 — Emit a custom action from a health event

A health monitor must set both fields — the action type and the custom action name:

```yaml
recommendedAction: CUSTOM
customRecommendedAction: hardware-rma
```

> **Platform-connector overrides cannot configure custom actions.** Overrides remap
> `recommendedAction` between built-in enum values only (`RESTART_VM`, `REPLACE_VM`, …) and do not
> set `customRecommendedAction`. If an override sets `recommendedAction: CUSTOM`, the custom string
> stays empty and fault-remediation skips remediation as an unsupported action.

### Step 2 — Register the action in fault-remediation Helm values

Add your action and its template to
`distros/kubernetes/nvsentinel/charts/fault-remediation/values.yaml`:

```yaml
maintenance:
  actions:
    hardware-rma:
      apiGroup: remediation.example.com
      version: v1alpha1
      kind: RemediationRequest
      scope: Namespaced
      namespace: remediation
      completeConditionType: Complete
      templateFileName: hardware-rma.yaml
      equivalenceGroup: external-rma
  templates:
    hardware-rma.yaml: |
      apiVersion: {{ .ApiGroup }}/{{ .Version }}
      kind: RemediationRequest
      metadata:
        name: maintenance-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
        namespace: {{ .Namespace }}
        labels:
          app.kubernetes.io/managed-by: nvsentinel
      spec:
        nodeName: {{ .HealthEvent.NodeName }}
        action: hardware-rma
        healthEventId: {{ .HealthEventID }}
```

Key fields:

| Field | Purpose |
|-------|---------|
| `equivalenceGroup` | Base group name; combined with the impacted entity into the effective group used for dedup |
| `completeConditionType` | Condition `type` fault-remediation waits for |
| `supersedingEquivalenceGroups` | Optional — e.g. `restart` supersedes `external-rma` |
| `impactedEntityScope` | Optional — per-entity (e.g. per-GPU) dedup; custom actions may set this |
| `templates` | Go template rendered into the CR body |

> **RBAC:** fault-remediation auto-generates RBAC from `maintenance.actions` (resource name =
> lowercase `kind` + `s`). Use CRD kinds with regular plurals (`RemediationRequest` →
> `remediationrequests`); irregular plurals (`Policy` → `policys`) break RBAC.

### Step 3 — Template variables

Available in every maintenance template:

| Variable | Description |
|----------|-------------|
| `.NodeName` / `.HealthEvent.NodeName` | Target node |
| `.HealthEventID` | Triggering event ID |
| `.HealthEvent` | Full health event (all fields) |
| `.RecommendedAction` | Numeric action code |
| `.RecommendedActionName` | Resolved action name (`hardware-rma`, `RESTART_VM`, …) |
| `.ImpactedEntityScopeValue` | GPU UUID for per-GPU actions |
| `.ApiGroup`, `.Version`, `.Kind`, `.Namespace` | From the action config |
| `.TraceID`, `.SpanID` | OpenTelemetry correlation (when tracing enabled) |

### Step 4 — Build your controller

Scaffold a controller that:

1. Watches your CR kind (`RemediationRequest`).
2. Executes the repair (call a CSP API, open a support ticket, run diagnostics, …).
3. Patches the status condition:

```yaml
status:
  conditions:
    - type: Complete          # must match completeConditionType
      status: "True"          # "True" = done; "False" makes NVSentinel retry with a new CR
      reason: RepairSucceeded
      message: Node hardware repaired and validated
```

**Golden rules** (same as any NVSentinel plugin):

1. Reconcile is **idempotent** — a no-op once `Complete=True`.
2. **Make non-idempotent repairs durably idempotent.** A controller restart (or a retry after a
   `False` condition, which spawns a fresh CR) can re-run your repair before `Complete=True` is
   persisted. Guard one-shot side effects (opening a ticket, calling a CSP API) with a durable key
   such as `spec.healthEventId` or a status phase, so the effect happens **at most once**.
3. Only set `Complete=True` when the repair is **actually** done; requeue until then. Set
   `Complete=False` only when you want NVSentinel to retry with a new CR.
4. One CR per health event — NVSentinel manages its lifecycle via the equivalence group.

Build and push the controller image with Kubebuilder's Makefile, pointing `IMG` at a registry
**your cluster can pull from**:

```bash
docker login   # once, to authenticate

# Replace YOUR_USER and my-remediation with your Docker Hub user and image name.
export IMG=docker.io/YOUR_USER/my-remediation:dev   # or your cluster's private registry
make docker-build docker-push IMG=$IMG
make install && make deploy IMG=$IMG
```

> Use a **public** repo so the cluster can pull without credentials. For a private registry (e.g.
> NVCR), add the cluster's `imagePullSecret` to the controller Deployment via `config/`.

For a full Kubebuilder scaffold, reconciler walkthrough, and one-shot AI prompt, see
[Writing a Drain Plugin — Appendix: One-shot AI prompt](./writing-a-drain-plugin.md#appendix-one-shot-ai-prompt)
— the pattern is the same (NVSentinel creates the CR and polls a status condition; your controller
reconciles it and sets completion), even though the upstream stage is node-drainer instead of
fault-remediation.

For a full working example (custom health monitor, CRD, controller, and Helm values), see the
[local custom remediation demo](../../demos/local-custom-remediation-demo/README.md) — a memory-pressure
monitor and reclaim controller on a local KIND cluster (no GPU required).

---

## Appendix: One-shot AI prompt

Paste this to an AI coding agent. It is **self-contained**: the controller is a standalone Kubebuilder
project in **any repository**; produce Helm values snippets for fault-remediation registration
(separate from the controller repo). Replace the bracketed parts.

```text
Create a new NVSentinel custom remediation plugin named "[my-action]" (e.g. "hardware-rma") that
runs [the repair, e.g. "opens a CSP support ticket and waits for hardware replacement"] after
fault-remediation has cordoned and drained the node. Follow this spec exactly.

Architecture (direct CR plugin):
- A health monitor emits recommendedAction=CUSTOM with customRecommendedAction="[my-action]".
- fault-remediation looks up maintenance.actions["[my-action]"], renders a Go template into a
  Kubernetes CR, creates it, and polls status.conditions[] until completeConditionType is True.
- Your controller watches that CR, performs the repair, and sets the completion condition.

Health event contract (emit from a health monitor):
- Set BOTH recommendedAction=CUSTOM and customRecommendedAction="[my-action]" on the HealthEvent
  (protobuf field customRecommendedAction on datamodels.HealthEvent).
- Platform-connector overrides cannot configure custom actions — they remap built-in
  recommendedAction enums only and do not set customRecommendedAction.

Controller (standalone Kubebuilder project, any repo):
- Scaffold: kubebuilder init --domain example.com --repo github.com/<your-org>/[my-remediation]
  then kubebuilder create api --group remediation --version v1alpha1 --kind RemediationRequest
  --resource --controller.
- Spec: nodeName (string), action (string), healthEventId (string) — minimal fields from the
  fault-remediation template below.
- Status: conditions []metav1.Condition with a subresource.
- Reconciler: idempotent; no-op once Complete=True; run the repair at most once (guard non-idempotent
  side effects with a durable key such as spec.healthEventId so a restart or retry cannot repeat them);
  patch status with Type="Complete", Status="True" when repaired (or "False" to make NVSentinel retry
  with a new CR) using append-or-update-by-type (meta.SetStatusCondition + Status().Update).
  RBAC: remediationrequests + status; add any API permissions your repair needs.
- Build and push with the generated Dockerfile/Makefile, pointing IMG at a registry your cluster can
  pull from (e.g. docker.io/<your-user>/[my-remediation]:dev or a private registry such as NVCR).
  Run docker login once, then make docker-build docker-push IMG=$IMG. Use a public repo so the
  cluster can pull without credentials; for a private registry, add the cluster's imagePullSecret
  to the controller Deployment via config/. Deploy with make install && make deploy IMG=$IMG.

fault-remediation registration (Helm values snippet — merge into your NVSentinel release's
fault-remediation chart values; exact file/chart path depends on your deployment):
- Add under maintenance.actions:
    [my-action]:
      apiGroup: remediation.example.com
      version: v1alpha1
      kind: RemediationRequest
      scope: Namespaced
      namespace: remediation
      completeConditionType: Complete
      templateFileName: [my-action].yaml
      equivalenceGroup: [my-equivalence-group]
- Add under maintenance.templates.[my-action].yaml a Go text/template body:
    apiVersion: {{ .ApiGroup }}/{{ .Version }}
    kind: RemediationRequest
    metadata:
      name: maintenance-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
      namespace: {{ .Namespace }}
      labels:
        app.kubernetes.io/managed-by: nvsentinel
    spec:
      nodeName: {{ .HealthEvent.NodeName }}
      action: [my-action]
      healthEventId: {{ .HealthEventID }}
- Template rules: plain Go text/template ONLY (no Sprig — no quote, trunc, or hash helpers).
  Use full {{ .HealthEvent.NodeName }}-{{ .HealthEventID }} for metadata.name. Do not template
  free-form message text into YAML.
- Use a CRD kind with a regular plural (RemediationRequest -> remediationrequests); irregular
  plurals break fault-remediation's auto-generated RBAC.
- Deploy the values via your usual NVSentinel Helm upgrade (e.g. helm upgrade <release>
  <chart> -n nvsentinel --reuse-values -f <overrides>.yaml), then wait for the fault-remediation
  Deployment rollout.

Verification (against a running NVSentinel cluster):
- Trigger a health event with CUSTOM + customRecommendedAction="[my-action]" on a test node.
- Confirm fault-remediation created the RemediationRequest (kubectl get remediationrequests -n
  remediation).
- Confirm your controller reconciles it and sets status.conditions Type=Complete Status=True.
- Confirm fault-remediation logs show the repair as complete and does not create a duplicate CR.

Ensure controller `go mod tidy` and `go build ./...` pass. Do not run docker push, helm upgrade,
or kubectl apply unless credentials/cluster are available — but do create the Dockerfile, CRD,
controller, and Helm values snippets.
```
