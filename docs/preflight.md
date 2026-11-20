# Preflight

## Overview

Preflight validates GPU health **before** your workload starts. It is a mutating admission webhook that injects diagnostic init containers into GPU pods. The init containers find hardware and interconnect failures at pod creation time, not in the middle of training.

Preflight is similar to the pre-flight checklist of an aircraft. A pilot confirms that the engines operate before takeoff, not after.

### Why Do You Need This?

The NVSentinel health monitors (GPU, syslog, CSP) detect failures continuously at runtime and trigger quarantine. Preflight complements them with a **point-in-time gate** at pod creation:

- **Timing gap**: A GPU can degrade between the last poll of a health monitor and the start of the job. Preflight removes this gap with an on-demand check immediately before the workload runs
- **Interconnect coverage**: Runtime monitors examine the health of individual GPUs. The preflight NCCL checks validate the real communication links that a distributed job uses, such as NVLink, PCIe, InfiniBand and EFA
- **Fast failure**: Without preflight, a bad interconnect usually becomes visible some minutes after training starts, when NCCL operations hang or the bandwidth drops. Preflight fails the pod in seconds, before the job wastes compute resources
- **Multi-node validation**: Single-node monitors cannot verify the health of the cross-node fabric. The gang-aware `nccl-allreduce` check tests the real multi-node path from end to end

If a preflight check fails, the pod stays in `Init:Error`. Unless the check fails before it can report, it also sends a health event into the standard NVSentinel pipeline. For `preflight-nccl-allreduce`, only the rank-0 pod reports the result of the benchmark.

The fatal health events are the hardware and interconnect findings, and an unreachable DCGM hostengine. They send the node through quarantine and remediation, the same workflow that the runtime monitors use. As a result, if the `DCGM_HOSTENGINE_ADDR` value is wrong, NVSentinel cordons the node. If a check reports any other coordination or configuration failure, it marks the failure as non-fatal. These failures block the pod, but NVSentinel does not cordon the node.

## How It Works

Preflight runs as a Deployment with a mutating admission webhook:

1. **Namespace opt-in**: Label namespaces with `nvsentinel.nvidia.com/preflight=enabled`
2. **Pod admission**: The webhook intercepts each request that creates a GPU pod in a labeled namespace. An optional `objectSelector` can also limit the intercepted pods by their pod labels
3. **Init container injection**: The webhook injects diagnostic init containers into the pod spec. By default, it adds them after the existing init containers. To add them before the existing init containers, set `initContainerPlacement: prepend`
4. **Checks run**: The init containers run in sequence before the main workload starts
5. **Health reporting**: Each check reports its results as health events to the platform connector. If `preflight.publishTo` is `deployment`, the check sends them directly to the deployment platform connector. Otherwise, it uses gRPC over the node-local Unix domain socket
6. **Pass/fail**: If all checks pass (exit code 0), the main containers start normally. If any check fails, the pod stays in `Init:Error`. If the failed check sends a fatal health event, that health event triggers quarantine. For the meaning of each exit code of each check, see [exit codes](./configuration/preflight.md#1-check-the-exit-code)

### Available Checks

| Check | Scope | What it validates | Approximate duration |
|-------|-------|-------------------|---------------------|
| `preflight-dcgm-diag` | Single node | GPU hardware health with DCGM diagnostics (ECC, PCIe, thermal, stress) | 30s–15min, depending on the diagnostic level |
| `preflight-nccl-loopback` | Single node | Intra-node GPU-to-GPU interconnect (NVLink or PCIe) with NCCL all-reduce | ~5s |
| `preflight-nccl-allreduce` | Multi-node (gang) | Cross-node GPU communication over the cluster fabric (InfiniBand, EFA, TCPXO) | ~30s + gang wait time |

All checks are optional. To select the checks that the webhook injects, configure `initContainers` in the Helm values.

### Gang Coordination (Multi-Node Checks)

The `preflight-nccl-allreduce` check requires coordination across all nodes in a scheduling gang. Preflight uses these mechanisms for the coordination:

1. **Gang discovery** — finds the pods that belong to the same group. It uses scheduler annotations or labels (Volcano, Run:ai/OSMO), or native Kubernetes APIs (`workloadRef` in 1.35, `schedulingGroup` in 1.36 and later)
2. **ConfigMap-based coordination** — the gang controller writes peer information (IPs, ranks) into a ConfigMap. The init containers poll this ConfigMap until it lists all members
3. **PyTorch distributed bootstrap** — when the check knows all peers, it uses `torchrun` to run a multi-node NCCL all-reduce benchmark

Gang coordination requires a gang-aware scheduler and `gangCoordination.enabled: true` (the default). The native path needs Kubernetes 1.35 or later. On an earlier cluster, configure a PodGroup-based scheduler or disable gang coordination. For a single node, a single GPU, or a cluster without a gang-aware scheduler, see [Single-node and single-GPU clusters](./configuration/preflight.md#single-node-and-single-gpu-clusters).

## Configuration

Enable preflight in the parent chart:

```yaml
global:
  preflight:
    enabled: true
```

Key configuration areas:

| Area | Description |
|------|-------------|
| `preflight.initContainers` | The checks to inject, with their images, environment variables and resource limits. The `preflight-dcgm-diag` container holds the DCGM configuration (`DCGM_HOSTENGINE_ADDR`, `DCGM_DIAG_LEVEL`) as environment variables |
| `preflight.gangDiscovery` | Gang identification for each scheduler (Volcano, Run:ai, native Kubernetes) |
| `preflight.gangCoordination` | Timeouts for multi-node coordination, NCCL topology, extra mounts |
| `preflight.webhook` | TLS, failure policy, certificate provider |
| `preflight.namespaceSelector` | The namespaces that the webhook applies to |
| `preflight.objectSelector` | The pods that the webhook applies to (filter by label in the selected namespaces) |

For the full configuration, see [Preflight configuration](./configuration/preflight.md). It includes the environment variables of each check, the NCCL configuration for each fabric, and examples of gang discovery.

## Related Documentation

- [Preflight configuration guide](./configuration/preflight.md) — full reference of the Helm values
- [GPU Health Monitor](./gpu-health-monitor.md) — continuous GPU monitoring at runtime (it complements preflight)
