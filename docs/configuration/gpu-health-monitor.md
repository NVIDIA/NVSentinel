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
    suppressedErrorCodes: []
```

### suppressedErrorCodes
List of DCGM error code names (as reported by DCGM, e.g. `DCGM_FR_CLOCK_THROTTLE_POWER`) to suppress. Empty by default (no suppression). Suppression is scoped to the listed error codes only — other incidents on the same health watch (e.g. other `GpuPowerWatch` error codes) are still reported.

### Example: Suppress power-cap throttle flaps

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    suppressedErrorCodes:
      - DCGM_FR_CLOCK_THROTTLE_POWER
```

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
