# Preflight configuration

Preflight is a mutating admission webhook. It injects GPU diagnostic init containers (DCGM diagnostics, optional NCCL loopback / all-reduce) into pods that request GPUs. It does this only in the namespaces that you label for preflight. Preflight answers the question "is this GPU healthy enough to start?" before your workload runs. Preflight is separate from continuous [GPU health monitoring](../gpu-health-monitor.md).


## Prerequisites

- The Helm subchart is off by default. To enable it, set `global.preflight.enabled` (see below).
- cert-manager (or OpenShift service CA) for the webhook TLS. The rest of the NVSentinel chart has the same requirement.
- A DCGM endpoint that the injected init containers can reach. This is usually the DCGM / hostengine service of the NVIDIA GPU Operator. To configure the endpoint, set `DCGM_HOSTENGINE_ADDR` on the `preflight-dcgm-diag` init container. If the check cannot reach this endpoint, it reports a fatal health event. If Fault Quarantine is enabled and `processingStrategy` is `EXECUTE_REMEDIATION` (the default), a default Fault Quarantine ruleset then cordons the node. If `processingStrategy` is `STORE_ONLY`, NVSentinel only stores the health event.
- Multi-node / gang checks (for example, `preflight-nccl-allreduce`) need gang coordination and gang discovery for your scheduler (see below). The default gang discovery uses the native Kubernetes gang APIs, which need Kubernetes 1.35 or later. On an earlier cluster, configure a PodGroup-based scheduler or disable gang coordination. See [Single-node and single-GPU clusters](#single-node-and-single-gpu-clusters).

## Enable preflight

1. Set the global flag:

```yaml
global:
  preflight:
    enabled: true
```

2. Configure the preflight subchart under the top-level `preflight:` key. The values merge into `distros/kubernetes/nvsentinel/charts/preflight/values.yaml`. At a minimum, review `initContainers` (including the DCGM and NCCL environment variables) and `webhook.failurePolicy`.

3. Label the namespaces in which the webhook injects the checks:

```bash
kubectl label namespace {namespace} nvsentinel.nvidia.com/preflight=enabled
```

The default `namespaceSelector` of the chart matches that label.

### Single-node and single-GPU clusters

The chart enables gang coordination by default. The default `gangDiscovery` (`{}`) selects native Kubernetes gang discovery. Preflight validates that discoverer against the cluster at startup, so the preflight pod does not start on a cluster older than Kubernetes 1.35. Its log names the requirement:

```bash
kubectl -n nvsentinel logs deploy/preflight
```

```text
"level":"ERROR","msg":"Fatal error","error":"failed to create gang discoverer resolver: failed to create default gang discoverer: kubernetes native gang API not available (requires K8s 1.35 Workload or K8s 1.36+ PodGroup)"
```

A single node has no gang to coordinate. To disable coordination and inject only the single-node GPU check, use these values:

```yaml
global:
  preflight:
    enabled: true

preflight:
  gangCoordination:
    enabled: false
  initContainers:
    - name: preflight-dcgm-diag
      image:
        repository: ghcr.io/nvidia/nvsentinel/preflight-dcgm-diag
        tag: ""
      env:
        - name: DCGM_HOSTENGINE_ADDR
          value: "nvidia-dcgm.gpu-operator.svc:5555"
        - name: DCGM_DIAG_LEVEL
          value: "2"
      volumeMounts:
        - name: nvsentinel-socket
          mountPath: /var/run
```

Helm replaces the `initContainers` list. It does not merge it. As a result, this example also removes the two NCCL checks:

- `preflight-nccl-allreduce` needs gang coordination. If coordination is off, the webhook injects no gang environment. The check then exits `2` (`NCCL_GANG_CONFIG_ERROR`), and the pod stays in `Init:Error`.
- `preflight-nccl-loopback` measures the GPU-to-GPU path on the node. A single-GPU node does not have this path. On a multi-GPU node, keep this check. Then set `BW_THRESHOLD_GBPS` for the interconnect: approximately `15` for PCIe and `150` for NVLink.

The same values apply to a multi-node cluster whose scheduler is not gang-aware. If your scheduler uses PodGroups (Volcano, Run:ai / OSMO), keep gang coordination enabled. Configure [gang discovery](#gang-discovery) for your scheduler instead. That path works on any Kubernetes version that runs the scheduler.

### Image cache

The image cache runs a DaemonSet that holds every configured check image on the matching nodes. The kubelet then treats the images as in use, so its garbage collector does not evict them.

```yaml
preflight:
  imageCache:
    enabled: false
    resources:
      requests:
        cpu: 10m
        memory: 16Mi
      limits:
        cpu: 50m
        memory: 64Mi
    podAnnotations: {}
```

The image cache is off by default. If image pulls on your nodes are slow or metered, enable the image cache. Preflight runs on the pod admission path. As a result, a cold pull of a multi-gigabyte CUDA check image delays the start of every GPU workload on that node. The cost is one small pod on each node, plus the disk space of the cached images.

## Init container placement

By default, the webhook **appends** the preflight init containers after the existing init containers in the pod spec. This order makes sure that the setup containers that a provider injects (for example, the GCP TCPXO daemon) complete before the preflight checks run.

Set `initContainerPlacement` to change this behavior:

```yaml
# "append" (default): add after existing init containers
# "prepend": add before existing init containers
initContainerPlacement: "prepend"
```

If the preflight checks must run before the other init containers, use `prepend`. For example, use it to make the workload setup wait for GPU health validation.

## Per-pod check selection

By default, the webhook injects all init containers with `defaultEnabled: true` (or with no value, which defaults to true) into every GPU pod. To select a subset of checks for a specific pod, annotate the pod:

```yaml
metadata:
  annotations:
    nvsentinel.nvidia.com/preflight-checks: "preflight-dcgm-diag,preflight-nccl-loopback"
```

The webhook injects only the named containers, in the order of the annotation. If a container name is a duplicate or is unknown, the webhook rejects the admission with an error.

An empty value disables all checks:

```yaml
nvsentinel.nvidia.com/preflight-checks: ""
```

If the annotation is absent, `defaultEnabled` on each init container controls whether it runs. For gang-aware checks (`nccl-allreduce`), all pods in the gang must have the same annotation value. The check finds a mismatch and fails fast, before torchrun starts.

## Init containers (check configuration)

The `initContainers` list in the preflight chart defines the checks that the webhook injects. Each entry is a standard `corev1.Container` plus controls specific to preflight, such as `defaultEnabled`, `inheritUserEnv`, and `inheritUserVolumeMounts`. You control the images, environment variables, resource limits, security contexts and volume mounts.

The webhook injects these environment variables into every init container automatically. You do not have to set them:

| Env var | Source | Purpose |
|---------|--------|---------|
| `NODE_NAME` | Downward API (`spec.nodeName`) | The Kubernetes node name for health events |
| `PLATFORM_CONNECTOR_SOCKET` | Chart `connectorSocket` | The Unix socket for the gRPC endpoint of the platform connector |
| `PROCESSING_STRATEGY` | Chart `processingStrategy` | `EXECUTE_REMEDIATION` or `STORE_ONLY`. Controls the downstream action |
| `PLATFORM_CONNECTOR_TOKEN_PATH` | Chart `global.platformConnectorAuth` | The projected token that the check presents, when platform connector authentication is on |
| `HEALTH_PUBLISH_TARGET`, `HEALTH_PUBLISH_TOKEN_PATH`, `HEALTH_PUBLISH_TLS_CA_FILE` (or `HEALTH_PUBLISH_INSECURE`) | Chart `publishTo: deployment` | Direct connection to the deployment platform connector. See [below](#publishing-to-the-deployment-platform-connector) |

For gang-aware containers, the webhook also injects `GANG_ID`, `GANG_CONFIG_DIR`, `GANG_TIMEOUT_SECONDS`, and `POD_NAME`.

`inheritUserEnv` and `inheritUserVolumeMounts` both default to `true` when omitted. The chart sets both to `true` on each built-in check. As a result, every check mirrors the NCCL and fabric configuration of the workload by default. For the patterns that select what the webhook copies, see [Fabric-specific NCCL configuration](#fabric-specific-nccl-configuration). To give a check a curated environment instead, set either flag to `false` on its `initContainers` entry.

### preflight-dcgm-diag

This check runs DCGM diagnostics on every GPU allocated to the pod. It uses the remote hostengine.

| Env var | Default | Description |
|---------|---------|-------------|
| `DCGM_DIAG_LEVEL` | `2` | The diagnostic depth. 1 = short (approximately 30 s, software deployment checks). 2 = medium (approximately 2 min, adds PCIe and basic GPU stress). 3 = long (approximately 15 min, adds Diagnostic plugin stress). 4 = xlong (1-2 hr, extended stress) |
| `DCGM_HOSTENGINE_ADDR` | `nvidia-dcgm-dra.gpu-operator.svc:5555,nvidia-dcgm.gpu-operator.svc:5555` | A comma-separated list of gRPC endpoints of the DCGM hostengine. The check tries them in order. Only one of them exists in a cluster (`nvidia-dcgm-dra` in GPU Operator GPUCluster mode) |
| `DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS` | `10` | The maximum number of diagnostic attempts if DCGM returns a `DCGM_ST_*` status while it starts or runs the diagnostics |
| `DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS` | `10` | The delay between the `DCGM_ST_*` retry attempts |

Example override of the values:

```yaml
initContainers:
  - name: preflight-dcgm-diag
    image:
      repository: ghcr.io/nvidia/nvsentinel/preflight-dcgm-diag
      tag: ""
    env:
      - name: DCGM_HOSTENGINE_ADDR
        value: "nvidia-dcgm-dra.gpu-operator.svc:5555,nvidia-dcgm.gpu-operator.svc:5555"
      - name: DCGM_DIAG_LEVEL
        value: "2"
      - name: DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS
        value: "10"
      - name: DCGM_DIAG_STATUS_RETRY_INTERVAL_SECONDS
        value: "10"
    volumeMounts:
      - name: nvsentinel-socket
        mountPath: /var/run
```

A `DCGM_ST_*` status can still prevent the completion of the diagnostic after the retries. In this case, `preflight-dcgm-diag` emits a non-fatal unhealthy HealthEvent with `RecommendedAction=NONE`, and it exits successfully. As a result, a preflight infrastructure failure does not block the workload.

### preflight-nccl-loopback

This check runs a single-node NCCL all-reduce across all GPUs on the node. It validates the intra-node interconnect (NVLink or PCIe).

| Env var | Default | Description |
|---------|---------|-------------|
| `BW_THRESHOLD_GBPS` | `150` | The minimum acceptable bus bandwidth in GB/s. An NVLink interconnect usually sustains 150+ GB/s. For a PCIe interconnect, set approximately 15 GB/s |
| `TEST_SIZE_MB` | `256` | The message size in MB for the all-reduce benchmark |
| `SKIP_BANDWIDTH_CHECK` | `false` | When `true`, the check passes if the benchmark completes, whatever the measured bandwidth |

Example override of the values:

```yaml
initContainers:
  - name: preflight-nccl-loopback
    image: ghcr.io/nvidia/nvsentinel/preflight-nccl-loopback:latest
    env:
      - name: BW_THRESHOLD_GBPS
        value: "15"           # PCIe interconnect
      - name: TEST_SIZE_MB
        value: "512"
```

### preflight-nccl-allreduce

This check runs a multi-node NCCL all-reduce across the full gang. It requires `gangCoordination.enabled: true` and a gang-aware scheduler.

| Env var | Default | Description |
|---------|---------|-------------|
| `BW_THRESHOLD_GBPS` | `100` | The minimum acceptable bus bandwidth in GB/s |
| `MESSAGE_SIZES` | `4G` | A comma-separated list of message sizes for the benchmark (for example, `"4G"` or `"4G,8G"`). The built-in default of the check is `4G,8G`. The Helm chart overrides it to `4G` |
| `BENCHMARK_ITERS` | `20` | The number of timed iterations for each message size |
| `WARMUP_ITERS` | `5` | The number of warmup iterations before the timing starts |
| `NCCL_REDUCE_OP` | `sum` | The reduction operation (`sum`, `prod`, `min`, `max`, `avg`) |
| `SKIP_BANDWIDTH_CHECK` | `false` | When `true`, the check passes if the benchmark completes, whatever the bandwidth |
| `NCCL_DEBUG` | — | The NCCL log verbosity (for example, `INFO` or `WARN`) |
| `NCCL_DEBUG_SUBSYS` | — | The NCCL subsystems to log (for example, `INIT,NET`) |

The container also requires the `IPC_LOCK` capability for RDMA memory registration:

```yaml
initContainers:
  - name: preflight-nccl-allreduce
    image: ghcr.io/nvidia/nvsentinel/preflight-nccl-allreduce:latest
    securityContext:
      capabilities:
        add: ["IPC_LOCK"]
    env:
      - name: BW_THRESHOLD_GBPS
        value: "100"
      - name: MESSAGE_SIZES
        value: "4G"
```

### Fabric-specific NCCL configuration

If a check has `inheritUserEnv` enabled, the webhook copies the matching NCCL environment variables from the pod's main containers. If a check has `inheritUserVolumeMounts` enabled, the webhook copies the matching volume mounts. Both flags are on by default. The webhook uses these glob patterns to match the names:

```yaml
ncclEnvPatterns:    ["NCCL_*", "FI_*", "LD_LIBRARY_PATH", "UCX_*", "TORCH_NCCL_*", "CUDA_DEVICE_ORDER"]
volumeMountPatterns: ["host-opt-amazon*", "nvtcpxo-*", "nccl-*", "dev-shm"]
```

This means that if your training container already has the correct `NCCL_TOPO_FILE`, `FI_PROVIDER`, or `LD_LIBRARY_PATH`, a preflight init container inherits them. You do not have to configure them manually.

Inheritance applies to each init container separately, and it is on by default. If two names conflict, the environment variable that the chart defines wins. As a result, an inherited value only fills a name that the check does not set itself. For a check whose environment must not depend on the NCCL tuning of the workload, set the flags to `false`:

```yaml
initContainers:
  - name: preflight-nccl-allreduce
    inheritUserEnv: false
    inheritUserVolumeMounts: false
```

For standalone testing (for example, with a busybox main container), use `ncclAllreduceExtraEnv` and `gangCoordination.extraHostPathMounts` to supply the fabric configuration explicitly.

## Gang discovery

Gang discovery identifies the pods that belong to the same scheduling group. With this information, the multi-node preflight checks (NCCL all-reduce) know their peers. A pod carries a "gang anchor". This is a reference to a parent object that holds the gang metadata, such as the minimum member count.

Preflight supports two discovery mechanisms:

### Native Kubernetes: schedulingGroup / workloadRef

This mechanism is the default if `gangDiscovery` is empty (`{}`). Preflight uses the native PodGroup API (Kubernetes 1.36 and later) if it is available. Otherwise, it uses the native Workload API of Kubernetes 1.35. Neither API exists before Kubernetes 1.35, and preflight validates the discoverer at startup. As a result, the pod fails to start on an earlier cluster. See [Single-node and single-GPU clusters](#single-node-and-single-gpu-clusters).

Preflight uses the PodGroup version that the cluster prefers. You do not configure a version:

| Kubernetes | PodGroup API | Maturity |
|---|---|---|
| 1.36 | `scheduling.k8s.io/v1alpha2` | Alpha |
| 1.37 | `scheduling.k8s.io/v1beta1` | Beta |

> In both versions, Kubernetes disables the `PodGroup` resource and `spec.schedulingGroup` by default. To use this path, enable the `GenericWorkload` feature gate on the API server and the scheduler. On Kubernetes 1.37, also enable the API version on the API server with `--runtime-config=scheduling.k8s.io/v1beta1=true`.

In Kubernetes 1.36 and later, each pod links to a `PodGroup` resource through `spec.schedulingGroup`:

```yaml
spec:
  schedulingGroup:
    podGroupName: training-workers
```

The PodGroup object contains a gang policy with `minCount`:

```yaml
apiVersion: scheduling.k8s.io/v1beta1  # v1alpha2 on Kubernetes 1.36
kind: PodGroup
metadata:
  name: training-workers
spec:
  schedulingPolicy:
    gang:
      minCount: 2
```

This path needs no `gangDiscovery` configuration.

In Kubernetes 1.35, each pod links to a native `Workload` resource through `spec.workloadRef`:

```yaml
spec:
  workloadRef:
    name: training-job-workload
    podGroup: workers
```

The Workload object contains pod groups and a gang policy:

```yaml
apiVersion: scheduling.k8s.io/v1alpha1
kind: Workload
metadata:
  name: training-job-workload
spec:
  podGroups:
    - name: workers
      policy:
        gang:
          minCount: 2
```

This fallback path also needs no `gangDiscovery` configuration.

The default RBAC of the chart grants read access to both native resources: `scheduling.k8s.io/podgroups` for Kubernetes 1.36 and later, and `scheduling.k8s.io/workloads` for Kubernetes 1.35.

### PodGroup-based schedulers (Volcano, Run:ai / OSMO, and similar)

For schedulers that use PodGroup CRDs, configure `gangDiscovery` with these fields:

| Field | Purpose |
|-------|---------|
| `name` | The identifier of the discoverer. Preflight uses it in the gang ID prefix and in logs (for example, `"volcano"`) |
| `annotationKeys` | The pod annotation keys that preflight checks, in order, for the PodGroup name |
| `labelKeys` | Optional pod label keys that preflight checks as a fallback |
| `podGroupGVR` | `group`, `version`, `resource` of the PodGroup CRD |
| `minCountExpr` | A CEL expression that extracts the minimum member count from the PodGroup object. The expression gets `podGroup` as the unstructured object. Default: `"podGroup.spec.minMember"` |

Volcano example:

```yaml
gangDiscovery:
  name: "volcano"
  annotationKeys:
    - "scheduling.k8s.io/group-name"
  podGroupGVR:
    group: "scheduling.volcano.sh"
    version: "v1beta1"
    resource: "podgroups"
  minCountExpr: "podGroup.spec.minMember"
```

Volcano sets the `scheduling.k8s.io/group-name` annotation on each pod. The discoverer reads that annotation and gets the corresponding `PodGroup` CRD. Then it evaluates `minCountExpr` to find the expected gang size.

OSMO + Kai scheduler example:

```yaml
gangDiscovery:
  name: "osmo-with-kai"
  labelKeys:
    - "osmo.group_uuid"
  podGroupGVR:
    group: "scheduling.run.ai"
    version: "v2alpha2"
    resource: "podgroups"
  minCountExpr: "podGroup.spec.minMember"
```

In this example, a pod label sets the membership, not an annotation. The rest of the flow is the same: the discoverer gets the PodGroup CRD and extracts `minCount` with CEL.

### OSMO + KAI Scheduler

[KAI Scheduler](https://github.com/kai-scheduler/KAI-Scheduler) uses the `scheduling.run.ai/v2alpha2` PodGroup CRD. KAI Scheduler can run on its own. But in that setup, the pod gets its gang anchor only **after** admission. As a result, preflight has no label or annotation to use when the webhook runs.

This section documents the **OSMO + KAI** integration instead. OSMO creates the PodGroup and labels each pod with `osmo.group_uuid` at admission time. Preflight reads that label, gets the PodGroup, and uses `spec.minMember` as the expected gang size for `preflight-nccl-allreduce`.

#### Step 1 — Enable preflight cluster-wide

```yaml
global:
  preflight:
    enabled: true

preflight:
  gangDiscovery:
    name: "osmo-with-kai"
    labelKeys:
      - "osmo.group_uuid"
    podGroupGVR:
      group: "scheduling.run.ai"
      version: "v2alpha2"
      resource: "podgroups"
    minCountExpr: "podGroup.spec.minMember"
```

If you set `gangDiscovery.podGroupGVR` in the Helm values, the built-in RBAC contributor role of the chart grants read access to `scheduling.run.ai/podgroups`. See [RBAC (aggregated ClusterRole)](#rbac-aggregated-clusterrole) below.

#### Step 2 — Label namespaces for injection

```bash
kubectl label namespace {training-namespace} nvsentinel.nvidia.com/preflight=enabled
```

#### Step 3 — Verify image pull secrets

If the preflight check images are in a private registry, set `injectedImagePullSecrets` in the preflight Helm values. This value is in `charts/preflight/values.yaml` of the NVSentinel chart. The webhook injects those secrets into the admitted pods as `spec.imagePullSecrets`.

Check whether the preflight configuration contains pull secrets:

```bash
kubectl -n nvsentinel get configmap preflight -o jsonpath='{.data.config\.yaml}' | yq .imagePullSecrets
```

If `imagePullSecrets` is present, confirm that each secret exists in your workload namespace:

```bash
kubectl -n {training-namespace} get secret {secret-name}
```

If `injectedImagePullSecrets` is empty (`[]`), the webhook injects no pull secrets. In that case, skip this step.

#### Step 4 — Submit a workload

Submit your OSMO gang-scheduled workload in the labeled namespace.

#### Step 5 — Verify injection

After GPU pod admission, confirm that the webhook injected the preflight init containers and created the gang ConfigMap:

```bash
# Init containers injected (preflight-dcgm-diag, preflight-nccl-loopback, preflight-nccl-allreduce)
kubectl -n {training-namespace} get pod {pod-name} -o jsonpath='{range .spec.initContainers[*]}{.name}{": "}{.image}{"\n"}{end}'

# Gang ConfigMap created at admission
kubectl -n {training-namespace} get configmap -l nvsentinel.nvidia.com/managed-by=preflight -o yaml
```

The gang ID is `{gangDiscovery.name}-{namespace}-{podGroupName}`. For example, with `name: osmo-with-kai` in namespace `team-a` and PodGroup `myjob`, the gang ID is `osmo-with-kai-team-a-myjob`.

At admission, the ConfigMap contains `gang_id`, `expected_count`, `master_port`, `peers`, and `master_addr`. If the PodGroup does not exist at admission, `expected_count` is `"0"`, and `peers` and `master_addr` are empty.

The ConfigMap name starts with `preflight-`. But preflight sanitizes long gang IDs and truncates them with a hash suffix. To find the ConfigMap, use the label selector above. Do not construct the name by hand.

### Grove

Preflight gang coordination does **not support** [Grove](https://github.com/ai-dynamo/grove). The gang model of Grove is **hierarchical**, with multiple nested scheduling layers. But the coordination of `preflight-nccl-allreduce` expects a **single flat PodGroup** for each gang.

Grove uses this hierarchy:

```text
PodCliqueSet
  └── PodClique / PodCliqueScalingGroup
        └── PodGang
              └── podgroups[]
                    └── podReferences[]
```

### Per-namespace gang discovery

The Helm `gangDiscovery` value sets only the **cluster-wide default**. A specific namespace can use a different gang-scheduling system. For example, one team uses Volcano, and all other teams use native Kubernetes. To do this, create a **`PreflightConfig`** custom resource in that namespace. The controller reconciles it at runtime, without a Helm upgrade or a controller restart.

Preflight resolves the discoverer for a pod as follows:

1. If the namespace of the pod has a `PreflightConfig` with a `gangDiscovery` block, preflight uses that discoverer.
2. Otherwise, preflight uses the cluster-wide `gangDiscovery` Helm value as the default.

`PreflightConfig` is a namespaced resource (`preflight.nvsentinel.nvidia.com/v1alpha1`). The namespace of the object is its scope. Its `spec.gangDiscovery` block uses the same schema as the Helm `gangDiscovery` value. An empty block selects native Kubernetes discovery.

```yaml
# team-a uses Volcano; every other namespace uses the cluster-wide default.
apiVersion: preflight.nvsentinel.nvidia.com/v1alpha1
kind: PreflightConfig
metadata:
  name: default
  namespace: team-a
spec:
  gangDiscovery:
    name: "volcano"
    annotationKeys: ["scheduling.k8s.io/group-name"]
    podGroupGVR:
      group: "scheduling.volcano.sh"
      version: "v1beta1"
      resource: "podgroups"
    minCountExpr: "podGroup.spec.minMember"
```

Keep at most one `PreflightConfig` in each namespace. If there is more than one, the **oldest** object stays active, and the name breaks a tie. As a result, a new object does not disrupt a working configuration. The controller marks the other objects as not ready, because the active object supersedes them. To check the resolved state, examine the status of the object:

```console
$ kubectl -n team-a get preflightconfig
NAME      DISCOVERER   READY   AGE
default   volcano      True    10s
```

The `Ready` status condition reports the readiness. The `READY` column above shows its status. The message of the condition explains why a configuration is not ready (invalid or superseded). To examine the condition, run `kubectl -n team-a get preflightconfig default -o yaml`. The condition is under `.status.conditions`.

#### RBAC (aggregated ClusterRole)

The controller reads the scheduler `PodGroup` resources cluster-wide through an **aggregated ClusterRole** (`{release}-gang-discovery`). The chart includes a built-in contributor role. This role covers the native `scheduling.k8s.io` resources and the default `gangDiscovery.podGroupGVR`.

For a scheduler CRD that the built-in role does not cover, apply a `ClusterRole` with the aggregation label. For example, a namespace registers Volcano, but the default is native. Kubernetes merges the new role automatically. Preflight needs no change and no restart:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: preflight-gang-discovery-volcano
  labels:
    preflight.nvsentinel.nvidia.com/aggregate-to-gang-discovery: "true"
rules:
  - apiGroups: ["scheduling.volcano.sh"]
    resources: ["podgroups"]
    verbs: ["get", "list", "watch"]
```

A `ClusterRole` is cluster-scoped, so a platform or cluster administrator must create it. A namespace tenant declares its scheduler in the `PreflightConfig`, and the platform grants the related read access. Aggregation is eventually consistent. For a short time after you add a new contributor role, the controller can get `Forbidden` errors. The controller then tries again.

The controller validates each `PreflightConfig` when it reconciles the object. It resolves the `gangDiscovery.podGroupGVR` against the API RESTMapper of the cluster. For a native spec, it verifies the `scheduling.k8s.io` resources. An invalid or unresolvable configuration does not disrupt admission. The namespace uses the default instead, and the controller shows the error in the status of the object.

#### Changing gang discovery configuration

A `PreflightConfig` change takes effect on **newly-admitted gangs**. Preflight applies it to each pod at admission. **While multi-node preflight gangs start** in a namespace, do not edit or delete the active `PreflightConfig` of that namespace. This also applies to a deletion that makes a different object active.

The gang ID contains the discoverer name (`{discoverer}-{namespace}-{podGroup}`). Preflight resolves the discovery for each pod separately. The effective discoverer of a namespace can change while a gang starts. Then pods of the same gang that the webhook admits before and after the change can get **different gang IDs**. These pods fail to coordinate, because the peers never converge.

The `preflight-nccl-allreduce` check of such a gang waits until `gangCoordination.timeout`, and then it fails. The pod stays in `Init:Error`. This gang timeout is non-fatal, so Fault Quarantine does not cordon the node. This failure is safe, because it gives no false "healthy" result. But it causes a false preflight failure. Before you change the gang discovery configuration of a namespace, make sure that no multi-node gangs start there.

It is safe to add a *second* `PreflightConfig`, because the active (oldest) object does not change (see above). The risk comes only from a change to the active configuration or from its removal.

#### Gang coordination Pod cache

If gang coordination is enabled, preflight watches Pods cluster-wide. The reason is that you can add or remove a namespace-specific `PreflightConfig` resource while preflight runs. Preflight cannot change the namespace set of its Pod cache after it starts. As a result, a cache limited to the namespaces known at startup misses the Pods of a namespace that gets a new configuration later.

To keep the cluster-wide cache small, preflight stores only these Pod data:

- the Pod identity and deletion metadata
- the annotations and labels that configurable discoverers use
- the gang ConfigMap volume
- the node and scheduling-group references
- the Pod IP and phase

Preflight discards the containers, init containers, unrelated volumes, conditions, managed fields, and other unused Pod data before it caches a Pod.

## Gang coordination

When `gangCoordination.enabled` is true (the default in the preflight chart), the controller coordinates the multi-node checks through ConfigMaps:

1. At admission time, the webhook creates a skeleton ConfigMap for the gang. It injects the ConfigMap as a volume mount on the preflight init containers of the pod.
2. When the pods become ready, the gang controller adds the peer information (IP, rank) to the ConfigMap.
3. The init containers read the ConfigMap at `gangCoordination.configMapMountPath` (default `/etc/preflight`) to find the master address and the peer list.

Each gang ConfigMap contains these keys:

| Key | Value |
|-----|-------|
| `expected_count` | The minimum number of members (from the Workload / PodGroup CRD) |
| `peers` | A newline-separated list of `podName;podIP;rank` |
| `master_addr` | The IP of the rank-0 pod |
| `master_port` | The port for the PyTorch distributed TCP bootstrap (default `29500`) |
| `gang_id` | The unique gang identifier (discoverer prefix + namespace + group) |

Each of these ConfigMaps has the label `nvsentinel.nvidia.com/managed-by: preflight` and a name with the prefix `preflight-`.

### Key `gangCoordination` values

If there is no gang to coordinate, set `enabled: false`. Examples are a single node, or a scheduler that is not gang-aware. At the same time, remove `preflight-nccl-allreduce` from `initContainers`. See [Single-node and single-GPU clusters](#single-node-and-single-gpu-clusters).

```yaml
gangCoordination:
  enabled: true
  timeout: "10m"            # Max wait for all members to register
  masterPort: 29500         # PyTorch distributed bootstrap port
  configMapMountPath: "/etc/preflight"

  # Azure InfiniBand topology (required for NDv4/v5)
  ncclTopoConfigMap: ""     # Pre-existing ConfigMap name, or use ncclTopoShape
  ncclTopoShape: ""         # "ndv4" or "ndv5" to auto-create from bundled XML

  extraHostPathMounts: []   # Host paths for NCCL/OFI/CUDA libraries
  extraVolumeMounts: []     # Mount existing pod volumes (e.g. GCP TCPXO plugin)
  # mirrorResourceClaims: true  # Mirror DRA claims to init containers (default true)
```

With `mirrorResourceClaims: true` (the default), the webhook adds each claim in `spec.resourceClaims` of a gang member pod to the injected init containers. The webhook always mirrors the claims of a DRA GPU pod, whatever the value of `mirrorResourceClaims`. See [DRA GPU pods](#dra-gpu-pods).

## DRA GPU pods

A pod that gets its GPUs through Dynamic Resource Allocation (DRA) has no GPU resource from `gpuResourceNames` (for example, `nvidia.com/gpu`). The webhook finds these pods from their resource claims and also injects the init containers into them. This applies when DRA allocates the GPUs, for example with the GPU Operator in GPUCluster mode or with the standalone NVIDIA DRA driver for GPUs.

The detection is always on and has no configuration value. Preflight does not read `global.gpuDraEnabled`. On a cluster without DRA, no pod has `spec.resourceClaims`, so the webhook makes no extra API calls.

### How the webhook detects a DRA GPU pod

For a pod with no GPU resource request, the webhook reads each entry in `spec.resourceClaims`. It reads the ResourceClaimTemplate or the ResourceClaim that the entry names. The pod is a DRA GPU pod when one device request matches these rules:

| Device request | Counts as a GPU request |
|----------------|-------------------------|
| `exactly` with `deviceClassName: gpu.nvidia.com` | Yes |
| `exactly` with `adminAccess: true` | No. Admin access is for monitoring, and other pods can use the same devices |
| `firstAvailable` where every subrequest uses `gpu.nvidia.com` | Yes |
| `firstAvailable` where one or more subrequests use another class | No. The scheduler can select the non-GPU device |
| Any request for another class | No |

One GPU request is enough. The webhook then injects the checks and copies all of the pod's resource claims into each check, including NIC and IMEX claims. The NCCL checks need the NIC and IMEX devices to test the network. The webhook adds each claim one time only.

The webhook does not apply this detection to a pod that requests a GPU resource from `gpuResourceNames`. The webhook makes no API calls for it.

### Failures

The webhook does not reject a pod because of DRA detection. If it cannot read a claim or a template, it counts that entry as non-GPU and continues. If no entry is a GPU request, the webhook admits the pod without the checks. It never injects the checks without the pod's claims, because a check without GPU access reports the node as unhealthy.

| Condition | Result | Log level |
|-----------|--------|-----------|
| Claim or template does not exist | Entry counts as non-GPU | Warn |
| RBAC error, API error, or `resource.k8s.io/v1` not served | Entry counts as non-GPU | Error |
| All reads for the pod take more than 3 seconds | Remaining entries count as non-GPU | Error |

An Error log means that the webhook counts that entry as non-GPU. The pod still gets the checks if another entry is a GPU request. An RBAC or API error usually affects every claim, so DRA GPU pods then do not get the checks. Search the preflight logs for `Failed to read DRA claim, treating it as non-GPU`.

### RBAC and API load

The chart always creates the ClusterRole `preflight-dra` and binds it to the preflight ServiceAccount. It gives `get` access to `resourceclaims` and `resourceclaimtemplates` in the `resource.k8s.io` API group, in all namespaces. It gives no `list` or `watch` access. It does not depend on gang coordination. On a cluster without DRA, the role exists but the webhook does not use it.

The webhook reads claims directly from the API server, with no cache. This keeps preflight memory usage the same on large clusters. Each DRA pod admission costs one `get` for each entry in `spec.resourceClaims`, until the webhook finds a GPU request.

## Publishing to the deployment platform connector

By default, the checks report through the node-local platform connector socket that the webhook mounts into the pod. A cluster can also run the [deployment platform connector](platform-connectors.md#deployment-platform-connector).

To use it, set `preflight.publishTo: deployment`. The checks then publish directly to the deployment platform connector, like the health monitors do. Preflight then does not need the node-local DaemonSet. This setting needs `platformConnector.deployment.enabled: true` and platform connector authentication enabled. The chart refuses the other combinations.

These changes apply to an injected pod:

- Every check gets `HEALTH_PUBLISH_TARGET`, `HEALTH_PUBLISH_TOKEN_PATH` and `HEALTH_PUBLISH_TLS_CA_FILE`. The target is the Service of the deployment platform connector. The token is the same projected token as on the socket path, minted against the pod's own ServiceAccount. The socket environment variable and mount stay as they are. The checks ignore them while the target has a value.
- The CA bundle comes from a ConfigMap named `nvsentinel-platform-connector-ca`. The preflight controller keeps this ConfigMap in every namespace that it injects into, because a pod cannot mount a Secret from the NVSentinel namespace. The controller creates it at pod admission and for every labeled namespace. It rewrites every copy when cert-manager rotates the CA. The pod mounts it at `/etc/nvsentinel/platform-connector-deployment-ca`.
- The pod gets the label `nvsentinel.nvidia.com/health-publisher: "true"`. The network policy of the deployment platform connector admits pods with this label in the namespaces that `preflight.namespaceSelector` selects. These are the namespaces that the webhook injects into. The policy cannot match workload pods on the host network. For such pods, set `preflight.hostNetworkWorkloads: true`. The chart then opens the gRPC rule to every source, and the caller token is the only gate.

A check waits at most five minutes in total for an unreachable deployment platform connector. This budget is `HEALTH_PUBLISH_RETRY_WINDOW`, a client default. One attempt lasts at most 30 seconds. After the budget expires, each later report of the same run gets one attempt of 5 seconds. As a result, a check that sends many reports, like `preflight-dcgm-diag`, waits through one outage at most.

The deployment platform connector refuses a report permanently if the report is invalid or names another node. The check then drops that report immediately.

An incomplete direct-mode environment is a configuration error. Examples are a target without a token path, a missing CA bundle without `HEALTH_PUBLISH_INSECURE`, or a bad `HEALTH_PUBLISH_RETRY_WINDOW`. A configuration error gives exit `1` for `preflight-dcgm-diag` and exit `2` for the two NCCL checks.

The [exit codes](#1-check-the-exit-code) below do not change:

- A report that the check cannot deliver still gives exit `3` for `preflight-nccl-loopback` and exit `4` for `preflight-nccl-allreduce`.
- `preflight-dcgm-diag` still keeps its diagnostic verdict.
- `STORE_ONLY` still changes a test failure into exit `0`. Configuration errors keep their exit code.

The health events, and the actions of Fault Quarantine on them, are the same on both paths.

## Key Helm values (subchart)

| Area | Location |
|------|-----------|
| Webhook TLS, failure policy, certificate provider | `preflight.webhook` |
| Where the checks publish (socket or deployment), host-network workloads | `preflight.publishTo`, `preflight.hostNetworkWorkloads` |
| Init container placement (append/prepend) | `preflight.initContainerPlacement` |
| Images and environment variables of the injected init containers | `preflight.initContainers` |
| GPU / network resource names | `preflight.gpuResourceNames`, `preflight.networkResourceNames` |
| Copy the NCCL / fabric environment variables and mounts from workload containers | `preflight.ncclEnvPatterns`, `preflight.volumeMountPatterns` |
| Gang discovery | `preflight.gangDiscovery` |
| Gang coordination (timeouts, topology, mounts) | `preflight.gangCoordination` |
| Namespace selector for the webhook | `preflight.namespaceSelector` |
| Pod-level selector for the webhook | `preflight.objectSelector` |
| Webhook log level (`debug`, `info`, `warn`, `error`) | `preflight.logLevel` |

## Webhook

These values control the TLS and the admission behavior of the mutating webhook.

```yaml
preflight:
  webhook:
    port: 8443
    failurePolicy: Fail
    timeoutSeconds: 10
    createIssuer: true
    certIssuer: ""
    caCertificateName: ""
    # certProvider: openshift-service-ca
```

### failurePolicy

This value sets what the API server does when the webhook does not answer. `Fail` rejects the pod, so a webhook outage blocks GPU pod creation in the selected namespaces. `Ignore` admits the pod without preflight checks. This keeps the availability, but it removes the gate.

### timeoutSeconds

This value sets how long the API server waits for the webhook, up to the Kubernetes maximum of 30. The default is `10`. Preflight resolves gang membership during admission, so a cluster with slow PodGroup lookups can need a larger value. But every GPU pod creation waits for this call. Increase the value only as much as the lookups need.

### createIssuer

This value makes the chart create the two-tier cert-manager CA for the webhook certificate. To use a CA that you already run, set it to `false`. Then set `certIssuer` and `caCertificateName`.

### certIssuer

The name of the existing cert-manager issuer that signs the webhook certificate. The chart uses it only when `createIssuer` is `false`.

### caCertificateName

The name of the existing CA certificate. cert-manager injects the bundle of this certificate into the webhook configuration. The chart uses it only when `createIssuer` is `false`. Without it, the API server cannot verify the webhook. Then, with `failurePolicy: Fail`, admission fails closed.

### certProvider

The default is `cert-manager`. On OpenShift, set it to `openshift-service-ca`. The built-in service-ca-operator then issues the webhook certificate instead of cert-manager.

## Object selector (pod-level filtering)

By default, the webhook intercepts all GPU pods in the labeled namespaces. To limit the intercepted pods further, set `objectSelector` with standard Kubernetes label selectors. If the value is empty (`{}`), the chart emits no `objectSelector`. The webhook then intercepts all pods in the matching namespaces.

Example: intercept only the pods that have an explicit preflight label:

```yaml
objectSelector:
  matchLabels:
    nvsentinel.nvidia.com/preflight: "enabled"
```

You can also use `matchExpressions`:

```yaml
objectSelector:
  matchExpressions:
    - key: nvsentinel.nvidia.com/preflight
      operator: In
      values: ["enabled", "true"]
```

This is useful if you select full namespaces with `namespaceSelector`, but you want to run preflight only on specific workloads in those namespaces.

For the full defaults and their comments, see `distros/kubernetes/nvsentinel/charts/preflight/values.yaml`.

## Observability

- Webhook pod: the liveness and readiness probes use `/healthz` on the webhook port.

## Debugging preflight failures

When a preflight check fails, the pod stays in `Init:Error`, and the init container exits with a non-zero code.

### 1. Check the exit code

```bash
kubectl -n {namespace} get pod {pod-name} -o jsonpath=\
'{range .status.initContainerStatuses[*]}{.name}{"\t"}{.state.terminated.exitCode}{"\n"}{end}'
```

Each check has its own codes:

| Exit code | `preflight-dcgm-diag` | `preflight-nccl-loopback` | `preflight-nccl-allreduce` |
|---|---|---|---|
| `0` | Passed, or reported a non-fatal finding | Passed | Passed |
| `1` | Fatal test failure, DCGM unreachable, or a configuration error | Test failed: bandwidth below threshold, or the benchmark did not run | Test failed: bandwidth degraded, all-reduce timeout, or NCCL init failure |
| `2` | — | Configuration error | Gang configuration error (`NCCL_GANG_CONFIG_ERROR`) |
| `3` | — | Failed to send the health event | Gang formation timed out (`NCCL_GANG_TIMEOUT`) |
| `4` | — | — | Failed to send the health event |

The exit code decides whether the pod starts. The health event decides what happens to the node. Fault Quarantine cordons the node only for preflight health events with `isFatal: true`. Hardware findings and an unreachable DCGM hostengine are fatal. Other coordination and configuration problems are not. As a result, a gang timeout holds the pod in `Init:Error`, but Fault Quarantine does not cordon the node.

A check that fails before it can report sends no health event. A `preflight-dcgm-diag` configuration error (exit `1`) and a `preflight-nccl-loopback` configuration error (exit `2`) both block the pod. They leave no health event to find. `preflight-nccl-allreduce` reports the benchmark result only from the rank-0 pod. If the benchmark fails, the other pods of the gang exit `1` and send no health event.

When `processingStrategy` is `STORE_ONLY`, a check reports its result and then exits `0`. It logs `Check failed (STORE_ONLY — not blocking pod)`. NVSentinel records the health event, and the workload still starts. The gang stage of `preflight-nccl-allreduce` runs before the benchmark, and it returns exit `2` or `3` directly. For this reason, `STORE_ONLY` does not unblock a gang failure.

#### A check can exit `0` and still report an unhealthy GPU

`preflight-dcgm-diag` sends one health event for each DCGM test result. Three outcomes report a problem, but they do not fail the gate:

- A test result of `warn`.
- A test result of `fail` whose error code has no actionable remediation, for example `DCGM_FR_XID_ERROR`.
- A `DCGM_ST_*` status that still prevents the completion of the diagnostic after `DCGM_DIAG_STATUS_RETRY_MAX_ATTEMPTS` attempts.

Each outcome emits an unhealthy health event with `isFatal: false`, and the check exits `0`. As a result, a warning or an infrastructure problem does not block the workload. The node then has a preflight health event and a node condition, but it stays schedulable. A non-fatal health event can still have a recommended action. Read the health event. Do not infer the finding from the exit code.

### 2. Check health events

All three checks (`preflight-dcgm-diag`, `preflight-nccl-loopback`, `preflight-nccl-allreduce`) report their results as health events. From the root of the NVSentinel repository, query them with the MongoDB shell helper:

```bash
./scripts/mongodb-shell.sh
```

When the shell connects, filter for the failing node (replace `{NODE_NAME}`):

```javascript
db.HealthEvents.find({"healthevent.nodename": "{NODE_NAME}"}).pretty()
```

For more query examples, see [Connecting to the Datastore](../runbooks/datastore-connection.md).

The init container logs are not always available, for example after the deletion of the pod. In that case, use the health events and node conditions instead:

```bash
kubectl describe node {node-name} | grep -A3 Conditions
```

### 3. Act on the failure

- **`preflight-dcgm-diag` failed** — the GPU or the node is in a bad state. Repair the node (reboot or terminate it).
- **`preflight-nccl-loopback` or `preflight-nccl-allreduce` failed** — the default `BW_THRESHOLD_GBPS` (`150` for loopback, `100` for all-reduce) assumes NVLink. It is too high for a PCIe-only interconnect or for lower-bandwidth GPUs (for example, L40S sustains ~20 GB/s). With this hardware, the check never passes. Do these steps:
  1. Confirm that the `BW_THRESHOLD_GBPS` value is realistic for the hardware.
  2. If the threshold is too high for your GPUs, decrease it to the expected value for that interconnect.
  3. If the threshold is already correct for the hardware, do **not** decrease it to force a pass. Run the check again one time to exclude a false positive.
  4. If the check fails again, examine the health event. Then do the necessary action, such as a reboot or a termination of the node.

## Related documentation

- Helm chart guide (`distros/kubernetes/README.md` in the NVSentinel repository)
