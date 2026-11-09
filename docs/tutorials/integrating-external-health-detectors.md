# Tutorial: Integrating an External Health Detector

This tutorial explains how to connect an external health-detection tool to
NVSentinel using the Kubernetes Object Monitor (KOM). After following it you
will understand the integration pattern, the rules a detector must follow, and
how to write a KOM policy for both node-level and GPU-level faults.

The [Node Problem Detector tutorial](integrating-node-problem-detector.md) is a
complete worked example of this pattern for host-log faults. Read the present
page first if you are integrating a different tool, or if you want to understand
the general design before reading the NPD walkthrough.

---

## 1. How the integration works

An external detector publishes its finding as a permanent Kubernetes
**Node Condition** on the affected node. KOM watches every node and fires a
NVSentinel `HealthEvent` when a condition's type, status, and reason match a
configured policy. The existing quarantine, cordon, and drain pipeline then
handles the node exactly as it would any other fault.

```mermaid
flowchart LR
    Det["External detector"] -->|"Node Condition"| API["Kubernetes API"]
    API --> KOM["Kubernetes Object Monitor"]
    KOM -->|"HealthEvent"| PC["Platform Connector"]
    PC --> NVS["NVSentinel quarantine / drain"]
```

No NVSentinel code changes are needed. The detector is fully decoupled: it only
writes a standard Kubernetes resource that KOM already knows how to read.

---

## 2. Rules a detector must follow

A detector that publishes a Node Condition for KOM to consume must follow these
rules to avoid false alarms and split-ownership conflicts.

**Ownership**
- Only one controller writes a given condition type on a given node. If two
  tools write the same condition type, they will overwrite each other.

**Signaling a problem**
- Set `status: "True"` and a stable, non-empty `reason` string when a problem is
  detected.
- The reason string must be constant across repeated firings of the same fault
  class so the KOM CEL predicate can match it reliably.

**Signaling recovery**
- Set `status: "False"` and a matching healthy-reason string *only after the
  detector has confirmed recovery*, not simply because the check interval
  elapsed. Setting `False` before recovery causes KOM to observe a healthy
  transition that can cancel active break-fix processing.
- Do not set `status: "Unknown"` while a fault is active. If the condition
  transitions to `Unknown`, the KOM predicate (which matches `status == "True"`)
  becomes false, and KOM will publish a healthy event that can cancel active
  remediation. The condition must remain `True` with the same `reason` string
  until the detector has confirmed recovery.

**Policy naming**
- Each condition type should have its own named KOM policy. The policy `name`
  becomes the `checkName` in the resulting `HealthEvent`, so choose a stable,
  descriptive string (for example `ExternalGPUIntegrityCheck`).

---

## 3. Write a KOM policy for the condition

### Node-level faults

Use `componentClass: Node` when the condition represents a problem with the
host that affects the whole node (filesystem errors, hardware memory errors,
kernel panics, and so on). This is the pattern used by Node Problem Detector.

```yaml
- name: ExternalHostFaultDetected
  enabled: true
  resource:
    group: ""
    version: v1
    kind: Node
  predicate:
    expression: |
      resource.status.conditions.exists(c,
        c.type   == "ExternalHostFault" &&
        c.status == "True" &&
        c.reason == "HostFaultDetected")
  healthEvent:
    componentClass: Node
    isFatal: true
    message: "An external host-fault detector reported a problem on this node"
    recommendedAction: CONTACT_SUPPORT
    errorCode:
      - EXTERNAL_HOST_FAULT
```

Replace `ExternalHostFault`, `HostFaultDetected`, the policy `name`, `message`,
`recommendedAction`, and `errorCode` with the values your detector uses.

### GPU-level faults

Use `componentClass: GPU` when the condition represents a problem specific to a
GPU workload rather than the host itself (inference-integrity errors, model
divergence, and similar). NVSentinel will associate the event with the GPU
rather than the whole node.

> **Limitation:** KOM publishes the Node as the impacted entity in the
> `HealthEvent` — it does not enumerate individual GPUs on that node. The event
> is therefore node-scoped: consumers that require a GPU UUID cannot derive one
> from this event. If you need per-GPU precision, see the
> [Writing a Health Monitor](writing-a-health-monitor.md) guide, which describes
> how to send `HealthEvent`s over gRPC with an explicit GPU UUID in
> `entitiesImpacted`.

```yaml
- name: ExternalGPUIntegrityCheck
  enabled: true
  resource:
    group: ""
    version: v1
    kind: Node
  predicate:
    expression: |
      resource.status.conditions.exists(c,
        c.type   == "ExternalGPUIntegrity" &&
        c.status != "False" &&
        c.reason == "GPUIntegrityFailed")
  healthEvent:
    componentClass: GPU
    isFatal: true
    message: "An external GPU integrity checker reported a fault on this node"
    recommendedAction: RUN_FIELDDIAG
    errorCode:
      - EXTERNAL_GPU_INTEGRITY_FAULT
```

### Add the policy to your values file

Append the policy block to the existing
`kubernetes-object-monitor.policies` list in your Helm values file. Helm
replaces the complete list on upgrade, so your values file must include
**all** policies you need — both any existing custom policies and the chart's
default policies (such as `ReplaceNotReadyNode`). Omitting a policy from the
list silently removes it on the next upgrade.

To see the default policies shipped by the KOM subchart before editing,
set your version first and inspect the subchart directly:

```bash
NVSENTINEL_VERSION="<your-version>"

# Inspect the KOM subchart defaults (policies live there, not in the umbrella chart)
# Pull the umbrella chart and inspect the bundled KOM subchart defaults:
helm pull oci://ghcr.io/nvidia/nvsentinel \
  --version "$NVSENTINEL_VERSION" --untar
grep -A 60 'policies:' nvsentinel/charts/kubernetes-object-monitor/values.yaml
rm -rf nvsentinel/

# On an existing release, also check the currently active effective values
# (--all includes chart defaults, not just user-supplied overrides):
helm get values nvsentinel --namespace nvsentinel --all
```

```yaml
global:
  kubernetesObjectMonitor:
    enabled: true

kubernetes-object-monitor:
  policies:
    # Include all default policies (e.g. ReplaceNotReadyNode) and any
    # existing custom policies here before adding the new one below.
    - name: ExternalGPUIntegrityCheck
      enabled: true
      resource:
        group: ""
        version: v1
        kind: Node
      predicate:
        expression: |
          resource.status.conditions.exists(c,
            c.type   == "ExternalGPUIntegrity" &&
            c.status == "True" &&
            c.reason == "GPUIntegrityFailed")
      healthEvent:
        componentClass: GPU
        isFatal: true
        message: "An external GPU integrity checker reported a fault on this node"
        recommendedAction: RUN_FIELDDIAG
        errorCode:
          - EXTERNAL_GPU_INTEGRITY_FAULT
```

Apply the updated values:

> **Warning:** **Use a non-production cluster for this validation step.**
> On an existing release, `--reuse-values` retains any enabled remediation
> pipeline. A matching condition on any node will trigger a fatal `HealthEvent`
> and may cordon or drain that node before you reach the validation steps below.
>
> If a non-production cluster is not available, you may disable all downstream
> remediation components, but note that disabling `faultQuarantine` carries an
> additional risk: any fault event emitted while quarantine restarts may be
> missed, because KOM does not republish an unchanged match and MongoDB change
> streams do not replay missed events on cold start. Only proceed with this
> approach if you understand and accept this risk:
>
> ```yaml
> global:
>   faultRemediation:
>     enabled: false
>   faultQuarantine:
>     enabled: false
>   node-drainer:
>     enabled: false
> ```

```bash
helm upgrade --install nvsentinel oci://ghcr.io/nvidia/nvsentinel \
  --version "$NVSENTINEL_VERSION" \
  --namespace nvsentinel \
  --create-namespace \
  --reuse-values \
  --values your-values.yaml \
  --wait
```

Verify KOM picked up the new policy by checking the rollout and the rendered
configuration:

```bash
# Confirm the new pod is running
kubectl rollout status deployment/kubernetes-object-monitor \
  --namespace nvsentinel

# Inspect the rendered policy list loaded by KOM
kubectl logs deployment/kubernetes-object-monitor \
  --namespace nvsentinel | grep -i "loaded policy"
# Expected: a "Loaded policy configuration" entry appearing after the
# most recent pod start, confirming KOM read its configuration successfully.
# Cross-check by viewing the ConfigMap KOM reads from:
kubectl get configmap -n nvsentinel -l app.kubernetes.io/name=kubernetes-object-monitor \
  -o yaml | grep -A 5 "ExternalGPUIntegrityCheck"
```

---

## 4. Validate the integration

After the detector sets the condition on a test node, confirm the full chain
fires before enabling downstream remediation in production.

**Step 1 — confirm the condition is set**

```bash
NODE="<your-test-node>"

kubectl get node "$NODE" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} reason={.reason}{"\n"}{end}' \
  | grep ExternalGPUIntegrity
# Expected:
# ExternalGPUIntegrity=True reason=GPUIntegrityFailed
```

**Step 2 — confirm KOM matched the policy and the HealthEvent was received**

```bash
kubectl logs deployment/kubernetes-object-monitor \
  --namespace nvsentinel \
  | grep "ExternalGPUIntegrityCheck"
# Expected: a log entry showing the policy matched

# Confirm the HealthEvent was received downstream by checking for the
# condition the platform connector sets, named after the policy check:
kubectl get node "$NODE" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status}{"\n"}{end}' \
  | grep "ExternalGPUIntegrityCheck=True"
# Expected: ExternalGPUIntegrityCheck=True
#
# Note: if kubernetes-object-monitor.processingStrategy is set to STORE_ONLY,
# the platform connector does not write a node condition, so this check will
# return no output even when delivery succeeded. In that mode, confirm receipt
# by querying the datastore (MongoDB or PostgreSQL) for the stored HealthEvent
# instead of checking the node condition.
```

**Step 3 — confirm the node was cordoned** (only if downstream remediation is
enabled)

```bash
kubectl get node "$NODE" -o jsonpath='{.spec.unschedulable}'
# Expected: true
```

**Step 4 — confirm recovery**

Once the detector clears the condition (`status: "False"`), verify that KOM
observes the transition and that any pending break-fix is resolved before
uncordoning the node.

---

## 5. Worked example: Node Problem Detector

[Node Problem Detector (NPD)](https://github.com/kubernetes/node-problem-detector)
publishes permanent Node Conditions from kernel log patterns. It is the
canonical example of this integration pattern for host-level faults.

See the full walkthrough, including installation, policy configuration, and
end-to-end validation with synthetic kernel-message injection, in the
[Node Problem Detector tutorial](integrating-node-problem-detector.md).

The key difference between NPD and a GPU-workload checker is the
`componentClass` in the KOM policy:

| Detector type | Fault class | `componentClass` | Clears condition when |
|---|---|---|---|
| Node Problem Detector | Host/kernel errors | `Node` | NPD process restarts |
| GPU workload checker | Inference / compute errors | `GPU` | Detector confirms GPU recovery |

---

## Troubleshooting

### KOM does not publish a HealthEvent after the condition is set

- Confirm `global.kubernetesObjectMonitor.enabled: true` in your Helm values.
- Confirm the policy has `enabled: true`.
- Compare the actual condition type, status, and reason on the node with the
  CEL predicate in the policy — they must match exactly, including case.

```bash
kubectl get node "<node>" \
  -o jsonpath='{range .status.conditions[*]}{.type}={.status} reason={.reason}{"\n"}{end}'

kubectl logs deployment/kubernetes-object-monitor --namespace nvsentinel
# Expected when healthy: "Loaded policy configuration", "Registered controller",
# and "Starting manager" entries, with no predicate errors.
```

### The condition is set but KOM reports a predicate error

CEL expressions are strict about quoting and field paths. Test the predicate
against a node's JSON with `kubectl get node <node> -o json` and the
[CEL Playground](https://playcel.undistro.io/) before deploying.

### A node is cordoned but the condition was already cleared

NVSentinel cordons on the fatal `HealthEvent`. Whether the node is subsequently
uncordoned automatically depends on your deployment configuration:

- **With fault quarantine enabled:** The fault-quarantine component tracks all
  active `HealthEvent`s for the node. When all tracked events recover (the
  detector sets `status: "False"`), fault quarantine can automatically uncordon
  the node after any configured validation gate passes.
- **Without fault quarantine, or with manual gates configured:** A human
  operator must uncordon the node after break-fix is complete.

Clearing the Node Condition alone does not uncordon the node — KOM must observe
the `False` transition and publish a recovery event for the downstream pipeline
to act on it.
