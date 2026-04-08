# ADR-035: Inline DCGM Config into Init Container

## Context

The preflight webhook injects init containers into GPU pods. Each check's configuration follows one of two patterns:

1. **Inline** (NCCL checks): config lives as env vars on the container in `values.yaml`. The webhook doesn't touch them.
2. **Split** (DCGM): config lives in a separate `dcgm:` Helm block. The webhook reads it and injects env vars into the container by matching the hardcoded name `"preflight-dcgm-diag"`.

The NCCL checks originally had a separate config block too (ADR-026, lines 666-668):

```yaml
# ADR-026 original design (not shipped)
nccl:
  loopbackThresholdGBps: 10.0
  allreduceThresholdGBps: 5.0
```

This was inlined before shipping. DCGM was not.

### Current state

```yaml
# values.yaml — config here
dcgm:
  service:
    endpoint: "nvidia-dcgm.gpu-operator.svc"
    port: 5555
  diagLevel: 2
  processingStrategy: "EXECUTE_REMEDIATION"

# values.yaml — container here (easy to forget or get out of sync)
initContainers:
  - name: preflight-dcgm-diag
    image: ...
```

The webhook bridges these with `injectDCGMEnv()` (`injector.go:715-736`), which checks `container.Name != "preflight-dcgm-diag"` to decide which container gets DCGM env vars.

### Problems

1. **Split-brain config**: the `dcgm:` block is meaningless without a container named exactly `preflight-dcgm-diag`, and vice versa. Rename the container and injection silently breaks.
2. **Overloaded `dcgm:` block**: it also carries `connectorSocket` and `processingStrategy`, which are injected into *all* init containers (`injectCommonEnv`), not just the DCGM one. These are global preflight config, not DCGM-specific.
3. **Inconsistent with NCCL**: NCCL checks define their config inline (`BW_THRESHOLD_GBPS`, `MESSAGE_SIZES`). DCGM is the only check that relies on webhook-side injection from a separate config block.

### Why it was split originally

The ADR-026 design assumed `dcgm:` config would be shared with the GPU health monitor at the parent chart level. In practice, the preflight chart has its own `dcgm:` block in its configmap (confirmed on sea1-a). The shared-config rationale didn't hold.

## Decision

Inline DCGM-specific config as env vars on the container, matching how NCCL checks work. Move global config out of the `dcgm:` block.

### Proposed values.yaml

```yaml
# Global preflight config (was incorrectly scoped under dcgm:)
connectorSocket: "unix:///var/run/nvsentinel.sock"
processingStrategy: "EXECUTE_REMEDIATION"

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

  - name: preflight-nccl-loopback
    # ... unchanged, already inline
```

### Code changes

1. **Delete `injectDCGMEnv()`** (`injector.go:715-736`): no longer needed. The env vars are on the container already; `mergeEnvVars()` preserves user-defined values over webhook-injected ones.
2. **Move `connectorSocket` and `processingStrategy`** out of `DCGMConfig` into a top-level preflight config in `config.go`. `injectCommonEnv()` reads from there instead of `cfg.DCGM`.
3. **Remove `dcgm:` from `values.yaml`** and `configmap.yaml` template. Update `_helpers.tpl` accordingly.
4. **Update `global.dcgm` fallback** in `_helpers.tpl`: if the parent chart still exposes a global `dcgm.service` block, the template can render it into the container's env vars directly rather than routing through a config struct.

### What stays the same

- Gang config (`gangDiscovery:`, `gangCoordination:`) is unaffected. These are webhook-level orchestration concerns, not per-container config. `injectGangEnv()` has no name check — it applies to all containers when a gang context exists.
- NCCL checks are already inline, no changes needed.
- The `initContainers:` list remains the plugin registry for checks. Third-party checks still just add an entry.

## Consequences

### Positive

- DCGM config follows the same pattern as NCCL — one place to look
- No more hardcoded container name matching in webhook code
- `dcgm:` block gone; global config is properly scoped
- Adding new checks never requires a new config block or webhook injection function

### Negative

- Breaking change for existing Helm values (`dcgm:` block removed, env vars move to container)
- Users with `global.dcgm` overrides at the parent chart level need migration

### Migration

The `dcgm:` block values map directly to container env vars:

| Old (`dcgm:` block)              | New (container env var)    |
|-----------------------------------|----------------------------|
| `dcgm.service.endpoint` + `port` | `DCGM_HOSTENGINE_ADDR`     |
| `dcgm.diagLevel`                 | `DCGM_DIAG_LEVEL`          |
| `dcgm.processingStrategy`        | top-level `processingStrategy` |
| `dcgm.connectorSocket`           | top-level `connectorSocket`    |
