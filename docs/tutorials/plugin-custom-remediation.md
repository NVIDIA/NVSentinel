# Tutorial: Plugin Custom Remediation

This tutorial shows how to extend the **fault-remediation** stage of NVSentinel with your own repair automation, for example a custom controller.

When you complete this tutorial, you understand:

- Where custom remediation fits in the NVSentinel pipeline.
- How to register a **custom remediation action** with no change to the NVSentinel code.
- The status-condition completion contract that your controller must implement.

> **Who is this for?** Teams that **already run NVSentinel** and need a repair step that NVSentinel does not include, instead of the built-in reboot, terminate or GPU-reset actions. Examples are CSP RMA tickets, multi-step orchestration and other custom repair steps.

> **Do you want an AI agent to do it?** Go to [Appendix: One-shot AI prompt](#appendix-one-shot-ai-prompt).

---

## 1. Where custom remediation fits

NVSentinel remediates a faulty node in stages:

```text
health-monitor → fault-quarantine → node-drainer → fault-remediation → your system
```

**fault-remediation** is the extension point. After fault-quarantine cordons a node and node-drainer drains it, fault-remediation does these steps:

1. Resolves the **recommended action** of the health event (a built-in enum value or a custom string).
2. Finds a matching entry in the `maintenance.actions` Helm value.
3. Renders a **Go template** into a Kubernetes Custom Resource.
4. **Creates** that CR in the cluster.
5. **Polls** the status conditions of the CR (`completeConditionType`) to prevent duplicate repair requests.

Your controller watches the CR and does the actual repair.

```text
  fault-remediation ──create CR──► Kubernetes API ◄──watch── Your controller
        ▲                              │
        └── poll completeConditionType ┘
                    (repair + set status condition)
```

### Built-in and custom actions

| Action source | Example | Config key in `maintenance.actions` |
|---------------|---------|-------------------------------------|
| Built-in enum | `RESTART_VM`, `COMPONENT_RESET`, `REPLACE_VM` | `RESTART_VM`, `COMPONENT_RESET`, … |
| Custom string | `recommendedAction: CUSTOM`, `customRecommendedAction: hardware-rma` | `hardware-rma` |

---

## 2. Integration approach

fault-remediation creates a Kubernetes CR for your action. Your controller watches the CR, runs the repair and reports completion through a status condition.

### Why your CR must expose status conditions

The status checker of fault-remediation understands only **Kubernetes-style conditions**. It looks for a `status.conditions[]` entry whose `type` equals `completeConditionType`. Then it reads the `status` of that entry (`True` → done, `False` → failed and retry).

Many external orchestrators report completion in a different way, for example with a phase field, a job exit code or a ticket status. For these orchestrators, use a thin wrapper CRD. Its controller translates the external completion into `status.conditions[Complete]=True`.

---

## 3. The completion contract

For each CR that NVSentinel creates, fault-remediation reads a status condition to decide "is this repair done?":

```yaml
status:
  conditions:
    - type: "{completeConditionType}"   # e.g. "Complete"
      status: "True"                  # True = success, False = failed (retry allowed)
      reason: RepairSucceeded
      message: Node hardware repaired and validated
```

Behavior of the status checker:

- **Condition `True`** → the repair succeeded. NVSentinel marks the equivalent health events that arrived before the CR as covered.
- **Condition `False`** → the repair failed. NVSentinel drops the group and **creates a new CR to retry**. No "terminal failure" state exists. If a repair is unrecoverable, leave the condition `Unknown` or absent. Do not set it to `True` or `False`. As an alternative, stop requeueing after your own retry budget.
- **Condition missing or `Unknown`** → the repair is in progress. NVSentinel **does not create** a duplicate CR.

fault-remediation derives an **effective equivalence group** from the matched action. If the action sets `impactedEntityScope`, the effective group also includes the impacted entity, for example `reset-GPU-123`. While a CR in that effective group or in a superseding group is in progress, fault-remediation does not create a new CR. After that CR succeeds, fault-remediation creates no new CR for a health event that arrived before the CR. For a health event that arrives later, fault-remediation can create a new CR. fault-remediation does **not** deduplicate every repair on the node.

`supersedingEquivalenceGroups` lets a broader action cover a narrower one. The **subordinate** action lists the group that supersedes it. For example, a node reboot has the same effect as an RMA, so the `hardware-rma` action lists `restart`:

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

A health monitor must set two fields, the action type and the custom action name:

```yaml
recommendedAction: CUSTOM
customRecommendedAction: hardware-rma
```

> If you cannot change a monitor, use a platform-connector override to route its health events to your custom action. The override sets `recommendedAction: CUSTOM` and `customRecommendedAction`. See [Override Transformer Configuration](../configuration/platform-connectors.md#override-transformer-configuration).

### Step 2 — Register the action in fault-remediation Helm values

Add your action and its template to your own Helm values file, under the `fault-remediation` key:

```yaml
fault-remediation:
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
| `equivalenceGroup` | Base group name. fault-remediation combines it with the impacted entity into the effective group for dedup. |
| `completeConditionType` | The condition `type` that fault-remediation waits for |
| `supersedingEquivalenceGroups` | Optional. For example, `restart` supersedes `external-rma`. |
| `impactedEntityScope` | Optional. Dedup for each entity, for example for each GPU. Custom actions can set this field. |
| `templates` | The Go template that fault-remediation renders into the CR body |

> **RBAC:** fault-remediation generates RBAC rules automatically from `maintenance.actions`. The resource name is the lowercase `kind` plus `s`. Use CRD kinds with regular plurals (`RemediationRequest` → `remediationrequests`). Irregular plurals (`Policy` → `policys`) break RBAC.

Apply the values file to your NVSentinel release with `helm upgrade`. Use `--reset-then-reuse-values`, so that Helm keeps the values that you set before. Helm adds the new action to the default actions, so the default actions stay. For a complete command, see [Integrating External Remediation](integrating-external-remediation.md#4-add-the-externalremediationrequest-template).

### Step 3 — Template variables

These variables are available in every maintenance template:

| Variable | Description |
|----------|-------------|
| `.NodeName` / `.HealthEvent.NodeName` | Target node |
| `.HealthEventID` | ID of the triggering health event |
| `.HealthEvent` | Full health event (all fields) |
| `.RecommendedAction` | Numeric action code |
| `.RecommendedActionName` | Resolved action name (`hardware-rma`, `RESTART_VM`, …) |
| `.ImpactedEntityScopeValue` | GPU UUID for per-GPU actions |
| `.ApiGroup`, `.Version`, `.Kind`, `.Namespace` | From the action config |
| `.TraceID`, `.SpanID` | OpenTelemetry correlation (with tracing enabled) |

### Step 4 — Build your controller

Scaffold a controller that:

1. Watches your CR kind (`RemediationRequest`).
2. Does the repair. For example, it calls a CSP API, opens a support ticket or runs diagnostics.
3. Patches the status condition:

```yaml
status:
  conditions:
    - type: Complete          # must match completeConditionType
      status: "True"          # "True" = done; "False" makes NVSentinel retry with a new CR
      reason: RepairSucceeded
      message: Node hardware repaired and validated
```

**Golden rules** (the same as for any NVSentinel plugin):

1. Make Reconcile **idempotent**. If the CR already has `Complete=True`, make Reconcile a no-op.
2. **Make non-idempotent repairs durably idempotent.** Your repair can run again after a controller restart before `Complete=True`, or after a retry for a `False` condition, which creates a new CR. Examples of one-shot side effects are the creation of a ticket and a call to a CSP API. Send a durable key, such as `spec.healthEventId`, in each request that causes one of these effects. The external system must reject a repeated key, or your controller must find the first result by the key before it tries again. A status phase alone cannot prevent a duplicate, because your controller can crash after the effect and before it saves the phase.
3. Set `Complete=True` only when the repair is **complete**. Until then, requeue. Set `Complete=False` only when you want NVSentinel to retry with a new CR.
4. One CR for each health event. NVSentinel manages the lifecycle of the CR through the equivalence group.

Set `IMG` to a registry that **your cluster can pull from**. Then build and push the controller image with the Kubebuilder Makefile:

```bash
docker login   # once, to authenticate

# Replace YOUR_USER and my-remediation with your Docker Hub user and image name.
export IMG=docker.io/YOUR_USER/my-remediation:dev   # or your cluster's private registry
make docker-build docker-push IMG=$IMG
make install && make deploy IMG=$IMG
```

> Use a **public** repository, so that the cluster can pull the image without credentials. For a private registry, for example NVCR, add the `imagePullSecret` of the cluster to the controller Deployment in `config/`.

For a full Kubebuilder scaffold and a reconciler walkthrough, see [Writing a Drain Plugin — Appendix: One-shot AI prompt](./writing-a-drain-plugin.md#appendix-one-shot-ai-prompt). That tutorial also has a one-shot AI prompt, and it uses the same pattern. NVSentinel creates the CR and polls a status condition, and your controller reconciles the CR and sets completion. The upstream stage is different: node-drainer, not fault-remediation.

---

## Appendix: One-shot AI prompt

Paste this prompt into an AI coding agent. The prompt is **self-contained**. The controller is a standalone Kubebuilder project in **any repository**. The prompt also asks for Helm values snippets for the fault-remediation registration, separate from the controller repository. Replace the bracketed parts.

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
- To route an event from a monitor you cannot change, add a platform-connector override that
  sets BOTH recommendedAction: "CUSTOM" and customRecommendedAction: "[my-action]".

Controller (standalone Kubebuilder project, any repo):
- Scaffold: kubebuilder init --domain example.com --repo github.com/{your-org}/[my-remediation]
  then kubebuilder create api --group remediation --version v1alpha1 --kind RemediationRequest
  --resource --controller.
- Spec: nodeName (string), action (string), healthEventId (string) — minimal fields from the
  fault-remediation template below.
- Status: conditions []metav1.Condition with a subresource.
- Reconciler: idempotent; no-op once Complete=True; a restart or a retry can run the repair again, so
  send a durable key such as spec.healthEventId in each request that has a one-shot side effect (a
  ticket or a CSP API call); the external system must reject a repeated key, or the reconciler must
  find the first result by that key before it tries again (a status phase alone cannot prevent a
  duplicate, because the controller can crash after the effect and before it saves the phase);
  patch status with Type="Complete", Status="True" when repaired (or "False" to make NVSentinel retry
  with a new CR) using append-or-update-by-type (meta.SetStatusCondition + Status().Update).
  RBAC: remediationrequests + status; add any API permissions your repair needs.
- Build and push with the generated Dockerfile/Makefile, pointing IMG at a registry your cluster can
  pull from (e.g. docker.io/{your-user}/[my-remediation]:dev or a private registry such as NVCR).
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
- Deploy the values via your usual NVSentinel Helm upgrade (e.g. helm upgrade {release}
  {chart} -n nvsentinel --reset-then-reuse-values -f {overrides}.yaml), then wait for the fault-remediation
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
