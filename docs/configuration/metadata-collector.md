# Metadata Collector Configuration

## Overview

The Metadata Collector module collects GPU metadata using NVIDIA NVML (Management Library) and writes it to a shared file. Other modules read this file to enrich health events with GPU serial numbers, UUIDs, and topology information. This component will also expose the pod-to-GPU mapping as an annotation on each pod requesting GPUs. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the metadata-collector module is deployed in the cluster.

```yaml
global:
  metadataCollector:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the metadata-collector init container.

```yaml
metadata-collector:
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

## Runtime Class

Specifies the container runtime class for GPU device access.

```yaml
metadata-collector:
  runtimeClassName: "nvidia"
```

### Parameters

#### runtimeClassName

Runtime class name that provides GPU device access. Required for NVML to query GPU information when GPU Operator creates a matching `RuntimeClass` (the common non-NRI setup).

**Common values:**
- `nvidia` - NVIDIA container runtime (default)
- `nvidia-container-runtime` - value some GPU Operator installs use
- `nvidia-legacy` - Legacy NVIDIA runtime
- Empty string - Uses the default cluster runtime. Used for CRI-O environments and for NRI-mode clusters (see below)

## Host-path driver access (NRI-mode clusters)

On clusters where GPU Operator is configured for CDI + NRI device injection, a `RuntimeClass` matching `operator.runtimeClass` is often never created. Setting `runtimeClassName` then fails admission, and leaving it unset crash-loops with `NVML: ERROR_LIBRARY_NOT_FOUND`. Requesting `nvidia.com/gpu` works but reserves a GPU for the DaemonSet.

Use the same extra volume pattern as `gpu-health-monitor`: clear `runtimeClassName` and mount the host NVIDIA libraries. Set `LD_LIBRARY_PATH` when the mount path is not already on the dynamic linker search path. The container already runs as root (`runAsUser: 0`). NVLink/NIC topology also shells out to `nvidia-smi`; mount that host binary the same way if those fields are required.

```yaml
metadata-collector:
  runtimeClassName: ""
  extraEnv:
    - name: LD_LIBRARY_PATH
      value: /usr/local/nvidia/lib
  additionalVolumeMounts:
    - name: nvidia-driver-libs
      mountPath: /usr/local/nvidia/lib
      readOnly: true
  additionalHostVolumes:
    - name: nvidia-driver-libs
      hostPath:
        path: /usr/lib/x86_64-linux-gnu   # amd64; use /usr/lib/aarch64-linux-gnu on arm64
        type: Directory
```

`additionalHostVolumes`, `additionalVolumeMounts`, and `extraEnv` default to empty lists. Existing RuntimeClass-based installs are unchanged.
