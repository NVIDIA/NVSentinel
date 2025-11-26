# Labeler Configuration

## Overview

The Labeler module automatically applies labels to Kubernetes nodes based on GPU runtime components. It watches DCGM and driver pods deployed by GPU Operator and detects Kata Containers runtime. This document covers all Helm configuration options for system administrators.

## Labels Applied

The labeler automatically manages these node labels:

| Label | Values | Purpose |
|-------|--------|---------|
| `nvsentinel.dgxc.nvidia.com/dcgm.version` | `3.x`, `4.x` | DCGM major version detected from DCGM pods |
| `nvsentinel.dgxc.nvidia.com/driver.installed` | `true`, `false` | NVIDIA driver pod status on node |
| `nvsentinel.dgxc.nvidia.com/kata.enabled` | `true`, `false` | Kata Containers runtime presence |

## Configuration Reference

### Module Enable/Disable

Controls whether the labeler module is deployed in the cluster.

```yaml
global:
  labeler:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the labeler pod.

```yaml
labeler:
  resources:
    requests:
      cpu: 100m
      memory: 128Mi
    limits:
      cpu: 500m
      memory: 256Mi
```

### Logging

Sets the verbosity level for labeler logs.

```yaml
labeler:
  logLevel: info  # Options: debug, info, warn, error
```

## Kata Containers Detection

Configures detection of Kata Containers runtime on nodes.

```yaml
labeler:
  kataLabelOverride: ""
```

### Parameters

#### kataLabelOverride

Optional custom node label to check for Kata Containers detection, in addition to the default label.

**Default Label:** `katacontainers.io/kata-runtime`

When empty, only the default label is checked. When set, both default and custom labels are checked.

### Truthy Values

The following label values (case-insensitive) are considered truthy for Kata detection:
- `"true"`
- `"enabled"`
- `"1"`
- `"yes"`

Any other value or missing label results in `kata.enabled=false`.

### Kata Detection Examples

#### Example 1: Default Detection

```yaml
labeler:
  kataLabelOverride: ""
```

Checks only `katacontainers.io/kata-runtime` label on nodes.

#### Example 2: Custom Kata Label

```yaml
labeler:
  kataLabelOverride: "io.katacontainers.config.runtime.oci_runtime"
```

Checks both `katacontainers.io/kata-runtime` and `io.katacontainers.config.runtime.oci_runtime`. Kata is enabled if either label has a truthy value.

## GPU Operator Integration

The labeler watches for specific pod labels to detect DCGM and driver status.

### Expected Pod Labels

**DCGM Pods:**
```yaml
metadata:
  labels:
    app: nvidia-dcgm
```

**Driver Pods:**
```yaml
metadata:
  labels:
    app: nvidia-driver-daemonset
```

If your GPU Operator configures its operands with different labels, the labeler will not detect the components.
