# Fault Remediation Configuration

## Overview

The Fault Remediation module creates maintenance Custom Resources (CRs) that trigger external repair systems to fix faulty nodes. This document covers all Helm configuration options and extension points for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the fault-remediation module is deployed in the cluster.

```yaml
global:
  faultRemediation:
    enabled: true
```

> Note: This module depends on the results from fault-quarantine and node-drainer. It also depends on the datastore being enabled. Therefore, ensure the datastore and the other modules are also enabled.

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

## Maintenance Resource Configuration

Defines the Custom Resource that will be created to trigger remediation actions.

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
        completeConditionType: "Complete"
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
API group of the maintenance CRD installed by your maintenance operator.

#### version
API version of the maintenance CRD.

#### kind
Kubernetes Kind of the maintenance CRD.

#### scope
Determines whether the maintenance CRD is cluster-scoped or namespaced.

#### completeConditionType
Status condition name to check for maintenance completion. Used to prevent duplicate CRs when multiple faults occur on the same node. If condition status is `True`, maintenance is complete. 

#### namespace
Kubernetes namespace where maintenance CRs will be created.

#### equivalenceGroup
Defines which remediation actions are considered equivalent for deduplication. Actions in the same group will deduplicate against each other regardless of CRD type if a previous CRD is in a non-terminal state.

#### supersedingEquivalenceGroups
Defines additional equivalence groups that are considered equivalent for deduplication. For example, the COMPONENT_RESET action in the reset group should be deduplicated with the RESTART_VM action in the restart group. In other words, rebooting a node will have the same effect as resetting a GPU whereas the inverse is not true.

#### impactedEntityScope
For the COMPONENT_RESET action, the impacted entity scope should be defined so that there's a unique equivalence group for each entity. The unique equivalence group is constructed by appending the value for the given impacted entity to the equivalence group name. For example, each GPU needing reset will be in its own equivalence group named like reset-<GPU_UUID>.

#### templates
Go template that generates the maintenance CR YAML. See Template Extension Point section below.

## Template Extension Point

The maintenance template is a Go template that generates the Kubernetes CR YAML for remediation actions.

### Available Template Variables

- `.NodeName` (string) - Name of the node requiring maintenance
- `.HealthEventID` (string) - Unique ID of the triggering health event
- `.HealthEvent` (HealthEvent) - The entire content of the triggering health event
- `.RecommendedAction` (int) - Numeric action code from health event (see [health_event.proto](https://github.com/NVIDIA/NVSentinel/blob/main/data-models/protobufs/health_event.proto))
- `.RecommendedActionName` (string) - Action name from the health event
- `.ImpactedEntityScopeValue` (string) - The GPU_UUID used in COMPONENT_RESET remediation actions
- `.ApiGroup` (string) - Value from `maintenance.apiGroup`
- `.Version` (string) - Value from `maintenance.version`
- `.Kind` (string) - Value from `maintenance.kind`
- `.Namespace` (string) - Value from `maintenance.namespace`

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

1. **Unique Names**: Use `.NodeName` and `.HealthEventID` in CR name to ensure uniqueness
2. **Owner Reference**: The module automatically adds the Node as owner for automatic cleanup
3. **Action Codes**: Use conditional logic based on `.RecommendedAction` for different repair types

## Update Retry Configuration

Controls retry behavior when updating node annotations after creating maintenance CRs.

```yaml
fault-remediation:
  updateRetry:
    maxRetries: 5
    retryDelaySeconds: 10
```

### Parameters

#### maxRetries
Maximum number of retry attempts if annotation updates fail due to conflicts or network issues.

#### retryDelaySeconds
Base delay in seconds between retry attempts. Uses exponential backoff.

## Log Collector Configuration

Optionally collects diagnostic logs from nodes before remediation.

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
Enable or disable automatic log collection before creating maintenance CRs.

#### image.repository
Container image for the log collector.

#### image.pullPolicy
Pull policy for the log collector image.

#### uploadURL
HTTP endpoint where collected logs will be uploaded.

#### gpuOperatorNamespaces
Comma-separated list of namespaces containing GPU operator components for log collection.

#### enableGcpSosCollection
Enable collection of GCP-specific SOS reports.

#### enableAwsSosCollection
Enable collection of AWS-specific SOS reports.

#### timeout
Maximum time to wait for log collection job to complete.

#### env
Additional environment variables to pass to the log collector container.

## Integration with External Operators

The fault-remediation module is designed to integrate with external maintenance operators:

1. **CR Creation**: Fault-remediation creates a maintenance CR based on your template
2. **Operator Detection**: Your maintenance operator watches for new CRs
3. **Remediation Execution**: Operator performs the actual remediation (reboot, terminate, etc.)
4. **Status Update**: Operator updates the CR status with completion/failure information
5. **Completion Detection**: Fault-remediation checks `completeConditionType` to detect completion

### Operator Requirements

Your maintenance operator must:
- Watch for CRs matching your configured `apiGroup`, `version`, and `kind`
- Update CR status with a condition matching `completeConditionType`
- Set condition status to `True` on success, `False` on failure
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
