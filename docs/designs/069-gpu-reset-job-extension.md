# ADR-069: Janitor — Extend the GPU Reset Job with a Strategic Merge Patch

Status: Proposed (tracking issue [#1782](https://github.com/NVIDIA/NVSentinel/issues/1782))

## Context

Janitor resets GPUs with a Kubernetes Job ([ADR-019](019-janitor-gpu-reset.md)) that it builds in `getDefaultGPUResetJobTemplate` (`janitor/pkg/config/default.go`). The `config.controllers.gpuReset.resetJob` keys set `image`, `resources`, `runtimeClassName`, `hostDriverRootPath`, `driverRoot`, `driverBinPath`, `writeSysLogEvent` and `uploadURL`. The other fields are fixed:

- `activeDeadlineSeconds: 300`, `backoffLimit: 2` and `ttlSecondsAfterFinished: 86400`.
- Four host path volumes and their mounts: `host-dev`, `dev-log`, `driver-root` and `host-sys`.
- One privileged container, `gpu-reset`, with the environment variables that janitor sets.
- One toleration, `operator: Exists`, and the default service account.
- No init containers, no other containers, and no pod labels or annotations.

For each reset, `newGpuResetJob` sets the Job name, namespace and owner labels, `nodeName` and the owner reference, and adds `NVIDIA_GPU_RESETS` to each container. The Job result is the reset result.

Issue #1782 describes a deployment that resets GPUs on Blackwell bare metal across four infrastructure providers. It maintains a fork of janitor to mount provider-specific host services in the reset Job, to stop provider-owned DaemonSets before the reset, and to run a client barrier, a timeout latch and host-service restoration around the reset. These steps can run inside the reset Job if the Job can be changed. Each step adds to the Job, and none changes what janitor builds.

## Decision

Add an optional key, `config.controllers.gpuReset.resetJob.jobTemplatePatch`, that janitor applies as a strategic merge patch to the built-in reset Job template. The patch can add fields and change most fields, for example the deadline, the backoff limit and the command. It cannot change the fields that the reset depends on or that a `resetJob` key sets. [Validation at startup](#validation-at-startup) lists these fields.

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

- The key is empty by default. An empty patch leaves the Job unchanged.
- The chart takes structured YAML and renders it into the janitor ConfigMap as a string with `toYaml`. Janitor reads a string because viper changes map keys to lowercase (`volumeMounts` becomes `volumemounts`) and does not change strings.

### Order

1. Janitor builds the built-in template and applies the existing `resetJob` keys.
2. Janitor applies the patch with `strategicpatch.StrategicMergePatch`, with `batchv1.JobTemplateSpec` as the schema.
3. Janitor validates the result and stores it as `ResolvedJobTemplate`.
4. For each reset, `newGpuResetJob` sets its fields as it does now, over any value from the patch. It also adds `NVIDIA_GPU_RESETS` to each init container, so that a step before the reset can find the GPUs that janitor resets.

### Validation at startup

Janitor rejects the configuration at startup when:

- The merged result does not decode strictly into `batchv1.JobTemplateSpec`, for example because of a misspelled field name. Janitor decodes the result and not the patch, because a patch can contain strategic merge directives such as `$patch`, which are not fields of the type. The merge keeps unknown fields, so the strict decode of the result finds them.
- The patch sets `metadata.name`, `metadata.generateName`, `metadata.namespace` or `spec.template.spec.nodeName`.
- The result has no `gpu-reset` container, for example after `$patch: delete`.
- The `gpu-reset` container is not `privileged: true`.
- The result changes or removes the image, image pull secrets, resources, RuntimeClass, volumes, volume mounts or environment variables that janitor builds for `gpu-reset`. If a `resetJob` key sets the value, the error names the key.
- The result does not contain the built-in toleration: `operator: Exists` with an empty key, value and effect, and no `tolerationSeconds`. A toleration with an effect, for example `effect: NoSchedule`, does not count.

The patch can change the other fields that janitor builds: `activeDeadlineSeconds`, `backoffLimit`, `ttlSecondsAfterFinished`, `restartPolicy`, and the `imagePullPolicy`, `command` and `args` of `gpu-reset`.

The last two checks cover two strategic merge rules, which were checked against the built-in template:

- `volumeMounts` merge on `mountPath`. A patch that changes the `mountPath` of `driver-root` adds a second mount and does not move the first. PR #1803 reported this case.
- `tolerations` has no merge key, so a patch replaces the list. Without the built-in toleration, a `NoExecute` taint can evict the reset pod.

Janitor then creates a Job from the resolved template once with `dryRun: All`. It sets the fields that `newGpuResetJob` sets, with `metadata.generateName: gpu-reset-validation-`, a placeholder node name and no owner reference, because no GPUReset owns this Job. The API server validates the Job, including its pod template, and runs the admission for Jobs. It does not check that the ServiceAccount or the image pull secrets exist. When the Job controller creates the pod, ServiceAccount admission rejects the pod if its ServiceAccount does not exist. No admission checks the image pull secrets: the kubelet uses them when it pulls the image, so a missing secret causes an image pull error.

### Changes

- `janitor/pkg/config`: the `JobTemplatePatch` field on `ResetJobConfig`, the patch, the validation, and table tests for each rejected case, including a toleration with `effect: NoSchedule`.
- `janitor/go.mod`: `sigs.k8s.io/yaml` changes from an indirect to a direct dependency. `strategicpatch` is part of `k8s.io/apimachinery`, which janitor already uses.
- Janitor startup: the dry-run.
- `newGpuResetJob`: `NVIDIA_GPU_RESETS` in each init container.
- The janitor chart: the value, the ConfigMap rendering and a helm-unittest.
- `docs/configuration/janitor.md`: the key, the order, the rejected cases, the merge rules and an example.

The GPUReset CRD, its conditions and reasons, the reconcile steps and the protobufs do not change.

## Rationale

- One key covers the needs in #1782: host mounts, a service account, init containers, a wrapper command and a longer deadline.
- Later changes to the built-in template also apply to patched Jobs.
- The driver paths, where a patch gives a wrong result, have dedicated keys and a startup check.
- Janitor keeps the CR lifecycle, the node lock, the teardown and restore of the services it manages, and the metrics. The Job result remains the reset result.

## Consequences

### Positive

- The GPU reset Job can be changed without a fork of janitor.
- The default Job, the CRD, the protobufs and the reconcile steps do not change.
- Most configuration errors fail at startup instead of during a reset.

### Negative

- The patch can set most fields of the Job, and janitor validates only the fields in [Validation at startup](#validation-at-startup).
- A missing ServiceAccount or image pull secret passes the startup checks. With a missing ServiceAccount, admission rejects the reset pod. With a missing image pull secret, the image pull fails if the registry requires that secret. In both cases, the first reset fails when the Job deadline expires.
- Janitor does not restore host changes that the patched Job makes. If the reset fails after a step stops a host service, janitor restores only the services that it manages through node labels.
- The patch can add host paths and set a service account. The reset Job is already privileged.
- The merge rules differ from list to list.
- `LoadConfig` rejects unknown keys, so an earlier janitor image does not start with a ConfigMap that contains `jobTemplatePatch`.

### Mitigations

- The startup validation and the dry-run.
- Janitor logs the resolved template at startup at debug level.
- `docs/configuration/janitor.md` gives the merge key of each list in the reset Job: `containers`, `initContainers`, `volumes` and `env` merge on `name`, `volumeMounts` on `mountPath`, and `tolerations` is replaced. It also states that janitor does not restore host changes that the Job makes.
- A Helm rollback reverts the ConfigMap and the image together.

## Alternatives Considered

### Replace the whole Job template (PR #1803)

**Rejected** because the deployment then maintains a full copy of the template, and later changes to the built-in template, for example #1813 and #2013, do not apply to it. The existing `resetJob` keys have no effect while a replacement is set. The driver root case that PR #1803 gave against a patch is covered by the dedicated keys and the startup check.

### Patch only the pod template (`podTemplateOverride`, as #1782 proposed)

**Rejected** because it cannot change `activeDeadlineSeconds` (300 seconds) or `backoffLimit`.

### A typed key for each addition

**Rejected** because #1782 needs about eight keys (`extraVolumes`, `extraVolumeMounts`, `extraEnv`, `initContainers`, `serviceAccountName`, `activeDeadlineSeconds`, `backoffLimit` and a command), and each new need adds a key and a code change.

### gRPC calls that delegate the reset to a provider (as #1782 first proposed)

Delegate the reset to a provider service, as [ADR-017](017-remediation-plugins.md) does for reboot and terminate.

**Not chosen** because the 2026-09-10 discussion in #1782 settled on a Job template change, and the calls need a provider service in each deployment. A later ADR can propose them if a reset cannot run in a Job.

## Notes

- Non-goals: custom GPUReset conditions or reasons, hooks that janitor calls, and changes to the reset steps.
- The chart renders `gpuReset.cspProviderHost`, but no GPU reset code reads it. Its removal is a separate change.
- The merge behaviour above was checked with `strategicpatch.StrategicMergePatch` against `getDefaultGPUResetJobTemplate` at commit `e21aa579`.

## References

- Issue [#1782](https://github.com/NVIDIA/NVSentinel/issues/1782) and PR [#1803](https://github.com/NVIDIA/NVSentinel/pull/1803) (closed)
- PRs [#1785](https://github.com/NVIDIA/NVSentinel/pull/1785), [#1813](https://github.com/NVIDIA/NVSentinel/pull/1813) and [#2013](https://github.com/NVIDIA/NVSentinel/pull/2013): the driver path keys
- [ADR-017: Remediation Plugins](017-remediation-plugins.md)
- [ADR-019: Janitor Support for GPU Reset](019-janitor-gpu-reset.md)
- [ADR-020: NVSentinel Support for GPU Reset](020-nvsentinel-gpu-reset.md)
- [Kubernetes: Update API objects with a strategic merge patch](https://kubernetes.io/docs/tasks/manage-kubernetes-objects/update-api-object-kubectl-patch/)
