# Preflight configuration

Preflight is a mutating admission webhook that injects GPU diagnostic init containers (DCGM diagnostics, optional NCCL loopback / all-reduce) into pods that request GPUs in namespaces you opt in via labels. It answers "is this GPU healthy enough to start?" before your workload runs—separate from continuous [GPU health monitoring](../gpu-health-monitor.md).

For architecture, tradeoffs, and metrics design, see [ADR-026: Preflight checks](../designs/026-preflight-checks.md).

## Prerequisites

- Helm subchart is off by default; enable with `global.preflight.enabled` (see below).
- cert-manager (or OpenShift service CA) for webhook TLS—same expectation as the rest of the NVSentinel chart.
- DCGM reachable from injected init containers (typically the NVIDIA GPU Operator's DCGM / hostengine service). The chart shares DCGM settings with the GPU health monitor where applicable (`global.dcgm`).
- Multi-node / gang checks (e.g. `preflight-nccl-allreduce`): enable gang coordination and configure gang discovery for your scheduler (see below).

## Enable preflight

1. Set the global flag:

```yaml
global:
  preflight:
    enabled: true
```

2. Configure the preflight subchart under the top-level `preflight:` key (values merge into `distros/kubernetes/nvsentinel/charts/preflight/values.yaml`). At minimum, review `initContainers`, `webhook.failurePolicy`, and DCGM connectivity.

3. Label namespaces where injection should apply:

```bash
kubectl label namespace <namespace> nvsentinel.nvidia.com/preflight=enabled
```

The chart default `namespaceSelector` matches that label.

## Init containers (check configuration)

The `initContainers` list in the preflight chart defines which checks the webhook injects. Each entry is a standard `corev1.Container` — you control images, env vars, resource limits, security contexts, and volume mounts.

The webhook automatically injects these env vars into every init container (you do not need to set them):

| Env var | Source | Purpose |
|---------|--------|---------|
| `NODE_NAME` | Downward API (`spec.nodeName`) | Kubernetes node name for health events |
| `PLATFORM_CONNECTOR_SOCKET` | Chart `dcgm.connectorSocket` | Unix socket for the platform-connector gRPC endpoint |
| `PROCESSING_STRATEGY` | Chart `dcgm.processingStrategy` | `EXECUTE_REMEDIATION` or `STORE_ONLY` — controls downstream action |

For gang-aware containers the webhook also injects `GANG_ID`, `GANG_CONFIG_DIR`, `GANG_TIMEOUT_SECONDS`, and `POD_NAME`.

### preflight-dcgm-diag

Runs DCGM diagnostics against every GPU allocated to the pod via the remote hostengine.

The webhook auto-injects `DCGM_DIAG_LEVEL` and `DCGM_HOSTENGINE_ADDR` from the chart-level `dcgm` block. Override per-container if needed.

| Env var | Default | Description |
|---------|---------|-------------|
| `DCGM_DIAG_LEVEL` | `2` (from chart `dcgm.diagLevel`) | Diagnostic depth: 1 = short (approx 30 s, software deployment checks), 2 = medium (approx 2 min, adds PCIe and basic GPU stress), 3 = long (approx 15 min, adds Diagnostic plugin stress), 4 = xlong (1-2 hr, extended stress) |
| `DCGM_HOSTENGINE_ADDR` | `nvidia-dcgm.gpu-operator.svc:5555` | DCGM hostengine gRPC endpoint |

Chart-level DCGM settings (apply to dcgm-diag):

```yaml
dcgm:
  service:
    endpoint: "nvidia-dcgm.gpu-operator.svc"
    port: 5555
  diagLevel: 2
  processingStrategy: "EXECUTE_REMEDIATION"
```

### preflight-nccl-loopback

Single-node NCCL all-reduce across all GPUs on the node. Validates intra-node interconnect (NVLink or PCIe).

| Env var | Default | Description |
|---------|---------|-------------|
| `BW_THRESHOLD_GBPS` | `150` | Minimum acceptable bus bandwidth in GB/s. NVLink interconnect typically sustains 150+ GB/s; set to approx 15 GB/s for PCIe interconnect |
| `TEST_SIZE_MB` | `256` | Message size in MB for the all-reduce benchmark |
| `SKIP_BANDWIDTH_CHECK` | `false` | When `true`, pass if the benchmark completes regardless of measured bandwidth |

Example values override:

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

Multi-node NCCL all-reduce across the entire gang. Requires `gangCoordination.enabled: true` and a gang-aware scheduler.

| Env var | Default | Description |
|---------|---------|-------------|
| `BW_THRESHOLD_GBPS` | `100` | Minimum acceptable bus bandwidth in GB/s |
| `MESSAGE_SIZES` | `4G` | Comma-separated message sizes for the benchmark (e.g. `"4G"`, `"4G,8G"`). Code default is `4G,8G`; Helm chart overrides to `4G` |
| `BENCHMARK_ITERS` | `20` | Number of timed iterations per message size |
| `WARMUP_ITERS` | `5` | Warmup iterations before timing begins |
| `NCCL_REDUCE_OP` | `sum` | Reduction operation (`sum`, `prod`, `min`, `max`, `avg`) |
| `SKIP_BANDWIDTH_CHECK` | `false` | Pass if benchmark completes regardless of bandwidth |
| `NCCL_DEBUG` | — | NCCL log verbosity (`INFO`, `WARN`, etc.) |
| `NCCL_DEBUG_SUBSYS` | — | NCCL subsystems to log (`INIT,NET`, etc.) |

The container also requires `IPC_LOCK` capability for RDMA memory registration:

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

In production the webhook automatically copies NCCL env vars and volume mounts from the pod's main containers into preflight init containers using glob patterns:

```yaml
ncclEnvPatterns:    ["NCCL_*", "FI_*", "LD_LIBRARY_PATH", "UCX_*", "TORCH_NCCL_*", "CUDA_DEVICE_ORDER"]
volumeMountPatterns: ["host-opt-amazon*", "nvtcpxo-*", "nccl-*", "dev-shm"]
```

This means if your training container already has the correct `NCCL_TOPO_FILE`, `FI_PROVIDER`, or `LD_LIBRARY_PATH`, the preflight init containers inherit them with no manual configuration.

For standalone testing (e.g. busybox main container), use `ncclAllreduceExtraEnv` and `gangCoordination.extraHostPathMounts` to provide fabric config explicitly.

## Gang discovery

Gang discovery identifies pods that belong to the same scheduling group so multi-node preflight checks (NCCL all-reduce) know their peers. A pod carries a "gang anchor"—a reference to a parent object—that holds gang metadata such as the minimum member count.

Two discovery mechanisms are supported:

### Native Kubernetes (1.35+): workloadRef

> The `Workload` resource (`scheduling.k8s.io/v1alpha1`) and `spec.workloadRef` are alpha in Kubernetes 1.35 and disabled by default. Enable the `GenericWorkload` feature gate on the API server and scheduler to use this path.

The default when `gangDiscovery` is left empty (`{}`). Each pod links to a `Workload` resource via `spec.workloadRef`:

```yaml
spec:
  workloadRef:
    name: training-job-workload
    podGroup: workers
```

The Workload object contains a gang policy with `minCount`:

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

No `gangDiscovery` configuration is needed for this path.

### PodGroup-based schedulers (Volcano, Run:ai / OSMO, and similar)

For schedulers that use PodGroup CRDs, configure `gangDiscovery` with:

| Field | Purpose |
|-------|---------|
| `name` | Discoverer identifier, used in the gang ID prefix and logging (e.g. `"volcano"`) |
| `annotationKeys` | Pod annotation keys checked (in order) for the PodGroup name |
| `labelKeys` | Optional pod label keys checked as fallback |
| `podGroupGVR` | `group`, `version`, `resource` of the PodGroup CRD |
| `minCountExpr` | CEL expression to extract the minimum member count from the PodGroup object. Receives `podGroup` as the unstructured object. Default: `"podGroup.spec.minMember"` |

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

Volcano sets the `scheduling.k8s.io/group-name` annotation on each pod. The discoverer reads that annotation, fetches the corresponding `PodGroup` CRD, and evaluates `minCountExpr` to determine expected gang size.

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

Here membership is determined by a pod label instead of an annotation. The rest of the flow is the same: look up the PodGroup CRD and extract `minCount` via CEL.

## Gang coordination

When `gangCoordination.enabled` is true (default in the preflight chart), the controller coordinates multi-node checks through ConfigMaps:

1. At admission time the webhook creates a skeleton ConfigMap for the gang and injects it as a volume mount on the pod's preflight init containers.
2. As pods become ready the gang controller populates the ConfigMap with peer information (IP, rank).
3. Init containers read the ConfigMap at `gangCoordination.configMapMountPath` (default `/etc/preflight`) to discover the master address and peer list.

Each gang ConfigMap contains:

| Key | Value |
|-----|-------|
| `expected_count` | Minimum members needed (from the Workload / PodGroup CRD) |
| `peers` | Newline-separated list of `podName;podIP;rank` |
| `master_addr` | IP of the rank-0 pod |
| `master_port` | Port for PyTorch distributed TCP bootstrap (default `29500`) |
| `gang_id` | Unique gang identifier (discoverer prefix + namespace + group) |

ConfigMaps are labeled `nvsentinel.nvidia.com/managed-by: preflight` and named with a `preflight-` prefix.

### Key `gangCoordination` values

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

For DRA / device claims mirrored into init containers, see [ADR-026 §DRA Integration](../designs/026-preflight-checks.md) and `mirrorResourceClaims` above.

## Key Helm values (subchart)

| Area | Location |
|------|-----------|
| Webhook TLS, failure policy, cert provider | `preflight.webhook` |
| Injected init container images and env | `preflight.initContainers` |
| GPU / network resource names | `preflight.gpuResourceNames`, `preflight.networkResourceNames` |
| Copy NCCL / fabric env and mounts from user containers | `preflight.ncclEnvPatterns`, `preflight.volumeMountPatterns` |
| Gang discovery | `preflight.gangDiscovery` |
| Gang coordination (timeouts, topology, mounts) | `preflight.gangCoordination` |
| Namespace selector for the webhook | `preflight.namespaceSelector` |
| Pod-level selector for the webhook | `preflight.objectSelector` |

## Object selector (pod-level filtering)

By default the webhook intercepts all GPU pods in labeled namespaces. To further restrict which pods are intercepted, set `objectSelector` with standard Kubernetes label selectors. When empty (`{}`), no `objectSelector` is emitted and all pods in matching namespaces are intercepted.

Example — only intercept pods explicitly labeled for preflight:

```yaml
objectSelector:
  matchLabels:
    nvsentinel.nvidia.com/preflight: "enabled"
```

`matchExpressions` are also supported:

```yaml
objectSelector:
  matchExpressions:
    - key: nvsentinel.nvidia.com/preflight
      operator: In
      values: ["enabled", "true"]
```

This is useful when you want namespace-wide opt-in via `namespaceSelector` but only run preflight on specific workloads within those namespaces.

Full defaults and comments: `distros/kubernetes/nvsentinel/charts/preflight/values.yaml`.

Tilt development often trims init containers to DCGM-only; see `distros/kubernetes/nvsentinel/values-tilt.yaml`.

## Observability

- Webhook pod: liveness/readiness probes use `/healthz` on the webhook port.
- Prometheus metric names for check containers and the injector are specified in [ADR-026 § Metrics](../designs/026-preflight-checks.md#metrics); wire scrapers to your init container images and deployment as your environment allows.

## Related documentation

- [ADR-026: Preflight checks](../designs/026-preflight-checks.md)
- [gRPC / TLS authentication](../designs/030-grpc-tls-authentication.md) (mentions preflight among webhooks)
- [Helm chart README](../../distros/kubernetes/README.md)
- E2E test entry point: `tests/preflight_test.go` (build tag `amd64_group`)
