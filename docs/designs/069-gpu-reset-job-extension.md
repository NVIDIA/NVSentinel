# ADR-069: Janitor — Extend the GPU Reset Job with a Strategic Merge Patch

Status: Accepted (tracking issue [#1782](https://github.com/NVIDIA/NVSentinel/issues/1782))

## Context

Janitor resets GPUs with a Kubernetes Job ([ADR-019](019-janitor-gpu-reset.md)) that it builds in `getDefaultGPUResetJobTemplate` (`janitor/pkg/config/default.go`). The `config.controllers.gpuReset.resetJob` keys set `image`, `resources`, `runtimeClassName`, `hostDriverRootPath`, `driverRoot`, `driverBinPath`, `writeSysLogEvent` and `uploadURL`. Janitor builds the other fields in code, and no key changes them:

- `activeDeadlineSeconds: 300`, `backoffLimit: 2` and `ttlSecondsAfterFinished: 86400`.
- Four host path volumes and their mounts: `host-dev`, `dev-log`, `driver-root` and `host-sys`.
- One privileged container, `gpu-reset`, with the environment variables that janitor sets.
- One toleration, `operator: Exists`, and the default service account.
- No init containers, no other containers, and no pod labels or annotations.

For each reset, `newGpuResetJob` sets the Job name, namespace and owner labels, `nodeName` and the owner reference, and adds `NVIDIA_GPU_RESETS` to each container. The Job result is the reset result.

Issue #1782 describes a deployment that resets GPUs on Blackwell bare metal across four infrastructure providers. It maintains a fork of janitor to mount provider-specific host services in the reset Job, to stop provider-owned DaemonSets before the reset, and to run a client barrier, a timeout latch and host-service restoration around the reset. These steps can run inside the reset Job if the Job can be changed.

## Decision

Add an optional key, `config.controllers.gpuReset.resetJob.jobTemplatePatch`, that janitor applies as a strategic merge patch to the built-in reset Job template. Janitor applies the patch as written and does not restrict which fields it changes.

## Implementation

### Configuration

```yaml
config:
  controllers:
    gpuReset:
      resetJob:
        jobTemplatePatch:
          spec:
            activeDeadlineSeconds: 900
            template:
              spec:
                serviceAccountName: provider-reset
                volumes:
                  - name: host-systemd
                    hostPath:
                      path: /run/systemd
                initContainers:
                  - name: stop-provider-daemonsets
                    image: provider/reset-hooks:1.0
                containers:
                  - name: gpu-reset
                    command: ["/hooks/reset-with-restore.sh"]
                    volumeMounts:
                      - name: host-systemd
                        mountPath: /run/systemd
```

The key is empty by default. An empty patch leaves the Job unchanged.

### Order

1. Janitor builds the built-in template and applies the existing `resetJob` keys.
2. Janitor applies the patch with `strategicpatch.StrategicMergePatch`, with `batchv1.JobTemplateSpec` as the schema. Where the patch and a `resetJob` key set the same field, the patch value is used.
3. Janitor decodes the result strictly into `batchv1.JobTemplateSpec` and stores it as `ResolvedJobTemplate`. A misspelled field name stops janitor at startup. Janitor also checks the patch itself the same way, with directives such as `$patch` removed. This catches a misspelled field set to `null`, which the merge removes before the result is checked.
4. For each reset, `newGpuResetJob` sets the fields listed in [Context](#context), over any value from the patch. It also adds `NVIDIA_GPU_RESETS` to each init container, so that a step before the reset can find the GPUs that janitor resets.

### Merge rules

Two strategic merge rules can give a Job that the API server accepts but that fails on the node. The documentation describes them:

- `volumeMounts` merge on `mountPath`. A patch that changes the `mountPath` of `driver-root` adds a second mount and does not move the first. The `hostDriverRootPath` and `driverRoot` keys set the driver paths instead.
- `tolerations` has no merge key, so a patch replaces the list. Without `operator: Exists` and an empty effect, a `NoExecute` taint can evict the reset pod.

### Changes

- `janitor/pkg/config`: the `JobTemplatePatch` field on `ResetJobConfig`, the patch, the strict decode, and tests for an added container, a misspelled field and the two merge rules.
- `newGpuResetJob`: `NVIDIA_GPU_RESETS` in each init container.
- The janitor chart: the value, the ConfigMap rendering and a helm-unittest.
- `docs/configuration/janitor.md`: the key, the order, the merge rules, the fields that `newGpuResetJob` sets, and an example.

The GPUReset CRD, its conditions and reasons, the reconcile steps and the protobufs do not change.

## Rationale

- One key covers the needs in #1782: host mounts, a service account, init containers, a wrapper command and a longer deadline.
- Later changes to the built-in template also apply to patched Jobs.
- The deployment that writes a patch owns the resulting Job. Janitor sets only the fields that it needs for each reset.
- Janitor keeps the CR lifecycle, the node lock, the teardown and restore of the services it manages, and the metrics. The Job result remains the reset result.

## Consequences

### Positive

- The GPU reset Job can be changed without a fork of janitor.
- The default Job, the CRD, the protobufs and the reconcile steps do not change.
- A misspelled field stops janitor at startup.

### Negative

- A patch can break the reset, for example through one of the [merge rules](#merge-rules) or a missing ServiceAccount. The error shows only when a reset runs, on a node that is already cordoned and drained.
- Janitor uses the Job result as the reset result, so a patch that removes or replaces the reset step makes the reset report success without a reset.
- Janitor does not restore host changes that the patched Job makes.

### Mitigations

- Janitor logs the resolved template at startup, in the `Loaded configuration` message at info level.
- `docs/configuration/janitor.md` gives the merge key of each list in the reset Job: `containers`, `initContainers`, `volumes` and `env` merge on `name`, `volumeMounts` on `mountPath`, and `tolerations` is replaced. It also states that janitor does not restore host changes that the Job makes.

## Alternatives Considered

### Replace the whole Job template (PR #1803)

**Rejected** because the deployment then maintains a full copy of the template, and later changes to the built-in template, for example #1813 and #2013, do not apply to it. The existing `resetJob` keys have no effect while a replacement is set.

### Patch only the pod template (`podTemplateOverride`, as #1782 proposed)

**Rejected** because it cannot change `activeDeadlineSeconds` (300 seconds) or `backoffLimit`.

### Individual settings for the host paths (suggested in #1782)

**Rejected** because #1782 also needs init containers, a service account, a command and a longer deadline, which host path settings do not cover.

### gRPC calls that delegate the reset to a provider (as #1782 first proposed)

Delegate the reset to a provider service, as [ADR-017](017-remediation-plugins.md) does for reboot and terminate.

**Not chosen** because the steps that #1782 needs can run inside the reset Job, and each deployment would have to deploy and operate a provider service.

## Notes

- Non-goals: custom GPUReset conditions or reasons, hooks that janitor calls, and changes to the reset steps.
- The chart renders `gpuReset.cspProviderHost`, but no GPU reset code reads it. Its removal is a separate change.

## References

- Issue [#1782](https://github.com/NVIDIA/NVSentinel/issues/1782) and PR [#1803](https://github.com/NVIDIA/NVSentinel/pull/1803) (closed)
- PRs [#1785](https://github.com/NVIDIA/NVSentinel/pull/1785), [#1813](https://github.com/NVIDIA/NVSentinel/pull/1813) and [#2013](https://github.com/NVIDIA/NVSentinel/pull/2013): the driver path keys
- [ADR-017: Remediation Plugins](017-remediation-plugins.md)
- [ADR-019: Janitor Support for GPU Reset](019-janitor-gpu-reset.md)
- [ADR-020: NVSentinel Support for GPU Reset](020-nvsentinel-gpu-reset.md)
- [Kubernetes: Update API objects with a strategic merge patch](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/update-api-object-kubectl-patch/)
