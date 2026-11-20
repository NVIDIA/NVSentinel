# NVSentinel Integration Guide

NVSentinel detects GPU and hardware faults and exposes them through standard Kubernetes primitives. This document gives a high-level overview of how to integrate with NVSentinel for scheduling, monitoring, and remediation.

## Integration Model

NVSentinel integration has five layers:

1. **Is a node bad?** → Check **[Taints](#1-is-a-node-bad-check-taints)**
   - Taints mark nodes with hardware issues
   - Use taints for scheduling decisions and filtering
   - In automation, react to the presence or absence of a taint

2. **Why is a node bad?** → Check **[Node Conditions](#2-why-is-a-node-bad-check-node-conditions)**
   - Node conditions give detailed diagnostic information
   - Use node conditions for monitoring, alerting, and dashboards
   - Each node condition explains which hardware component failed

3. **Can I use my own remediation?** → Provide a **[Custom Resource](#3-can-i-use-my-own-remediation-provide-a-custom-resource)**
   - NVSentinel triggers external systems through CRs
   - Integrate with cloud APIs, DCIM, or custom controllers
   - You keep full control of how your systems repair the nodes

4. **How do I customize drain behavior?** → Configure **[per-namespace eviction modes](#4-how-do-i-customize-drain-behavior-configure-eviction-modes)**
   - Control how Node Drainer evicts workloads from faulty nodes
   - Define different policies for stateless and stateful workloads
   - Set timeouts and grace periods for each namespace

5. **Do GPU pods need to run diagnostics before the workload starts?** → Enable **[Preflight](./configuration/preflight.md)**
   - You enable Preflight for each namespace (opt-in). A webhook injects init-container checks (DCGM, optional NCCL)
   - Multi-node jobs use **gang discovery** (native Kubernetes gang APIs or PodGroup-style schedulers, such as Volcano and Run:ai)
   - The checks publish health events through Platform Connectors. If Fault Quarantine is enabled, a default Fault Quarantine ruleset cordons the node for a fatal health event (see [Data flow](./DATA_FLOW.md#preflight-optional-admission-checks))

## Quick Start

**For Scheduling Decisions:**

If you configured NVSentinel taints, find the nodes that have them:

```shell
kubectl get nodes -o json | jq '.items[] 
  | select(.spec.taints[]? 
  | select(.key | startswith("nvidia.com/"))) 
  | .metadata.name'
```

**For Monitoring:**

Get the detailed fault information:

```shell
kubectl get nodes -o json | jq '.items[].status.conditions[] 
  | select(.type | startswith("Gpu"))'
```

**For Pod Tolerations:**

```yaml
# Match the taint configured in your fault-quarantine rulesets
tolerations:
  - key: "nvidia.com/gpu-xid-error"
    operator: "Equal"
    value: "true"
    effect: "NoSchedule"
```

## Architecture

For more context, see [DATA_FLOW.md](./DATA_FLOW.md). At a high level, NVSentinel detects hardware faults and applies graduated responses in these stages:

1. **Detection**: Health monitors check the GPUs, the system logs, and the cloud maintenance events
2. **Classification**: Platform Connectors validate the health events and set node conditions
3. **Quarantine**: Fault Quarantine evaluates rules, then applies taints and cordons
4. **Evacuation**: Node Drainer evicts workloads according to the configured policies
5. **Remediation**: Fault Remediation triggers external systems through CRs

```text
┌─────────────────────┐
│  Health Monitors    │ GPU, Syslog, CSP health detection
└──────────┬──────────┘
           │ Detect failures
           ▼
┌─────────────────────┐
│ Platform Connectors │ Set NodeConditions (why is it bad?)
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Fault Quarantine    │ Apply Cordon/Taints (node is bad)
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Node Drainer        │ Evict workloads per policy
└──────────┬──────────┘
           │
           ▼
┌─────────────────────┐
│ Fault Remediation   │ Trigger external systems (your CR)
└─────────────────────┘
```

## 1. Is a Node Bad? Check Taints

**Use taints for all scheduling and automation decisions.**

Taints are the primary signal that a node has hardware issues. Configure your external systems to monitor the presence or absence of taints. Use this signal to make scheduling decisions, trigger alerts, or start remediation workflows.

> **Note**: Taints are **optional and disabled by default**. To use taints, uncomment the `taint` section in the Fault Quarantine rulesets. By default, NVSentinel only cordons nodes.

### Taint Structure

**Format:** You configure the format in the rulesets. Common patterns:

**Option 1: Component-specific (recommended)**

```text
nvidia.com/gpu-xid-error
nvidia.com/gpu-nvlink-error
nvidia.com/syslog-xid-error
```

**Option 2: Hierarchical**

```text
gpu.health/memory-error
nvlink.health/link-down
nvswitch.health/fatal-error
```

### Default Taint Examples

The NVSentinel test suite shows these taint configurations:

| Taint Key                       | Value  | Effect       | Use Case                    |
|---------------------------------|--------|--------------|-----------------------------|
| `nvidia.com/gpu-xid-error`      | `true` | `NoSchedule` | GPU XID critical errors     |
| `nvidia.com/gpu-nvlink-error`   | `true` | `NoSchedule` | NVLink connection failures  |
| `nvidia.com/syslog-xid-error`   | `true` | `NoSchedule` | Syslog-detected XID errors  |
| `nvidia.com/gpu-error`          | `true` | `NoSchedule` | Generic GPU hardware errors |

You can configure any taint keys and values in your rulesets, based on your needs.

### Taint Effect Guidelines

| Effect              | Use Case                         | Impact                                                     |
|---------------------|----------------------------------|------------------------------------------------------------|
| `NoSchedule`        | Fatal errors that require remediation | The scheduler does not schedule new pods without a toleration |
| `PreferNoSchedule`  | Degraded state or warnings       | The scheduler tries to avoid the node, but schedules pods on it if necessary |
| `NoExecute`         | Immediate evacuation needed      | Kubernetes evicts the existing pods without a toleration (rarely used) |

### Configuring Taints

You define taints in the Fault Quarantine rulesets. This example shows how to enable taints:

```yaml
# distros/kubernetes/nvsentinel/charts/fault-quarantine/values.yaml
ruleSets:
  - version: "1"
    name: "GPU XID Critical Errors"
    priority: 100
    match:
      any:
        - kind: "HealthEvent"
          expression: 'event.checkName == "GpuXidError" && event.isFatal == true'
    # Uncomment to enable tainting:
    #taint:
    #  key: "nvidia.com/gpu-xid-error"  # Choose your own key format
    # value: "true"                    # Or use "fatal", "degraded", etc.
    # effect: "NoSchedule"
    cordon:
      shouldCordon: true  # Enabled by default
```

**Key Points:**

- Taints are **commented out by default**, so you must enable them
- You control the taint key format (`nvidia.com/*` or `gpu.health/*` or any custom format)
- You control the taint values (`true`, `fatal`, `degraded`, or other values)
- Cordoning is enabled by default. Tainting is opt-in.

### Integration Patterns

**Check if a node has any NVIDIA-related taints:**

```shell
kubectl get nodes -o json | jq '.items[] 
  | select(.spec.taints[]? 
  | select(.key | startswith("nvidia.com/"))) 
  | .metadata.name'
```

**Check for a specific error type:**

```shell
kubectl get nodes -o json | jq '.items[] 
  | select(.spec.taints[]? 
  | select(.key == "nvidia.com/gpu-xid-error")) 
  | .metadata.name'
```

**Tolerate specific taints in pod specs:**

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: gpu-workload
spec:
  tolerations:
    # Match the exact taint configured in your rulesets
    - key: "nvidia.com/gpu-xid-error"
      operator: "Equal"
      value: "true"
      effect: "NoSchedule"
```

**Watch for taint changes (automation):**

```go
informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
    UpdateFunc: func(oldObj, newObj interface{}) {
        newNode := newObj.(*corev1.Node)
        
        // Check for NVIDIA-related taints
        for _, taint := range newNode.Spec.Taints {
            if strings.HasPrefix(taint.Key, "nvidia.com/") {
                // Trigger alert, update scheduler, etc.
                log.Printf("Node %s has taint %s=%s", 
                    newNode.Name, taint.Key, taint.Value)
            }
        }
    },
})
```

## 2. Why is a Node Bad? Check Node Conditions

**Use node conditions for monitoring, alerting, and detailed diagnostics.**

Taints tell you "this node is bad". Node conditions tell you *why* the node is bad. Use node conditions for dashboards, alerts, and troubleshooting.

### Monitoring with kube-state-metrics

**Prerequisites:** Install [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) to expose node conditions as Prometheus metrics. NVSentinel sets node conditions through the Kubernetes API. You must have kube-state-metrics to convert these node conditions into metrics.

**Available Metrics:**

```promql
# Monitor specific GPU health conditions
kube_node_status_condition{condition="GpuMemWatch",status="true"} == 1
kube_node_status_condition{condition="GpuNvlinkWatch",status="true"} == 1
kube_node_status_condition{condition="SysLogsXIDError",status="true"} == 1

# Count unhealthy GPU nodes
count(kube_node_status_condition{condition=~"Gpu.*|SysLogs.*",status="true"})

# Alert on any GPU or syslog condition
kube_node_status_condition{condition=~"Gpu.*|SysLogs.*",status="true"}
```

**Example Prometheus Alert:**

```yaml
- alert: GPUNodeUnhealthy
  expr: |
    kube_node_status_condition{condition=~"Gpu.*",status="true"} == 1
  for: 5m
  labels:
    severity: critical
  annotations:
    summary: "GPU node {{ $labels.node }} has condition {{ $labels.condition }}"
    description: "Node {{ $labels.node }} is unhealthy due to {{ $labels.condition }}"
```

**Grafana Dashboard Query:**

```promql
# Show all nodes with active GPU health conditions
kube_node_status_condition{condition=~"Gpu.*|SysLogs.*",status="true"}
```

> **Note:** NVSentinel also exposes its own Prometheus metrics for its internal operations. For the complete list of NVSentinel-native metrics, see [METRICS.md](METRICS.md).

### Condition Structure

Platform Connectors set node conditions based on the checks of the health monitors. Each node condition explains which hardware component failed.

**Naming:** PascalCase, directly from the check names of the health monitors  
**Examples:** `GpuMemWatch`, `GpuThermalWatch`, `SysLogsXIDError`

### Condition and Event Behavior

NVSentinel uses different Kubernetes primitives based on error severity:

| Error Type                      | Condition Set          | Event Created? | Use Case                                         |
|---------------------------------|------------------------|----------------|--------------------------------------------------|
| **Fatal** (`isFatal=true`)      | ✅ Yes (`status=True`)  | ❌ No          | Critical errors that require quarantine or remediation |
| **Non-Fatal** (`isFatal=false`) | ❌ No                   | ✅ Yes         | Warnings, transient issues, informational        |
| **Healthy** (`isHealthy=true`)  | ✅ Yes (`status=False`) | ❌ No          | Health recovery, condition cleared               |

**Why this design?**

- **Conditions** are durable state. NVSentinel uses them for errors that require action (cordon, drain, remediation).
- **Events** are transient notifications. NVSentinel uses them for warnings and non-critical issues that do not require node isolation.

### Using Events for Non-Fatal Errors

Non-fatal errors (such as thermal throttling warnings or transient issues) create Kubernetes Events instead of node conditions. This prevents alert fatigue, and you still have visibility of these errors.

**View recent events for a node:**

```shell
kubectl get events --field-selector involvedObject.kind=Node,involvedObject.name=gpu-node-01 \
  --sort-by='.lastTimestamp'
```

**Filter for GPU-related events:**

```shell
kubectl get events --all-namespaces \
  -o json | jq '.items[] | select(.type | startswith("Gpu") or startswith("SysLogs"))'
```

**Watch for real-time events:**

```shell
kubectl get events --watch --field-selector involvedObject.kind=Node
```

**Example non-fatal event:**

```yaml
apiVersion: v1
kind: Event
metadata:
  name: gpu-node-01.17a3b2c4d5e6f7
  namespace: default
involvedObject:
  kind: Node
  name: gpu-node-01
reason: Warning
message: "[DCGM_FR_CLOCK_THROTTLE_THERMAL] GPU thermal throttling detected - RecommendedAction: NONE"
type: GpuThermalWatch
source:
  component: gpu-health-monitor
  host: gpu-node-01
firstTimestamp: "2025-11-06T10:05:00Z"
lastTimestamp: "2025-11-06T10:05:00Z"
count: 1
```

**Integration patterns for events:**

```go
// Watch for non-fatal GPU events
eventInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
    AddFunc: func(obj interface{}) {
        event := obj.(*corev1.Event)
        if event.InvolvedObject.Kind == "Node" && 
           (strings.HasPrefix(event.Type, "Gpu") || strings.HasPrefix(event.Type, "SysLogs")) {
            // Log warning, update dashboard, etc.
            log.Printf("Non-fatal issue on %s: %s", 
                event.InvolvedObject.Name, event.Message)
        }
    },
})
```

### Condition Status

| Status    | Meaning                           |
|-----------|-----------------------------------|
| `True`    | Error or fault detected           |
| `False`   | Component healthy                 |
| `Unknown` | Health state cannot be determined |

### Condition Message Format

Messages include error codes and recommended actions:

```text
[ErrorCode1, ErrorCode2] Human-readable description - RecommendedAction: ACTION_NAME
```

**Example:**

```yaml
conditions:
  - type: GpuMemoryError
    status: "True"
    reason: HardwareFailure
    message: "[DCGM_FR_FAULTY_MEMORY] GPU memory failure detected on GPU 0 - RecommendedAction: RESTART_VM"
    lastTransitionTime: "2025-11-06T10:00:00Z"
```

### Standard Condition Types

#### GPU Conditions (from GPU Health Monitor - DCGM)

- `GpuMemWatch` - GPU memory failures (ECC errors, faulty memory)
- `GpuThermalWatch` - Thermal throttling or temperature violations
- `GpuPcieWatch` - PCIe link issues (replay rate, bandwidth)
- `GpuPowerWatch` - Power-related issues
- `GpuInforomWatch` - Inforom corruption detected
- `GpuSmWatch` - Streaming Multiprocessor errors
- `GpuNvlinkWatch` - NVLink connection failures
- `GpuMcuWatch` - Microcontroller unit errors
- `GpuPmuWatch` - Power management unit errors
- `GpuDriverWatch` - GPU driver errors
- `GpuCpusetWatch` - CPU affinity issues

#### Syslog Conditions (from Syslog Health Monitor)

- `SysLogsXIDError` - GPU XID errors detected in system logs
- `SysLogsSXIDError` - NVSwitch SXID errors detected in system logs
- `SysLogsGPUFallenOff` - GPU fallen off bus errors detected in system logs

#### NVSwitch Conditions

- `GpuNvswitchFatalWatch` - Fatal NVSwitch hardware error
- `NVSwitchDown` - NVSwitch unavailable
- `NVSwitchNonFatalError` - Non-fatal NVSwitch errors (warnings)

#### System Conditions

- `GpuDcgmConnectivityFailure` - DCGM daemon or API failures
- `CSPMaintenance` - Cloud provider scheduled maintenance
- `SyslogError` - Issues that the system log analysis detected

### Integration Patterns

**Monitor specific condition types:**

```shell
kubectl get nodes -o json | jq '.items[] 
  | select(.status.conditions[] | select(.type=="GpuMemWatch" and .status=="True")) 
  | .metadata.name'
```

**Watch for condition changes:**

```shell
kubectl get nodes -w -o json | jq -c 'select(.status.conditions[] | select(.type | startswith("Gpu")))'
```

**Prometheus alert example:**

```yaml
groups:
  - name: nvsentinel
    rules:
      - alert: GpuMemoryError
        expr: kube_node_status_condition{condition="GpuMemWatch",status="true"} == 1
        annotations:
          summary: "GPU memory error on {{ $labels.node }}"
```

**client-go example:**

```go
informer.AddEventHandler(cache.ResourceEventHandlerFuncs{
    UpdateFunc: func(oldObj, newObj interface{}) {
        newNode := newObj.(*corev1.Node)
        for _, condition := range newNode.Status.Conditions {
            if strings.HasPrefix(string(condition.Type), "Gpu") && condition.Status == corev1.ConditionTrue {
                // Send alert with condition.Message
                log.Printf("GPU issue on %s: %s", newNode.Name, condition.Message)
            }
        }
    },
})
```

## 3. Can I Use My Own Remediation? Provide a Custom Resource

**NVSentinel triggers external systems by creating Kubernetes Custom Resources.**

After NVSentinel detects a faulty node and drains it, NVSentinel creates a CR that your controller watches. This gives you full control of the remediation. You can integrate with cloud APIs, DCIM systems, or custom workflows.

### Integration Architecture

```text
┌────────────────────┐
│ Fault Remediation  │ Watches drained nodes
│     Module         │
└─────────┬──────────┘
          │ Creates CR based on RecommendedAction
          ▼
┌────────────────────┐
│ Kubernetes API     │ Custom Resource created
│  (RebootNode,      │
│   TerminateNode)   │
└─────────┬──────────┘
          │ Watched by external controller
          ▼
┌────────────────────┐
│ External System    │ Janitor, cloud APIs, DCIM
│  (Your Controller) │
└────────────────────┘
```

### Configuration

Configure the maintenance CR templates and behavior. This excerpt of the chart shows two of the four default actions:

```yaml
# distros/kubernetes/nvsentinel/charts/fault-remediation/values.yaml
maintenance:
  # Per-action remediation definitions
  # Key is the RecommendedAction string (e.g., "COMPONENT_RESET", "RESTART_BM")
  actions:
    COMPONENT_RESET:
      apiGroup: "janitor.dgxc.nvidia.com"
      version: "v1alpha1"
      kind: "RebootNode"
      scope: "Cluster"
      completeConditionType: "NodeReady"
      templateFileName: "nvidia-reboot.yaml"
      equivalenceGroup: "restart"

    # REPLACE_VM is used when the VM needs to be terminated and replaced.
    REPLACE_VM:
      apiGroup: "janitor.dgxc.nvidia.com"
      version: "v1alpha1"
      kind: "TerminateNode"
      scope: "Cluster"
      completeConditionType: "NodeTerminated"
      templateFileName: "terminate-node.yaml"
      equivalenceGroup: "terminate"

  # Template content for each remediation action
  # Key matches the templateFileName name from actions above
  templates:
    "nvidia-reboot.yaml": |
      apiVersion: janitor.dgxc.nvidia.com/v1alpha1
      kind: RebootNode
      metadata:
        name: maintenance-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
        labels:
          app.kubernetes.io/managed-by: nvsentinel
        annotations:
          nvsentinel.nvidia.com/trace-id: "{{ .TraceID }}"
          nvsentinel.nvidia.com/span-id: "{{ .SpanID }}"
          # TTL-based auto-cleanup by janitor after completion.
          # See docs/designs/037-janitor-cr-ttl-cleanup.md.
          nvsentinel.nvidia.com/ttl: "336h"
      spec:
        nodeName: {{ .HealthEvent.NodeName }}
        force: false
    "terminate-node.yaml": |
      apiVersion: janitor.dgxc.nvidia.com/v1alpha1
      kind: TerminateNode
      metadata:
        name: maintenance-{{ .HealthEvent.NodeName }}-{{ .HealthEventID }}
        labels:
          app.kubernetes.io/managed-by: nvsentinel
        annotations:
          nvsentinel.nvidia.com/trace-id: "{{ .TraceID }}"
          nvsentinel.nvidia.com/span-id: "{{ .SpanID }}"
          nvsentinel.nvidia.com/ttl: "336h"
      spec:
        nodeName: {{ .HealthEvent.NodeName }}
        force: false

# Retry configuration for maintenance resource updates
# Used when updating annotations on nodes after creating maintenance resources
updateRetry:
  # Maximum number of retry attempts if the update fails (due to conflicts, network issues, etc.)
  maxRetries: 5
  # Delay in seconds between retry attempts (uses exponential backoff)
  retryDelaySeconds: 10
```

### Custom Resource Template

The templates use Go template syntax. They can use `.HealthEvent` (the full health event), `.TraceID` and `.SpanID` (the trace context), and these variables:

| Variable             | Type    | Description                                  |
|----------------------|---------|----------------------------------------------|
| `.ApiGroup`          | string  | API group from the matching `maintenance.actions` entry |
| `.Version`           | string  | API version from the matching `maintenance.actions` entry |
| `.Kind`              | string  | Resource kind from the matching `maintenance.actions` entry |
| `.RecommendedAction` | int     | Numeric action code, for example `2` (`COMPONENT_RESET`) or `15` (`RESTART_VM`) |
| `.NodeName`          | string  | Name of the node that requires remediation   |
| `.HealthEventID`     | string  | Unique ID of the triggering health event     |
| `.Namespace`         | string  | Namespace from the matching `maintenance.actions` entry |

### RecommendedAction Codes

| Code | Action            | Typical Use Case              |
|------|-------------------|-------------------------------|
| `2`  | `COMPONENT_RESET` | GPU or driver reset, node reboot |
| `5`  | `CONTACT_SUPPORT` | Manual intervention needed    |
| `15` | `RESTART_VM`      | Reboot VM instance            |
| `24` | `RESTART_BM`      | Reboot bare metal node        |
| `25` | `REPLACE_VM`      | Terminate and replace VM      |

### Integration Examples

#### Example 1: Janitor Controller Integration

The Janitor controller watches for `RebootNode` and `TerminateNode` CRs:

```yaml
apiVersion: janitor.dgxc.nvidia.com/v1alpha1
kind: RebootNode
metadata:
  name: maintenance-gpu-node-01-673bac8e9f1234567890abcd
  namespace: nvsentinel
spec:
  nodeName: gpu-node-01
  reason: "Health event 673bac8e9f1234567890abcd"
  force: false
status:
  conditions:
    - type: NodeReady
      status: "False"
      reason: "RebootInProgress"
```

#### Example 2: Cloud Provider Integration

Custom template for cloud-specific maintenance:

```yaml
maintenance:
  apiGroup: "cloud.example.com"
  version: "v1"
  kind: "NodeMaintenance"
  template: |
    apiVersion: {{ .ApiGroup }}/{{ .Version }}
    kind: NodeMaintenance
    metadata:
      name: {{ .NodeName }}-{{ .HealthEventID }}
    spec:
      nodeName: {{ .NodeName }}
      action: {{ if eq .RecommendedAction 2 }}"reboot"{{ else if eq .RecommendedAction 15 }}"restart"{{ else }}"replace"{{ end }}
      provider:
        region: "us-west-2"
        instanceId: "{{ .NodeName }}"
```

#### Example 3: DCIM Integration

Template for data center infrastructure management:

```yaml
maintenance:
  apiGroup: "dcim.example.com"
  version: "v1alpha1"
  kind: "ServerMaintenance"
  template: |
    apiVersion: {{ .ApiGroup }}/{{ .Version }}
    kind: ServerMaintenance
    metadata:
      name: server-{{ .NodeName }}
    spec:
      serverName: {{ .NodeName }}
      maintenanceType: {{ if eq .RecommendedAction 2 }}"reboot"{{ else }}"replace"{{ end }}
      priority: "high"
      ticketId: "HEALTH-{{ .HealthEventID }}"
```

### Completion Detection

Before Fault Remediation creates a new CR, it checks the `completeConditionType` status on the existing CRs:

- **Status: True** - Maintenance completed successfully. Fault Remediation can create a new CR.
- **Status: False** - Maintenance failed. Fault Remediation can create a new CR to retry.
- **Condition Missing** - Maintenance in progress. Fault Remediation does not create a new CR.

This prevents duplicate remediation requests for nodes with ongoing maintenance.

### Testing Your Integration

1. **Validate Template Syntax**:
   
   ```shell
   # Dry-run mode to validate template without creating CRs
   helm install nvsentinel --set global.dryRun=true ...
   ```

2. **Monitor CR Creation**:

   ```shell
   # Watch for maintenance CRs
   kubectl get rebootnodes -n nvsentinel -w
   ```

3. **Check Fault Remediation Logs**:

   ```shell
   kubectl logs -n nvsentinel deployment/fault-remediation -f
   ```

### External Remediation and Planned Maintenance

For a repair that NVSentinel cannot do itself, Fault Remediation can create an ExternalRemediationRequest. NVSentinel then stops managing the node. NVSentinel manages the node again when the external system reports that the repair is complete, or when someone deletes the ExternalRemediationRequest. See [Integrating External Remediation](./tutorials/integrating-external-remediation.md).

To tell NVSentinel about planned maintenance, create a MaintenanceRequest. NVSentinel then cordons and drains the node. See [Requesting Node Maintenance](./tutorials/requesting-node-maintenance.md).

**Configuration Location:** `distros/kubernetes/nvsentinel/charts/fault-remediation/values.yaml`

## 4. How Do I Customize Drain Behavior? Configure Eviction Modes

**Control how Node Drainer evicts workloads from faulty nodes.**

The Node Drainer module gracefully evicts workloads from cordoned nodes. You can customize the eviction behavior for each namespace. This lets you accommodate different workload types and operational requirements.

### Eviction Modes

NVSentinel supports three eviction modes:

| Mode                 | Behavior                                | Use Case                                                      |
|----------------------|-----------------------------------------|---------------------------------------------------------------|
| `Immediate`          | Evicts the pod immediately, without waiting | Fast failover for stateless workloads                     |
| `AllowCompletion`    | Waits for the pod to terminate gracefully | Respects terminationGracePeriodSeconds for stateful workloads |
| `DeleteAfterTimeout` | Waits up to the timeout, then force deletes the pod | Long-running jobs that need time to checkpoint      |

### Configuration

Configure eviction behavior in Helm values:

```yaml
# distros/kubernetes/nvsentinel/charts/node-drainer/values.yaml
# Eviction timeout in seconds for pod eviction operations
evictionTimeoutInSeconds: "60"

# System namespaces are skipped during drain
systemNamespaces: "^(nvsentinel|kube-system|gpu-operator|gmp-system|network-operator)$"

# Time after which pods in DeleteAfterTimeout mode will be force deleted
deleteAfterTimeoutMinutes: 60

# Time after which a pod in NotReady state is considered stuck
notReadyTimeoutMinutes: 5

# Per-namespace eviction configuration
userNamespaces:
  # Default for all user namespaces
  - name: "*"
    mode: "AllowCompletion"
  
  # Fast failover for stateless web services
  - name: "web-tier"
    mode: "Immediate"
  
  # Allow ML training jobs to checkpoint before eviction
  - name: "ml-training"
    mode: "DeleteAfterTimeout"
```

### Eviction Workflow

1. **System Namespace Skip**: Node Drainer never evicts pods in system namespaces (kube-system, nvsentinel, and the other system namespaces)
2. **Mode Selection**: The namespace match selects the eviction mode (the most specific match wins)
3. **Graceful Termination**: In `AllowCompletion` mode, Node Drainer respects the `terminationGracePeriodSeconds` of the pod
4. **Timeout Handling**: Node Drainer force deletes stuck or timed-out pods, based on the configuration
5. **NotReady Detection**: Node Drainer automatically force deletes pods that stay in the NotReady state for longer than the threshold

### Example: Multi-Tier Application

```yaml
userNamespaces:
  # Critical database - wait for graceful shutdown
  - name: "database"
    mode: "AllowCompletion"
  
  # Batch processing - allow time for checkpoint
  - name: "batch-jobs"
    mode: "DeleteAfterTimeout"
  
  # Web frontend - fast failover
  - name: "frontend"
    mode: "Immediate"
  
  # Default for everything else
  - name: "*"
    mode: "AllowCompletion"
```

**Configuration Location:** `distros/kubernetes/nvsentinel/charts/node-drainer/values.yaml`

## Topology Awareness (Topograph)

When you deploy [Topograph](https://github.com/NVIDIA/topograph) in the cluster, it applies four node labels that describe the physical network topology:

- `accelerator.topograph.run/domain` — NVLink domain (clique) ID
- `fabric.topograph.run/tier-0` — leaf switch identifier
- `fabric.topograph.run/tier-1` — spine switch identifier
- `fabric.topograph.run/tier-2` — core switch identifier

By default, the `allowedLabels` list of the Metadata Augmentor includes these keys. On clusters where Topograph applied these labels, NVSentinel automatically propagates them into the health event metadata. On clusters without Topograph, the labels are absent, and the Metadata Augmentor skips them. In both cases, no configuration change is necessary.

Downstream consumers of NVSentinel health events can then consider the topological locality. These consumers include Fault Quarantine CEL rules, remediation custom resources, dashboards, and blast-radius analysis. For example, a CEL rule can compare the `accelerator.topograph.run/domain` value across a set of recent health events. The rule can then find whether a fault affects only one NVLink domain or spans multiple domains.

The authoritative reference for these labels is [topograph's `docs/reference/node-labels.md`](https://github.com/NVIDIA/topograph/blob/main/docs/reference/node-labels.md). It describes the value semantics, the hashing behavior for long identifiers, and the provider matrix.

## Error Code Mapping Reference

NVSentinel uses a canonical CSV file to map DCGM error codes to recommended actions.

**Mapping File:** `distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv`

### Recommended Actions

| Action            | Meaning                         | Typical Resolution                          |
|-------------------|---------------------------------|---------------------------------------------|
| `RESTART_VM`      | Software-recoverable error      | Node reboot through Janitor                 |
| `COMPONENT_RESET` | Hardware reset required         | GPU or driver reset                         |
| `CONTACT_SUPPORT` | Manual intervention needed      | Create a support ticket, manual investigation |
| `NONE`            | Health check informational      | No action required                          |

### Example Mappings

| DCGM Error Code                     | Recommended Action | Typical Condition      |
|-------------------------------------|--------------------|------------------------|
| `DCGM_FR_FAULTY_MEMORY`             | `CONTACT_SUPPORT`  | `GpuMemoryError`       |
| `DCGM_FR_VOLATILE_DBE_DETECTED`     | `COMPONENT_RESET`  | `GpuMemoryError`       |
| `DCGM_FR_PENDING_PAGE_RETIREMENTS`  | `COMPONENT_RESET`  | `GpuMemoryError`       |
| `DCGM_FR_UNCORRECTABLE_ROW_REMAP`   | `COMPONENT_RESET`  | `GpuMemoryError`       |
| `DCGM_FR_NVLINK_DOWN`               | `RESTART_VM`       | `NVLinkDown`           |
| `DCGM_FR_NVSWITCH_FATAL_ERROR`      | `CONTACT_SUPPORT`  | `GpuNvswitchFatalWatch` |
| `DCGM_FR_CLOCK_THROTTLE_THERMAL`    | `NONE`             | `GpuThermalWatch`      |
| `DCGM_FR_SXID_ERROR`                | `RESTART_VM`       | `GpuXidError`          |

The full mapping contains 138 error codes. For the complete reference, see the CSV file.

## Node Status Examples

### Example 1: Node with Fatal GPU XID Error (With Optional Taint)

```yaml
apiVersion: v1
kind: Node
metadata:
  name: gpu-node-01
spec:
  unschedulable: true  # Cordoned (enabled by default)
  taints:
    # Optional - only present if configured in rulesets
    - key: "nvidia.com/gpu-xid-error"
      value: "true"
      effect: "NoSchedule"
status:
  conditions:
    - type: Ready
      status: "False"
      reason: "GpuHealthCheckFailed"
      message: "GPU health check failed"
    - type: SysLogsXIDError
      status: "True"
      reason: "HardwareFailure"
      message: "[DCGM_FR_SXID_ERROR] GPU XID error detected on GPU 0 - RecommendedAction: RESTART_VM"
      lastTransitionTime: "2025-11-06T10:00:00Z"
```

### Example 2: Node with Non-Fatal GPU Thermal Issue

```yaml
apiVersion: v1
kind: Node
metadata:
  name: gpu-node-02
spec:
  # May or may not be cordoned depending on ruleset configuration
  taints:
    # Optional - only present if configured in rulesets
    - key: "nvidia.com/gpu-thermal"
      value: "true"
      effect: "PreferNoSchedule"
status:
  conditions:
    - type: Ready
      status: "True"
    - type: GpuThermalWatch
      status: "True"
      reason: "ThermalThrottling"
      message: "[DCGM_FR_CLOCK_THROTTLE_THERMAL] GPU thermal throttling detected - RecommendedAction: NONE"
      lastTransitionTime: "2025-11-06T10:02:00Z"
```

### Example 3: Healthy Node

```yaml
apiVersion: v1
kind: Node
metadata:
  name: gpu-node-03
status:
  conditions:
    - type: Ready
      status: "True"
    - type: GpuMemWatch
      status: "False"
      reason: "HealthCheckPassed"
      message: "GPU memory health check passed"
      lastTransitionTime: "2025-11-06T10:10:00Z"
    - type: GpuThermalWatch
      status: "False"
      reason: "HealthCheckPassed"
      message: "GPU thermal health check passed"
      lastTransitionTime: "2025-11-06T10:10:00Z"
```

## Implementation Notes

### Module Responsibilities

| Module                  | Responsibility                            | What It Sets           |
|-------------------------|-------------------------------------------|------------------------|
| **Platform Connectors** | Process health events, update node status | Node conditions        |
| **Fault Quarantine**    | Apply operational policies                | Taints, cordon status  |
| **Node Drainer**        | Evict workloads                           | Drain nodes            |
| **Fault Remediation**   | Trigger maintenance                       | Create maintenance CRs |

### Configuration Files

- **Error Mapping:** `distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv`
- **Quarantine Rules:** `distros/kubernetes/nvsentinel/charts/fault-quarantine/values.yaml`
- **Module Config:** `distros/kubernetes/nvsentinel/values.yaml`

## Related Documentation

- [Data Flow Documentation](./DATA_FLOW.md) - End-to-end flow of health events
- Helm Chart Configuration (`distros/kubernetes/README.md` in the NVSentinel repository) - Deployment configuration

## API Stability

This document describes the API contract for NVSentinel node health signaling. Each change to condition types, taint keys, or label keys requires a review and follows the deprecation policy.
