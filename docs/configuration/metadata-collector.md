# Metadata Collector Configuration

## Overview

The Metadata Collector module collects GPU metadata with NVIDIA NVML (Management Library) and writes it to a shared file. Other modules read this file to add GPU serial numbers, UUIDs, and topology information to health events. This component also exposes the pod-to-GPU mapping as an annotation on each pod that requests GPUs. This document describes the main Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

This setting enables or disables the deployment of the Metadata Collector in the cluster.

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

Defines the CPU and memory resource requests and limits for the Metadata Collector container.

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

## Kubelet Host

Sets the `KUBELET_HOST` environment variable, which tells the collector where to reach the kubelet `/pods` endpoint.

```yaml
metadata-collector:
  kubeletHost:
    valueFrom:
      fieldRef:
        fieldPath: status.hostIP
```

The default gets the primary IP of the node through the downward API. This works if the kubelet binds to `0.0.0.0` or to the node IP. A static address also works:

```yaml
metadata-collector:
  kubeletHost:
    value: "10.0.0.1"
```

To leave the variable unset, set `kubeletHost: {}`. The collector then uses `localhost` instead. An explicit `--kubelet-kubeconfig` fully overrides this value.

## Kubelet Root Directory

The collector maps pods to GPUs through the kubelet PodResources socket. Kubelet creates that socket under its `--root-dir`. Thus, the chart mounts `<kubeletRootDir>/pod-resources` from the host at the fixed path that the collector reads. If a distribution runs kubelet from a non-default directory, set this value to the kubelet `--root-dir`. For example, k0s uses `/var/lib/k0s/kubelet`:

```yaml
global:
  kubeletRootDir: /var/lib/kubelet   # default
```

If the value does not match kubelet, one of these occurs:

- The pod stays in `ContainerCreating` with a `FailedMount` event, because the host directory does not exist.
- The container logs `Pod device mapper failed` with the error `got an error creating Kubelet gRPC client: stat /var/lib/kubelet/pod-resources/kubelet.sock: no such file or directory` and exits, because the host directory exists but kubelet does not use it.

## Pod Mapper Failure Tolerance

The number of consecutive failed poll cycles that the pod mapper tolerates before the container exits with a non-zero code.

```yaml
metadata-collector:
  podMapper:
    maxConsecutiveFailures: 10
```

The poll period is 30 seconds. Thus, the default tolerates five minutes of failures. This time is long enough to outlast a credential rotation or a kubelet restart. It is also short enough to fail visibly when the collector has a real fault. The minimum is `1`, which exits on the first failed poll. This value sets the `--pod-mapper-max-consecutive-failures` flag that [Startup and failure handling](#startup-and-failure-handling) describes.

## Disabling the Pod Mapper

The pod device mapper writes the `dgxc.nvidia.com/devices` annotation on GPU pods. Partial drains in the node-drainer need that annotation. Disable the mapper only where it cannot work. For example, the mapper cannot work when the pods that the kubelet reports are not the pods that the collector sees through the Kubernetes API.

```yaml
metadata-collector:
  podMapper:
    enabled: false
```

With the mapper disabled, the collector still writes the GPU metadata file. It then continues to run, but it does not poll the kubelet. Pods get no device annotations. Thus, keep `node-drainer.partialDrainEnabled` set to `false`, because a partial drain fails for a GPU pod without the annotation. Full drains still detect GPU pods from their container limits and DRA claims. This value sets the `--pod-mapper-enabled` flag.

## Host-native authentication

If the collector runs outside a pod, use two explicit kubeconfigs:

```bash
metadata-collector \
  --kubeconfig=/etc/nvsentinel/metadata-collector/apiserver.kubeconfig \
  --kubelet-kubeconfig=/etc/nvsentinel/metadata-collector/kubelet.kubeconfig \
  --output-path=/var/lib/nvsentinel/gpu_metadata.json
```

| Flag | Purpose | Default |
|---|---|---|
| `--kubeconfig` | Kubernetes API endpoint, trust, and credentials for pod annotation updates | In-cluster configuration |
| `--kubelet-kubeconfig` | Kubelet HTTPS endpoint, trust, and credentials for `/pods` | `KUBELET_HOST:10250` and the projected ServiceAccount token |

If `KUBELET_HOST` is not set, the default is `localhost`. An explicit kubelet kubeconfig overrides that variable. The collector does not use the API server credentials for the kubelet. It does not load `KUBECONFIG` or a home-directory kubeconfig implicitly.

Explicit configurations must use HTTPS, verify server certificates, and provide credentials. Set the kubelet server to an address in its serving certificate, such as `https://gpu-node.example:10250`. Provide the correct CA and, if needed, `tls-server-name`.

### Credentials and permissions

Provision credentials at node runtime. Keep kubeconfig and credential files accessible only to the service account or root. Kubeconfigs are trusted input, because client-go can run a configured credential plugin.

- The Kubernetes API identity needs `patch` on pods in each workload namespace.
- The kubelet identity needs permission to read `/pods`. With fine-grained kubelet authorization, use `get` on `nodes/pods`. Other configurations require `get` on `nodes/proxy`, which grants broader access.
- The process needs access to `/var/lib/kubelet/pod-resources/kubelet.sock`, NVIDIA devices and libraries, and its output directory. The binary always uses that path. If kubelet uses a different `--root-dir`, make the socket available at that path.

Authentication does not grant permissions. Do not assume that the client identity of the kubelet can patch workload pods. This feature creates no credentials or RBAC bindings.

Client-go supports kubeconfig bearer tokens, token files, client certificates, and configured credential providers. Prefer rotating file credentials to embedded long-lived credentials. Token-file reload is periodic, so provision a new token before the old token expires. Restart the collector after changes to kubeconfig settings or CA trust. Client certificate files reload through client-go. Embedded certificate data does not reload.

Certificate authentication requires both a certificate and its private key. You can provide each as a file or as embedded data. A username and password alone do not satisfy the credential requirement.

Without an explicit kubelet kubeconfig, the transport reads the projected ServiceAccount token again on every request, also on retries. Thus, recovery continues to work when that token rotates between attempts. The client does not keep tokens or set authentication headers itself.

### Startup and failure handling

Hardware inventory stays enabled. Pod-to-GPU mapping stays enabled, unless `--pod-mapper-enabled=false` disables it. Refer to [Disabling the Pod Mapper](#disabling-the-pod-mapper). If the mapper is enabled, the kubelet socket must exist and be accessible when the mapper starts.

The 30-second poll period and the `--pod-mapper-max-consecutive-failures` limit also apply. The default limit is 10 failed polls. Persistent authentication, authorization, or socket failures stay errors. The collector does not report them as successful mapping.

A missing kubeconfig or credential file is a configuration error. HTTP 401 indicates rejected credentials. HTTP 403 indicates denied permission. To correct a TLS error, configure the correct CA trust and server identity. Do not disable verification to bypass TLS errors.
