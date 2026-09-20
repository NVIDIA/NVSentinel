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

Expected device-count labeling is disabled by default. When enabled, the labeler evaluates enabled classes and writes current/expected count labels only when the configured CEL expression returns a valid non-negative integer.

The Helm chart renders this values block into a TOML ConfigMap entry and mounts it into the labeler pod. Because expressions are compiled at startup, Helm also annotates the pod template with a checksum so changes to the ConfigMap roll the Deployment.

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
        groupingLabels:
          - node.kubernetes.io/instance-type
          - nvidia.com/gpu.product
        expectedCountOverrides:
          - matchLabels:
              nvidia.com/gpu.product: NVIDIA-GB200
            count: 8
        currentExpression: |
          int(node.metadata.labels['nvidia.com/gpu.count'])
```

The CEL context exposes:

- `node`: the cached projection of the Kubernetes Node being reconciled.
- `resourceSlices`: ResourceSlice objects associated with the node. When the class sets `resourceSliceDriver`, only slices whose `spec.driver` matches are included. Within each driver/pool only the slices of the highest `spec.pool.generation` are exposed, and the class is skipped as a missing source until that generation has published all `spec.pool.resourceSliceCount` slices, so a driver rollout or a multi-slice pool is never counted twice or short.
- `sum(list<int>)`: helper that returns the sum of a list of integers.

### Class fields

| Field | Required | Description |
|---|---|---|
| `name` | yes | Class name, used in metrics and logs |
| `enabled` | yes | Whether the class is evaluated |
| `labels.current`, `labels.expected` | yes | Node labels written by the class. Several classes may share one label pair when at most one of them can succeed on any node |
| `groupingLabels` | no | Node labels whose values define the hardware partition for expected-count learning |
| `expectedCountOverrides` | no | Pin the expected count for nodes matching `matchLabels` |
| `currentExpression` | yes | CEL expression returning the node's current device count as an integer |
| `resourceSliceDriver` | no | DRA driver name (`ResourceSlice.spec.driver`). Restricts `resourceSlices` to that driver, so a node with no slices from that driver is skipped as a missing source instead of receiving a `0` count. The expression must reference `resourceSlices` |

### ResourceSlice-backed classes and mode-specific sources

A class whose expression references `resourceSlices` is skipped on nodes with no matching ResourceSlices, so a missing DRA source never turns into `current=0`. This makes it possible to ship one class per inventory source writing the same label pair. The chart default does this for GPUs:

- `gpu` reads the GPU Feature Discovery label `nvidia.com/gpu.count` (GPU Operator ClusterPolicy mode).
- `gpu-dra` counts `type == 'gpu'` devices in the node's `gpu.nvidia.com` ResourceSlices (GPU Operator GPUCluster / DRA mode, where GFD is not deployed). `resourceSliceDriver: gpu.nvidia.com` keeps it skipped on ClusterPolicy nodes even when another DRA driver publishes slices for the node.

Exactly one of the two succeeds per GPU node; the other is skipped and recorded under `labeler_device_count_skipped_updates_total{class=...}` with reason `missing_source`. The same skip is recorded transiently while a DRA driver is still publishing a new pool generation. Expected counts are learned per class from peers that share the same source.

### Limitation: no `nvidia.com/gpu.product` grouping in GPUCluster mode

`groupingLabels` must be **node labels**, because the expected count is learned per partition across peer nodes and the partition key is built from the node object alone. In ClusterPolicy mode the default `gpu` class groups by `node.kubernetes.io/instance-type` and `nvidia.com/gpu.product`, so two GPU models in the same instance type learn separate expected counts. `nvidia.com/gpu.product` is written by GPU Feature Discovery, which GPUCluster mode does not deploy, so the `gpu-dra` class groups by `node.kubernetes.io/instance-type` only. The GPU model is present in the ResourceSlice as the `productName` device attribute, but a `ResourceSlice` attribute cannot be used as a grouping label.

On GPUCluster clusters this means all GPU nodes with the same instance-type value share one learned expected count, and nodes without `node.kubernetes.io/instance-type` all fall into a single partition (rendered as `instance-type=` in `labeler_device_count_expected`). When such a partition mixes nodes with different GPU counts (for example 8-GPU and 4-GPU nodes), the expected count ratchets to the maximum and the smaller nodes are reported as missing GPUs. To avoid this, in order of preference:

1. Ensure every GPU node in the cluster carries `node.kubernetes.io/instance-type` with a value that implies one GPU configuration. Cloud providers set this label by default; on bare-metal or self-managed clusters the node provisioning process must set it.
2. Add a node label that encodes the GPU model (for example `nvidia.com/gpu.product` applied by the provisioning process, or a Node Feature Discovery rule that derives it from the PCI device ID) and list it under `groupingLabels` for `gpu-dra` in the cluster's values.
3. Pin the count with `expectedCountOverrides` on whichever node labels do exist, which bypasses learning for the matched nodes entirely.

ResourceSlices are read through a `resource.k8s.io/v1` informer, which is created only when an enabled class needs it **and** the API server serves that group version (Kubernetes 1.34 or newer). On older clusters the labeler logs a warning at start-up and leaves ResourceSlice-backed classes permanently skipped.

### Node fields available to expressions

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
