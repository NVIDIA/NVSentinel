# GPU Health Monitor Configuration

## Overview

The GPU Health Monitor module watches GPU health using NVIDIA DCGM (Data Center GPU Manager) and reports hardware failures. This document covers all Helm configuration options for system administrators.

## DCGM Deployment Modes

DCGM (Data Center GPU Manager) always runs as a DaemonSet with one pod per GPU node. The GPU Health Monitor can connect to DCGM in two modes:

### DCGM with Kubernetes Service

DCGM DaemonSet exposes a Kubernetes service. GPU Health Monitor pods connect to DCGM on their local node via this service endpoint.

**Characteristics:**
- DCGM runs as a DaemonSet (one pod per GPU node)
- Kubernetes service provides DNS endpoint for DCGM
- GPU Health Monitor connects via service DNS name

### DCGM with Host Networking

DCGM DaemonSet uses host networking. GPU Health Monitor pods connect to DCGM via `localhost:5555` on the host network.

**Characteristics:**
- DCGM runs as a DaemonSet with `hostNetwork: true`
- No Kubernetes service needed
- GPU Health Monitor connects to `localhost:5555`

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

### DCGM Service Mode

Configuration for connecting to DCGM running as a Kubernetes service.

```yaml
gpu-health-monitor:
  dcgm:
    dcgmK8sServiceEnabled: true
    service:
      endpoint: "nvidia-dcgm.gpu-operator.svc"
      port: 5555
```

#### Parameters

##### dcgmK8sServiceEnabled
Enables connection to DCGM via Kubernetes service. When `true`, uses `service.endpoint` and `service.port`. When `false`, connects to `localhost:5555` (sidecar mode).

##### service.endpoint
Kubernetes service DNS name for DCGM. Typically the DCGM service deployed by GPU Operator.

##### service.port
Port where DCGM is listening. Default is `5555`.

#### DCGM Service Examples

##### Example 1: GPU Operator DCGM Service

```yaml
dcgm:
  dcgmK8sServiceEnabled: true
  service:
    endpoint: "nvidia-dcgm.gpu-operator.svc"
    port: 5555
```

##### Example 2: Custom Namespace DCGM Service

```yaml
dcgm:
  dcgmK8sServiceEnabled: true
  service:
    endpoint: "dcgm-service.custom-namespace.svc.cluster.local"
    port: 5555
```

### Host Networking

Enables host network mode for GPU Health Monitor pods.

```yaml
gpu-health-monitor:
  useHostNetworking: false
```

Set to `true` when DCGM is deployed with host networking (`dcgm.dcgmK8sServiceEnabled: false`). In this mode, GPU Health Monitor connects to DCGM via `localhost:5555` on the host network.

#### Example: Host Networking Mode for connecting to DCGM

```yaml
dcgm:
  dcgmK8sServiceEnabled: false

useHostNetworking: true
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
