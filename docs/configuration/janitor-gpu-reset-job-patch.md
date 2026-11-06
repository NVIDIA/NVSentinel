# GPU Reset Job Template Patch

Janitor resets GPUs with a Kubernetes Job. The `resetJob.jobTemplatePatch` value changes that Job, for example to add host mounts, init containers, a service account, a wrapper command or a longer deadline. Janitor applies it as a strategic merge patch to the built-in Job template. The design is in [ADR-069](../designs/069-gpu-reset-job-extension.md).

The value is empty by default. An empty patch leaves the Job unchanged.

## How janitor builds the reset Job

1. Janitor builds the built-in template and applies the other `resetJob` keys.
2. Janitor applies the patch as a strategic merge patch, with the Kubernetes `JobTemplateSpec` as the schema. Where the patch and a `resetJob` key set the same field, janitor uses the patch value.
3. Janitor decodes the result strictly. A misspelled field name, a field name in the wrong case or a value of the wrong type stops janitor at startup.
4. For each reset, janitor sets these fields over any value in the patch: the Job name, namespace and owner labels, `nodeName`, the owner reference, and `NVIDIA_GPU_RESETS` in each container and init container.

Janitor reads the patch at startup. It logs the resolved template in the `Loaded configuration` message.

The chart writes the patch to the janitor ConfigMap as a YAML string. If you write the janitor configuration file without the chart, set `gpuResetController.resetJob.jobTemplatePatch` to a YAML string.

## The built-in Job

The patch changes this Job:

- `activeDeadlineSeconds: 300`, `backoffLimit: 2` and `ttlSecondsAfterFinished: 86400`.
- One privileged container, `gpu-reset`, with the environment variables that janitor sets.
- The volumes `host-dev`, `dev-log`, `driver-root`, `host-sys` and `reset-state`, and their mounts in `gpu-reset`. With `driverRoot: "/"`, the Job has no `driver-root` volume.
- One toleration, `operator: Exists`, the default service account, and `restartPolicy: OnFailure`.
- No init containers and no other containers.

## Merge rules

A strategic merge patch merges each list on a merge key:

- `containers`, `initContainers`, `volumes` and `env` merge on `name`. An item with a new name is added. An item with the name of a built-in item changes only the fields that the patch sets.
- `volumeMounts` merge on `mountPath`. A patch that gives a built-in mount a new `mountPath` adds a second mount and does not move the first one. Use `hostDriverRootPath` and `driverRoot` to change the driver paths.
- `tolerations` has no merge key, so a patch replaces the whole list. Keep a toleration with `operator: Exists` and no `effect`, or a `NoExecute` taint on the node can evict the reset pod.
- `command` and `args` are replaced.

To remove a built-in item, add the item with its merge key and `$patch: delete`. For example, under `spec.template.spec`:

```yaml
containers:
  - name: gpu-reset
    env:
      - name: UPLOAD_URL_BASE
        $patch: delete
```

## Example

This patch extends the deadline, adds a service account and a host mount, stops provider services in an init container, and runs the reset through a wrapper command:

```yaml
janitor:
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

The `gpu-reset` container keeps its image, environment, security context and built-in mounts. The init container gets `NVIDIA_GPU_RESETS`, so it can find the GPUs that janitor resets.

## Before you set a patch

- Keep the reset in the Job. Janitor uses the Job result as the reset result. If a patch removes or replaces the reset step, the reset reports success without a reset. A wrapper command must run the reset and return its exit code.
- Create the objects that the patch names, such as the ServiceAccount, its RBAC and image pull secrets, in the janitor namespace. A missing object shows only when a reset runs, on a node that is already cordoned and drained.
- Restore the host changes that the Job makes. Janitor restores only the GPU services that it stops. If an init container stops a host service, the Job must start it again.
- Test a patch with one GPU reset on one node before you use it across the cluster.
