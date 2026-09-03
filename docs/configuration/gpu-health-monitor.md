# GPU Health Monitor Configuration

## Overview

The GPU Health Monitor module watches GPU health using NVIDIA DCGM (Data Center GPU Manager) and reports hardware failures. This document covers all Helm configuration options for system administrators.

## DCGM Deployment Modes

The GPU Health Monitor supports three DCGM source modes, selected with `global.dcgm.mode`.

### Operator Service

The GPU Operator runs DCGM as a DaemonSet and exposes it through a Kubernetes service. GPU Health Monitor pods connect to the service endpoint.

**Characteristics:**
- DCGM runs as a DaemonSet (one pod per GPU node)
- Kubernetes service provides DNS endpoint for DCGM
- GPU Health Monitor connects via service DNS name

### External Hostengine

An externally managed hostengine runs on each GPU node. GPU Health Monitor pods use host networking and connect to the configured endpoint, which defaults to `localhost:5555`.

**Characteristics:**
- The hostengine lifecycle is managed outside NVSentinel
- No Kubernetes service needed
- GPU Health Monitor enables host networking automatically

### Embedded Mode

GPU Health Monitor starts an in-process DCGM hostengine and exposes it to pod-local clients on a loopback endpoint.

**Characteristics:**
- No separate DCGM DaemonSet or service is needed
- `gpu-health-monitor.runtimeClassName` must name the cluster's NVIDIA RuntimeClass
- The chart automatically sets `privileged: true` on the GPU Health Monitor container
- The endpoint must be `localhost`, `127.0.0.1`, or `::1`

## Configuration Reference

### Module Enable/Disable

Controls whether the gpu-health-monitor module is deployed in the cluster.

```yaml
global:
  gpuHealthMonitor:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the gpu-health-monitor pod.

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

Controls verbosity of gpu-health-monitor logs.

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
      endpoint: "nvidia-dcgm.gpu-operator.svc"
      port: 5555
```

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

NVSentinel does not deploy the hostengine in this mode. The configured hostengine must already be running and reachable on every selected GPU node.

```yaml
global:
  dcgm:
    mode: external-hostengine
    externalHostengine:
      endpoint: localhost
      port: 5555
```

GPU Health Monitor enables host networking automatically in this mode.

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

`runtimeClassName` is required and must match an NVIDIA RuntimeClass installed in the cluster. The chart automatically sets the GPU Health Monitor container to privileged in embedded mode so the NVIDIA Container Toolkit can provide GPU and driver access; no separate security-context value is required.

### Host Networking Override

`external-hostengine` enables host networking automatically. For other modes, it can be enabled explicitly when required by a custom deployment:

```yaml
gpu-health-monitor:
  useHostNetworking: true
```

## DCGM Health Check Incident Suppression

Drops DCGM health check incidents matching specific error codes before they generate a health event, so they are never persisted or acted on. Useful for high-frequency, non-actionable flaps (e.g. normal power-cap boost-clock behavior).

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    suppressedErrorCodes:
      - DCGM_FR_CLOCK_THROTTLE_POWER
      - DCGM_FR_CLOCKS_EVENT_POWER
      - DCGM_FR_CLOCK_THROTTLE_THERMAL
      - DCGM_FR_CLOCKS_EVENT_THERMAL
```

### suppressedErrorCodes
List of DCGM error code names (as reported by DCGM, e.g. `DCGM_FR_CLOCK_THROTTLE_POWER`) to suppress. Suppression is scoped to the listed error codes only — other incidents on the same health watch (e.g. other `GpuPowerWatch` error codes) are still reported.

The default is throttling: of the errors these two watches raise, it is the only one that tracks load rather than a fault, and it maps to `NONE`, so it only ever produced node events.

| Watch | Suppressed | Still reported |
| --- | --- | --- |
| `GpuPowerWatch` | `DCGM_FR_CLOCKS_EVENT_POWER` (12) | `DCGM_FR_POWER_UNREADABLE`, `DCGM_FR_XID_ERROR` (XIDs 54, 56, 58, 78) |
| `GpuThermalWatch` | `DCGM_FR_CLOCKS_EVENT_THERMAL` (10) | `DCGM_FR_THERMAL_VIOLATIONS`, `DCGM_FR_XID_ERROR` (XID 61) |

Four names, two codes: `DCGM_FR_CLOCK_THROTTLE_*` is a deprecated alias of `DCGM_FR_CLOCKS_EVENT_*` with the same number, and either name can be reported, so both are listed.

Genuine power and cooling faults are unaffected, arriving as `GPU_HW_POWER_BRAKE_VIOLATION` via [`GpuPowerBrakeWatch`](#hardware-power-brake-detection) and `GPU_TEMP_HW_SLOWDOWN_VIOLATION` via `GpuThermalMarginWatch`, both `CONTACT_SUPPORT`. `DCGM_FR_THROTTLING_VIOLATION` is not suppressed either: it comes only from `dcgmi diag`, not these watches.

### Example: Report throttling again

Use this to investigate throttling on a specific cluster; it restores the `GpuPowerWatch` and `GpuThermalWatch` events.

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    suppressedErrorCodes: []
```

## Hardware Power Brake Detection

`GpuPowerBrakeWatch` fails a GPU whose clocks-event-reasons mask has the hardware power brake bit (`0x80`) set, meaning the power delivery path is forcing clocks down. It reads `DCGM_FI_DEV_CLOCKS_EVENT_REASONS` directly, in the same way `GpuThermalMarginWatch` reads field 153, because DCGM's POWER health watch does not report the brake.

That distinction matters. The POWER watch's dominant code, `DCGM_FR_CLOCK_THROTTLE_POWER`, tracks power-capped clock throttling, maps to `NONE`, and is suppressed by default under [DCGM Health Check Incident Suppression](#dcgm-health-check-incident-suppression) above. On a 288-node GB200 cluster it was observed active on 69% of nodes while exactly 50% had a brake asserted, correlating with neither the brake bit nor the SW power cap bit (`0x04`). A sustained brake is a power delivery fault, so it needs its own signal rather than sharing a non-actionable one.

`GPU_HW_POWER_BRAKE_VIOLATION` maps to `CONTACT_SUPPORT` in `dcgmerrorsmapping.csv`, matching the thermal precedent: an asserted brake is a facility or power delivery problem, not something a node reboot resolves.

```yaml
gpu-health-monitor:
  dcgmFieldsMonitoring:
    gpuPowerBrakeMonitoringEnabled: false
    gpuPowerBrakeStoreOnly: true
    gpuPowerBrakeMinConsecutivePolls: 3
```

### gpuPowerBrakeMonitoringEnabled

Enables the watch. Off by default so existing deployments see no behaviour change. If neither `DCGM_FI_DEV_CLOCKS_EVENT_REASONS` nor the older `DCGM_FI_DEV_CLOCK_THROTTLE_REASONS` is available in the running DCGM build, the monitor logs a warning and disables itself.

### gpuPowerBrakeStoreOnly

Dry run. When true, this check's events are emitted with `processingStrategy=STORE_ONLY`, so they are persisted and exported as metrics but excluded from the remediation pipeline: no node condition and no cordon. Defaults to true, so enabling the watch is observable before it can act.

### gpuPowerBrakeMinConsecutivePolls

Consecutive polls with the bit set before the GPU is failed. A brake asserted for a single poll can be a load transient; a sustained assertion is the actionable case. A clear resets the counter, so a flapping brake never accumulates to a failure. `1` fails on first observation. A GPU with no usable sample is skipped and keeps its counter, so a gap in DCGM data neither raises nor clears a finding. This includes DCGM's int64 "no data" sentinels, whose low byte has the brake bit set and which would otherwise read as an assertion.

## Unresponsive DCGM Detection

A DCGM call that stops answering never returns an error — callers park and the probe blocks forever rather than raising `DCGMError_Timeout`. Meanwhile the node can still report `Ready` with every GPU allocatable and no taint, so no other signal in the stack registers a fault. In `embedded-mode` that hang is node-local, but it is not yet proof of a kernel-driver wedge: DCGM userspace deadlock or lock contention can look the same until an independent NVML/`nvidia-smi` probe confirms the driver itself.

The poll loop cannot report this itself: it is blocked before the point where it would publish anything, and `/healthz` only observes that the loop is frozen, so kubelet restarts the container and the replacement blocks in the same place. The settings below close that gap.

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

Ships the check in dry-run. While `true` (the default) `GpuDcgmUnresponsive` is emitted with `processingStrategy=STORE_ONLY`, so it is persisted and exported as metrics but excluded from the remediation pipeline — no node condition, no cordon, no reboot. The event still carries `RESTART_BM` so the record shows what the node needs.

Watch `dcgm_probe_hangs` and the stored events for a release or two, confirm the detections match real on-node hangs on your fleet, then set `probeStoreOnly: false` to let remediation act on them. Both the unhealthy and the clearing event use the same strategy, so fault-quarantine always sees a consistent pair.

### probeDeadlineSeconds

Seconds a single DCGM probe may run before a watchdog thread — which the blocked probe cannot stop — reports the stalled operation. In `embedded-mode` the call is in-process and node-local, so it publishes `GpuDcgmUnresponsive` with error code `DCGM_PROBE_HANG` and recommended action `RESTART_BM`. In `operator-service` and `external-hostengine` modes, the same symptom can come from the endpoint, DNS, or network; those modes publish `GpuDcgmConnectivityFailure` with `CONTACT_SUPPORT` instead. Defaults to `PollIntervalSeconds * 3` when unset. Set to `0` to disable the watchdog.

The default equals the `/healthz` staleness window (`PollIntervalSeconds * 3`), so the monitor reports when the poll loop is officially considered stalled. Critical event delivery is capped at 15 seconds, leaving the liveness probe's remaining failure budget to persist the finding before kubelet restarts the container.

DCGM exposes timeout errors but does not document a fixed timeout for every RPC. Treat any deadline you configure as a fleet-specific value, not proof that every slower operation is a hard hang. The chart templates `PollIntervalSeconds` from `dcgm.pollIntervalSeconds` and, when `probeDeadlineSeconds` is null/omitted, sets the deadline to `pollIntervalSeconds * 3` so the two stay coupled. Leave `probeStoreOnly` enabled while measuring normal embedded-mode probe latencies. If you substantially raise `probeDeadlineSeconds`, verify the resulting deadline still precedes the configured liveness restart; the chart exposes `livenessProbe.periodSeconds` and `livenessProbe.failureThreshold` for that adjustment.

The event reports once per hang episode. After delivery, a marker under the monitor's persistent `/var/run/nvsentinel` state survives liveness restarts; it prevents the same hang from being republished and lets the first successful probe emit the healthy clearing event. Every DCGM call in the poll loop is tracked, including connect, health check, thermal margin evaluation, and the cleanup that follows a connectivity failure. Cleanup during intentional shutdown is not tracked, so a slow teardown while DCGM is restarting cannot be reported as a hang. `dcgm_probe_hangs` increments when the deadline is crossed even if event delivery must be retried.

### connectivityFailureEscalationThreshold

Number of consecutive `GpuDcgmConnectivityFailure` cycles after which the recommended action escalates from `CONTACT_SUPPORT` to `RESTART_BM`.

Enable this only when the configured DCGM endpoint is node-local and repeated unreachability has been validated as a driver wedge. With a shared service, service failure, DNS issue, or network policy, rebooting the node is not a valid remediation.

Defaults to `0`, which disables escalation and keeps every connectivity failure at `CONTACT_SUPPORT`. The counter resets once connectivity is restored, and the escalated event is published once rather than on every subsequent cycle.

> **Note**: Both settings recommend `RESTART_BM`, which fault-remediation maps to a `RebootNode` CR. A reboot is the practical recovery when an on-node DCGM probe will not return — whether the underlying cause is a wedged driver or DCGM userspace holding driver locks. Nodes are drained before the reboot by node-drainer. Note that `probeStoreOnly` gates this for `GpuDcgmUnresponsive`, while `connectivityFailureEscalationThreshold` is opt-in by being `0` by default.

### minConsecutivePolls

Number of consecutive polls a health check incident must persist, per error code and GPU, before it is published. Codes absent from the map publish on their first observation, which is the default behaviour.

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    minConsecutivePolls:
      DCGM_FR_NVLINK_DOWN: 2
```

Use this for a code whose single-poll occurrences are transients rather than faults. `DCGM_FR_NVLINK_DOWN` is the motivating case: on SXM systems the NVLink links are briefly down after every node boot while they train, so a routine maintenance reboot otherwise produces a FATAL event carrying `RESTART_VM`. `_is_nvlink_down_false_positive` cannot cover this, because it is metadata-based and deliberately fails closed on SXM where an untrained link is indistinguishable from a dead one. A time-based threshold separates them: an untrained link trains, a dead one does not.

The streak is keyed on `(error code, GPU)` and resets whenever the code is absent for that GPU in a poll, so a code that appears on alternate polls for a GPU never accumulates and is never published. Note the key is the code and GPU, not the individual link: a GPU reporting one down link on one poll and a different one on the next keeps its streak, because that GPU has had a link down continuously. A streak also survives a failed poll, since a DCGM timeout observes nothing and must not clear it.

**Tradeoff**: a threshold of `N` delays a genuinely sustained fault by up to `N-1` poll intervals. At the default `pollIntervalSeconds: 15`, a threshold of `2` costs at most 15 seconds of detection latency. Prefer the smallest threshold that suppresses the transient; permanently suppressing the code via `suppressedErrorCodes` would also mask genuine failures, which this avoids.

Withheld incidents are counted by `dcgm_health_check_debounced_incidents{error_code, gpu_id}`, so suppression stays observable per GPU. It increments once per poll per `(error code, GPU)`, not once per incident record.

## Additional Volumes

Extension point for mounting additional host paths required by DCGM in specific environments.

### Configuration Structure

```yaml
gpu-health-monitor:
  additionalVolumeMounts: []
  additionalHostVolumes: []
```

### Parameters

#### additionalVolumeMounts
List of volume mounts to add to the GPU Health Monitor container. Each mount specifies where a volume should be mounted inside the container.

#### additionalHostVolumes
List of host path volumes to make available to the pod. Each volume references a path on the host node.

### When to Use Additional Volumes

Additional volumes are required in environments where DCGM needs access to GPU drivers or libraries installed in non-standard host locations.

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
