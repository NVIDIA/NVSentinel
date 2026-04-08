# Preflight

## Overview

Preflight validates GPU health **before** your workload starts. It is a mutating admission webhook that injects diagnostic init containers into GPU pods, catching hardware and interconnect failures at pod creation time rather than mid-training.

Think of it as a pre-flight checklist for an aircraft — you want to know the engines work before takeoff, not after.

### Why Do You Need This?

NVSentinel's health monitors (GPU, syslog, CSP) continuously detect failures at runtime and trigger quarantine. Preflight complements them by adding a **point-in-time gate** at pod creation:

- **Timing gap**: A GPU can degrade between the last health-monitor poll and job startup. Preflight closes that window with an on-demand check right before the workload runs
- **Interconnect coverage**: Runtime monitors focus on individual GPU health. Preflight's NCCL checks validate the actual communication path — NVLink, PCIe, InfiniBand, EFA — that a distributed job will use
- **Fast failure**: Without preflight, a bad interconnect is typically discovered minutes into training when NCCL operations hang or bandwidth drops. Preflight fails the pod in seconds, before any compute is wasted
- **Multi-node validation**: Single-node monitors can't verify cross-node fabric health. The gang-aware `nccl-allreduce` check exercises the real multi-node path end-to-end

If a preflight check fails, the pod stays in `Init:Error`, a health event enters the standard NVSentinel pipeline, and the node proceeds through quarantine and remediation — the same workflow the runtime monitors use.

## How It Works

Preflight runs as a Deployment with a mutating admission webhook:

1. **Namespace opt-in**: Label namespaces with `nvsentinel.nvidia.com/preflight=enabled`
2. **Pod admission**: When a GPU pod is created in a labeled namespace, the webhook intercepts the request. Optionally, an `objectSelector` can further restrict which pods are intercepted based on pod labels
3. **Init container injection**: The webhook prepends diagnostic init containers to the pod spec
4. **Checks run**: Init containers execute sequentially before the main workload starts
5. **Health reporting**: Each check reports results as health events via the Platform Connector (gRPC over Unix domain socket)
6. **Pass/fail**: If all checks pass (exit code 0), the main containers start normally. If any check fails, the pod stays in `Init:Error` and a health event triggers quarantine

### Available Checks

| Check | Scope | What it validates | Approximate duration |
|-------|-------|-------------------|---------------------|
| `preflight-dcgm-diag` | Single node | GPU hardware health via DCGM diagnostics (ECC, PCIe, thermal, stress) | 30s–15min depending on diag level |
| `preflight-nccl-loopback` | Single node | Intra-node GPU-to-GPU interconnect (NVLink or PCIe) via NCCL all-reduce | ~5s |
| `preflight-nccl-allreduce` | Multi-node (gang) | Cross-node GPU communication over the cluster fabric (InfiniBand, EFA, TCPXO) | ~30s + gang wait time |

All checks are optional — configure which ones to inject via `initContainers` in the Helm values.

### Gang Coordination (Multi-Node Checks)

The `preflight-nccl-allreduce` check requires coordination across all nodes in a scheduling gang. Preflight handles this through:

1. **Gang discovery** — identifies which pods belong to the same group using scheduler annotations/labels (Volcano, Run:ai/OSMO, or native Kubernetes WorkloadRef)
2. **ConfigMap-based coordination** — the gang controller writes peer information (IPs, ranks) into a ConfigMap that init containers poll until all members are registered
3. **PyTorch distributed bootstrap** — once all peers are known, the check uses `torchrun` to execute a multi-node NCCL all-reduce benchmark

Gang coordination requires a gang-aware scheduler and `gangCoordination.enabled: true` (default).

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
| `preflight.initContainers` | Which checks to inject, their images, env vars, and resource limits |
| `preflight.dcgm` | DCGM hostengine endpoint, diagnostic level, processing strategy |
| `preflight.gangDiscovery` | Scheduler-specific gang identification (Volcano, Run:ai, native K8s) |
| `preflight.gangCoordination` | Multi-node coordination timeouts, NCCL topology, extra mounts |
| `preflight.webhook` | TLS, failure policy, cert provider |
| `preflight.namespaceSelector` | Which namespaces the webhook applies to |
| `preflight.objectSelector` | Which pods the webhook applies to (label-based filtering within selected namespaces) |

For detailed configuration including per-check env vars, fabric-specific NCCL setup, and gang discovery examples, see [Preflight configuration](./configuration/preflight.md).

## Related Documentation

- [Preflight configuration guide](./configuration/preflight.md) — full Helm values reference
- [ADR-026: Preflight checks](./designs/026-preflight-checks.md) — architecture and design rationale
- [GPU Health Monitor](./gpu-health-monitor.md) — continuous runtime GPU monitoring (complementary to preflight)
