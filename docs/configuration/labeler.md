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
- the [relearn annotation](#relearning-expected-counts) of each class that sets `relearnID`, when present
- the [partition annotation](#expected-counts-stay-in-their-partition) of each enabled class, when present

When expected device counts have at least one enabled class, the Labeler also
retains `status.allocatable` and `status.capacity`. Device-count expressions
that read Node data must use `node.metadata.labels`,
`node.status.allocatable`, or `node.status.capacity`.

All other Node fields are discarded before caching, including `spec`, other
annotations, `status.conditions`, addresses, images, and node information.
When expected device counts are disabled, all of `status` is discarded.
Expressions that reference discarded fields are unsupported and receive only
the field's empty or absent value.

For classes without a matching override, the expected value is learned as the maximum current or existing expected count among nodes with the same configured grouping-label values. Learned expected counts can rise automatically, but do not fall automatically when a node reports fewer devices. To lower them after a deliberate change, see [Relearning Expected Counts](#relearning-expected-counts).

### Expected Counts Stay in Their Partition

An expected label counts only in the partition where the labeler learned it. With every expected label it writes, the labeler writes the annotation `nvsentinel.dgxc.nvidia.com/<class>.count.partition` with the partition key of the node, for example `source=gfd|node.kubernetes.io/instance-type=gb200|nvidia.com/gpu.product=NVIDIA-GB200`, in the same patch. When the grouping of a node changes, for example when MIG is turned off and `nvidia.com/gpu.product` loses its MIG suffix, or when the node gets another instance type, its old expected label no longer counts, neither for the node nor for the other nodes of its new partition. The node learns its expected count again from its own current count and from the other nodes of its new partition, and the labeler logs that at Info level. Each time it computes the expected count of a partition, the labeler counts the labels it leaves out this way in the `labeler_device_count_skipped_updates_total` counter with `reason="partition_changed"`.

- A change between the sources of a class, such as a move between GPU Operator modes, is not a partition change. The node keeps its expected count, as described in [Grouping with sources](#grouping-with-sources).
- Labels written before the annotation existed still count, and the labeler adds the annotation to each node at its next reconcile, which is one extra patch per node. A node that has no count at that reconcile, for example because its device plugin is not running, or that is [not settled](#nodes-whose-devices-are-being-reconfigured), keeps its old label without the annotation, and that label counts in any partition. If the node changes partition before the labeler counts it again, its old count reaches the new partition. Upgrade the labeler outside MIG changes, and check afterwards that every node with an expected label carries the annotation:

  ```bash
  kubectl get nodes -l nvsentinel.dgxc.nvidia.com/gpu.count.expected -o custom-columns='NAME:.metadata.name,PARTITION:.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/gpu\.count\.partition'
  ```

- A change to the grouping itself changes the partition key of every node: new `groupingLabels` or their order, a renamed source, or a product name that a new GFD or driver release reports differently. Every node then learns its expected count once more from current counts, as in a relearn. A node that is missing devices still gets the higher count from the other nodes of its partition, but a device that every node of a partition lost at that same moment is accepted.
- The class name must form a valid annotation key, at most 47 characters of letters, digits, `-`, `_` and `.`, starting with a letter or digit, and two enabled classes cannot have the same name. The labeler does not start otherwise.
- Do not change the annotation by hand. A label whose annotation names another partition stops counting, and the node learns its count again at its next reconcile. A label without the annotation counts in any partition until the labeler writes the annotation again.

### Relearning Expected Counts

A learned expected count never goes down on its own, so that a node or a whole partition that loses devices keeps being reported ([ADR-043](../designs/043-expected-device-count-labels.md)). After a change that lowers the count on purpose, the labeler can relearn a class once from the current counts of its nodes ([ADR-070](../designs/070-expected-device-count-relearn.md)). For example, two nodes switched to MIG can report more GPUs while they still carry the plain product label, which raises the expected count of every node in the plain partition. The [partition annotation](#expected-counts-stay-in-their-partition) keeps a count from moving to another partition, but it does not repair a count that is already wrong, such as one raised before an upgrade to a labeler that writes the annotation.

To relearn a class, set `relearnID` on it to a new value, such as a date and a reason:

```yaml
labeler:
  expectedDeviceCounts:
    enabled: true
    classes:
      - name: gpu
        enabled: true
        relearnID: "2026-11-05-mig"
        # labels, expectedCountOverrides and sources as in the chart values
```

Helm replaces lists in values, so give the class list in full, as for `expectedCountOverrides`.

The new value changes the configuration checksum, which restarts the labeler. Its startup reconcile sets the expected count of each partition to the highest current count among its nodes. Next to each expected label of the class, in the same patch, the labeler writes the annotation `nvsentinel.dgxc.nvidia.com/<class>.count.relearn` with the ID and the value, for example `2026-11-05-mig/4`. While the ID is set, an expected label counts only when its annotation has the ID and the label's value. Old labels on nodes not reconciled yet therefore cannot bring the old count back, and with the same ID a node that carries the annotation is not relearned again: afterwards its count rises as before, and a lost device is still reported. The labeler logs each node whose expected count goes down. Overrides still take precedence.

- **Before:** a relearn lowers every partition where all nodes report fewer devices than expected, including a partition that has really lost a device on every node. Check the current and expected labels of each partition first, for example with `kubectl get nodes -L nvsentinel.dgxc.nvidia.com/gpu.count.current,nvsentinel.dgxc.nvidia.com/gpu.count.expected,node.kubernetes.io/instance-type,nvidia.com/gpu.product`, and pin with an override any partition that should not go down. Do not start a relearn while drivers or device plugins are being upgraded: a node that has no count when the labeler reaches it is relearned from whatever it reports once its count returns, and a device lost in between is accepted.
- **After:** every node of the class should carry the annotation with the new ID:

  ```bash
  kubectl get nodes -l nvsentinel.dgxc.nvidia.com/gpu.count.expected -o custom-columns='NAME:.metadata.name,RELEARN:.metadata.annotations.nvsentinel\.dgxc\.nvidia\.com/gpu\.count\.relearn'
  ```

  A node without it had no count, because its count source is missing or its expression failed, it was [not settled](#nodes-whose-devices-are-being-reconfigured), or its patch failed. It keeps its labels, and its old label does not count for the other nodes. The labeler relearns it once it has a count again, and retries a failed patch at the node's next reconcile or the next restart. If such a node should not be relearned from the count it comes back with, pin it with an override before then.
- **Keep the ID:** leave `relearnID` set. Removing it makes every old label count again, including one that a node without a count still carries. Use a new value for the next relearn and never reuse an old one: the annotations stay on the nodes after the ID is cleared, so a reused ID treats unchanged nodes as already relearned. Do not remove the annotation by hand either, since without it the node's label stops counting and the node is relearned at its next reconcile, and note that renaming a class with an ID set relearns the whole class, because the annotation key carries the class name.

`relearnID` must be a valid label value: at most 63 characters of letters, digits, `-`, `_` and `.`, starting and ending with a letter or digit. Quote it in YAML, since an unquoted number such as 20261105 is rendered as a float and rejected. The labeler does not start with an invalid value. The default, an empty value, changes nothing.

### Nodes Whose Devices Are Being Reconfigured

While the devices of a node are being reconfigured, its counts and its grouping labels can disagree. A class can set `settledExpression`, a CEL expression that returns `false` while such a change is going on. It sees `node` and `resourceSlices` like `currentExpression`, with every ResourceSlice of the node, also in a class with sources. Point it at whatever marks your nodes as being in the middle of a change. The default GPU class reads the state label of the GPU Operator MIG manager:

```yaml
settledExpression: |
  !('nvidia.com/mig.config.state' in node.metadata.labels) ||
  node.metadata.labels['nvidia.com/mig.config.state'] == 'success'
```

While the expression returns `false` for a node:

- The labeler still writes the current label of the class, so the node shows what it reports.
- It leaves the expected label and both annotations of the class as they are, even when an override matches the node or a new `relearnID` is set. They are written once the node is settled.
- Neither the current count nor the expected label of the node raises the expected count of another node. The `labeler_device_count_skipped_updates_total` counter with `reason="unsettled"` counts the skipped updates and the nodes left out of a partition this way.
- The labeler logs each reconcile that leaves the expected label alone at Info level.

When the expression returns `true` again, the node is reconciled like any other. If its partition changed meanwhile, for example because it now carries the MIG product, it learns its expected count in the new partition.

What the default covers follows from how the MIG manager works. It sets `nvidia.com/mig.config.state=pending`, stops the device plugin and GFD and waits until their pods are gone, changes the MIG layout, sets `rebooting` if it has to reboot the node, starts the device plugin and GFD again, and then sets `success`. If the change fails, it starts them again and then sets `failed`. While a node is `pending` or `rebooting`, the device plugin and GFD of the GPU Operator therefore report nothing about the new layout, which they report only after `success`. The default matters for a node whose MIG change failed, which reports again while its state is `failed` and keeps its expected count until the MIG manager succeeds, and for any count source that the MIG manager does not stop. It does not cover what GFD and the device plugin report after `success`.

If the expression fails, for example because it reads a label that the node does not have, or returns something other than a bool, the labeler leaves the labels of the class on the node as they are, as for a failed `currentExpression`, logs a warning, and uses neither the count nor the expected label of the node for other nodes. The labeler does not start with an expression that does not compile, or that CEL can tell returns something other than a bool.

Change the expression if other tooling reconfigures devices and marks the nodes it works on, or set it to an empty string to treat every node as settled, which is the default for a class without it. [ADR-070](../designs/070-expected-device-count-relearn.md) describes what is left to the relearn.

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
| `resourceSliceDriver` | No | The DRA driver of the source. When set, the source applies only to nodes with ResourceSlices from this driver. |
| `groupingLabels` | No | Node labels that put nodes into the same partition when they use this source. |
| `currentExpression` | Yes | CEL expression that returns the current count. In a source without `resourceSliceDriver`, a negative result means that the source does not apply to the node. |
| `groupingExpression` | No | CEL expression that returns a `list<string>`, for example the GPU models on the node. The labeler adds the values to the partition key. |

For each node, the labeler examines the sources in order and uses the first source that applies:

- A source with `resourceSliceDriver` applies when the node has at least one ResourceSlice from that driver. The labeler uses only the slices of the newest generation of each pool.
  - If a pool is incomplete, the labeler skips the class until the next ResourceSlice update. It does not try the next source.
  - If the expression fails, the labeler skips the class. It does not try the next source, so a stale node label cannot replace the DRA count.
- A source without `resourceSliceDriver` applies when its `currentExpression` returns a count of `0` or more. If the expression returns a negative number, or fails, for example because a label is missing, the labeler tries the next source. Return `-1` to skip a source on purpose. The default `gfd` source returns `-1` when the NVIDIA device plugin advertises no GPUs (`nvidia.com/gpu`, `nvidia.com/gpu.shared` or `nvidia.com/mig-*` in `node.status.allocatable`). GPUCluster mode has no device plugin, so a `nvidia.com/gpu.count` label left from ClusterPolicy mode is not counted on a node whose DRA plugin is down.
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
