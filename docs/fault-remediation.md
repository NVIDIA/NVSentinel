# Fault Remediation

## Overview

The Fault Remediation module connects NVSentinel to external repair systems. After NVSentinel quarantines and drains a node, Fault Remediation creates maintenance CRs. These maintenance CRs start remediation actions, for example node reboots, hardware replacements or cloud provider interventions.

Fault Remediation works as a dispatch coordinator. A facility manager calls specialists when equipment needs repair. In the same way, Fault Remediation tells your maintenance systems that a node is ready for maintenance.

### Why Do You Need This?

After NVSentinel isolates a faulty node and evacuates its workloads, the hardware needs a repair:

- **Hardware replacement**: Faulty GPUs need a physical replacement
- **Node reboots**: A clean restart corrects some faults
- **GPU resets**: A GPU reset corrects some faults
- **Cloud provider actions**: Some VMs need termination and recreation

Fault Remediation creates Kubernetes custom resources (maintenance CRs). External operators, for example Janitor, watch these maintenance CRs and do the repair work.

## How It Works

Fault Remediation watches the datastore for drained nodes that need repair. For each health event, Fault Remediation does these steps:

1. Receives the health event and its recommended action, for example RESTART_VM or REPLACE_VM
2. Skips the NONE action. For an action that has no entry in `maintenance.actions`, for example UNKNOWN, Fault Remediation creates no maintenance CR. It increments `fault_remediation_unsupported_actions_total`. If the node is still quarantined for this health event, it sets the state label to `remediation-failed`.
3. Records the selected remediation action on the health event
4. Checks if a maintenance CR already exists for the node
5. Before a new remediation starts, checks that the node is still quarantined for this health event. If its quarantine already ended, the event is closed without a maintenance CR. An event that a maintenance CR already covers waits for that CR or is closed with it, as in step 4.
6. Starts log collection, if log collection is enabled
7. Sets the state label to `remediating`
8. Creates a maintenance CR from the configured template
9. Sets the state label to `remediation-succeeded` if it created the maintenance CR, or to `remediation-failed` if the creation failed. The label shows the result of the CR creation. The status of the repair itself is on the maintenance CR.

External operators watch these maintenance CRs and do the maintenance work, for example a reboot, a termination or a replacement.

## Configuration

Configure the Fault Remediation module through Helm values:

```yaml
fault-remediation:
  enabled: true
  dryRun: false          # Test mode - logs actions without executing

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
      "COMPONENT_RESET": 
        apiGroup: "janitor.dgxc.nvidia.com"
        version: "v1alpha1"
        kind: "GPUReset"
        scope: "Cluster"
        completeConditionType: "Succeeded"
        templateFileName: "gpureset-template.yaml"
        equivalenceGroup: "reset"
        impactedEntityScope: "GPU_UUID"
        supersedingEquivalenceGroups: ["restart"]

    templates:
      "rebootnode-template.yaml": |
        apiVersion: {{ .ApiGroup }}/{{ .Version }}
        kind: RebootNode
        metadata:
          name: maintenance-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
        spec:
          nodeName: {{ .HealthEvent.NodeName }}
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
  
  logCollector:
    enabled: false       # Enable log collection before remediation
    uploadURL: "http://nvsentinel-incluster-file-server.nvsentinel.svc.cluster.local/upload"
    timeout: "10m"
```

### Configuration Options

- **Dry Run**: Tests the creation of maintenance CRs, but does not create them
- **Maintenance CRD**: Defines the custom resource to create (apiGroup, version, kind, namespace)
- **Template**: Go template that generates the maintenance CRs
- **Log Collection**: Optional collection of diagnostic logs before remediation (syslog, GPU logs, driver information)

## Key Features

### Template-Based CRD Generation

The Go templates let you match the maintenance CRs to your maintenance operator:

- Customize the structure of the maintenance CR
- Use the node name, the impacted GPU UUID, the health event ID and other properties
- Support different remediation types (reboot, terminate, replace)

### Intelligent Filtering

Fault Remediation creates a maintenance CR only when it is necessary:

- Skips NONE actions, and actions that have no entry in `maintenance.actions` (for example UNKNOWN)
- Checks for existing maintenance CRs
- Prevents duplicate maintenance CRs

### State Tracking

Fault Remediation updates node labels during the remediation lifecycle:

- `remediating`: Fault Remediation is creating the maintenance CR
- `remediation-succeeded`: Fault Remediation created the maintenance CR. The repair can still be in progress.
- `remediation-failed`: Fault Remediation created no maintenance CR, for example because the creation failed, the action has no entry in `maintenance.actions`, or the equivalence group used all its remediation attempts

### Remediation Decision

For each health event, Fault Remediation records the entry of `maintenance.actions` that it uses. It writes this record in `remediationDecision` of the health event status. It writes the record before the log collector runs and before it creates the maintenance CR. Thus, the record is on the health event while the remediation is in progress.

The entry is the recommended action of the health event, or `customRecommendedAction` for a `CUSTOM` health event. Fault Remediation writes no record for a health event that it skips, for example a health event with an action that has no entry. See [Drain and Remediation Decisions](health-event-data-model.md#drain-and-remediation-decisions).

### Optional Log Collection

Fault Remediation can collect diagnostics before remediation, for troubleshooting and root cause analysis.

## Integration Patterns

Fault Remediation creates maintenance CRs. External operators use these maintenance CRs:

- **Janitor**: Watches the maintenance CRs and calls the cloud provider API to reboot or terminate nodes.
- **Custom Break-Fix Systems**: Define custom CRD schemas. Then deploy operators that integrate with your own maintenance systems.
- **Manual Workflow Systems**: Deploy a controller that creates tickets in your ticketing system from the maintenance CRs, for manual processing.
