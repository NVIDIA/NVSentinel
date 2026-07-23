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

#### suppressedErrorCodes
List of DCGM error code names (as reported by DCGM, e.g. `DCGM_FR_CLOCK_THROTTLE_POWER`) to suppress. Empty by default (no suppression). Suppression is scoped to the listed error codes only — other incidents on the same health watch (e.g. other `GpuPowerWatch` error codes) are still reported.

#### Example: Suppress power-cap throttle flaps

```yaml
gpu-health-monitor:
  dcgmHealthCheck:
    suppressedErrorCodes:
      - DCGM_FR_CLOCK_THROTTLE_POWER
```

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
