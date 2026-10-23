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
| `nvsentinel.dgxc.nvidia.com/gpu.count.current` | non-negative integer | Current GPU count from the configured class expression |
| `nvsentinel.dgxc.nvidia.com/gpu.count.expected` | non-negative integer | Expected GPU count from override or learned hardware-class baseline |
| `nvsentinel.dgxc.nvidia.com/nic.count.current` | non-negative integer | Current NIC count from the configured class expression |
| `nvsentinel.dgxc.nvidia.com/nic.count.expected` | non-negative integer | Expected NIC count from override or learned hardware-class baseline |

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

### Kubernetes API Rate Limits

The labeler inherits the Kubernetes client limits from `global.qps` and `global.burst` (defaults: `5` and `10`). Set component values only when the labeler needs different limits:

```yaml
labeler:
  qps: 40
  burst: 80
```

Positive `qps` values enable client-side throttling, `0` uses the client-go default, and a negative value disables client-side throttling. `burst` must be non-negative; `0` uses the client-go default.

## Pre-Installed Drivers

Assumes NVIDIA drivers are installed directly on the host rather than via GPU Operator driver containers. When enabled, the labeler sets `nvsentinel.dgxc.nvidia.com/driver.installed=true` on all GPU nodes it manages (`nvidia.com/gpu.present=true`; nodes opted out with `nvsentinel.dgxc.nvidia.com/managed=false` — for example during external remediation — are excluded), skipping driver pod detection.

```yaml
labeler:
  assumeDriverInstalled: false
```

## DCGM Pod Discovery

Comma-separated values of the `app` pod label used to find the DCGM pod on each node. The default matches both the classic GPU Operator DCGM DaemonSet and the GPUCluster (DRA) mode one, so the correct pod is found in either mode without per-cluster configuration.

```yaml
labeler:
  dcgmAppLabel: "nvidia-dcgm,nvidia-dcgm-dra"
```

## Driver Pod Discovery

Pod label values used to find the driver pod on each node: the `app` label of the GPU Operator driver pod, and the `k8s-app` label of the GKE driver installer pod. The defaults match GPU Operator and GKE.

```yaml
labeler:
  driverAppLabel: "nvidia-driver-daemonset"
  gkeInstallerAppLabel: "nvidia-driver-installer"
```

## DCGM Bootstrap Gating

Controls whether the DCGM pod must be ready before the DCGM version label is set for the first time on a node.

```yaml
labeler:
  requireDCGMReadyForBootstrap: true
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

## Expected Device Counts

Expected device-count labeling is enabled by default. The labeler evaluates enabled classes and writes current/expected count labels only when the configured CEL expression returns a valid non-negative integer.

The Helm chart renders this values block into a TOML ConfigMap entry and mounts it into the labeler pod. Because expressions are compiled at startup, Helm also annotates the pod template with a checksum so changes to the ConfigMap roll the Deployment.

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

- `node`: the cached projection of the Kubernetes Node being reconciled.
- `resourceSlices`: ResourceSlice objects associated with the node. In a source with `resourceSliceDriver`, this list holds only the slices of that driver.
- `sum(list<int>)`: helper that returns the sum of a list of integers.
- The cel-go list extensions, for example `flatten()`, which turns a list of lists into one list.

#### Node fields available to expressions

To limit informer memory use, the Labeler does not cache complete Node objects.
The following fields are always retained:

- `metadata.name`, `metadata.uid`, and `metadata.resourceVersion`
- all `metadata.labels`
- the `nvsentinel.dgxc.nvidia.com/dcgm-bootstrap-completed` annotation, when present

When expected device counts have at least one enabled class, the Labeler also
retains `status.allocatable` and `status.capacity`. Device-count expressions
that read Node data must use `node.metadata.labels`,
`node.status.allocatable`, or `node.status.capacity`.

All other Node fields are discarded before caching, including `spec`, other
annotations, `status.conditions`, addresses, images, and node information.
When expected device counts are disabled, all of `status` is discarded.
Expressions that reference discarded fields are unsupported and receive only
the field's empty or absent value.

For classes without a matching override, the expected value is learned as the maximum current or existing expected count among nodes with the same configured grouping-label values. Learned expected counts can rise automatically, but do not fall automatically when a node reports fewer devices.

### Device-Count Sources

A class can define an ordered list of `sources` instead of a class-level `currentExpression` and `groupingLabels`. Each source can read a different kind of data. Thus one class can count the same devices in different cluster setups. The default GPU class uses sources so that it works in both GPU Operator modes:

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
            currentExpression: |
              int(node.metadata.labels['nvidia.com/gpu.count'])
```

| Field | Required | Description |
|---|---|---|
| `name` | Yes | Unique name of the source in the class. It appears in logs, in the `source` label of `labeler_device_count_source` and in the partition key. |
| `resourceSliceDriver` | No | The DRA driver of the source. When set, the source applies only to nodes with ResourceSlices from this driver. |
| `groupingLabels` | No | Node labels that put nodes into the same partition when they use this source. |
| `currentExpression` | Yes | CEL expression that returns the current count. |
| `groupingExpression` | No | CEL expression that returns a `list<string>`, for example the GPU models on the node. The labeler adds the values to the partition key. |

For each node, the labeler examines the sources in order and uses the first source that applies:

- A source with `resourceSliceDriver` applies when the node has at least one ResourceSlice from that driver. The labeler uses only the slices of the newest generation of each pool.
  - If a pool is incomplete, the labeler skips the class until the next ResourceSlice update. It does not try the next source.
  - If the expression fails, the labeler skips the class. It does not try the next source, so a stale node label cannot replace the DRA count.
- A source without `resourceSliceDriver` applies when its `currentExpression` returns a count. If the expression fails, for example because a label is missing, the labeler tries the next source.
- If no source applies, the labeler skips the class and keeps the existing labels. It never writes a current count of `0` for a missing source.

Put DRA sources before label sources. A ResourceSlice exists only while its driver runs on the node, but a node label can stay on the node after the component that set it is gone.

All sources of a class write the same labels, and the expected count does not fall automatically. Thus all sources of a class must count the same unit. For example, both GPU sources count physical GPUs.

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
- Classes without `sources` work as before. A hand-written GPU DRA class in that form can report `0` on a node with only another driver's slices, so move it to `sources`.

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
