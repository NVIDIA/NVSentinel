# ADR-034: Per-Pod Preflight Configuration via PreflightProfile CRD

## Context

NVSentinel's preflight webhook injects GPU diagnostic init containers into
pods that request GPU resources. Today the set of checks, their environment
variables, and enabled/disabled state are configured at the Helm chart level
and apply uniformly to every intercepted pod in a namespace.

Real clusters need more flexibility:

- **Fast-check profiles** for interactive jobs (DCGM level 1 only, skip NCCL).
- **Full-diagnostics profiles** for batch training (DCGM level 3 + NCCL
  all-reduce).
- **NCCL-only profiles** when DCGM diagnostics are handled externally.
- Per-workload bandwidth thresholds or message sizes.

A multi-layer configuration model lets platform teams set safe defaults at
the chart level while allowing workload owners to override per pod.

## Problem

1. Operators cannot tune preflight checks per workload without creating
   separate namespaces or Helm releases.
2. Gang-coordinated checks (NCCL all-reduce) can silently deadlock when one
   pod in a gang disables a check that other pods expect to run collectively.
3. There is no mechanism to protect operator-controlled env vars (e.g.
   `PLATFORM_CONNECTOR_SOCKET`) from being overridden by workload owners.

## Solution: PreflightProfile CRD

Introduce a `PreflightProfile` namespaced CRD
(`nvsentinel.nvidia.com/v1alpha1`) that lets workload owners customise which
init containers are enabled and override their env vars.

### CRD schema

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: PreflightProfile
metadata:
  name: fast-check
  namespace: training
spec:
  initContainers:
    - name: preflight-dcgm-diag
      enabled: true
      env:
        - name: DCGM_DIAG_LEVEL
          value: "1"
    - name: preflight-nccl-allreduce
      enabled: false
```

### Example profiles

**fast-check** -- quick DCGM level 1, no NCCL:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: PreflightProfile
metadata:
  name: fast-check
spec:
  initContainers:
    - name: preflight-dcgm-diag
      enabled: true
      env:
        - name: DCGM_DIAG_LEVEL
          value: "1"
    - name: preflight-nccl-loopback
      enabled: false
    - name: preflight-nccl-allreduce
      enabled: false
```

**full-diagnostics** -- DCGM level 3 + NCCL all-reduce:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: PreflightProfile
metadata:
  name: full-diagnostics
spec:
  initContainers:
    - name: preflight-dcgm-diag
      env:
        - name: DCGM_DIAG_LEVEL
          value: "3"
    - name: preflight-nccl-allreduce
      enabled: true
      env:
        - name: BW_THRESHOLD_GBPS
          value: "200"
```

**nccl-only** -- skip DCGM, run NCCL all-reduce with custom sizes:

```yaml
apiVersion: nvsentinel.nvidia.com/v1alpha1
kind: PreflightProfile
metadata:
  name: nccl-only
spec:
  initContainers:
    - name: preflight-dcgm-diag
      enabled: false
    - name: preflight-nccl-allreduce
      enabled: true
      env:
        - name: MESSAGE_SIZES
          value: "1G,4G,8G"
```

### Referencing a profile from a pod

Pods reference a profile via annotation:

```yaml
metadata:
  annotations:
    nvsentinel.nvidia.com/preflight-profile: "fast-check"
```

The webhook reads the annotation, fetches the named `PreflightProfile` from
the pod's namespace, and merges it with the chart-level defaults.

## Precedence

Configuration is resolved in layers, highest priority last:

| Layer | Source | Who controls | Example |
|-------|--------|-------------|---------|
| 1 (base) | Helm `values.yaml` | Platform team | Init container images, default env |
| 2 (profile) | `PreflightProfile` CRD | Workload owner | `DCGM_DIAG_LEVEL=1`, disable NCCL |
| 3 (protected) | Webhook hardcoded | NVSentinel | `NODE_NAME`, `PLATFORM_CONNECTOR_SOCKET` |

Layer 3 (protected env vars) always wins. The webhook overwrites these
regardless of what the profile or Helm values specify.

### Protected env vars

The following env vars are injected by the webhook and cannot be overridden
by profiles:

| Env var | Source |
|---------|--------|
| `NODE_NAME` | Downward API (`spec.nodeName`) |
| `PLATFORM_CONNECTOR_SOCKET` | Chart `dcgm.connectorSocket` |
| `PROCESSING_STRATEGY` | Chart `dcgm.processingStrategy` |
| `POD_NAME` | Downward API (`metadata.name`) |
| `GANG_ID` | Gang coordinator |
| `GANG_CONFIG_DIR` | Gang coordinator |
| `GANG_TIMEOUT_SECONDS` | Gang coordinator |

## Fail-closed behavior

If the webhook cannot fetch the referenced `PreflightProfile` (not found,
API error, RBAC denied), it falls back to the chart-level defaults and logs
a warning. This is fail-closed: all checks run with default settings rather
than silently skipping checks.

## Gang fail-fast validation

When pods in a gang have different profiles, one pod may disable a
collective-op check that other pods expect to run. Without detection this
causes a distributed deadlock (the remaining pods wait indefinitely for the
missing peer to join the NCCL communicator).

### Data flow

1. **Controller** writes each pod's PreflightProfile name into the gang
   ConfigMap peer line:

   ```
   pod-0;10.0.0.1;0;full-diagnostics
   pod-1;10.0.0.2;1;full-diagnostics
   ```

   Field 4 (profile name) is optional for backward compatibility.

2. **Init container** calls `gang_config.validate_peers()` before launching
   torchrun.

3. **Validation** checks:
   - All peers reference the same PreflightProfile CRD (or all use defaults).

4. **On mismatch**: the init container logs the error, reports
   `GANG_CONFIG_ERROR` to the platform connector, and exits immediately
   (no deadlock).

### Collective-op checks

The following checks are collective operations that require all gang members
to participate:

| Check name | Type | Gang required |
|------------|------|---------------|
| `preflight-nccl-allreduce` | Multi-node all-reduce | Yes |
| `preflight-dcgm-diag` | Per-node DCGM | No |
| `preflight-nccl-loopback` | Per-node loopback | No |

### Timeout safety net

Even with fail-fast validation, a timeout remains as a safety net.
`GANG_TIMEOUT_SECONDS` (default 600) bounds the maximum wait for gang
formation. If validation passes but torchrun itself hangs, the container's
`activeDeadlineSeconds` or Kubernetes liveness probe terminates it.

## Key files

| File | Purpose |
|------|---------|
| `preflight-checks/nccl-allreduce/nccl_allreduce/gang.py` | `PeerInfo` dataclass, `GangConfig.validate_peers()` |
| `preflight-checks/nccl-allreduce/scripts/entrypoint.py` | Pre-torchrun validation call |
| `preflight-checks/nccl-allreduce/nccl_allreduce/tests/test_gang.py` | Unit tests for peer parsing and validation |
| `distros/kubernetes/nvsentinel/charts/preflight/crds/nvsentinel.nvidia.com_preflightprofiles.yaml` | CRD definition |
| `distros/kubernetes/nvsentinel/charts/preflight/templates/clusterrole.yaml` | RBAC for CRD access |
| `docs/configuration/preflight.md` | User-facing configuration guide |
