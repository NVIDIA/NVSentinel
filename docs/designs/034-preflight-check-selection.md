# ADR-034: Feature — Per-Pod Preflight Check Selection

## Context

Preflight injects all configured init containers into every GPU pod in a labeled namespace. There is no way for workload owners to control which checks run on their pods without the platform team creating separate namespaces or Helm releases.

Real clusters need this flexibility:

- Interactive jobs want a fast DCGM-only check (~30s), not the full suite
- Batch training jobs need NCCL all-reduce validation that interactive jobs can skip
- Some teams handle DCGM diagnostics externally and only want NCCL checks

A secondary problem: if pods in a gang run different checks, collective-op checks like `nccl-allreduce` deadlock. One pod waits for peers that never join the NCCL communicator.

## Decision

Pods select which preflight checks to run via a simple annotation listing init container names. Gang members are validated for consistency before torchrun launches.

## Implementation

### Pod annotation

Pods annotate with a comma-separated list of check names:

```yaml
metadata:
  annotations:
    nvsentinel.nvidia.com/preflight-checks: "preflight-dcgm-diag,preflight-nccl-loopback"
```

Only named containers are injected, in the order they appear in the annotation. When no annotation is present, chart order is used. Duplicate or unknown names reject admission with an error. An empty value disables all checks:

```yaml
nvsentinel.nvidia.com/preflight-checks: ""
```

### Default behavior (no annotation)

Each init container in `values.yaml` can set `defaultEnabled`:

```yaml
initContainers:
  - name: preflight-dcgm-diag
    # defaultEnabled: true  (default when omitted)
    image: ...
  - name: preflight-nccl-allreduce
    defaultEnabled: false
    image: ...
```

When the annotation is absent, containers with `defaultEnabled: true` (or omitted, which defaults to true) are injected. When the annotation is present, it overrides `defaultEnabled` entirely.

### Example: multiple DCGM diag levels for different workloads

Define three DCGM containers in `values.yaml`, each with a different diagnostic level. Only the fast check is enabled by default — jobs opt into deeper diagnostics via annotation:

```yaml
initContainers:
  # Quick hardware check (~30s) — runs on every GPU pod by default
  - name: preflight-dcgm-fast
    image:
      repository: ghcr.io/nvidia/nvsentinel/preflight-dcgm-diag
      tag: ""
    env:
      - name: DCGM_DIAG_LEVEL
        value: "1"

  # Medium diagnostics (~2 min) — opt-in via annotation
  - name: preflight-dcgm-medium
    defaultEnabled: false
    image:
      repository: ghcr.io/nvidia/nvsentinel/preflight-dcgm-diag
      tag: ""
    env:
      - name: DCGM_DIAG_LEVEL
        value: "2"

  # Full stress test (~15 min) — opt-in via annotation
  - name: preflight-dcgm-full
    defaultEnabled: false
    image:
      repository: ghcr.io/nvidia/nvsentinel/preflight-dcgm-diag
      tag: ""
    env:
      - name: DCGM_DIAG_LEVEL
        value: "3"

  - name: preflight-nccl-loopback
    image: ...
```

Jobs select the level they need:

```yaml
# Interactive notebook — fast check only (default, no annotation needed)
apiVersion: v1
kind: Pod
metadata:
  name: notebook
spec: ...
```

```yaml
# Batch training — medium DCGM + NCCL loopback
apiVersion: v1
kind: Pod
metadata:
  name: training-job
  annotations:
    nvsentinel.nvidia.com/preflight-checks: "preflight-dcgm-medium,preflight-nccl-loopback"
spec: ...
```

```yaml
# Post-maintenance validation — full stress test, no NCCL
apiVersion: v1
kind: Pod
metadata:
  name: gpu-validation
  annotations:
    nvsentinel.nvidia.com/preflight-checks: "preflight-dcgm-full"
spec: ...
```

All three use the same DCGM image with different env vars. The platform team defines the levels once in the chart; workload owners pick by name.

### Gang validation

The peer line format in gang ConfigMaps is extended from 3 to 4 fields:

```text
pod-0;10.0.0.1;0;preflight-dcgm-diag,preflight-nccl-allreduce
pod-1;10.0.0.2;1;preflight-dcgm-diag,preflight-nccl-allreduce
```

The 4th field (`checkNames`) is a comma-separated list of injected check names. Old 3-field lines parse with an empty check names field for backward compatibility.

Before launching torchrun, the NCCL all-reduce init container calls `validate_peers()`. If any peer has a different check list, it logs the mismatch, reports `GANG_CONFIG_ERROR`, and exits immediately instead of hanging.

### Key files

| File | Change |
|------|--------|
| `preflight/pkg/config/config.go` | `InitContainerSpec` wrapper with `DefaultEnabled *bool` |
| `preflight/pkg/webhook/injector.go` | `selectInitContainers()`, `ParseCheckNames()`, `GangContext.CheckNames` |
| `preflight/pkg/gang/coordinator/coordinator.go` | 4-field peer line serialization and parsing |
| `preflight/pkg/gang/types/types.go` | `CheckNames` field on `PeerInfo` |
| `preflight/pkg/controller/gang_controller.go` | Reads annotation, normalizes into `peer.CheckNames` |
| `preflight-checks/nccl-allreduce/nccl_allreduce/gang.py` | `PeerInfo.check_names`, `GangConfig.validate_peers()` |
| `preflight-checks/nccl-allreduce/scripts/entrypoint.py` | Calls `validate_peers()` before torchrun |

## Rationale

- **Simple**: a pod annotation is the lightest-weight per-pod override. No CRD, no dynamic client, no extra RBAC.
- **Consistent with K8s patterns**: annotations for admission webhook hints are standard (e.g., Istio sidecar injection uses `sidecar.istio.io/inject`).
- **Safe**: gang validation catches mismatches at startup instead of 10+ minutes into a deadlocked torchrun.
- **Backward compatible**: no annotation means all `defaultEnabled` checks run, same as today. Old peer lines parse without errors.

## Consequences

### Positive

- Workload owners can select checks without platform team involvement
- No new CRD to deploy, version, or grant RBAC for
- Gang deadlocks from inconsistent check config are caught in seconds
- Zero overhead when annotation is absent — existing behavior unchanged

### Negative

- Annotation cannot override env vars (e.g., DCGM diag level) — that stays in `values.yaml` or on the init container spec
- Annotation is free-form text, though typos in container names are caught — admission is rejected with an error listing the unknown names and the available checks
- An empty annotation disables all checks, which may be surprising if set accidentally

### Mitigations

- Env var overrides can be addressed separately if needed; the annotation mechanism is orthogonal
- Unknown container names reject admission with an error listing the invalid names and configured checks

## Alternatives Considered

### PreflightProfile CRD

A namespaced CRD that pods reference by name, supporting both check selection and env var overrides.

**Not chosen** because: the added complexity (CRD definition, dynamic client, RBAC for CRD access, profile.go reader) is not justified when the primary need is check selection. Env var overrides can be handled by defining them inline on the init container in `values.yaml`.

## References

- [ADR-026: Preflight checks](./026-preflight-checks.md) — base preflight architecture
