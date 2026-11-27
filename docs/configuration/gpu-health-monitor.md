# GPU Health Monitor Configuration

## Overview

The GPU Health Monitor module uses NVIDIA DCGM (Data Center GPU Manager) to watch GPU health and report hardware failures. This document describes all the Helm configuration options for system administrators.

## DCGM Deployment Modes

The GPU Health Monitor supports three DCGM source modes. To select a mode, set `global.dcgm.mode`.

### Operator Service

The GPU Operator runs DCGM as a DaemonSet and exposes it through a Kubernetes service. GPU Health Monitor pods connect to the service endpoint.

**Characteristics:**

- DCGM runs as a DaemonSet (one pod for each GPU node).
- A Kubernetes service provides a DNS endpoint for DCGM.
- The GPU Health Monitor connects through the DNS name of the service.

### External Hostengine

An externally managed hostengine runs on each GPU node. GPU Health Monitor pods use host networking and connect to the configured endpoint. The default endpoint is `localhost:5555`.

**Characteristics:**

- The hostengine lifecycle is managed outside NVSentinel.
- No Kubernetes service is necessary.
- The GPU Health Monitor enables host networking automatically.
- You must label each GPU node with its [DCGM version](#dcgm-version-node-label) before you install.

### Embedded Mode

The GPU Health Monitor starts an in-process DCGM hostengine and exposes it to pod-local clients on a loopback endpoint.

**Characteristics:**

- No separate DCGM DaemonSet or service is necessary.
- `gpu-health-monitor.runtimeClassName` must name the NVIDIA RuntimeClass of the cluster.
- The chart automatically sets `privileged: true` on the GPU Health Monitor container.
- The endpoint must be `localhost`, `127.0.0.1` or `::1`.
- You must label each GPU node with its [DCGM version](#dcgm-version-node-label) before you install.

## DCGM Version Selection

> [!WARNING]
> **Deprecated:** DCGM 3.x support is deprecated and will be removed on 30 November 2026. DCGM 3.x is end-of-life and gets no more patches. After the removal, NVSentinel will not detect faults on nodes that run DCGM 3.x. To find these nodes, run `kubectl get nodes -l nvsentinel.dgxc.nvidia.com/dcgm.version=3.x`. Upgrade DCGM on these nodes to 4.x. For the details, refer to [GPU Health Monitor: DCGM 3.x Support Removal](../deprecations/gpu-health-monitor-dcgm-3-support-removal.md).

Use a GPU Health Monitor image that has the same DCGM major version as its hostengine. The `nvsentinel.dgxc.nvidia.com/dcgm.version` node label selects the 3.x or 4.x image of the GPU Health Monitor.

In `operator-service` mode, labeler sets this label from the version in the DCGM pod image. In `external-hostengine` and `embedded-mode`, you must supply the label.

## Configuration Reference

### Module Enable/Disable

This value controls whether the chart deploys the GPU Health Monitor module in the cluster.

```yaml
global:
  gpuHealthMonitor:
    enabled: true
```

### Resources

This value defines the CPU and memory resource requests and limits for the GPU Health Monitor pod.

```yaml
gpu-health-monitor:
  resources:
    limits:
      cpu: 500m
      memory: 512Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

### Logging

This value controls the verbosity of the GPU Health Monitor logs.

```yaml
gpu-health-monitor:
  verbose: "False"  # Options: "True", "False"
```

## DCGM Configuration

### Operator Service Mode

This is the default mode.

```yaml
global:
  dcgm:
    mode: operator-service
    enabled: true
    service:
      endpoint: "nvidia-dcgm-dra.gpu-operator.svc,nvidia-dcgm.gpu-operator.svc"
      port: 5555
```

The endpoint can be a comma-separated list of hosts. The GPU Health Monitor tries the hosts in order each time that it connects. Only one DCGM Service exists in each cluster:

- `nvidia-dcgm-dra` in GPU Operator GPUCluster (DRA) mode
- `nvidia-dcgm` in other modes

Thus, the default works in both modes without a configuration for each mode.

To use a service in another namespace, override its endpoint:

```yaml
global:
  dcgm:
    mode: operator-service
    service:
      endpoint: "dcgm-service.custom-namespace.svc.cluster.local"
      port: 5555
```

### External Hostengine Mode

In this mode, NVSentinel does not deploy the hostengine. The configured hostengine must already run and be reachable on every selected GPU node.

```yaml
global:
  dcgm:
    mode: external-hostengine
    externalHostengine:
      endpoint: localhost
      port: 5555
```

In this mode, the GPU Health Monitor enables host networking automatically.

This mode does not set the [DCGM version node label](#dcgm-version-node-label) for you. Label every GPU node before you install. Otherwise, Kubernetes schedules no GPU Health Monitor pod.

### Embedded Mode

```yaml
global:
  dcgm:
    mode: embedded-mode
    embedded:
      endpoint: localhost
      port: 5555

gpu-health-monitor:
  runtimeClassName: nvidia
```

`runtimeClassName` is required. It must match an NVIDIA RuntimeClass that is installed in the cluster. In embedded mode, the chart automatically sets the GPU Health Monitor container to privileged. Thus, the NVIDIA Container Toolkit can provide GPU and driver access. You do not need a separate security-context value.

This mode does not set the [DCGM version node label](#dcgm-version-node-label) for you. Label every GPU node before you install. Otherwise, Kubernetes schedules no GPU Health Monitor pod.

In embedded mode, the GPU Health Monitor holds the GPUs open. As a result, it can stop the GPU Operator from unloading the driver. Thus, the DaemonSet gets the same required node affinity on the `nvidia.com/gpu.deploy.client` label as the metadata collector:

- The GPU Health Monitor runs when the label is `true` or is not present.
- It leaves the node for all other values, such as `paused-for-driver-upgrade` and `paused-for-mig-change`.

If you set `affinity` or `global.affinity`, the chart uses your value unchanged instead of this rule. For the full rules and limits, see [GPU Operator driver upgrades and MIG changes](metadata-collector.md#gpu-operator-driver-upgrades-and-mig-changes). In the other modes, the GPU Health Monitor connects to DCGM over the network, and the chart renders your `affinity` unchanged.

The GPU Operator sets the label to `true` again before the new driver loads. Thus, the GPU Health Monitor can start again before the driver is ready. Until the driver is ready, the GPU Health Monitor pod can fail to start, or it can report `GpuDcgmConnectivityFailure`.

### DCGM Version Node Label

The GPU Health Monitor has one DaemonSet for each DCGM major version. Each DaemonSet selects nodes by the `nvsentinel.dgxc.nvidia.com/dcgm.version` label. Thus, every node gets the GPU Health Monitor image that matches its DCGM. The chart renders this `nodeSelector` in all three modes, but only `operator-service` supplies the label for you.

| Mode | Who sets the label |
|---|---|
| `operator-service` | Labeler, from the GPU Operator DCGM pod image |
| `external-hostengine` | You, before you install NVSentinel |
| `embedded-mode` | You, before you install NVSentinel |

In `external-hostengine` and `embedded-mode`, there is no DCGM pod for labeler to read. Thus, labeler cannot get the version. If you select one of these modes, the chart automatically configures labeler to keep a valid label that already exists. Labeler never creates the label.

One case still removes the label. NVSentinel does not manage a node that has the opt-out label `nvsentinel.dgxc.nvidia.com/managed=false`. Labeler removes all its detection labels from that node, including `dcgm.version`. This applies in every source mode. Clear the opt-out before you label the node.

An unlabeled node runs no GPU Health Monitor pod, and nothing reports an error. The DaemonSet stays healthy, because Kubernetes never schedules a pod that it can reject. GPU health monitoring is silently absent on that node.

Label every GPU node with its DCGM major version. The only accepted values are `3.x` and `4.x`. Labeler treats any other value as absent.

```bash
# One node
kubectl label node <node-name> nvsentinel.dgxc.nvidia.com/dcgm.version=4.x

# Many nodes at once. Label each DCGM version separately, using a selector that
# matches only the nodes running that version.
kubectl label node -l <your-4.x-selector> nvsentinel.dgxc.nvidia.com/dcgm.version=4.x
kubectl label node -l <your-3.x-selector> nvsentinel.dgxc.nvidia.com/dcgm.version=3.x
```

Do not label every GPU node with one version. If a node runs DCGM 3.x and has the label `4.x`, it gets the 4.x image of the GPU Health Monitor. That image then fails against the hostengine that it finds.

If your fleet is truly uniform, `nvidia.com/gpu.present=true` is a usable selector. This label comes from Node Feature Discovery, which the GPU Operator installs. If you run neither Node Feature Discovery nor the GPU Operator, use your own selector. This is common in `embedded-mode`.

Make sure that a GPU Health Monitor pod runs on each labeled node:

```bash
kubectl get pods -n nvsentinel -l app.kubernetes.io/name=gpu-health-monitor -o wide
```

Compare that count with the nodes that have the label:

```bash
kubectl get nodes -L nvsentinel.dgxc.nvidia.com/dcgm.version
```

### Host Networking Override

`external-hostengine` enables host networking automatically. In other modes, if a custom deployment requires host networking, enable it explicitly:

```yaml
gpu-health-monitor:
  useHostNetworking: true
```

## DCGM Health Check Incident Suppression

Incident suppression drops the DCGM health check incidents that match specific error codes, before they generate a health event. Thus, NVSentinel never persists these incidents and never acts on them. Use it for high-frequency, non-actionable flaps, for example normal power-cap boost-clock behavior.

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    imexMonitoringEnabled: false
    suppressedErrorCodes:
      - DCGM_FR_CLOCK_THROTTLE_POWER
      - DCGM_FR_CLOCKS_EVENT_POWER
      - DCGM_FR_CLOCK_THROTTLE_THERMAL
      - DCGM_FR_CLOCKS_EVENT_THERMAL
      - DCGM_FR_NVLINK_EFFECTIVE_BER_THRESHOLD
      - DCGM_FR_NVLINK_SYMBOL_BER_THRESHOLD
```

### imexMonitoringEnabled

This value enables DCGM IMEX health monitoring. The default is `false`. In DRA deployments, IMEX can be created only while a workload requests IMEX channels. Thus, an idle node can have no IMEX daemon, and this state is valid. A DCGM health failure for that state is not actionable.

With a DCGM 4.7 or newer hostengine, `false` removes IMEX from the requested health-watch mask and keeps NVLink monitoring. With an older hostengine, the GPU Health Monitor filters the legacy `DCGM_FR_IMEX_UNHEALTHY` incident before it combines the incident with GPU incidents. The hostengine version controls which behavior is available.

If you do not expect the IMEX daemon to be continuously available, keep this value `false`. IMEX incidents are global DCGM entities, but the routing of the GPU Health Monitor can attribute them to GPU 0.

### suppressedErrorCodes

This value is the list of DCGM error code names to suppress, as DCGM reports them (for example, `DCGM_FR_CLOCK_THROTTLE_POWER`). Suppression applies only to the listed error codes. The GPU Health Monitor still reports other incidents on the same health watch, for example other `GpuPowerWatch` error codes.

The defaults include throttling. Of the errors that these two watches raise, throttling is the only one that tracks load and not a fault. It maps to `NONE`, so it produces only node events.

| Watch | Suppressed | Still reported |
| --- | --- | --- |
| `GpuPowerWatch` | `DCGM_FR_CLOCKS_EVENT_POWER` (12) | `DCGM_FR_POWER_UNREADABLE`, `DCGM_FR_XID_ERROR` (XIDs 54, 56, 58, 78) |
| `GpuThermalWatch` | `DCGM_FR_CLOCKS_EVENT_THERMAL` (10) | `DCGM_FR_THERMAL_VIOLATIONS`, `DCGM_FR_XID_ERROR` (XID 61) |

The list has four names for two codes. `DCGM_FR_CLOCK_THROTTLE_*` is a deprecated alias of `DCGM_FR_CLOCKS_EVENT_*`, with the same number. DCGM can report either name, so the list includes both names.

The defaults also include the NVLink bit error rate (BER) codes `DCGM_FR_NVLINK_EFFECTIVE_BER_THRESHOLD` (119) and `DCGM_FR_NVLINK_SYMBOL_BER_THRESHOLD` (121) from `GpuNvlinkWatch`. [DCGM 4.7.0 removed these checks](https://docs.nvidia.com/datacenter/dcgm/latest/release-notes/changelog.html) from the health watch and from diagnostics, because they failed healthy GPUs. A DCGM 4.7 or newer hostengine does not report these codes, so this suppression has no effect there. An older hostengine still reports these codes, and `dcgmerrorsmapping.csv` maps them to `CONTACT_SUPPORT`. Then the default quarantine policy cordons the node. The suppression makes an older hostengine behave like DCGM 4.7.

The cost is the same as in DCGM 4.7: a link with a truly high BER also produces no health event. The GPU Health Monitor still reports other `GpuNvlinkWatch` codes. To alert on BER, use the DCGM fields `DCGM_FI_DEV_NVLINK_SYMBOL_BER_RATIO` (1216) and `DCGM_FI_DEV_NVLINK_EFFECTIVE_BER_RATIO` (1218) with a threshold from your hardware vendor. If you set your own `suppressedErrorCodes`, your list replaces the default list. In that case, add these two codes to your list. The `dcgm_health_check_suppressed_incidents` metric counts each dropped incident by `error_code`.

`DCGM_FR_NVLINK_ERROR_CRITICAL` (71) maps to `NONE`, because DCGM does not define a supported recovery and counter-clearing procedure for it. Both WARN and FAIL incidents produce health events that are unhealthy and non-fatal. The default quarantine policy does not cordon nodes for these health events. This applies to every incident with code 71, including other NVLink errors that share this code. Other error codes keep their configured actions.

The suppression does not affect real power and cooling faults. Power faults arrive as `GPU_HW_POWER_BRAKE_VIOLATION` through [`GpuPowerBrakeWatch`](#hardware-power-brake-detection). Cooling faults arrive as `GPU_TEMP_HW_SLOWDOWN_VIOLATION` through `GpuThermalMarginWatch`. Both map to `CONTACT_SUPPORT`. The suppression also does not affect `DCGM_FR_THROTTLING_VIOLATION`, because this code comes only from `dcgmi diag` and not from these watches.

### Example: Disable suppression

Use this configuration to report all error codes again, including throttling and NVLink BER.

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    suppressedErrorCodes: []
```

## GPU Thermal Margin Detection

`GpuThermalMarginWatch` reads DCGM field 153, which is the margin in degrees to the slowdown temperature limit of the GPU. It fails a GPU whose margin reaches the hardware slowdown threshold. It reads the field directly, because the thermal health watch of DCGM does not report the margin.

```yaml
gpu-health-monitor:
  dcgmFieldsMonitoring:
    gpuTempLimitMonitoringEnabled: true
    gpuTempLimitStoreOnly: true
    gpuTempLimitMinConsecutivePolls: 3
```

### gpuTempLimitMonitoringEnabled

This value enables the watch. By default, the watch is on.

### gpuTempLimitStoreOnly

This value enables a dry run for this check. When it is true, the GPU Health Monitor emits the health events of this check with `processingStrategy=STORE_ONLY`. NVSentinel persists these health events and exports them as metrics, but it excludes them from the remediation pipeline. Thus, there is no node condition and no cordon. The default is true, so you can observe the watch before it can act. When you have confirmed that the thresholds suit your hardware and cooling, set it to `false`.

### gpuTempLimitMinConsecutivePolls

This value is the number of consecutive polls with the margin below the slowdown threshold before the GPU Health Monitor fails the GPU. The default is 3.

The GPU Health Monitor reads the margin as one sample for each poll. Under load, the margin can move by tens of degrees within seconds. Thus, one sample past the threshold is usually a transient that the hardware slowdown of the GPU has already handled. A sustained excursion is the actionable case.

A sample at or above the threshold resets the counter. A value of `1` fails the GPU on the first observation. The GPU Health Monitor skips a GPU that has no usable sample, and that GPU keeps its counter. Thus, a gap in DCGM data neither raises nor clears a finding.

While the counter is below the threshold, the GPU Health Monitor reports the GPU neither as healthy nor as unhealthy. Thus, a restart cannot publish a healthy event for a GPU that is still past the threshold.

To interpret a check that fires, see the [GPU Thermal Margin runbook](../runbooks/gpu-thermal-margin.md).

## NVLink Suppression on Unbridged PCIe Cards

This value suppresses the NVLink-down fault on PCIe cards that have NVLink silicon but no installed bridge. Examples are A100/H100 PCIe, GeForce RTX 3090 and GeForce RTX 2080 Ti. Without this suppression, DCGM reports those permanently inactive links as a fatal fault with `RESTART_VM`.

```yaml
gpu-health-monitor:
  suppressNvlinkDownOnUnbridgedPcie: "False"
```

The value is a quoted string, not a boolean.

If any GPU pool uses NVLink bridges, leave it `"False"`. An unbridged card looks identical to a card whose bridge was already dead when the metadata was collected. Thus, if you enable suppression on a bridged pool, it can hide a real bridge failure that was present at boot. For GPUs with no NVLink silicon, such as L40 and A40, the GPU Health Monitor always suppresses `DCGM_FR_NVLINK_DOWN` and `DCGM_FR_FABRIC_PROBE_STATE`, regardless of this value.

## Hardware Power Brake Detection

`GpuPowerBrakeWatch` fails a GPU whose clocks-event-reasons mask has the hardware power brake bit (`0x80`) set. This bit means that the power delivery path forces the clocks down. The watch reads `DCGM_FI_DEV_CLOCKS_EVENT_REASONS` directly, in the same way that `GpuThermalMarginWatch` reads field 153. It does this because the POWER health watch of DCGM does not report the brake.

That difference is important. The dominant code of the POWER watch is `DCGM_FR_CLOCK_THROTTLE_POWER`. This code tracks power-capped clock throttling and maps to `NONE`. By default, the GPU Health Monitor suppresses it, as [DCGM Health Check Incident Suppression](#dcgm-health-check-incident-suppression) describes. A sustained brake is a power delivery fault, so it needs its own signal instead of a shared non-actionable signal.

`GPU_HW_POWER_BRAKE_VIOLATION` maps to `CONTACT_SUPPORT` in `dcgmerrorsmapping.csv`, the same as the thermal margin fault. An asserted brake is a facility or power delivery problem, and a node reboot does not resolve it.

```yaml
gpu-health-monitor:
  dcgmFieldsMonitoring:
    gpuPowerBrakeMonitoringEnabled: false
    gpuPowerBrakeStoreOnly: true
    gpuPowerBrakeMinConsecutivePolls: 3
```

### gpuPowerBrakeMonitoringEnabled

This value enables the watch. By default, the watch is off. If the running DCGM build has neither `DCGM_FI_DEV_CLOCKS_EVENT_REASONS` nor the older `DCGM_FI_DEV_CLOCK_THROTTLE_REASONS`, the GPU Health Monitor logs a warning and disables the watch.

### gpuPowerBrakeStoreOnly

This value enables a dry run for this check. When it is true, the GPU Health Monitor emits the health events of this check with `processingStrategy=STORE_ONLY`. NVSentinel persists these health events and exports them as metrics, but it excludes them from the remediation pipeline. Thus, there is no node condition and no cordon. The default is true, so after you enable the watch, you can observe it before it can act.

### gpuPowerBrakeMinConsecutivePolls

This value is the number of consecutive polls with the bit set before the GPU Health Monitor fails the GPU. A brake that is asserted for one poll can be a load transient. A sustained assertion is the actionable case. A clear resets the counter, so a flapping brake never accumulates to a failure. A value of `1` fails the GPU on the first observation.

The GPU Health Monitor skips a GPU that has no usable sample, and that GPU keeps its counter. Thus, a gap in DCGM data neither raises nor clears a finding. This includes the int64 "no data" sentinels of DCGM. The low byte of these sentinels has the brake bit set. The GPU Health Monitor skips them, so they do not read as an assertion.

While the counter is below the threshold, the GPU Health Monitor reports the GPU neither as healthy nor as unhealthy. Thus, a restart cannot publish a healthy event for a GPU whose brake is still asserted.

## Uptime Limit for Known Defects

`GpuUptimeLimitWatch` reboots a node before a known uptime-limited defect occurs. Some GPU firmware defects fail at a fixed node uptime. For example, a vBIOS defect makes NVLink communication fail after more than 60 days of uptime, in some cases after 61 to 63 days. Until you install a fixed vBIOS, the mitigation is to reboot each affected node before it reaches that uptime.

When the check is enabled, the GPU Health Monitor reads the vBIOS version of each GPU from DCGM. It reads the versions each time that it connects to DCGM. On each poll, it reads the time since the node booted from `CLOCK_BOOTTIME`, the kernel clock behind `/proc/uptime`. A per-container view of `/proc/uptime`, for example from LXCFS, does not change this clock. A GPU fails when it runs a listed vBIOS version and the node uptime is more than the limit of the node. This limit is `maxUptimeHours`, or less if you set `staggerHours`.

`GPU_UPTIME_LIMIT_EXCEEDED` maps to `RESTART_BM` in `dcgmerrorsmapping.csv`. Thus, NVSentinel quarantines, drains and reboots the node in the same way as for other faults that need a reboot. After the reboot, the node uptime is low again, and the GPU Health Monitor reports the GPUs as healthy.

The check uses the node uptime for every GPU. A driver reload or a GPU reset does not change the node uptime. Thus, the check does not fire later than the limit.

The DCGM hostengine must support GPU attribute requests. Otherwise, the check cannot read the versions, and it does not evaluate any GPU past the limit.

```yaml
gpu-health-monitor:
  uptimeLimit:
    enabled: true
    storeOnly: true
    affectedVbiosVersions:
      - "97.00.82.00.4B"
    maxUptimeHours: 1440
    staggerHours: 72
```

If many affected nodes started at the same time, they pass `maxUptimeHours` at the same time. Then the [circuit breaker](../runbooks/circuit-breaker.md) can trip and stop all remediation. To spread the reboots of these nodes over time, set `staggerHours`. Keep `storeOnly` true first, and examine which nodes fail and when. If you prefer to tune the breaker, use the `circuitBreaker` settings of [fault-quarantine](fault-quarantine.md#circuit-breaker): `percentage`, `maxNodes`, `maxCordonedNodes` and `maxCordonedPercentage`.

### enabled

This value enables the check. By default, the check is off. If you enable the check, list at least one vBIOS version and set `maxUptimeHours` to more than 0. Otherwise, Helm does not render the chart. If you write `config.ini` yourself, the GPU Health Monitor does not start.

If you disable the check, the GPU Health Monitor stops sending its health events. Thus, a node with an active `GpuUptimeLimitWatch` fault stays quarantined. Reboot such nodes before you disable the check.

### storeOnly

This value enables a dry run for this check. When it is true, the GPU Health Monitor emits the health events of this check with `processingStrategy=STORE_ONLY`. NVSentinel persists these health events and exports them as metrics, but it excludes them from the remediation pipeline. Thus, there is no node condition, no quarantine and no reboot. The default is true, so after you enable the check, you can observe it before it can act.

### affectedVbiosVersions

This value lists the affected vBIOS versions. Write each version exactly as `nvidia-smi` reports it, in quotes. YAML can read an unquoted version, such as `97.00`, as a number, so the chart refuses a version that is not a string. If you set the list on the command line, use `--set-json` or `--set-string`. A GPU is affected when its vBIOS version is in the list. The driver version does not change the result.

The match is exact, but it ignores case and surrounding spaces. For example, `97.00.82` does not match `97.00.82.00.4B`.

Within the limit, the GPU Health Monitor reports every GPU as healthy, also a GPU whose vBIOS version it cannot read. Thus, a fault clears after the reboot. Past the limit, it does not evaluate a GPU whose vBIOS version is unknown. A version is unknown when DCGM does not return it, or returns a marker such as `<<<NOT_SUPPORTED>>>`. The GPU Health Monitor reports such a GPU neither as healthy nor as unhealthy. It logs a warning that names the GPU, and it reads the version again every 10 minutes.

### maxUptimeHours

This value is the maximum node uptime in hours for a node with an affected GPU. Set it below the uptime at which the defect occurs. Leave enough time to drain and reboot every affected node. For example, for a defect that can occur after 61 days (1464 hours), a value of `1440` leaves one day.

### staggerHours

This value spreads the reboots of nodes that started at the same time. The default is `0`, which gives every node the limit `maxUptimeHours`.

If you set a value, each node gets its own limit between `maxUptimeHours` minus `staggerHours` and `maxUptimeHours`. The GPU Health Monitor calculates the limit from a hash of the node name. Thus, the limit of a node does not change when the GPU Health Monitor restarts. No node gets a limit that is more than `maxUptimeHours`. When the check starts, the GPU Health Monitor logs the limit of its node.

For example, with `maxUptimeHours: 1440` and `staggerHours: 72`, each affected node fails the check at an uptime between 1368 and 1440 hours. Thus, the reboots of nodes that started together spread over three days.

`staggerHours` moves the limits, not the time when the check acts. A node whose uptime is already more than its limit fails on the next poll. Thus, affected nodes that are already past their limits fail together when you enable the check, or when you set `storeOnly` to false after a long observation. Reboot these nodes in small batches before the check can act on them. Then `staggerHours` spreads the nodes that reach their limits later.

If you enable the check, set `staggerHours` to at least 0 and to less than `maxUptimeHours`. Otherwise, Helm does not render the chart. If you write `config.ini` yourself, the GPU Health Monitor does not start.

## DCGM Startup Gate

In remote DCGM modes, the GPU Health Monitor can wait for a functional DCGM API before its main container starts:

```yaml
gpu-health-monitor:
  dcgmConnectivity:
    startupGate:
      enabled: true
      retryIntervalSeconds: 5
      connectTimeoutSeconds: 10
```

When you enable the gate, the chart adds a `wait-for-dcgm` init container. Each attempt creates a DCGM handle and does supported-GPU discovery. An open TCP port alone does not count as ready. The init container retries failed attempts every `retryIntervalSeconds`, with no limit. Each attempt runs in a child process with `connectTimeoutSeconds` as its time limit. Thus, the init container can end a blocked native DCGM call, and it does not have to restart.

The init container does not publish health events. Thus, an unavailable DCGM endpoint during installation or restart causes no node conditions, no quarantine and no remediation. This stays true until the GPU Health Monitor makes its first connection.

The default 10-second hard timeout gives time for the 5-second connection timeout of DCGM to return first and log a specific connection error. The parent timeout stays as a second limit for a native call that is truly stuck.

By default, the gate is disabled. It supports `operator-service` and `external-hostengine`. You cannot enable it in `embedded-mode`, because the main GPU Health Monitor container starts the embedded hostengine. Helm rejects that combination, and it does not create a pod that can never leave init.

While DCGM stays unavailable, the pod stays in `Init:0/1`. Ordinary readiness failures do not restart the init container. Monitor this state with the existing pod-state alerts of your deployment (for example, kube-state-metrics). An `Init:CrashLoopBackOff` shows an image, configuration or implementation failure instead.

After startup, the connectivity checks of the GPU Health Monitor still handle runtime DCGM failures. The startup gate does not replace runtime failure handling or debouncing.

For example, send an alert when the startup gate has waited for more than five minutes:

```yaml
- alert: NVSentinelGPUHealthMonitorWaitingForDCGM
  expr: |
    kube_pod_init_container_status_running{
      container="wait-for-dcgm"
    } == 1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "gpu-health-monitor is waiting for DCGM"
    description: "Pod {{ $labels.namespace }}/{{ $labels.pod }} has been waiting for DCGM for more than 5 minutes."
```

Send a separate alert when the init program itself fails repeatedly:

```yaml
- alert: NVSentinelGPUHealthMonitorDCGMInitFailed
  expr: |
    kube_pod_init_container_status_waiting_reason{
      container="wait-for-dcgm",
      reason="CrashLoopBackOff"
    } == 1
  for: 2m
  labels:
    severity: critical
  annotations:
    summary: "gpu-health-monitor DCGM startup gate is failing"
    description: "The wait-for-dcgm init container in {{ $labels.namespace }}/{{ $labels.pod }} is repeatedly failing."
```

## Runtime DCGM Connectivity Debounce

After the main container starts, you can debounce transient DCGM timeouts or connection errors before they become `GpuDcgmConnectivityFailure` transitions. For example, the following configuration requires three consecutive failures:

```yaml
gpu-health-monitor:
  dcgmConnectivity:
    runtimeDebounce:
      failureThreshold: 3
      successThreshold: 1
```

`failureThreshold` is the number of consecutive failed DCGM connectivity cycles that are necessary before the GPU Health Monitor publishes the unhealthy event. A successful cycle before the threshold resets the failure streak. Thus, intermittent failures do not accumulate across healthy polls.

`successThreshold` is the number of consecutive successful cycles that are necessary to clear a connectivity failure that was already published. A failed cycle during recovery resets the success streak and keeps the existing unhealthy event active. The threshold does not delay the initial healthy baseline event that the GPU Health Monitor emits for a node with no previous connectivity state.

Both values must be integers greater than or equal to `1`. They default to `1`, which gives fail-once and recover-once behavior. A higher `failureThreshold` filters shorter connectivity interruptions. The cost is up to `failureThreshold - 1` additional poll intervals of detection latency.

The debounce state belongs to the running GPU Health Monitor process, and the process does not persist this state. A restart of the process resets both observation streaks. The replacement process emits its normal initial healthy baseline as soon as a complete health check succeeds. Thus, the platform state from the previous process can converge without a wait for `successThreshold`.

The following metric exposes the current streaks:

```text
dcgm_connectivity_consecutive_observations{result="failure"}
dcgm_connectivity_consecutive_observations{result="success"}
```

Runtime debounce and the startup gate apply to different phases:

- `startupGate` prevents the GPU Health Monitor from starting and publishing connectivity failures before DCGM has been functional one time.
- `runtimeDebounce` filters short connectivity transitions after the GPU Health Monitor starts.
- In remote modes, the GPU Health Monitor debounces a probe hang by time, not by cycles. It publishes the hang when DCGM has been unreachable for `failureThreshold * pollIntervalSeconds`:
  - A hung call has no later poll to count. Also, the liveness probe restarts the container before a long deadline ends. Thus, the GPU Health Monitor persists the start of the unreachable window under its state directory, and it resumes the window after a restart.
  - Any successful cycle ends the window. A failure more than ten minutes after the previous failure starts a new window, which covers the capped restart backoff of the kubelet.
  - With the default `failureThreshold: 1`, the GPU Health Monitor publishes the hang as soon as it detects the hang.
  - The embedded-mode `GpuDcgmUnresponsive` event has no delay, because its fault is node-local.

`dcgmHealthCheck.connectivityFailureEscalationThreshold` counts consecutive failed observations. If its threshold is lower than the debounce failure threshold, the first published unhealthy event can already recommend `RESTART_BM`. If you want a preliminary `CONTACT_SUPPORT` event, set the escalation threshold to `failureThreshold` or higher.

Example behavior for `failureThreshold: 4` and `successThreshold: 2`:

```text
failure 1-3  -> no unhealthy event
failure 4    -> publish GpuDcgmConnectivityFailure
success 1    -> keep the unhealthy event active
failure      -> reset the recovery streak
success 1    -> keep the unhealthy event active
success 2    -> publish the healthy recovery event
```

## Unresponsive DCGM Detection

A DCGM call that stops answering never returns an error. The callers wait, and the probe blocks forever instead of raising `DCGMError_Timeout`. During this time, the node can still report `Ready`, with every GPU allocatable and no taint. Thus, no other signal in the stack records a fault.

In `embedded-mode`, that hang is node-local, but the hang alone does not prove a kernel-driver wedge. A DCGM userspace deadlock or lock contention can look the same, until an independent NVML/`nvidia-smi` probe confirms the driver itself.

The poll loop cannot report this hang itself. The loop blocks before the point where it publishes anything. `/healthz` only observes that the loop is frozen, so kubelet restarts the container. Then the replacement container blocks in the same place. The settings that follow close that gap.

```yaml
gpu-health-monitor:
  dcgm:
    pollIntervalSeconds: 15
    # Omit (or null) to default to pollIntervalSeconds * 3; set 0 to disable.
    probeStoreOnly: true
  dcgmHealthCheck:
    connectivityFailureEscalationThreshold: 0
```

### probeStoreOnly

This value puts the probe-hang checks in dry-run mode. While it is `true` (the default), the GPU Health Monitor emits local `GpuDcgmUnresponsive` and remote `DCGM_PROBE_HANG` health events with `processingStrategy=STORE_ONLY`. NVSentinel persists these health events and exports them as metrics, but it excludes them from the remediation pipeline. Remote `DCGM_CONNECTIVITY_ERROR` health events continue to use the configured processing strategy. Probe-hang health events still carry their recommended action, so the record shows what the node needs.

To let the configured processing strategy act on probe-hang health events, do these steps:

1. Watch `dcgm_probe_hangs` and the stored health events for one or two releases.
2. Confirm that the detections match the hangs on your fleet.
3. Set `probeStoreOnly: false`.

Recovery matches the active event. When a remediable connectivity failure replaces a remote `STORE_ONLY` probe hang, the clearing event uses `EXECUTE_REMEDIATION`. Thus, the original unhealthy event and its clearing event do not always use the same strategy.

### nvswitchStoreOnly

This value puts NVSwitch health events in dry-run mode. While it is `true` (the default), the GPU Health Monitor emits every health event whose impacted entity is an NVSwitch with `processingStrategy=STORE_ONLY`, whichever DCGM watch reported it. NVSentinel persists these health events and exports them as metrics, but it excludes them from the remediation pipeline.

When you set it to `false`, NVSwitch health events use the same processing strategy as GPU health events from the same check: the configured processing strategy, or `STORE_ONLY` for a check that another observe-only setting covers.

A fatal NVSwitch health event then sets the node condition of its check, as a GPU health event does. The default fault-quarantine rules for the GPU Health Monitor match only `componentClass == 'GPU'`, so they do not cordon a node for an NVSwitch health event. To quarantine on NVSwitch faults, add a rule that matches `componentClass == 'NVSWITCH'`.

Set it back to `true` only when no NVSwitch fault is active. Platform connectors and fault quarantine skip `STORE_ONLY` health events. Thus, a `STORE_ONLY` clearing event does not clear a node condition or a quarantine that an earlier NVSwitch fault created. If you set it back while a fault was active, set it to `false` again. The configuration change restarts the GPU Health Monitor, and the first poll that finds the NVSwitch healthy emits a clearing event that uses the configured strategy. Then set it to `true`.

On NVL72 systems, the NVSwitches are in separate switch trays, and DCGM on a compute node enumerates none. This setting has no effect there.

### probeDeadlineSeconds

This value is the number of seconds that one DCGM probe can run before a watchdog thread reports the stalled operation. The blocked probe cannot stop the watchdog thread. The value has these effects in each mode:

- In `embedded-mode`, the call is in-process and node-local. Thus, the GPU Health Monitor publishes `GpuDcgmUnresponsive` with error code `DCGM_PROBE_HANG` and recommended action `RESTART_BM`.
- In `operator-service` and `external-hostengine` modes, the same symptom can come from the endpoint, DNS or the network. Thus, those modes publish `GpuDcgmConnectivityFailure` with `CONTACT_SUPPORT` instead.

If you do not set this value, it defaults to `PollIntervalSeconds * 3`. To disable the watchdog, set it to `0`.

The default equals the `/healthz` staleness window (`PollIntervalSeconds * 3`). Thus, the GPU Health Monitor reports the hang when this window officially marks the poll loop as stalled. The GPU Health Monitor limits the delivery of a critical health event to 15 seconds. The rest of the failure budget of the liveness probe is then available to persist the finding before kubelet restarts the container.

DCGM exposes timeout errors, but it does not document a fixed timeout for every RPC. Treat a deadline that you configure as a value specific to your fleet. It does not prove that every slower operation is a hard hang. The chart templates `PollIntervalSeconds` from `dcgm.pollIntervalSeconds`. If `probeDeadlineSeconds` is null or omitted, the chart sets the deadline to `pollIntervalSeconds * 3`, so the two values stay coupled.

While you measure normal embedded-mode probe latencies, keep `probeStoreOnly` enabled. If you raise `probeDeadlineSeconds` by a large amount, verify that the new deadline still comes before the configured liveness restart. For that adjustment, the chart exposes `livenessProbe.periodSeconds` and `livenessProbe.failureThreshold`.

The GPU Health Monitor reports the health event one time for each hang episode. After delivery, a marker under the persistent `/var/run/nvsentinel` state of the GPU Health Monitor survives liveness restarts. The marker prevents a second publication of the same hang. It also lets the first successful probe emit the healthy clearing event.

The GPU Health Monitor tracks every DCGM call in the poll loop. This includes the connect, the health check, the thermal margin evaluation and the cleanup after a connectivity failure. It does not track the cleanup during an intentional shutdown. Thus, it cannot report a slow teardown as a hang while DCGM restarts. `dcgm_probe_hangs` increments when the probe crosses the deadline, even if the GPU Health Monitor must retry the event delivery.

### connectivityFailureEscalationThreshold

This value is the number of consecutive `GpuDcgmConnectivityFailure` cycles after which the recommended action escalates from `CONTACT_SUPPORT` to `RESTART_BM`.

Before you enable the escalation, make sure that both of these conditions are true:

- The configured DCGM endpoint is node-local.
- You validated that repeated unreachability is a driver wedge.

A node reboot is not a valid remediation for a shared service, a service failure, a DNS issue or a network policy.

The default is `0`, which disables escalation and keeps every connectivity failure at `CONTACT_SUPPORT`. The counter resets when connectivity is restored. The GPU Health Monitor publishes the escalated event one time, not on every later cycle.

> **Note**: Probe-hang health events can recommend `RESTART_BM`, which fault-remediation maps to a `RebootNode` CR. `probeStoreOnly` gates these health events in local and remote modes. `connectivityFailureEscalationThreshold` is a separate opt-in for the escalation of repeated connectivity failures, and its default is `0`.

### minConsecutivePolls

This value is the number of consecutive polls that a health check incident must persist before the GPU Health Monitor publishes it. The GPU Health Monitor counts the polls separately for each error code and GPU. Codes that are not in the map publish on their first observation, which is the default behavior.

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    minConsecutivePolls:
      DCGM_FR_NVLINK_DOWN: 2
```

Use this value for a code whose single-poll occurrences are transients and not faults. `DCGM_FR_NVLINK_DOWN` is the main use case. On SXM systems, the NVLink links are down for a short time after every node boot, while they train. Without a threshold, a routine maintenance reboot produces a FATAL health event with `RESTART_VM`.

The [NVLink-down suppression](#nvlink-suppression-on-unbridged-pcie-cards) cannot cover this case. It is metadata-based, and it intentionally fails closed on SXM, where an untrained link looks the same as a dead link. A time-based threshold separates the two: an untrained link trains, but a dead link does not.

The key of the streak is `(error code, GPU)`. The streak resets when the code is absent for that GPU in a poll. Thus, a code that appears on alternate polls for a GPU never accumulates, and the GPU Health Monitor never publishes it.

The key is the code and the GPU, not the individual link. A GPU can report one down link on one poll and a different down link on the next poll. That GPU keeps its streak, because it has had a link down continuously. A streak also survives a failed poll, because a DCGM timeout observes nothing and must not clear the streak.

**Tradeoff**: a threshold of `N` delays a truly sustained fault by up to `N-1` poll intervals. At the default `pollIntervalSeconds: 15`, a threshold of `2` costs at most 15 seconds of detection latency. Use the smallest threshold that suppresses the transient. A permanent suppression of the code through `suppressedErrorCodes` also hides real failures, and the threshold avoids this problem.

`dcgm_health_check_debounced_incidents{error_code, gpu_id}` counts the withheld incidents, so you can observe the suppression for each GPU. It increments one time for each poll and each `(error code, GPU)`, not one time for each incident record.

## Additional Volumes

This setting is an extension point. Use it to mount additional host paths that DCGM requires in specific environments.

### Configuration Structure

```yaml
gpu-health-monitor:
  additionalVolumeMounts: []
  additionalHostVolumes: []
```

### Parameters

#### additionalVolumeMounts

This value is a list of volume mounts to add to the GPU Health Monitor container. Each mount specifies where to mount a volume in the container.

#### additionalHostVolumes

This value is a list of host path volumes to make available to the pod. Each volume refers to a path on the host node.

### When to Use Additional Volumes

Additional volumes are necessary in environments where DCGM needs access to GPU drivers or libraries that are installed in non-standard host locations.

**Common scenarios:**

- GCP GKE nodes with GPU drivers in `/home/kubernetes/bin/nvidia`
- Custom driver installation paths

### Volume Mount Examples

#### Example 1: GCP GKE Configuration

GCP GKE installs NVIDIA drivers and Vulkan ICD files in custom locations that the DCGM SDK needs to access.

```yaml
gpu-health-monitor:
  additionalVolumeMounts:
    - mountPath: /usr/local/nvidia
      name: nvidia-install-dir-host
      readOnly: true
    - mountPath: /etc/vulkan/icd.d
      name: vulkan-icd-mount
      readOnly: true
  
  additionalHostVolumes:
    - name: nvidia-install-dir-host
      hostPath:
        path: /home/kubernetes/bin/nvidia
        type: Directory
    - name: vulkan-icd-mount
      hostPath:
        path: /home/kubernetes/bin/nvidia/vulkan/icd.d
        type: Directory
```
