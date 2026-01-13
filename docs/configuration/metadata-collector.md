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

Runtime class name that provides GPU device access. Required for NVML to query GPU information.

**Common values:**
- `nvidia` - NVIDIA container runtime (default)
- `nvidia-legacy` - Legacy NVIDIA runtime
- Empty string - Uses default cluster runtime. Used for CRIO environments
