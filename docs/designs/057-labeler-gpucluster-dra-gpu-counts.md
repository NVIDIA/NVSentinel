# ADR-057: Labeler — GPU Count Labels in GPU Operator GPUCluster (DRA) Mode

## Context

GPU Operator 26.7 introduces the `GPUCluster` custom resource as an
alternative to `ClusterPolicy`. In GPUCluster mode the operator manages the
NVIDIA DRA driver instead of the device plugin, and does **not** deploy GFD.
The GPU inventory is instead published per node as `resource.k8s.io/v1`
`ResourceSlice` objects from driver `gpu.nvidia.com`, one device entry per GPU
(attribute `type` is `gpu`, or `mig` for MIG slices). On such nodes the
shipped `gpu` class fails every reconcile: the count labels are never written,
or freeze at their last value, and GPU-loss detection in KOM goes blind.

The two operator modes are mutually exclusive per cluster. NVSentinel
components are expected to detect the mode in effect and operate correctly in
either one from the same default configuration, with no mode-specific values
or per-cluster changes. Three facts constrain the design:

1. A class whose expression references `resourceSlices` is skipped as
   *missing source* when the node has no slices at all
   (`device_counts.go`). This is what prevents a DRA class from writing
   `current=0` on ClusterPolicy nodes. It is not sufficient: a ClusterPolicy
   GPU node that also runs an unrelated DRA driver (for example the AWS RoCE
   NIC driver, the documented `nic` example) *does* have slices, so a GPU DRA
   class would evaluate to `0` there and overwrite the GFD-derived count.
2. Any ResourceSlice-referencing class makes `labeler` start a
   `resource.k8s.io/v1` informer and wait for it to sync before writing any
   label. That API is only served from Kubernetes 1.34. GPU Operator 26.7 in
   ClusterPolicy mode still supports Kubernetes 1.33, so an unconditional
   informer would block every label on those clusters.

## Decision

Ship two enabled GPU classes that write the same label pair — the
existing GFD-label class `gpu` and a new DRA class `gpu-dra` — and add three
small mechanisms to `labeler` so that exactly one of them succeeds on any
node and only counts a complete inventory:

- a per-class `resourceSliceDriver` setting that restricts the slices a class
  sees to one DRA driver and treats a node with no slices from that driver as
  *missing source*;
- normalisation of the node's ResourceSlices to the current, complete pool
  generation before evaluation, so a driver republish or a multi-slice pool is
  never counted twice or short;
- API discovery before the `resource.k8s.io/v1` informer is created, so on API
  servers that do not serve `resourceslices` the labeler logs a warning and
  runs with the DRA class permanently skipped instead of never syncing.

## Implementation

All changes live in the `labeler` module and its Helm chart; the code is the
reference, this section only records the shape.

- `labeler/pkg/devicecounts`: `ClassConfig.ResourceSliceDriver` (TOML
  `resourceSliceDriver`) filters a node's slices by `spec.driver` before the
  missing-source check and CEL evaluation, for the target node and for peers
  used in expected-count learning. `completePoolSlices` keeps only the highest
  `spec.pool.generation` per driver/pool and reports the pool incomplete until
  `spec.pool.resourceSliceCount` slices of that generation are visible; an
  incomplete pool is skipped as a missing source. Validation rejects a driver
  on an expression that does not reference `resourceSlices`.
- `labeler/pkg/labeler`: the `resource.k8s.io/v1` informer is created only if
  discovery shows the API server serves `resourceslices`; `NotFound` logs a
  warning and leaves DRA classes skipped, any other discovery error fails
  start-up.
- Chart: second default class `gpu-dra` with `resourceSliceDriver:
  gpu.nvidia.com`, counting `type == 'gpu'` devices and grouping by
  `node.kubernetes.io/instance-type` only; the existing RBAC helper renders the
  `resourceslices` rule because the expression references `resourceSlices`.

Per-node outcome with the defaults:

| Node | `gpu` (GFD label) | `gpu-dra` (ResourceSlice) |
|---|---|---|
| ClusterPolicy GPU node | writes labels | skipped: no `gpu.nvidia.com` slices |
| ClusterPolicy GPU node + foreign DRA driver | writes labels | skipped: slices filtered to none |
| GPUCluster GPU node | skipped: no `nvidia.com/gpu.count` label | writes labels |
| GPUCluster GPU node, DRA pool mid-publication | skipped | skipped: pool incomplete, labels keep last value |
| Kubernetes < 1.34 | writes labels | skipped: no informer |
| CPU node | skipped | skipped |

Classes sharing a label pair are evaluated in configuration order and the
last to succeed wins; there is no precedence rule. Sufficient while at most
one can succeed per node, which the driver filter and the GFD-label dependency
guarantee for the shipped pair; marked in code as a deliberate ceiling.
Expected-count learning is per class, so each learns only from peers with its
own source, and both read the shared `gpu.count.expected` label as a floor.

## Rationale

- One chart, one configuration, both operator modes; no mode flag to keep in
  sync with the GPU Operator installation.
- Skipping, never `0`: a node whose source is absent keeps its last labels,
  which is the invariant ADR-043 established for missing sources. The driver
  filter closes the gap where "some slices exist" was mistaken for "the GPU
  source exists".

## Consequences

### Positive

- `gpu.count.*` labels and downstream GPU-loss detection work on GPUCluster
  nodes with no per-cluster values.
- `resourceSliceDriver` is reusable for any DRA class (NIC DRA, future
  drivers) that must coexist with non-DRA nodes.

### Negative

- `gpu-dra` cannot group by `nvidia.com/gpu.product`. That label is written
  by GFD, which GPUCluster mode does not deploy, and grouping labels must be
  node labels because partitions are learned across peer *nodes*; the
  `productName` attribute inside the ResourceSlice is not usable for this.
  Mixed GPU models or GPU counts within one instance type therefore share a
  learned expected count in GPUCluster mode, and the smaller nodes would be
  reported as missing GPUs. Nodes lacking `node.kubernetes.io/instance-type`
  (as on the canary) collapse into a single partition.
- The DRA slice reflects driver start-up enumeration, so a GPU lost at runtime
  is not visible through this class until the DRA plugin restarts.

### Mitigations

- Where model mixing within an instance type matters: guarantee an
  instance-type label that implies one GPU configuration, or add a node label
  for the GPU model (set by the node provisioning process or an NFD rule on
  the PCI device ID) to `gpu-dra`'s `groupingLabels`, or pin the count with
  `expectedCountOverrides`. The configuration reference documents these.
- DCGM-based health checks (GPU Health Monitor) remain the detection path for
  GPUs that drop off the driver at runtime; the count labels catch inventory
  that is wrong from node start.

## Alternatives Considered

### Replace the `gpu` class with the DRA expression

**Rejected** because: it breaks every existing ClusterPolicy installation and
requires operators to switch the labeler configuration in lock-step with the
GPU Operator mode on each cluster.

### One class whose CEL expression branches on the mode

**Rejected** because: CEL has no way to *skip*; a branch must return an
integer, so a node without either source would receive `0`, violating the
missing-source invariant. Encoding "skip" as a deliberate runtime error is
opaque and surfaces as `evaluation_error` rather than `missing_source`.

### Detect the operator mode centrally and enable one class

Detection itself is feasible from the mandatory operand labels
(`nvidia.com/gpu.deploy.device-plugin` for ClusterPolicy,
`nvidia.com/gpu.deploy.dra-driver` for GPUCluster; DCGM-based signals are
unreliable because DCGM is optional). The problem is how the mode would drive
CEL evaluation. Branching inside one expression is rejected above. The only
other shape is a per-class `matchLabels` gate checked in Go before
evaluation, which is the two classes shipped here plus a second gate: the
deploy label is set before the inventory is published, so the missing-source
check must stay, and the gate adds a condition that must agree with it while
removing nothing but skip noise from the inactive class.

**Rejected** because: each class already gates on its own source, the GFD
label or a `gpu.nvidia.com` ResourceSlice, which is closer to the data than
operator intent.

## References

- [ADR-043: Labeler — Expected Device Count Labels](043-expected-device-count-labels.md)
- NVIDIA/NVSentinel#1860 — GPUCluster (DRA) mode support in labeler
- GPU Operator 26.7 GPUCluster / DRA: <https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/26.7/dra-intro-install.html>
- Kubernetes DRA `ResourceSlice` (`resource.k8s.io/v1`, GA in 1.34): <https://kubernetes.io/docs/concepts/scheduling-eviction/dynamic-resource-allocation/>
