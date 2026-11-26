# Metadata Collector Configuration

## Overview

The Metadata Collector module collects GPU metadata with NVIDIA NVML (Management Library) and writes it to a shared file. Other modules read this file to add GPU serial numbers, UUIDs, and topology information to health events. It runs in the `nvidia-metadata-collector` DaemonSet. This document describes the main Helm configuration options for system administrators.

The metadata-collector chart also renders the `pod-device-mapper` DaemonSet, which writes the device annotation on pods that partial drains read. Its values are under `metadata-collector` too. See [Pod Device Mapper Configuration](pod-device-mapper.md).

## Configuration Reference

### Module Enable/Disable

This setting enables or disables the deployment of the Metadata Collector and the pod device mapper in the cluster.

```yaml
global:
  metadataCollector:
    enabled: true
```

### Logging

Sets the verbosity level of the Metadata Collector logs.

```yaml
metadata-collector:
  logLevel: info  # Options: debug, info, warn, error
```

### Resources

Defines the CPU and memory resource requests and limits for the Metadata Collector container. The pod device mapper uses them too, unless `podMapper.resources` is set.

```yaml
metadata-collector:
  resources:
    limits:
      cpu: 500m
      memory: 256Mi
    requests:
      cpu: 100m
      memory: 128Mi
```

## Runtime Class

Specifies the container runtime class for GPU device access.

```yaml
metadata-collector:
  runtimeClassName: "nvidia"
```

### Parameters

#### runtimeClassName

The runtime class name that provides GPU device access. NVML needs it to query GPU information when the GPU Operator creates a matching `RuntimeClass` (the common non-NRI setup).

**Common values:**

- `nvidia` - NVIDIA container runtime (default)
- `nvidia-container-runtime` - the value that some GPU Operator installations use
- `nvidia-legacy` - Legacy NVIDIA runtime
- Empty string - Uses the default cluster runtime. This value is for CRI-O environments and for NRI-mode clusters (see below)

## GPUCluster (DRA) mode

If you install the GPU Operator in `GPUCluster` (DRA) mode, there is no Container Toolkit and no `nvidia` RuntimeClass. Thus, the default `runtimeClassName: nvidia` fails admission. In this case, enable GPUCluster mode:

```yaml
global:
  gpuDraEnabled: true   # default false
```

With this mode enabled, the DaemonSet drops `runtimeClassName`. Instead, it holds a DRA admin-access claim on the GPUs of the node, the same way that the GPU Operator runs its own DCGM DaemonSet. The DRA driver injects the driver libraries through CDI. Admin access does not consume the GPUs.

Label the NVSentinel namespace one time, before the install or upgrade that switches to GPUCluster mode. Kubernetes accepts admin-access claims only from a labelled namespace. Otherwise, it rejects the `ResourceClaimTemplate` of the chart, and the Helm release fails:

```bash
kubectl label namespace nvsentinel resource.kubernetes.io/admin-access=true
```

To switch modes, do these steps in sequence:

1. Label the namespace (one time).
2. Switch the GPU Operator to `GPUCluster` mode.
3. Run `helm upgrade` with `global.gpuDraEnabled: true`.

To switch back, do these steps in sequence:

1. Switch the GPU Operator back to ClusterPolicy mode.
2. Run `helm upgrade` with `global.gpuDraEnabled: false`.

The namespace label stays.

## GPU Operator NRI plugin mode

If the GPU Operator NRI plugin is enabled (`cdi.nriPluginEnabled: true`), the GPU Operator does not create the `nvidia` RuntimeClass. It also deletes an existing one. Thus, the default `runtimeClassName: nvidia` fails admission. If you leave the runtime class unset, the collector crash-loops with `NVML: ERROR_LIBRARY_NOT_FOUND`. If you request `nvidia.com/gpu` instead, the DaemonSet reserves a GPU. To prevent these problems, enable NRI plugin mode:

```yaml
metadata-collector:
  nriPlugin:
    enabled: true   # default false
    # cdiDevice: management.nvidia.com/gpu=all   # default
```

The DaemonSet then omits `runtimeClassName` and adds the pod annotation `nvidia.cdi.k8s.io/container.metadata-collector: management.nvidia.com/gpu=all`. The NRI plugin of the Container Toolkit injects the management CDI device, the same way as for the management containers of the GPU Operator. This device carries the driver libraries, `nvidia-smi`, and the device nodes for every GPU on the node. This mode works with both the GPU Operator driver container and a host-installed driver. It does not consume a GPU. For more information, see [Requesting a Management CDI Device](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/cdi.html) in the GPU Operator documentation.

By default, the NRI plugin injects management devices only into pods in the GPU Operator namespace. Before you enable this mode, add the NVSentinel namespace to the `NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES` environment variable (comma-separated) of the Container Toolkit:

```yaml
# GPU Operator Helm values
toolkit:
  env:
    - name: NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES
      value: nvsentinel
```

If the namespace is missing, the toolkit skips the injection. It logs this only at info level, on its own pod. Thus, the Metadata Collector crash-loops with `NVML: ERROR_LIBRARY_NOT_FOUND` and shows no other error. Examine the toolkit log for `is not in one of the allowed namespaces`.

You cannot use `nriPlugin.enabled` together with `global.gpuDraEnabled`. GPUCluster (DRA) mode has no Container Toolkit, and thus no NRI plugin. If you set both, the chart does not render.

## Host-path driver access (host-installed driver)

On NRI-mode clusters with a host-installed driver (GPU Operator `driver.enabled: false`), you can mount the host driver libraries instead of [NRI plugin mode](#gpu-operator-nri-plugin-mode). This method does not work with the GPU Operator driver container. The library directory of that container, `/run/nvidia/driver/usr/lib/<arch>`, also contains the glibc of the container. The collector aborts when that glibc is on `LD_LIBRARY_PATH`.

The host library directory also contains the glibc of the host. The collector image uses glibc 2.35, from Ubuntu 22.04. If the host has a different glibc version, the collector also aborts. Thus, use this method only on hosts with glibc 2.35.

Use the same extra volume pattern as `gpu-health-monitor`. In the Helm values, make these changes:

- Clear `runtimeClassName`.
- Mount the host NVIDIA libraries.
- If the mount path is not already on the search path of the dynamic linker, set `LD_LIBRARY_PATH`.

The container already runs as root (`runAsUser: 0`). For the NVLink/NIC topology, the collector also runs `nvidia-smi`. If you need those fields, mount that host binary in the same way.

```yaml
metadata-collector:
  runtimeClassName: ""
  extraEnv:
    - name: LD_LIBRARY_PATH
      value: /usr/local/nvidia/lib
  additionalVolumeMounts:
    - name: nvidia-driver-libs
      mountPath: /usr/local/nvidia/lib
      readOnly: true
  additionalHostVolumes:
    - name: nvidia-driver-libs
      hostPath:
        path: /usr/lib/x86_64-linux-gnu   # amd64; use /usr/lib/aarch64-linux-gnu on arm64
        type: Directory
```

`additionalHostVolumes`, `additionalVolumeMounts`, and `extraEnv` default to empty lists.

## GPU Operator driver upgrades and MIG changes

The Metadata Collector keeps the GPUs open through NVML. GPU Operator 26.7 and later uses the `nvidia.com/gpu.deploy.client` node label to remove GPU clients from a node. It does this before it unloads the driver or changes the MIG layout. By default, the Metadata Collector DaemonSet has a required node affinity on this label.

| `nvidia.com/gpu.deploy.client` value | Metadata collector pod |
|---|---|
| Not present (GPU Operator before 26.7, or GPUCluster mode) | Runs |
| `true` | Runs |
| `paused-for-driver-upgrade` or `paused-for-mig-change` | Removed until the label is `true` again |
| `false`, or any other value | Removed |

If you set `affinity` or `global.affinity`, the chart uses your value unchanged and does not add the label rule. To keep the label rule, add both terms to your value. Kubernetes ORs the `nodeSelectorTerms` and ANDs the expressions in each term. Thus, if you have your own node expressions, add them to both terms.

```yaml
affinity:
  nodeAffinity:
    requiredDuringSchedulingIgnoredDuringExecution:
      nodeSelectorTerms:
        - matchExpressions:
            - key: nvidia.com/gpu.deploy.client
              operator: In
              values: ["true"]
        - matchExpressions:
            - key: nvidia.com/gpu.deploy.client
              operator: DoesNotExist
```

Know these limits:

- The GPU Operator driver manager waits only for pods that select the label with `nodeSelector`. It does not wait for this pod. When the label changes, the DaemonSet controller removes the pod, at the same time as the clients of the GPU Operator. But nothing makes sure that the pod stops before the driver unloads.
- An administrator can set the label to `false` to keep GPU clients off a node. The GPU Operator keeps that value, so the metadata collector does not run on that node.
- If someone removes the label, the pod still runs. The GPU Operator adds the label again only at its next reconcile, not immediately.
- The GPU Operator sets the label back to `true` before the new driver loads. The pod also needs `nvsentinel.dgxc.nvidia.com/driver.installed: "true"`, which the labeler sets when the node has a ready driver pod. Thus the pod starts again after the new driver is ready.
- In GPU Operator 26.7.0, a MIG change that times out can leave the label at `paused-for-mig-change`. The pod then stays off the node. Use GPU Operator 26.7.1 or later.
- A GPU reset also sets the label to `false`, and waits for this pod to leave the node. After the reset, the janitor sets the label back to `true`. Refer to [GPU clients](janitor.md#gpu-clients).

If the pod is missing on a GPU node, examine the label:

```bash
kubectl get node <node> -L nvidia.com/gpu.deploy.client
```
