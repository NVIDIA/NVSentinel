# Labeler Configuration

## Overview

The labeler chart automatically applies labels to Kubernetes nodes. It runs two Deployments:

- `nvidia-labeler` watches the DCGM and driver pods that the GPU Operator deploys, and writes the DCGM version and driver labels.
- `labeler` detects the Kata Containers runtime and writes the device-count labels. It has no access to pods.

This document describes all the Helm configuration options for system administrators.

## Labels Applied

The labeler automatically manages these node labels:

| Label | Values | Written by | Purpose |
|-------|--------|------------|---------|
| `nvsentinel.dgxc.nvidia.com/dcgm.version` | `3.x`, `4.x` | `nvidia-labeler` | DCGM major version detected from DCGM pods |
| `nvsentinel.dgxc.nvidia.com/driver.installed` | `true`, `false` | `nvidia-labeler` | NVIDIA driver pod status on node |
| `nvsentinel.dgxc.nvidia.com/kata.enabled` | `true`, `false` | `labeler` | Kata Containers runtime presence |
| `nvsentinel.dgxc.nvidia.com/gpu.count.current` | non-negative integer | `labeler` | Current GPU count from the configured class expression |
| `nvsentinel.dgxc.nvidia.com/gpu.count.expected` | non-negative integer | `labeler` | Expected GPU count from override or learned hardware-class baseline |
| `nvsentinel.dgxc.nvidia.com/nic.count.current` | non-negative integer | `labeler` | Current NIC count from the configured class expression |
| `nvsentinel.dgxc.nvidia.com/nic.count.expected` | non-negative integer | `labeler` | Expected NIC count from override or learned hardware-class baseline |

`nvidia-labeler` also writes the `nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed` node annotation. See [DCGM Bootstrap Gating](#dcgm-bootstrap-gating).

## Configuration Reference

### Module Enable/Disable

This value controls whether the chart deploys the labeler chart, with both Deployments, in the cluster.

```yaml
global:
  labeler:
    enabled: true
```

This value controls whether the chart deploys `nvidia-labeler`. It also sets its image.

```yaml
labeler:
  nvidiaLabeler:
    enabled: true
    image:
      repository: ghcr.io/nvidia/nvsentinel/nvidia-labeler
      pullPolicy: IfNotPresent
      tag: ""  # Defaults to global.image.tag
```

The GPU, syslog and NIC health monitors and metadata-collector select nodes on the `dcgm.version` and `driver.installed` labels. `nvidia-labeler` removes these labels from a node that is opted out with `nvsentinel.dgxc.nvidia.com/managed=false`, so that those pods leave the node. Thus, the chart refuses `nvidiaLabeler.enabled: false` while one of these components is enabled. The chart cannot see automation outside NVSentinel. Before you turn off `nvidia-labeler`, make sure that no other automation reads these labels: nothing updates them after that.

### Resources

This value defines the CPU and memory resource requests and limits for each of the two pods.

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

This value sets the verbosity level of the logs of both pods.

```yaml
labeler:
  logLevel: info  # Options: debug, info, warn, error
```

### Kubernetes API Rate Limits

Both pods inherit the Kubernetes client limits from `global.qps` and `global.burst` (defaults: `5` and `10`). If they need different limits, set the component values, which apply to both:

```yaml
labeler:
  qps: 40
  burst: 80
```

The `qps` value has these effects:

- A positive value enables client-side throttling.
- `0` uses the client-go default.
- A negative value disables client-side throttling.

`burst` must be non-negative. A value of `0` uses the client-go default.

## Pre-Installed Drivers

This value tells `nvidia-labeler` that the NVIDIA drivers are installed directly on the host, not through GPU Operator driver containers. When this value is enabled, `nvidia-labeler` sets `nvsentinel.dgxc.nvidia.com/driver.installed=true` on all GPU nodes that it manages (`nvidia.com/gpu.present=true`). It excludes nodes that have the opt-out label `nvsentinel.dgxc.nvidia.com/managed=false`. For example, a node has this label during external remediation. It also skips driver pod detection.

```yaml
labeler:
  assumeDriverInstalled: false
```

## DCGM Pod Discovery

This value is a comma-separated list of values of the `app` pod label. `nvidia-labeler` uses these values to find the DCGM pod on each node. The default matches both the classic GPU Operator DCGM DaemonSet and the DaemonSet of GPUCluster (DRA) mode. Thus, `nvidia-labeler` finds the correct pod in both modes without a configuration for each cluster.

```yaml
labeler:
  dcgmAppLabel: "nvidia-dcgm,nvidia-dcgm-dra"
```

## Driver Pod Discovery

These values are the pod label values that `nvidia-labeler` uses to find the driver pod on each node:

- the `app` label of the GPU Operator driver pod
- the `k8s-app` label of the GKE driver installer pod

The defaults match GPU Operator and GKE.

```yaml
labeler:
  driverAppLabel: "nvidia-driver-daemonset"
  gkeInstallerAppLabel: "nvidia-driver-installer"
```

## DCGM Bootstrap Gating

This value controls whether the DCGM pod must be ready before `nvidia-labeler` sets the DCGM version label for the first time on a node.

```yaml
labeler:
  requireDCGMReadyForBootstrap: true
```

## Kata Containers Detection

This value configures the detection of the Kata Containers runtime on nodes.

```yaml
labeler:
  kataLabelOverride: ""
```

### Parameters

#### kataLabelOverride

This value is an optional custom node label that the labeler checks for Kata Containers detection, in addition to the default label.

**Default Label:** `katacontainers.io/kata-runtime`

When this value is empty, the labeler checks only the default label. When this value is set, the labeler checks both the default label and the custom label.

### Truthy Values

The labeler considers these label values (case-insensitive) truthy for Kata detection:

- `"true"`
- `"enabled"`
- `"1"`
- `"yes"`

Any other value, or a missing label, results in `kata.enabled=false`.

## Expected Device Counts

By default, expected device-count labeling is enabled. The labeler evaluates the enabled classes. It writes the current and expected count labels only when the configured CEL expression returns a valid non-negative integer.

The Helm chart renders this values block into a TOML ConfigMap entry and mounts it into the labeler pod. The labeler compiles the expressions at startup. Thus, Helm also annotates the pod template with a checksum, so that changes to the ConfigMap roll the Deployment.

```yaml
labeler:
  expectedDeviceCounts:
    enabled: true
    classes:
      - name: nic
        enabled: true
        labels:
          current: nvsentinel.dgxc.nvidia.com/nic.count.current
          expected: nvsentinel.dgxc.nvidia.com/nic.count.expected
        groupingLabels:
          - node.kubernetes.io/instance-type
        expectedCountOverrides:
          - matchLabels:
              node.kubernetes.io/instance-type: Standard_ND96isr_H100_v5
            count: 8
        currentExpression: |
          int(node.status.allocatable['nvidia.com/mlnxnics'])
```

A class sets `currentExpression` and `groupingLabels` directly, as in this example, or it lists [sources](#device-count-sources). The default NIC class uses the first form. The default GPU class uses sources.

The CEL context exposes:

- `node`: the cached projection of the Kubernetes Node that the labeler reconciles.
- `resourceSlices`: ResourceSlice objects associated with the node. In a source with `resourceSliceDriver`, this list holds only the slices of that driver.
- `sum(list<int>)`: helper that returns the sum of a list of integers.
- The cel-go list extensions, for example `flatten()`, which turns a list of lists into one list.

#### Node fields available to expressions

To limit informer memory use, the labeler does not cache complete Node objects. The labeler always keeps these fields:

- `metadata.name`, `metadata.uid`, and `metadata.resourceVersion`
- all `metadata.labels`
- the `nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed` annotation, when present

When expected device counts have at least one enabled class, the labeler also keeps `status.allocatable` and `status.capacity`. Device-count expressions that read Node data must use `node.metadata.labels`, `node.status.allocatable` or `node.status.capacity`.

The labeler discards all other Node fields before caching. These fields include `spec`, other annotations, `status.conditions`, addresses, images and node information. When expected device counts are disabled, the labeler discards all of `status`. The labeler does not support expressions that reference discarded fields. These expressions receive only the empty or absent value of the field.

For classes without a matching override, the labeler learns the expected value. The expected value is the maximum current or existing expected count among nodes with the same configured grouping-label values. Learned expected counts can rise automatically. They do not fall automatically when a node reports fewer devices.

### Device-Count Sources

A class can define an ordered list of `sources` instead of a class-level `currentExpression` and `groupingLabels`. Each source can read a different kind of data. Thus, one class can count the same devices in different cluster setups. The default GPU class uses sources, so that it works in both GPU Operator modes:

```yaml
labeler:
  expectedDeviceCounts:
    enabled: true
    classes:
      - name: gpu
        enabled: true
        labels:
          current: nvsentinel.dgxc.nvidia.com/gpu.count.current
          expected: nvsentinel.dgxc.nvidia.com/gpu.count.expected
        expectedCountOverrides: []
        sources:
          # GPUCluster (DRA) mode of GPU Operator.
          - name: dra
            resourceSliceDriver: gpu.nvidia.com
            groupingLabels:
              - node.kubernetes.io/instance-type
            currentExpression: |
              sum(resourceSlices
                .filter(rs, has(rs.spec.devices))
                .map(rs, rs.spec.devices
                  .filter(d,
                    has(d.attributes) &&
                    'type' in d.attributes &&
                    has(d.attributes['type'].string) &&
                    d.attributes['type'].string == 'gpu'
                  )
                  .size()
                ))
            groupingExpression: |
              resourceSlices
                .map(rs, has(rs.spec.devices) ? rs.spec.devices : [])
                .flatten()
                .filter(d,
                  has(d.attributes) &&
                  'type' in d.attributes &&
                  has(d.attributes['type'].string) &&
                  d.attributes['type'].string == 'gpu' &&
                  'productName' in d.attributes &&
                  has(d.attributes['productName'].string)
                )
                .map(d, d.attributes['productName'].string)
          # ClusterPolicy mode of GPU Operator, where GFD sets the GPU labels.
          - name: gfd
            groupingLabels:
              - node.kubernetes.io/instance-type
              - nvidia.com/gpu.product
            # Count the GFD label only while the device plugin advertises GPUs.
            # -1 means that the source does not apply.
            currentExpression: |
              has(node.status.allocatable) &&
              node.status.allocatable.exists(r,
                (r == 'nvidia.com/gpu' || r == 'nvidia.com/gpu.shared' || r.startsWith('nvidia.com/mig-')) &&
                int(node.status.allocatable[r]) > 0)
                ? int(node.metadata.labels['nvidia.com/gpu.count'])
                : -1
```

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Unique name of the source in the class. It appears in logs, in the `source` label of `labeler_device_count_source` and in the partition key. |
| `resourceSliceDriver` | No | The DRA driver of the source. When this field is set, the source applies only to nodes with ResourceSlices from this driver. |
| `groupingLabels` | No | Node labels that put nodes into the same partition when they use this source. |
| `currentExpression` | Yes | CEL expression that returns the current count. In a source without `resourceSliceDriver`, a negative result means that the source does not apply to the node. |
| `groupingExpression` | No | CEL expression that returns a `list<string>`, for example the GPU models on the node. The labeler adds the values to the partition key. |

For each node, the labeler examines the sources in order and uses the first source that applies:

- A source with `resourceSliceDriver` applies when the node has at least one ResourceSlice from that driver. The labeler uses only the slices of the newest generation of each pool.
  - If a pool is incomplete, the labeler skips the class until the next ResourceSlice update. It does not try the next source.
  - If the expression fails, the labeler skips the class. It does not try the next source, so a stale node label cannot replace the DRA count.
- A source without `resourceSliceDriver` applies when its `currentExpression` returns a count of `0` or more. If the expression returns a negative number, or fails, for example because a label is missing, the labeler tries the next source. To skip a source on purpose, return `-1`. The default `gfd` source returns `-1` when the NVIDIA device plugin advertises no GPUs (`nvidia.com/gpu`, `nvidia.com/gpu.shared` or `nvidia.com/mig-*` in `node.status.allocatable`). GPUCluster mode has no device plugin. Thus, the labeler does not count a `nvidia.com/gpu.count` label that remains from ClusterPolicy mode on a node whose DRA plugin is down.
- If no source applies, the labeler skips the class and keeps the existing labels. It never writes a current count of `0` for a missing source.

Put DRA sources before label sources. A ResourceSlice exists only while its driver runs on the node. But a node label can stay on the node after the component that set it is gone.

All sources of a class write the same labels, and the expected count does not fall automatically. Thus, all sources of a class must count the same unit. For example, both GPU sources count physical GPUs.

#### Grouping with sources

The partition key of a source contains the source name, the values of its `groupingLabels` and the normalized result of its `groupingExpression`. The labeler removes empty and duplicate values from the `groupingExpression` result, sorts the values and joins them with `+`. Nodes learn from each other only when all three parts are equal.

- Use `groupingExpression` for hardware data that has no node label. For example, GPUCluster mode has no `nvidia.com/gpu.product` label, so the DRA source reads the GPU models from the ResourceSlices.
- Return model names, not counts. A node that loses a GPU then stays in the partition of its hardware and shows a current count lower than the expected count.
- If the `groupingExpression` fails or returns no values, the node learns only from its own current and expected counts. The labeler logs a warning.
- A node that moves between GPU Operator modes changes its source and its partition. It keeps its expected label, so its expected count does not change.

The `labeler_device_count_source{node, class, source}` gauge shows the source that the labeler used for each node.

#### Rules for sources

- A class with `sources` must not set a class-level `currentExpression` or `groupingLabels`. The labeler does not start with such a class.
- `expectedCountOverrides` stay at class level and apply to all sources. Use match labels that exist in every mode, such as `node.kubernetes.io/instance-type` or a nodepool label. Do not use `nvidia.com/gpu.product`, because GPUCluster mode does not set it.
- A class without `sources` uses its class-level `currentExpression` and `groupingLabels`. A hand-written GPU DRA class in that form can report `0` on a node that has only the slices of another driver. Thus, move such a class to `sources`.

### Kata Detection Examples

#### Example 1: Default Detection

```yaml
labeler:
  kataLabelOverride: ""
```

The labeler checks only the `katacontainers.io/kata-runtime` label on nodes.

#### Example 2: Custom Kata Label

```yaml
labeler:
  kataLabelOverride: "io.katacontainers.config.runtime.oci_runtime"
```

The labeler checks both `katacontainers.io/kata-runtime` and `io.katacontainers.config.runtime.oci_runtime`. If either label has a truthy value, Kata is enabled.

## GPU Operator Integration

`nvidia-labeler` watches for specific pod labels to detect DCGM and driver status.

### Expected Pod Labels

**DCGM Pods:**

```yaml
metadata:
  labels:
    app: nvidia-dcgm # or nvidia-dcgm-dra in GPUCluster (DRA) mode
```

**Driver Pods:**

```yaml
metadata:
  labels:
    app: nvidia-driver-daemonset
```

If your GPU Operator configures its operands with different labels, set them in [DCGM Pod Discovery](#dcgm-pod-discovery) and [Driver Pod Discovery](#driver-pod-discovery).
