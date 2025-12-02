# NVSentinel Integration Guide

NVSentinel detects GPU and hardware failures and exposes them using standard Kubernetes primitives. This document provides a high level overview of how to integrate with NVSentinel for scheduling, monitoring, and remediation purposes. 

### Integration Model

Think of NVSentinel integration in four layers:

1. **Is a node bad?** → Check **[Taints](#1-is-a-node-bad-check-taints)**
   - Taints mark nodes with hardware issues
   - Use taints for scheduling decisions and filtering
   - React to taint presence/absence in automation

2. **Why is a node bad?** → Check **[Node Conditions](#2-why-is-a-node-bad-check-node-conditions)**
   - Conditions provide detailed diagnostic information
   - Use conditions for monitoring, alerting, and dashboards
   - Each condition explains what hardware component failed

3. **Can I use my own remediation?** → Provide a **[Custom Resource](#3-can-i-use-my-own-remediation-provide-a-custom-resource)**
   - NVSentinel triggers external systems via CRs
   - Integrate with cloud APIs, DCIM, or custom controllers
   - You retain full control over how nodes are repaired

4. **How do I customize drain behavior?** → Configure **[per-namespace eviction modes](#4-how-do-i-customize-drain-behavior-configure-eviction-modes)**
   - Control how workloads are evicted from failing nodes
   - Define different policies for stateless vs stateful workloads
   - Set timeouts and grace periods per namespace

### Quick Start

**For Scheduling Decisions:**

Find nodes with NVSentinel taints (if configured):

```shell
kubectl get nodes -o json | jq '.items[] 
  | select(.spec.taints[]? 
  | select(.key | startswith("nvidia.com/"))) 
  | .metadata.name'
```

**For Monitoring:**

Get detailed failure information:

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

The [DATA_FLOW.md](./DATA_FLOW.md) provides more context on this, at the higher level though, NVSentinel detects hardware failures and applies graduated responses via:

1. **Detection**: Health monitors check GPU, system logs, and cloud maintenance events
2. **Classification**: Platform connectors validate and set node conditions
3. **Quarantine**: Fault quarantine evaluates rules and applies taints/cordons
4. **Evacuation**: Node drainer evicts workloads per configured policies
5. **Remediation**: Fault remediation triggers external systems via CRs

```
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

Taints are the primary signal that a node has hardware issues. External systems should watch for taint presence/absence to make scheduling decisions, trigger alerts, or initiate remediation workflows.

> **Note**: Taints are **optional and disabled by default**. You must configure them in fault-quarantine rulesets by uncommenting the `taint` section. NVSentinel only cordons nodes by default.

### Taint Structure

**Format:** User-configurable via rulesets. Common patterns:

**Option 1: Component-specific (recommended)**
```
nvidia.com/gpu-xid-error
nvidia.com/gpu-nvlink-error
nvidia.com/syslog-xid-error
```

**Option 2: Hierarchical (proposed pattern)**
```
gpu.health/memory-error
nvlink.health/link-down
nvswitch.health/fatal-error
```

### Default Taint Examples

NVSentinel's test suite demonstrates these taint configurations:

| Taint Key                       | Value  | Effect       | Use Case                    |
|---------------------------------|--------|--------------|-----------------------------|
| `nvidia.com/gpu-xid-error`      | `true` | `NoSchedule` | GPU XID critical errors     |
| `nvidia.com/gpu-nvlink-error`   | `true` | `NoSchedule` | NVLink connection failures  |
| `nvidia.com/syslog-xid-error`   | `true` | `NoSchedule` | Syslog-detected XID errors  |
| `nvidia.com/gpu-error`          | `true` | `NoSchedule` | Generic GPU hardware errors |

You can configure any taint keys/values in your rulesets based on your needs.

### Taint Effect Guidelines

| Effect              | Use Case                         | Impact                                                     |
|---------------------|----------------------------------|------------------------------------------------------------|
| `NoSchedule`        | Fatal errors requiring remediation | New pods without toleration won't be scheduled           |
| `PreferNoSchedule`  | Degraded state or warnings       | Scheduler tries to avoid but will schedule if necessary    |
| `NoExecute`         | Immediate evacuation needed      | Existing pods without toleration are evicted (rarely used) |

### Configuring Taints

Taints are defined in Fault Quarantine rulesets. Here's an example showing how to enable taints:

```yaml
# distros/kubernetes/nvsentinel/charts/fault-quarantine/values.yaml
rulesets:
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
- Taints are **commented out by default** - you must enable them
- You control the taint key format (`nvidia.com/*` or `gpu.health/*` or any custom format)
- You control the taint values (`true`, `fatal`, `degraded`, etc.)
- Cordoning is enabled by default; tainting is opt-in

### Integration Patterns

**Check if node has any NVIDIA-related taints:**
```shell
kubectl get nodes -o json | jq '.items[] 
  | select(.spec.taints[]? 
  | select(.key | startswith("nvidia.com/"))) 
  | .metadata.name'
```

**Check for specific error type:**
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

While taints tell you "this node is bad", conditions tell you *why* it's bad. Use conditions for dashboards, alerts, and troubleshooting.

### Monitoring with kube-state-metrics

**Prerequisites:** Install [kube-state-metrics](https://github.com/kubernetes/kube-state-metrics) to expose node conditions as Prometheus metrics. NVSentinel sets node conditions via the Kubernetes API, but kube-state-metrics is required to convert these into metrics.

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

> **Note:** NVSentinel also exposes its own Prometheus metrics for internal operations. See [METRICS.md](METRICS.md) for the complete list of NVSentinel-native metrics.

### Condition Structure

Platform Connectors set NodeConditions based on health monitor checks. Each condition explains what hardware component failed.

**Naming:** PascalCase, directly from health monitor check names  
**Examples:** `GpuMemWatch`, `GpuThermalWatch`, `SysLogsXIDError`

### Condition vs Event Behavior

NVSentinel uses different Kubernetes primitives based on error severity:

| Error Type                      | Condition Set          | Event Created? | Use Case                                         |
|---------------------------------|------------------------|----------------|--------------------------------------------------|
| **Fatal** (`isFatal=true`)      | ✅ Yes (`status=True`)  | ❌ No          | Critical errors requiring quarantine/remediation |
| **Non-Fatal** (`isFatal=false`) | ❌ No                   | ✅ Yes         | Warnings, transient issues, informational        |
| **Healthy** (`isHealthy=true`)  | ✅ Yes (`status=False`) | ❌ No          | Health recovery, condition cleared               |

**Why this design?**
- **Conditions** are durable state - used for errors that require action (cordon, drain, remediation)
- **Events** are transient notifications - used for warnings and non-critical issues that don't require node isolation

### Using Events for Non-Fatal Errors

Non-fatal errors (like thermal throttling warnings or transient issues) create Kubernetes Events instead of node conditions. This prevents alert fatigue while still providing visibility.

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
| `True`    | Error/fault detected              |
| `False`   | Component healthy                 |
| `Unknown` | Health state cannot be determined |

### Condition Message Format

Messages include error codes and recommended actions:

```
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

- `NVSwitchFatalError` - Fatal NVSwitch hardware error
- `NVSwitchDown` - NVSwitch unavailable
- `NVSwitchNonFatalError` - Non-fatal NVSwitch errors (warnings)

#### System Conditions

- `DCGMError` - DCGM daemon or API failures
- `CSPMaintenance` - Cloud provider scheduled maintenance
- `SyslogError` - System log analysis detected issues

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

After detecting and draining a failing node, NVSentinel creates a CR that your controller watches. This gives you full control over remediation - integrate with cloud APIs, DCIM systems, or custom workflows.

### Integration Architecture

```
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

Configure the maintenance CR template and behavior:

```yaml
# distros/kubernetes/nvsentinel/charts/fault-remediation/values.yaml
maintenance:
  # API group of your maintenance CRD
  apiGroup: "janitor.dgxc.nvidia.com"
  version: "v1alpha1"
  kind: "RebootNode"
  
  # Completion condition to check before creating new CRs
  # Prevents duplicate remediation requests for the same node
  completeConditionType: "NodeReady"
  
  # Namespace where maintenance CRs will be created
  namespace: "nvsentinel"
  
  # Resource names for RBAC permissions
  resourceNames:
    - "rebootnodes"
    - "terminatenodes"
  
  # Go template for generating maintenance CRs
  # Available variables: .ApiGroup, .Version, .RecommendedAction, .NodeName, .HealthEventID
  template: |
    apiVersion: {{ .ApiGroup }}/{{ .Version }}
    kind: {{ if eq .RecommendedAction 2 }}RebootNode{{ else }}TerminateNode{{ end }}
    metadata:
      name: maintenance-{{ .NodeName }}-{{ .HealthEventID }}
      namespace: {{ .Namespace }}
    spec:
      nodeName: {{ .NodeName }}
      reason: "Health event {{ .HealthEventID }}"
      force: false

# Retry configuration for CR creation
updateRetry:
  maxRetries: 5
  retryDelaySeconds: 10
```

### Custom Resource Template

The template uses Go template syntax with these variables:

| Variable             | Type    | Description                                  |
|----------------------|---------|----------------------------------------------|
| `.ApiGroup`          | string  | API group from `maintenance.apiGroup`        |
| `.Version`           | string  | API version from `maintenance.version`       |
| `.Kind`              | string  | Resource kind from `maintenance.kind`        |
| `.RecommendedAction` | int     | Numeric action code (2=reboot, 15=terminate) |
| `.NodeName`          | string  | Name of the node requiring remediation       |
| `.HealthEventID`     | string  | Unique ID of the triggering health event     |
| `.Namespace`         | string  | Namespace from `maintenance.namespace`       |

### RecommendedAction Codes

| Code | Action            | Typical Use Case              |
|------|-------------------|-------------------------------|
| `2`  | `COMPONENT_RESET` | GPU/driver reset, reboot node |
| `5`  | `CONTACT_SUPPORT` | Manual intervention needed    |
| `15` | `RESTART_VM`      | Reboot VM instance            |
| `24` | `RESTART_BM`      | Reboot bare metal node        |
| `25` | `REPLACE_VM`      | Terminate and replace VM      |

### Integration Examples

#### Example 1: Janitor Controller Integration

Janitor controller watches for `RebootNode` and `TerminateNode` CRs:

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

Fault Remediation checks the `completeConditionType` status on existing CRs before creating new ones:

- **Status: True** - Maintenance completed successfully, new CR can be created
- **Status: False** - Maintenance failed, new CR can be created for retry
- **Condition Missing** - Maintenance in progress, skip CR creation

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

**Configuration Location:** `distros/kubernetes/nvsentinel/charts/fault-remediation/values.yaml`

## 4. How Do I Customize Drain Behavior? Configure Eviction Modes

**Control how workloads are evicted from failing nodes.**

The Node Drainer module handles graceful workload eviction from cordoned nodes. Eviction behavior can be customized per namespace to accommodate different workload types and operational requirements.

### Eviction Modes

NVSentinel supports three eviction modes:

| Mode                 | Behavior                                | Use Case                                                      |
|----------------------|-----------------------------------------|---------------------------------------------------------------|
| `Immediate`          | Pod evicted immediately without waiting | Fast failover for stateless workloads                         |
| `AllowCompletion`    | Wait for pod to gracefully terminate    | Respects terminationGracePeriodSeconds for stateful workloads |
| `DeleteAfterTimeout` | Wait up to timeout, then force delete   | Long-running jobs that need time to checkpoint                |

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

1. **System Namespace Skip**: Pods in system namespaces (kube-system, nvsentinel, etc.) are never evicted
2. **Mode Selection**: Eviction mode determined by namespace match (most specific wins)
3. **Graceful Termination**: Respects pod's `terminationGracePeriodSeconds` for `AllowCompletion` mode
4. **Timeout Handling**: Force deletes stuck or timed-out pods based on configuration
5. **NotReady Detection**: Automatically force deletes pods stuck in NotReady state beyond threshold

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

## Error Code Mapping Reference

NVSentinel maps DCGM error codes to recommended actions using a canonical CSV file.

**Mapping File:** `distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv`

### Recommended Actions

| Action            | Meaning                         | Typical Resolution                          |
|-------------------|---------------------------------|---------------------------------------------|
| `RESTART_VM`      | Software-recoverable error      | Node reboot via janitor                     |
| `COMPONENT_RESET` | Hardware reset required         | GPU/driver reset                            |
| `CONTACT_SUPPORT` | Manual intervention needed      | Create support ticket, manual investigation |
| `NONE`            | Health check informational      | No action required                          |

### Example Mappings

| DCGM Error Code                     | Recommended Action | Typical Condition      |
|-------------------------------------|--------------------|------------------------|
| `DCGM_FR_FAULTY_MEMORY`             | `CONTACT_SUPPORT`  | `GpuMemoryError`       |
| `DCGM_FR_VOLATILE_DBE_DETECTED`     | `COMPONENT_RESET`  | `GpuMemoryError`       |
| `DCGM_FR_NVLINK_DOWN`               | `RESTART_VM`       | `NVLinkDown`           |
| `DCGM_FR_NVSWITCH_FATAL_ERROR`      | `CONTACT_SUPPORT`  | `NVSwitchFatalError`   |
| `DCGM_FR_CLOCK_THROTTLE_THERMAL`    | `NONE`             | `GpuThermalWatch`      |
| `DCGM_FR_SXID_ERROR`                | `RESTART_VM`       | `GpuXidError`          |

Full mapping contains 121 error codes. See CSV file for complete reference.


## Related Documentation

- [ADR-003: Rule-Based Node Quarantine](./designs/003-rule-based-node-quarantine.md) - CEL-based quarantine rules
- [ADR-009: Fault Remediation Triggering](./designs/009-fault-remediation-triggering.md) - Remediation workflow
- [Data Flow Documentation](./DATA_FLOW.md) - End-to-end event flow
- [Helm Chart Configuration](../distros/kubernetes/README.md) - Deployment configuration

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
| **Platform Connectors** | Process health events, update node status | NodeConditions         |
| **Fault Quarantine**    | Apply operational policies                | Taints, cordon status  |
| **Node Drainer**        | Evict workloads                           | Drain nodes            |
| **Fault Remediation**   | Trigger maintenance                       | Create maintenance CRs |

### Configuration Files

- **Error Mapping:** `distros/kubernetes/nvsentinel/charts/gpu-health-monitor/files/dcgmerrorsmapping.csv`
- **Quarantine Rules:** `distros/kubernetes/nvsentinel/charts/fault-quarantine/values.yaml`
- **Module Config:** `distros/kubernetes/nvsentinel/values.yaml`

### Code Locations

- **Condition Setting:** `platform-connectors/pkg/connectors/kubernetes/process_node_events.go`
- **Taint Application:** `fault-quarantine/pkg/informer/k8s_client.go`
- **Drain Logic:** `node-drainer/pkg/drainer/drainer.go`
- **Remediation Triggering:** `fault-remediation/pkg/remediation/remediation.go`

## Related Documentation

- [ADR-003: Rule-Based Node Quarantine](./designs/003-rule-based-node-quarantine.md) - CEL-based quarantine rules
- [ADR-009: Fault Remediation Triggering](./designs/009-fault-remediation-triggering.md) - Remediation workflow
- [Data Flow Documentation](./DATA_FLOW.md) - End-to-end event flow
- [Helm Chart Configuration](../distros/kubernetes/README.md) - Deployment configuration

## Contributing

This document describes the proposed API contract for NVSentinel node health signaling. Changes to condition types, taint keys, or label keys require review and follow the deprecation policy.

To propose changes:
1. Open an issue describing the use case
2. Discuss impact on external integrations
3. Follow the versioning and deprecation guidelines
4. Update this document as part of the PR
