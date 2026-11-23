# Fault Remediation Configuration

## Overview

The Fault Remediation module creates maintenance Custom Resources (CRs). These CRs trigger external repair systems to fix faulty nodes. This document gives all the Helm configuration options and extension points for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the Helm chart deploys the fault-remediation module in the cluster.

```yaml
global:
  faultRemediation:
    enabled: true
```

> Note: This module depends on the results from fault-quarantine and node-drainer. It also needs an enabled datastore. Therefore, also enable the datastore and the other modules.

### Resources

Defines CPU and memory resource requests and limits for the fault-remediation pod.

```yaml
fault-remediation:
  resources:
    limits:
      cpu: "200m"
      memory: "300Mi"
    requests:
      cpu: "200m"
      memory: "300Mi"
```

### Logging

Sets the verbosity level for fault-remediation logs.

```yaml
fault-remediation:
  logLevel: info  # Options: debug, info, warn, error
```

### Change Stream Resume Token

> **Important**: In step 4, start only one replica. If two replicas start at the same time, both can run the reset. The second reset can then discard the resume token that the first replica saved.

To make fault-remediation skip the accumulated health events and start from the current stream head, do these steps:

1. If a GitOps controller, such as Argo CD, manages NVSentinel, stop its reconciliation, as in [Stop reconciliation](../runbooks/mongodb-bitnami-to-percona-migration.md#step-2-stop-reconciliation-gitops-managed-installations-only). A running controller can restore the replicas during this procedure.
2. Scale fault-remediation to zero. Wait until no pod of fault-remediation exists.
3. Set the key of fault-remediation in the shared resume-control ConfigMap to `CREATE`.
4. Scale fault-remediation to one replica.
5. After fault-remediation sets the key back to `RESUME`, restore the replicas of fault-remediation.
6. If you stopped the reconciliation in step 1, start it again.

During the startup in step 4, fault-remediation does these actions:

- It resets only its own resume token. With MongoDB, it deletes the token. With PostgreSQL, it sets the token to the current head.
- It records a cold-start cutoff timestamp.
- It skips the startup cold-start recovery for that run.
- It resets the key to `RESUME`.

Later restarts still run the cold-start recovery, but only for records newer than the recorded cutoff.

These commands do steps 2 to 5. Run them one at a time. If a `kubectl wait` command fails, find the cause before you continue.

```bash
REPLICAS=$(kubectl -n nvsentinel get deployment fault-remediation -o jsonpath='{.spec.replicas}')
kubectl -n nvsentinel scale deployment/fault-remediation --replicas=0
kubectl -n nvsentinel rollout status deployment/fault-remediation --timeout=180s
kubectl -n nvsentinel wait --for=delete pod -l app.kubernetes.io/name=fault-remediation --timeout=180s
kubectl -n nvsentinel get configmap resume-control >/dev/null 2>&1 || \
  kubectl -n nvsentinel create configmap resume-control
kubectl -n nvsentinel patch configmap resume-control \
  --type merge \
  -p '{"data":{"fault-remediation":"CREATE"}}'
kubectl -n nvsentinel scale deployment/fault-remediation --replicas=1
kubectl -n nvsentinel rollout status deployment/fault-remediation --timeout=180s
kubectl -n nvsentinel wait configmap/resume-control \
  --for=jsonpath='{.data.fault-remediation}'=RESUME --timeout=180s && \
  kubectl -n nvsentinel scale deployment/fault-remediation --replicas="${REPLICAS:-1}"
kubectl -n nvsentinel rollout status deployment/fault-remediation --timeout=180s
```

## Maintenance Resource Configuration

Defines the Custom Resource that fault-remediation creates to trigger remediation actions.

### Configuration Structure

```yaml
fault-remediation:
  maintenance:
    actions:
      "COMPONENT_RESET":
        apiGroup: "janitor.dgxc.nvidia.com"
        version: "v1alpha1"
        kind: "GPUReset"
        scope: "Cluster"
        completeConditionType: "Succeeded"
        templateFileName: "gpureset-template.yaml"
        equivalenceGroup: "reset"
        supersedingEquivalenceGroups: ["restart"]
        impactedEntityScope: "GPU_UUID"

    templates:
      "gpureset-template.yaml": |
        apiVersion: {{.ApiGroup}}/{{.Version}}
        kind: GPUReset
        metadata:
          name: maintenance-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
        spec:
          nodeName: {{ .HealthEvent.NodeName }}
          selector:
            uuids:
              - {{ .ImpactedEntityScopeValue }}
```

### Parameters

#### apiGroup

API group of the maintenance CRD that your maintenance operator installs.

#### version

API version of the maintenance CRD.

#### kind

Kubernetes Kind of the maintenance CRD.

#### scope

Determines whether the maintenance CRD is cluster-scoped or namespaced.

#### completeConditionType

The type of the status condition that gives the outcome of the maintenance. Fault-remediation reads this condition before it creates a new CR for the same equivalence group.

- If the status is `True`, the maintenance succeeded.
- If the status is `False`, the maintenance failed, and fault-remediation can create a new CR.
- If the condition is absent or its status is `Unknown`, the maintenance is in progress.

For `GPUReset`, use `Succeeded`. Janitor sets the `Complete` condition of a GPUReset to `True` when the GPUReset ends, also when the reset failed. Thus, `Complete` cannot show the difference between a failed reset and a successful reset.

#### namespace

Kubernetes namespace in which fault-remediation creates the maintenance CRs.

#### equivalenceGroup

Defines which remediation actions are equivalent for deduplication. The actions in the same group deduplicate against each other, even if the CRD types are different. If a previous CR of the group is in progress, fault-remediation creates no new CR. If that CR succeeded, fault-remediation creates no new CR for a health event that arrived before the CR. It marks that health event as remediated. A health event that arrives later can get a new CR.

#### supersedingEquivalenceGroups

Defines additional equivalence groups that are equivalent for deduplication. For example, the COMPONENT_RESET action in the reset group deduplicates with the RESTART_VM action in the restart group. The reason is that a node reboot has the same effect as a GPU reset. The inverse is not true.

#### impactedEntityScope

For the COMPONENT_RESET action, define the impacted entity scope, so that each entity has a unique equivalence group. To make this unique group, fault-remediation appends the value of the impacted entity to the equivalence group name. For example, each GPU that needs a reset is in its own equivalence group, with a name such as reset-`{GPU_UUID}`.

fault-remediation selects the first impacted entity of `impactedEntityScope` with a non-empty value, in health event order. This value names both the equivalence group and the device in the maintenance CR. If no such entity exists, fault-remediation creates no maintenance CR and retries the health event with backoff. It logs the error and increments `fault_remediation_processing_errors_total` with `error_type="impacted_entity_selection_error"`. The retries continue while the event still needs remediation; an event that is already remediated, has no recommended action, or whose quarantine has ended is acknowledged instead. A persistent count for one node means its monitor published an event without a usable entity. Check the monitor's event, then let the node recover through a healthy event or remediate it manually.

For a partial GPU drain, set `impactedEntityScope` to `GPU_UUID`. node-drainer uses the same selection rule for `GPU_UUID`, so the drain and the reset target the same GPU.

#### templates

Go template that generates the maintenance CR YAML. See the Template Extension Point section below.

## Template Extension Point

The maintenance template is a Go template that generates the Kubernetes CR YAML for remediation actions.

### Available Template Variables

- `.NodeName` (string) - Name of the node that needs maintenance
- `.HealthEventID` (string) - Unique ID of the triggering health event
- `.HealthEvent` (HealthEvent) - The entire content of the triggering health event
- `.RecommendedAction` (int) - Numeric action code from the health event (see [RecommendedAction Enum](../health-event-data-model.md#recommendedaction-enum))
- `.RecommendedActionName` (string) - Action name from the health event
- `.ImpactedEntityScopeValue` (string) - The GPU_UUID used in COMPONENT_RESET remediation actions
- `.TraceID` (string) - Trace ID from the health event metadata
- `.SpanID` (string) - Span ID of the fault-remediation span
- `.ApiGroup` (string) - Value from `maintenance.actions.<ACTION>.apiGroup`
- `.Version` (string) - Value from `maintenance.actions.<ACTION>.version`
- `.Kind` (string) - Value from `maintenance.actions.<ACTION>.kind`
- `.Namespace` (string) - Value from `maintenance.actions.<ACTION>.namespace`

### Template Examples

#### Example 1: Basic Reboot Template

```yaml
maintenance:
  actions:
    "RESTART_VM":
      apiGroup: "janitor.dgxc.nvidia.com"
      version: "v1alpha1"
      kind: "RebootNode"
      scope: "Cluster"
      completeConditionType: "NodeReady"
      templateFileName: "rebootnode-template.yaml"
      equivalenceGroup: "restart"

  templates:
    "rebootnode-template.yaml": |
      apiVersion: janitor.dgxc.nvidia.com/v1alpha1
      kind: RebootNode
      metadata:
        name: maintenance-{{ .NodeName }}-{{ .HealthEventID }}
      spec:
        nodeName: {{ .NodeName }}
```

#### Example 2: Template with Conditional Logic

```yaml
maintenance:
  actions:
    "RESTART_VM":
      apiGroup: "maintenance.example.com"
      version: "v1"
      kind: "NodeMaintenance"
      scope: "Cluster"
      completeConditionType: "NodeReady"
      templateFileName: "maintenance-template.yaml"
      equivalenceGroup: "maintenance"

  templates:
    "maintenance-template.yaml": |
      apiVersion: maintenance.example.com/v1
      kind: NodeMaintenance
      metadata:
        name: maintenance-{{ .NodeName }}-{{ .HealthEventID }}
      spec:
        nodeName: {{ .NodeName }}
        {{- if eq .RecommendedAction 15 }}
        action: reboot
        {{- else if eq .RecommendedAction 25 }}
        action: terminate
        {{- else }}
        action: investigate
        {{- end }}
```

### Template Guidelines

1. **Unique Names**: Use `.NodeName` and `.HealthEventID` in the CR name to make the name unique
2. **Owner Reference**: The module automatically adds the Node as the owner, for automatic cleanup
3. **Action Codes**: Use conditional logic on `.RecommendedAction` for different repair types

## Maximum Remediation Attempts

Sets the maximum number of attempts that fault-remediation makes for one equivalence group, for example `restart`, before the node returns to service.

```yaml
fault-remediation:
  maxRemediationAttempts: 0
```

Fault-remediation counts attempts, not retries:

- `1` allows one attempt and no retry.
- `3` allows two retries after the first attempt.
- `0` is the default and removes the cap.

When a node reaches the cap, fault-remediation sets the state label of the node to `remediation-failed` and stops. It leaves the node for an operator. This stops the remediation loop of a node when remediation never fixes the underlying fault.

With post-remediation validation, the quarantine ends when fault-quarantine gives the node to validation. But the node stays out of service until validation passes. At the hand-off, fault-remediation keeps the spent counts and binds them to the ValidationRequests in the validation session of the node. If NVSentinel finds a fault while one of those requests is still in the session, the count continues. The quarantine that this fault starts keeps that count, even if validation finishes later.

When the quarantine ends with no request in the session, fault-remediation clears its state. Then the count for the next fault starts from zero. This occurs in these cases:

- After a quarantine without validation.
- After a hand-off that fault-remediation processes before lifecycle-manager registers the request.

After validation releases the node, the count for the next fault also starts from zero. Fault-remediation drops the kept counts when that fault occurs. Thus, the `latestFaultRemediationState` annotation stays on the node until then. A cancellation of the quarantine resets the count immediately.

After the cap, the health monitors can send more health events for the same fault. Fault-remediation closes each of these health events before the log collector starts. Thus, a capped node gets no more log-collector Jobs.

If an unattended loop is worse than an unremediated node in your cluster, set a cap. With `0`, fault-remediation never sets `remediation-failed` because of a loop. Thus, an operator who watches only that state does not see the loop. If an operator detects remediation loops by other means, you can keep the value at `0`. Otherwise, set a cap.

## Update Retry Configuration

Controls the retry behavior when fault-remediation updates node annotations after it creates maintenance CRs.

```yaml
fault-remediation:
  updateRetry:
    maxRetries: 5
    retryDelaySeconds: 10
```

### Parameters

#### maxRetries

Maximum number of retry attempts if an annotation update fails because of conflicts or network issues.

#### retryDelaySeconds

Base delay in seconds between retry attempts. Fault-remediation uses exponential backoff.

## Log Collector Configuration

Fault-remediation can collect diagnostic logs from nodes before remediation. This feature is optional.

### Configuration

```yaml
fault-remediation:
  logCollector:
    enabled: false
    image:
      repository: ghcr.io/nvidia/nvsentinel/log-collector
      pullPolicy: IfNotPresent
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    gpuOperatorNamespaces: "gpu-operator"
    enableGcpSosCollection: false
    enableAwsSosCollection: false
    timeout: "10m"
    env: {}
```

### Parameters

#### enabled

Enables or disables automatic log collection before fault-remediation creates maintenance CRs.

#### image.repository

Container image for the log collector.

#### image.pullPolicy

Pull policy for the log collector image.

#### uploadURL

HTTP endpoint to which the log collector uploads the collected logs.

#### gpuOperatorNamespaces

Comma-separated list of the namespaces that contain GPU operator components, for log collection.

#### enableGcpSosCollection

Enables the collection of GCP-specific SOS reports.

#### enableAwsSosCollection

Enables the collection of AWS-specific SOS reports.

#### timeout

Maximum time that fault-remediation waits for the log collection job to complete.

#### env

Additional environment variables to pass to the log collector container.

## Integration with External Operators

The fault-remediation module integrates with external maintenance operators:

1. **CR Creation**: Fault-remediation creates a maintenance CR from your template
2. **Operator Detection**: Your maintenance operator watches for new CRs
3. **Remediation Execution**: The operator does the remediation, for example a reboot or a termination
4. **Status Update**: The operator updates the CR status with completion or failure information
5. **Completion Detection**: Fault-remediation checks `completeConditionType` to detect completion

### Operator Requirements

Your maintenance operator must:

- Watch for CRs that match your configured `apiGroup`, `version`, and `kind`
- Update the CR status with a condition that matches `completeConditionType`
- Set the condition status to `True` on success and to `False` on failure
- Handle node reboots, terminations, or other remediation actions

### Example Operator Status Update

```yaml
status:
  conditions:
    - type: NodeReady
      status: "True"
      reason: RebootComplete
      message: Node successfully rebooted and returned to Ready state
      lastTransitionTime: "2025-11-28T10:30:00Z"
```
