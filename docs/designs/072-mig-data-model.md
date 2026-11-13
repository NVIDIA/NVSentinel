# ADR-072: Data Model - A MIG Instance Belongs to One GPU

Status: Proposed (tracking issue [#1914](https://github.com/NVIDIA/NVSentinel/issues/1914), question raised on [#2318](https://github.com/NVIDIA/NVSentinel/pull/2318))

## Context

Issue #1914 reports a GB200 fleet of 287 nodes with four GPUs each. Two nodes were switched to MIG with the `3g.93gb` profile. GPU Feature Discovery (GFD) then set their product to `NVIDIA-GB200-MIG-3g.93gb`, and the labeler counted 8 devices on each. The plain partition learned 8, and 285 nodes that never changed ended at `gpu.count.current=4` and `gpu.count.expected=8`. The reporter then pinned both partitions with `expectedCountOverrides` and asked for a relearn to get off the pin.

The relearn record in #2318 (ADR-071) adds an explicit relearn, and its prevention part keeps a learned count in the partition it was learned in. That repairs the ratchet, but the GPU class still reports eight devices on four GPUs.

On #2318 a maintainer asked whether each MIG instance should be a device, or whether NVSentinel should count the hardware underneath. The questions included whether all instances go bad with their GPU, and how contained, uncontained and fatal errors behave. This record is that data model.

The GPU is the device, and a MIG instance is configuration on one GPU. The expected count counts GPUs, and a fault on a GPU, or a reset the driver asks for, takes every instance on it. A contained error stays with the work that caused it, and how far that reaches the instance is an open question.

No component models MIG today, so each one inherits the unit of its input, as [MIG in NVSentinel today](#mig-in-nvsentinel-today) shows.

## Decision

The physical GPU is the device, and a MIG instance is a child of exactly one GPU. A component maps an instance to its GPU before any decision that keys on a device.

1. The expected device count counts GPUs. A MIG change is an operator action and does not change it.
2. If the instance layout is checked, which is open question 1, it is a second check with its own labels, derived per layout. It is not the health invariant.
3. A fault on a GPU reaches every instance on it. An uncontained or fatal error takes the GPU and all of its instances.
4. Phase one attributes every error to the GPU. Open question 2 decides how far phase two attributes a contained error to its instance. An error that NVML or DCGM pins to one instance then stops the work on that instance only.
5. A reset or drain request from the driver is a GPU fault under rule 3, also when it follows a contained error.

| Device | Identity | Parent | Where it is observed |
|---|---|---|---|
| GPU | GPU index, PCI address, `GPU-<uuid>` | the node | NVML and DCGM `DCGM_FE_GPU`, GFD `nvidia.com/gpu.count` under the none and mixed strategies, DRA `type: gpu` or the `parentUUID` of its instances, the `PCI:` field of a kernel XID line |
| MIG instance | `MIG-<uuid>`, with its GPU instance and compute instance IDs on the GPU | one GPU | NVML MIG handles and events, DCGM [`DCGM_FE_GPU_I` and `DCGM_FE_GPU_CI`](https://github.com/NVIDIA/DCGM/blob/v4.7.0/dcgmlib/dcgm_fields.h#L261-L275), GFD `nvidia.com/gpu.count` (single) or `nvidia.com/mig-<profile>.count` (mixed), DRA `type: mig` with `parentUUID` |

A MIG UUID, `MIG-<uuid>` since driver R470, carries no placement ([MIG device names](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/mig-device-names.html), [device plugin](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/rm/health.go#L296-L307)). It holds only [for the lifetime of the instance](https://github.com/NVIDIA/go-nvml/blob/v0.13.4-1/pkg/nvml/nvml.h#L5693-L5694), and a recreated instance [gets a new one](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/cmd/gpu-kubelet-plugin/mig.go#L55-L67). The parent comes from NVML, DCGM or the DRA `parentUUID`.

### Error classes

| Class | Examples | NVSentinel today | Phase one | Phase two |
|---|---|---|---|---|
| Contained | XID 94 with `RST: No, D-RST: No`, application XIDs such as 13 and 31, DCGM `DCGM_FR_CONTAINED_ERROR` | not fatal, action `NONE`, repeats on one GPU escalate in the health-events-analyzer | recorded on the GPU, which keeps serving | recorded on the instance when NVML or DCGM names one |
| Contained, recovery requested | XID 94 with `RST: Yes` or `D-RST: Yes`, then XID 154 or a DCGM GPU recovery action | `NONE` for XID 94, then `COMPONENT_RESET` | the GPU and every instance | unchanged |
| Uncontained | XID 95, DCGM `DCGM_FR_UNCONTAINED_ERROR` | fatal, `COMPONENT_RESET` from syslog, `RESTART_VM` from DCGM | the GPU and every instance | unchanged |
| Fatal | XID 48, 64, 79, 119 and 120, DCGM errors whose mapped action is not `NONE` | fatal, `COMPONENT_RESET`, `RESTART_VM`, `RESTART_BM` or `CONTACT_SUPPORT` | the GPU and every instance, or the node for a node action | unchanged |

The [Rationale](#rationale) gives the sources for each row.

## Implementation

### MIG in NVSentinel today

| MIG form | What it publishes | The labeler's GPU class counts | The pod device annotation records |
|---|---|---|---|
| GFD `mig.strategy=single`, the [GPU Operator default](https://github.com/NVIDIA/gpu-operator/blob/v26.7.2/deployments/gpu-operator/values.yaml#L136-L137) | `nvidia.com/gpu.count`, `.product` and `.memory` [describe instances](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/docs/gpu-feature-discovery/README.md?plain=1#L228-L249) | instances, 8 on the #1914 nodes | MIG UUIDs under `nvidia.com/gpu` |
| GFD `mig.strategy=mixed` | `nvidia.com/gpu.count` [counts every GPU](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/mig-strategy.go#L114-L180), `nvidia.com/mig-<profile>.count` [counts instances](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/mig-strategy.go#L266-L297) | GPUs | nothing, because `nvidia.com/mig-<profile>` is not a supported resource name |
| DRA driver, static MIG | a GPU in MIG mode is [not a `type: gpu` device](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/cmd/gpu-kubelet-plugin/nvlib.go#L318-L345), and each instance is a [`type: mig` device with `parentUUID`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/site/content/docs/reference/resourceslice-attributes.md?plain=1#L75-L121) | only the GPUs not in MIG mode, and 0 with no partition when every GPU is in MIG mode | nothing, because the MIG device name does not resolve to a GPU, and with every GPU in MIG mode the collector does not start |

The labeler column comes from the chart's GPU class, run on main. Under the single strategy GFD [writes the instance labels over the GPU labels](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/list.go#L34-L47), so the class counts instances, not GPUs, as [ADR-043](043-expected-device-count-labels.md) warned. Under the DRA driver it counts `type: gpu` devices, so a node with all GPUs in MIG mode reports 0, against a learned 4.

With partial drains on (`partialDrainEnabled`, off by default), node-drainer drains for a `COMPONENT_RESET` only the pods whose `dgxc.nvidia.com/devices` annotation holds the GPU UUID. For a GPU in MIG mode it finds none of the pods on its instances. Under the mixed strategy and with DRA claims, `isPodRequestingDevices` (`node-drainer/pkg/informers/informers.go`) does not even see them as GPU pods, and `drainGPUPods` full drains skip them too.

The monitors already report at the GPU. The gpu-health-monitor puts only GPU and NVSwitch entities in its DCGM group (`dcgm_watcher/dcgm.py`). Since #2060 the syslog monitor accepts `GPU-I` and `GPU-CI` fields in an XID line, reports the XID on the GPU, and drops them.

### Labeler

| Form | GPU count | Instance count |
|---|---|---|
| GFD, none | `nvidia.com/gpu.count` | not applicable |
| GFD, single | not published | `nvidia.com/gpu.count` while `nvidia.com/gpu.product` contains `-MIG-` |
| GFD, mixed | `nvidia.com/gpu.count` | the sum of `nvidia.com/mig-<profile>.count` |
| DRA, static MIG | distinct `uuid` of `type: gpu` and `parentUUID` of `type: mig` devices | devices of `type: mig`, as found when the kubelet plugin started |
| DRA, `DynamicMIG` (alpha, [off by default](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/pkg/featuregates/featuregates.go#L145-L149)) | as for static MIG | not applicable, the slice lists [possible placements](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/site/content/docs/concepts/gpu-allocation.md?plain=1#L74-L90) |

The single strategy does not expose the GPU count. GFD overwrites `nvidia.com/gpu.count`, `.product`, `.memory` and `.replicas` with [instance values](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/mig-strategy.go#L182-L241), and instances per GPU depend on profile and layout, so no divisor recovers it. The mixed strategy keeps it, because GFD [counts GPUs in MIG mode with the others](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/mig-strategy.go#L126-L166).

The `dra` source counts each GPU once, from its own device or from the instances that name it.

```text
resourceSlices
  .map(rs, has(rs.spec.devices) ? rs.spec.devices : [])
  .flatten()
  .filter(d,
    has(d.attributes) &&
    'type' in d.attributes &&
    has(d.attributes['type'].string) &&
    (d.attributes['type'].string == 'gpu' ||
      (d.attributes['type'].string == 'mig' &&
        'parentUUID' in d.attributes &&
        has(d.attributes['parentUUID'].string))))
  .map(d, d.attributes['type'].string == 'mig'
    ? d.attributes['parentUUID'].string
    : ('uuid' in d.attributes && has(d.attributes['uuid'].string)
        ? d.attributes['uuid'].string
        : d.name))
  .distinct()
  .size()
```

Its `groupingExpression` also reads `productName` from `type: mig` devices, which carry [their GPU's product name](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/cmd/gpu-kubelet-plugin/mig.go#L147-L189). The labeler deduplicates the values, so a node keeps its partition when its GPUs enter or leave MIG mode. The `gfd` source stops counting a node whose GFD labels describe instances.

```text
has(node.status.allocatable) &&
node.status.allocatable.exists(r,
  (r == 'nvidia.com/gpu' || r == 'nvidia.com/gpu.shared' || r.startsWith('nvidia.com/mig-')) &&
  int(node.status.allocatable[r]) > 0) &&
!('nvidia.com/gpu.product' in node.metadata.labels &&
  node.metadata.labels['nvidia.com/gpu.product'].contains('-MIG-'))
  ? int(node.metadata.labels['nvidia.com/gpu.count'])
  : -1
```

GFD writes its labels as [one set](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/output.go#L90-L171) that NFD applies [in one patch](https://github.com/kubernetes-sigs/node-feature-discovery/blob/v0.20.1/pkg/nfd-master/nfd-master.go#L1218-L1232), so the product test and the count agree. The test also covers `-MIG-INVALID`, set with a count of 0 [when the layout does not fit the single strategy](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/mig-strategy.go#L193-L239). A skipped node teaches nobody and counts in `labeler_device_count_skipped_updates_total` as `missing_source`.

A count that such a node keeps stays live for the [ADR-043 policy](043-expected-device-count-labels.md#consumer-policy), but nothing updates it while MIG is active. The class therefore sets a new field, `uncountedExpression`, that is true while the device plugin advertises GPUs and the product contains `-MIG-`. When no source applies and it returns true, the labeler removes `current` from a node whose `current` is not below `expected`. It keeps `expected` and the relearn record's partition annotation. The policy did not match that node, and it cannot match it later on an old count. A node that entered MIG short of a GPU keeps both labels and stays matched. The labeler can no longer see whether the GPU came back, so it does not clear the shortfall. An operator who confirms that the GPU is back removes `current`, and the policy then clears the node. A new policy that matches such a node requests `RESTART_VM` for a loss that nobody confirmed as fixed, so it fails closed. A source that is only absent for a while, such as a restarting device plugin, keeps both labels, as ADR-043 requires.

### If the instance check is wanted

The instance check is a second class, `mig`, disabled in the chart. It writes `nvsentinel.dgxc.nvidia.com/mig.count.current` and `.expected` from the GFD rows of the instance column. Its partitions add `nvidia.com/mig.config`, the layout the MIG manager applies, and the profiles GFD shows. From the label change until GFD writes the new layout, a node can carry the new layout label with the old GFD labels. The MIG manager sets `pending` only [after it checks the new layout](https://github.com/NVIDIA/mig-parted/blob/v0.15.1/pkg/mig/reconfigure/reconfigure.go#L127-L173). GFD [defaults to the NodeFeature API](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/cmd/gpu-feature-discovery/main.go#L100-L105), and with it GFD [keeps its labels when it stops](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/cmd/gpu-feature-discovery/main.go#L240-L258) until it writes again, usually after `success`. No label alone marks that time, and when both layouts use the same profiles the node shares the partition of settled nodes.

It skips a `-MIG-INVALID` node, so it misses a GPU that lost all its instances. A Kubernetes Object Monitor policy on that product can flag it. Since layout changes move nodes between partitions, it needs the relearn record's partition rule. It leaves out the DRA driver, which finds instances [only when its kubelet plugin starts](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/site/content/docs/concepts/gpu-allocation.md?plain=1#L66-L69). Labels per profile wait, because a total per layout already shows a missing instance.

The relearn record's settled expression applies only between `pending` and the final state. In that time the MIG manager [stops GFD, DCGM and other GPU clients](https://github.com/NVIDIA/mig-parted/blob/v0.15.1/pkg/mig/reconfigure/reconfigure.go#L373-L417) and later [restarts them](https://github.com/NVIDIA/mig-parted/blob/v0.15.1/pkg/mig/reconfigure/reconfigure.go#L542-L581).

The class therefore holds a node through a layout change, from the moment its `nvidia.com/mig.config` differs from the layout in its partition annotation. A count alone cannot end the hold, because GFD can report the old count under a new layout. GFD sets [`nvidia.com/gfd.timestamp`](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/lm/timestamp.go#L29-L37) [once per run](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/cmd/gpu-feature-discovery/main.go#L260-L270), and the MIG manager restarts GFD when it changes a layout. While a node is settled and outside a change, the class copies that timestamp into `nvsentinel.dgxc.nvidia.com/mig.count.gfd-timestamp`. A change ends when the node is settled and its GFD timestamp or count differs from the copy or from `mig.count.current`. Until then, and while it is not settled, the class writes neither label, so the labels keep the pre-change count. The node teaches nobody, and after the change it relearns in its new partition. The class skips a node whose GFD runs with [`--no-timestamp`](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/cmd/gpu-feature-discovery/main.go#L70-L75), because nothing then shows that GFD read the new layout.

### Fault-quarantine and node-drainer

Fault-quarantine keys each entity of an event separately (`HealthEventKey` in `health_events_annotation_map.go`). Phase one events name the GPU as today, so it does not change.

Node-drainer keeps keying on the GPU UUID, and the pod device annotation maps an instance to its GPU.

- The metadata collector appends the parent `GPU-<uuid>` of each MIG device a pod holds, next to the device ID. The device plugin reports an instance [by its MIG UUID](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/rm/devices.go#L60-L90), and a DRA MIG device is named [`gpu-<parent minor>-mig-...`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/cmd/gpu-kubelet-plugin/mig.go#L119-L122). The collector writes the MIG devices of a pod only together with the parents of all of them.
- Under the DRA driver the collector runs only where it can hold its admin-access claim on `gpu.nvidia.com` devices (`resourceclaimtemplate.yaml`). A node with every GPU in static MIG mode publishes none, and a request for `All` devices [needs at least one](https://github.com/kubernetes/kubernetes/blob/v1.34.0/staging/src/k8s.io/api/resource/v1/types.go#L846-L849). The DRA driver [refuses admin access](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/cmd/gpu-kubelet-plugin/device_state.go#L912-L936) to a MIG device, and a `firstAvailable` subrequest [cannot carry admin access](https://github.com/kubernetes/kubernetes/blob/v1.34.0/staging/src/k8s.io/api/resource/v1/types.go#L923-L930). The collector therefore does not start on such a node, and node-drainer stops partial drains there at the missing annotations.
- The `GPU_UUID` entry in `EntityTypeToResourceNames` covers the [`nvidia.com/mig-<profile>`](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/rm/rm.go#L145) names by prefix, and the [`mig.nvidia.com`](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/deployments/helm/dra-driver-nvidia-gpu/templates/deviceclass-mig.yaml) DeviceClass. Each place that reads the entry matches exact names today, so each one changes.
- A pod holds MIG devices when it requests an `nvidia.com/mig-<profile>` resource, claims from the `mig.nvidia.com` DeviceClass, or its annotation names a MIG device. Node-drainer accepts the annotation of such a pod only when it lists a MIG device and a `GPU-` UUID. Otherwise it returns the missing-annotation error (`informers.go` L486-L495), during the rollout and after it. The partial drain then does not complete (`informers.go` L1196-L1197), and the GPU is not reset. A stale annotation from an older collector or a failing mapper therefore stops the drain instead of hiding the pod.
- Both roll out in one Helm release, so the collector change ships one release before node-drainer learns the MIG names. The error then does not stop partial drains during an upgrade.

A `COMPONENT_RESET` of a GPU then drains each pod on its instances. The reset removes the instances. Under the single strategy the device plugin then [treats the layout as invalid](https://github.com/NVIDIA/k8s-device-plugin/blob/v0.20.1/internal/rm/device_map.go#L90-L97) and GFD marks the node `-MIG-INVALID`. The MIG manager applies a layout only [when it starts or its label changes](https://github.com/NVIDIA/mig-parted/blob/v0.15.1/cmd/nvidia-mig-manager/main.go#L556-L585), so a follow-up has to decide how the layout is restored.

### Health events and node conditions

Phase one changes no schema, and events keep naming the GPU. A monitor that knows the instance adds `mig_uuid`, `gpu_instance_id` and `compute_instance_id` to the event `metadata`, which no key or node condition reads.

### Phase two sketch

Phase two uses a convention, not a parent field. An event about an instance lists its GPU's entities, then a `MIG_UUID` entity, so unhealthy events keep working for GPU consumers. Fault-quarantine needs two changes first. It stores one key per entity and error code, and a newer event overwrites an older one under that key. An instance event therefore keys by its `MIG_UUID` only, or the instance joins `HealthEventKey`. Recovery is scoped the same way, because `keyMatchesEvent` matches any entity of a healthy event. A healthy event naming a `MIG_UUID` clears only that instance, and one for the GPU also clears its instances. The code is `HealthEventKey` (L33-L45), `createEntityKeys` (L110-L135) and `addEventKeys` (L195-L209) in `health_events_annotation_map.go`. Platform-connectors write a contained error, which is not fatal, as a node Event that names each entity.

### Monitors and metadata collector

- **gpu-health-monitor.** In phase two it adds `DCGM_FE_GPU_I` and `DCGM_FE_GPU_CI` entities to its group, if DCGM attributes its checks to them. DCGM [watches instances](https://github.com/NVIDIA/DCGM/blob/v4.7.0/modules/health/DcgmHealthWatch.cpp#L284-L295) and [resolves them to their GPU](https://github.com/NVIDIA/DCGM/blob/v4.7.0/modules/health/DcgmHealthWatch.cpp#L1329-L1351), but per instance incidents were not confirmed.
- **syslog-health-monitor.** In phase one the handler records `GPU-I` and `GPU-CI` in `metadata`, for drivers that print them. Phase two takes the instance from NVML or DCGM, because the kernel line may never carry it.
- **health-events-analyzer.** Its per-GPU repeat rules are on by default and publish a fatal event for any action but `NONE`. In phase one they add up application errors from different instances of one GPU, and counting per instance is part of open question 2. The rules are `RepeatedXIDErrorOnSameGPU` (L141-L147), `RepeatedXID31OnSameGPU` (L419-L424) and `RepeatedXID13OnSameGPCAndTPC` (L770-L775) in `values.yaml`, with `publisher.go` L192-L196.
- **metadata-collector.** Each GPU in `gpu_metadata.json` gets the additive fields `mig_enabled` and `mig_devices`, a list of `uuid`, `gpu_instance_id`, `compute_instance_id` and `profile`. Under GPU Operator 26.7 and later the collector [leaves the node during a MIG change](../configuration/metadata-collector.md#gpu-operator-driver-upgrades-and-mig-changes) and rewrites the file on return. The syslog monitor reloads it since #1736, and the open #2317 does so for the gpu-health-monitor.

### Migration

- A fleet that latched the instance count, as the #1914 fleet did, recovers through the relearn instead of a pinned override.
- The labeler change needs the relearn record's partition rule. Without it, a node that leaves MIG carries its instance count back to the plain partition.
- That change alters chart defaults and label values on MIG nodes on purpose, to match the chart comment that the class counts physical GPUs. The maintainers own that choice.
- A node with DRA MIG rises to its GPU count, which the labeler learns without help. A single strategy node with MIG active is no longer counted. The labeler removes its `current` label, except on a node that was short of a GPU, which stays matched.
- A deployment with its own `classes` list keeps its expressions, and the docs give the new ones. No label or Helm key is renamed or removed, as `AGENTS.md` requires.

## Rationale

- GPUs do not appear or disappear when an operator configures MIG, and instances do. An invariant tied to configuration turns each MIG change into a fault or a relearn, which is the defect behind #1914.
- The GPU is the unit of remediation. A contained error stays with the work that caused it, and any reset or reboot takes every instance.
- Each consumer already keys on the GPU, so phase one needs no schema change. Only the pod annotation needs the release order above.

The [XID catalog](https://docs.nvidia.com/deploy/xid-errors/analyzing-xid-catalog.html) that the syslog monitor embeds says XID 94 is contained to one application and XID 95 needs a GPU reset. For XID 94 it recommends a reset when convenient. DCGM reports them as a warning and a failure ([`DcgmHealthWatch.cpp`](https://github.com/NVIDIA/DCGM/blob/v4.7.0/modules/health/DcgmHealthWatch.cpp#L3281-L3295)). NVSentinel maps them to `NONE` and to a reset or reboot (`common.go`, `dcgmerrorsmapping.csv`), and the default fault-quarantine rules for monitor events require `isFatal`.

The driver reports recovery apart from containment. Under MIG some contained errors still [require a drain and reset](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/arch/ampere/kern_gpu_error_cont_ga100.c#L53-L84) of the GPU. The [Blackwell table](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/arch/blackwell/kern_gpu_error_cont_gb100.c#L52-L99), for the #1914 hardware, has the same MIG rows. Its contained errors E22 to E25 need a GPU reset with or without MIG. The driver [logs a changed recovery action as XID 154](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/gpu.c#L7042-L7056). The syslog monitor maps it to `COMPONENT_RESET` (`xid/parser/csv.go`), as the gpu-health-monitor maps `DCGM_FR_GPU_RECOVERY_RESET` and `_DRAIN_RESET`. Only a contained error without such a request leaves the GPU in service.

The kernel XID line has a [form with `GPU-I` and `GPU-CI`](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/rc/kernel_rc.c#L392-L412). In the open kernel modules [its attribution is always invalid](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/rc/kernel_rc.c#L276-L293), and GSP-RM XIDs [arrive without an instance](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/gsp/kernel_gsp.c#L797-L840). Whether any driver prints `GPU-I` was not confirmed. NVML events carry the GPU instance and compute instance IDs only when the XID is attributable to one ([nvml.h](https://github.com/NVIDIA/go-nvml/blob/v0.13.4-1/pkg/nvml/nvml.h#L3488-L3503), [GSP](https://github.com/NVIDIA/open-gpu-kernel-modules/blob/580.196.07/src/nvidia/src/kernel/gpu/gsp/kernel_gsp.c#L695-L711)). The DRA driver uses them to taint [one instance](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/cmd/gpu-kubelet-plugin/device_health.go#L241-L262). A reset takes every instance, because MIG devices must be [created again after a GPU reset](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/getting-started-with-mig.html).

## Consequences

### Positive

- Under the DRA driver a MIG change no longer moves the GPU count, and no MIG node teaches its instance count.
- With partial drains or `drainGPUPods`, a reset of a GPU in MIG mode drains the pods on its instances.

### Negative

- The single strategy, the GPU Operator default, has no GPU count until question 3 is answered. With chart defaults its nodes lose the `current` GPU count label. A node that entered MIG short of a GPU keeps it and stays matched until an operator removes it, also after the GPU comes back.
- The labeler logs a warning at each reconcile of a skipped node.
- The DRA driver does not publish a GPU in MIG mode [that has no instances](https://github.com/kubernetes-sigs/dra-driver-nvidia-gpu/blob/v0.5.1/site/content/docs/concepts/gpu-allocation.md?plain=1#L70-L72), so that GPU counts as missing. Its slices keep listing old instances until the kubelet plugin restarts.
- Until the layout is restored, a reset leaves a GPU in MIG mode without instances. Under the single strategy the whole node then offers no MIG device.
- A MIG pod that the collector has not annotated stops partial drains on its node, as an unannotated GPU pod does today. Under the DRA driver that is every MIG pod on a node with all GPUs in MIG mode.
- In phase one a contained error names only the GPU, so an operator cannot tell which instance it hit.
- The instance check adds two labels and an annotation on MIG nodes and depends on the relearn record. It misses a GPU that lost all its instances. A GFD restart or a count change for another reason before the MIG manager applies a new layout ends that change early. A change to a layout already in place does not restart GFD, so the node stays held until GFD restarts or its count changes.
- The MIG manager also stops the GPU Operator's DCGM, so a gpu-health-monitor in the default `operator-service` mode can report the fatal `GpuDcgmConnectivityFailure`. The default rules then cordon the node. This was read from the code, not reproduced.

### Mitigations

- The skipped metric shows the nodes that the GPU class no longer counts, and a relearn stops their old labels from counting.
- Once enabled, the instance check covers most single strategy nodes. The event metadata names the instance where the input does.

## Alternatives Considered

### An instance is the device

**Rejected** because it ties the health invariant to configuration. Each layout change then looks like a lost or new device, and the unit differs between strategies. A GPU fault still has to reach every instance.

### GPUs and instances counted as peers

**Rejected** because a count of both changes with each layout change and counts the hardware twice. It also cannot be computed under the single strategy, which hides the GPUs.

### MIG nodes ignored

**Rejected** because those nodes would lose missing GPU detection while partial drains still miss their pods and their faults still land on the GPU. Skipping single strategy nodes in the GPU class is a named gap, not a model.

### Status quo plus the partition rule

The GPU class keeps counting instances under the single strategy, and the partition rule keeps them out of plain partitions. **Not chosen** as the model, because the class then counts different units on different nodes. It is the interim option in open question 1.

### A parent field on `Entity`

**Rejected** because a new protobuf field reaches every producer, consumer and stored event. The `MIG_UUID` convention only adds an entity type.

## Notes

### Open questions

1. Is the instance check wanted in phase one, and should it ship enabled for the single strategy? The interim alternative keeps counting single strategy instances in their own partition, safe under the partition rule, until question 3 is answered.
2. How far should a contained error be attributed? The options are event metadata only, a `MIG_UUID` entity in the node Event, or per instance handling, including counting in the health-events-analyzer.
3. Which input should give the GPU count under the single strategy? The candidates, by privilege, are a [feature file or NodeFeature object](https://github.com/kubernetes-sigs/node-feature-discovery/blob/v0.20.1/docs/usage/customization-guide.md?plain=1#L36-L110) from the metadata collector, and an NFD rule that [counts NVIDIA PCI devices](https://github.com/kubernetes-sigs/node-feature-discovery/blob/v0.20.1/docs/usage/customization-guide.md?plain=1#L1176-L1190). A direct Node patch comes last, because it gives a DaemonSet write access to every Node.

### Follow-up PRs

1. The labeler gains `uncountedExpression`. Its GPU class counts GPUs, with the expressions above in `values.yaml`, `values-full.yaml` and the labeler docs, and a test for each MIG form.
2. The metadata collector records the instances of each GPU in `gpu_metadata.json`.
3. The metadata collector writes parent GPU UUIDs into the pod device annotation, one release before the next item.
4. Node-drainer learns the MIG resource names and DeviceClass for partial drains, and the annotation it accepts for a MIG pod.
5. After reproducing it, the gpu-health-monitor holds `GpuDcgmConnectivityFailure` while `nvidia.com/mig.config.state` is `pending` or `rebooting`.
6. Fault-remediation decides how the layout is restored after a reset, before the device plugin and GFD return, and how the DRA kubelet plugin restarts.
7. The labeler chart gets the instance check, if the first question says so.
8. Phase two adds the `MIG_UUID` convention, the keys and scoped recovery, and the NVML or DCGM attribution, within the answer to the second question.
9. The metadata collector decides how it runs on a DRA node with every GPU in MIG mode.

### Non-goals

- vGPU guests, MPS, time slicing, an instance check under `DynamicMIG`, MIG pods in preflight, and MIG instances in rules, which [ADR-064](064-node-and-device-selection.md) also leaves out.

### Versions

- Code links point at GFD and the device plugin v0.20.1, the MIG manager v0.15.1, the DRA driver v0.5.1 and DCGM v4.7.0. GPU Operator v26.7.2 ships the first two and [DCGM 4.6.1-1](https://github.com/NVIDIA/gpu-operator/blob/v26.7.2/deployments/gpu-operator/values.yaml#L289-L294).

## References

- [ADR-043: Expected Device Count Labels](043-expected-device-count-labels.md)
- ADR-071: Labeler - Explicit Relearn of Expected Device Counts, in [#2318](https://github.com/NVIDIA/NVSentinel/pull/2318)
- [ADR-064: Node and Device Selection in Each Component](064-node-and-device-selection.md)
- Issue [#1914](https://github.com/NVIDIA/NVSentinel/issues/1914), PRs [#2060](https://github.com/NVIDIA/NVSentinel/pull/2060), [#2108](https://github.com/NVIDIA/NVSentinel/pull/2108), [#1736](https://github.com/NVIDIA/NVSentinel/pull/1736) and [#2317](https://github.com/NVIDIA/NVSentinel/pull/2317)
- [Labeler configuration](../configuration/labeler.md)
- [NVIDIA MIG User Guide](https://docs.nvidia.com/datacenter/tesla/mig-user-guide/) and [GPU Operator with MIG](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/gpu-operator-mig.html)
