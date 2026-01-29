# Fault Remediation

## Overview

The Fault Remediation module is NVSentinel's bridge to external repair systems. After a node has been quarantined and drained, this module creates maintenance requests that trigger break-fix workflows - such as node reboots, hardware replacements, or cloud provider interventions.

Think of it as a dispatch coordinator - similar to how a facility manager calls in specialists when equipment needs repair, Fault Remediation notifies your maintenance systems that a node is ready for servicing.

### Why Do You Need This?

After NVSentinel isolates a faulty node and evacuates workloads, the hardware needs to be fixed:

- **Hardware replacement**: Faulty GPUs need to be physically replaced
- **Node reboots**: Some issues resolve with a clean restart
- **GPU resets**: Some issues resolve with a GPU reset
- **Cloud provider actions**: VMs may need termination and recreation

The Fault Remediation module creates Kubernetes Custom Resources (CRDs) that external operators (like Janitor) watch and act upon to perform the actual repair work.

## How It Works

The Fault Remediation module watches the datastore for drained nodes that need repair:

1. Receives events with recommended actions (RESTART_VM, REPLACE_VM, etc.)
2. Filters out NONE and UNKNOWN actions
3. Checks if a maintenance CR already exists for the node
4. Optionally triggers log collection
5. Creates maintenance Custom Resource using configured template
6. Updates node labels to track remediation state

External operators watch for these CRs and perform the actual maintenance work (reboot, terminate, replace, etc.).

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
        completeConditionType: "Complete"
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

- **Dry Run**: Test CRD creation without creating maintenance requests
- **Maintenance CRD**: Define the Custom Resource to create (apiGroup, version, kind, namespace)
- **Template**: Go template for generating CRDs
- **Log Collection**: Optionally collect diagnostic logs before remediation (syslog, GPU logs, driver information)

## Key Features

### Template-Based CRD Generation
Flexible Go template system to match your maintenance operator:
- Customize CRD structure 
- Access node name, the impacted GPU UUID, event ID, and other properties
- Support different remediation types (reboot, terminate, replace)

### Intelligent Filtering
Only creates requests when needed:
- Skips NONE and UNKNOWN actions
- Checks for existing maintenance CRs
- Prevents duplicate requests

### State Tracking
Updates node labels throughout remediation lifecycle:
- `remediating`: Maintenance request created
- `remediation-succeeded`: Maintenance completed
- `remediation-failed`: Maintenance encountered errors

### Optional Log Collection
Gather diagnostics before remediation for troubleshooting and root cause analysis.

## Integration Patterns

The Fault Remediation module creates CRDs consumed by external operators:

**Janitor Operator**: Watches for maintenance CRDs and performs cloud provider API calls to reboot/terminate nodes.
**Custom Break-Fix Systems**: Define custom CRD schemas and deploy operators to integrate with your own maintenance systems.
**Manual Workflow Systems**: Deploy a controller that creates tickets from CRs for manual processing in your ticketing system.
