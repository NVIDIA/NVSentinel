# Metadata Collector Configuration

## Overview

The Metadata Collector module collects GPU metadata using NVIDIA NVML (Management Library) and writes it to a shared file. Other modules read this file to enrich health events with GPU serial numbers, UUIDs, and topology information. This component will also expose the pod-to-GPU mapping as an annotation on each pod requesting GPUs. This document covers all Helm configuration options for system administrators.

## Configuration Reference

### Module Enable/Disable

Controls whether the metadata-collector module is deployed in the cluster.

```yaml
global:
  metadataCollector:
    enabled: true
```

### Resources

Defines CPU and memory resource requests and limits for the metadata-collector init container.

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

Runtime class name that provides GPU device access. Required for NVML to query GPU information when GPU Operator creates a matching `RuntimeClass` (the common non-NRI setup).

**Common values:**
- `nvidia` - NVIDIA container runtime (default)
- `nvidia-container-runtime` - value some GPU Operator installs use
- `nvidia-legacy` - Legacy NVIDIA runtime
- Empty string - Uses the default cluster runtime. Used for CRI-O environments and for NRI-mode clusters (see below)

## GPUCluster (DRA) mode

If the GPU Operator is installed in `GPUCluster` (DRA) mode, there is no Container Toolkit and no `nvidia` RuntimeClass, so the default `runtimeClassName: nvidia` fails admission. Enable GPUCluster mode:

```yaml
global:
  gpuDraEnabled: true   # default false
```

With it enabled the DaemonSet drops `runtimeClassName` and holds a DRA admin-access claim on the node's GPUs instead, the same way GPU Operator runs its own DCGM DaemonSet; the DRA driver injects the driver libraries via CDI, and admin access does not consume the GPUs.

Label the NVSentinel namespace once, before the install or upgrade that switches to GPUCluster mode. Kubernetes accepts admin-access claims only from a labelled namespace and rejects the chart's `ResourceClaimTemplate` otherwise, which fails the Helm release:

```bash
kubectl label namespace nvsentinel resource.kubernetes.io/admin-access=true
```

Switching modes in order:

1. Label the namespace (once).
2. Switch the GPU Operator to `GPUCluster` mode.
3. `helm upgrade` with `global.gpuDraEnabled: true`.

Switching back needs only steps 2 and 3 with `false`; the label stays. With the default `false` the chart renders exactly as before.

## GPU Operator NRI plugin mode

With the GPU Operator NRI plugin enabled (`cdi.nriPluginEnabled: true`), the GPU Operator does not create the `nvidia` RuntimeClass and deletes an existing one, so the default `runtimeClassName: nvidia` fails admission. Leaving it unset crash-loops with `NVML: ERROR_LIBRARY_NOT_FOUND`, and requesting `nvidia.com/gpu` reserves a GPU for the DaemonSet. Enable NRI plugin mode instead:

```yaml
metadata-collector:
  nriPlugin:
    enabled: true   # default false
    # cdiDevice: management.nvidia.com/gpu=all   # default
```

The DaemonSet then omits `runtimeClassName` and adds the pod annotation `nvidia.cdi.k8s.io/container.metadata-collector: management.nvidia.com/gpu=all`. The Container Toolkit's NRI plugin injects the management CDI device, which carries the driver libraries, `nvidia-smi`, and the device nodes for every GPU on the node, the same way as for the GPU Operator's own management containers. It works with both the GPU Operator driver container and a host-installed driver, and does not consume a GPU. See [Requesting a Management CDI Device](https://docs.nvidia.com/datacenter/cloud-native/gpu-operator/latest/cdi.html) in the GPU Operator documentation.

By default, the NRI plugin injects management devices only into pods in the GPU Operator namespace. Add the NVSentinel namespace to the Container Toolkit's `NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES` environment variable (comma-separated) before enabling this mode:

```yaml
# GPU Operator Helm values
toolkit:
  env:
    - name: NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES
      value: nvsentinel
```

If the namespace is missing, the toolkit skips the injection and logs only at info level on its own pod, so metadata-collector crash-loops with `NVML: ERROR_LIBRARY_NOT_FOUND` and shows no other error. Check the toolkit log for `is not in one of the allowed namespaces`.

`nriPlugin.enabled` cannot be combined with `global.gpuDraEnabled`: GPUCluster (DRA) mode has no Container Toolkit and so no NRI plugin. The chart refuses to render if both are set. With the default `false` the chart renders exactly as before.

## Host-path driver access (host-installed driver)

On NRI-mode clusters with a host-installed driver (GPU Operator `driver.enabled: false`), you can mount the host driver libraries instead of using [NRI plugin mode](#gpu-operator-nri-plugin-mode). This does not work with the GPU Operator driver container. Its library directory, `/run/nvidia/driver/usr/lib/<arch>`, also contains the container's own glibc, and the collector aborts when that glibc is on `LD_LIBRARY_PATH`.

Use the same extra volume pattern as `gpu-health-monitor`: clear `runtimeClassName` and mount the host NVIDIA libraries. Set `LD_LIBRARY_PATH` when the mount path is not already on the dynamic linker search path. The container already runs as root (`runAsUser: 0`). NVLink/NIC topology also shells out to `nvidia-smi`; mount that host binary the same way if those fields are required.

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

`additionalHostVolumes`, `additionalVolumeMounts`, and `extraEnv` default to empty lists. Existing RuntimeClass-based installs are unchanged.

## GPU Operator driver upgrades and MIG changes

The metadata collector holds the GPUs open through NVML. GPU Operator 26.7 and later uses the `nvidia.com/gpu.deploy.client` node label to remove GPU clients from a node before it unloads the driver or changes the MIG layout. The metadata collector DaemonSet has a required node affinity on this label. No Helm value controls it.

| `nvidia.com/gpu.deploy.client` value | Metadata collector pod |
|---|---|
| Not present (GPU Operator before 26.7, or GPUCluster mode) | Runs |
| `true` | Runs |
| `paused-for-driver-upgrade` or `paused-for-mig-change` | Removed until the label is `true` again |
| `false`, or any other value | Removed |

Your `affinity` value still applies. The chart adds the label rule to each of your `nodeSelectorTerms` and keeps all other affinity rules.

Know these limits:

- The GPU Operator driver manager waits only for pods that select the label with `nodeSelector`. It does not wait for this pod. The DaemonSet controller removes the pod when the label changes, at the same time as the GPU Operator's own clients, but nothing makes sure that the pod stops before the driver unloads.
- An administrator can set the label to `false` to keep GPU clients off a node. The GPU Operator keeps that value, so the metadata collector does not run on that node.
- If someone removes the label, the pod still runs. The GPU Operator adds the label again only at its next reconcile, not at once.
- The GPU Operator sets the label back to `true` before the new driver loads. The pod also needs `nvsentinel.dgxc.nvidia.com/driver.installed: "true"`, which the labeler sets when the node has a ready driver pod. Thus the pod starts again after the new driver is ready.
- In GPU Operator 26.7.0, a MIG change that times out can leave the label at `paused-for-mig-change`. The pod then stays off the node. Use GPU Operator 26.7.1 or later.

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

The default resolves the node's own primary IP through the downward API, which works whether the kubelet binds to `0.0.0.0` or to the node IP. A static address also works:

```yaml
metadata-collector:
  kubeletHost:
    value: "10.0.0.1"
```

Set `kubeletHost: {}` to leave the variable unset, which falls back to `localhost`. An explicit `--kubelet-kubeconfig` overrides this value entirely.

## Kubelet Root Directory

The collector maps pods to GPUs through the kubelet PodResources socket. Kubelet creates that socket under its `--root-dir`, so the chart mounts `<kubeletRootDir>/pod-resources` from the host at the fixed path the collector reads. Set this value to the kubelet `--root-dir` when a distribution runs kubelet from a non-default directory, for example `/var/lib/k0s/kubelet` on k0s:

```yaml
global:
  kubeletRootDir: /var/lib/kubelet   # default
```

If the value does not match kubelet, one of these occurs:

- The pod stays in `ContainerCreating` with a `FailedMount` event, because the host directory does not exist.
- The container logs `Pod device mapper failed` with the error `got an error creating Kubelet gRPC client: stat /var/lib/kubelet/pod-resources/kubelet.sock: no such file or directory` and exits, because the host directory exists but kubelet does not use it.

## Pod Mapper Failure Tolerance

Consecutive failed poll cycles the pod mapper tolerates before the container exits non-zero.

```yaml
metadata-collector:
  podMapper:
    maxConsecutiveFailures: 10
```

The poll period is 30 seconds, so the default rides out five minutes of failures — long enough to outlast a credential rotation or a kubelet restart, short enough to fail loudly when the collector is genuinely broken. The minimum is `1`, which exits on the first failed poll. This value sets the `--pod-mapper-max-consecutive-failures` flag described in [Startup and failure handling](#startup-and-failure-handling).

## Host-native authentication

Use two explicit kubeconfigs when the collector runs outside a pod:

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

An absent `KUBELET_HOST` defaults to `localhost`. An explicit kubelet kubeconfig overrides that variable. The collector does not reuse API server credentials for the kubelet. It does not load `KUBECONFIG` or a home-directory kubeconfig implicitly.

Explicit configurations must use HTTPS, verify server certificates, and provide credentials. Set the kubelet server to an address in its serving certificate, such as `https://gpu-node.example:10250`. Provide the correct CA and, if needed, `tls-server-name`. The existing in-cluster kubelet TLS behavior is unchanged.

### Credentials and permissions

Provision credentials at node runtime. Keep kubeconfig and credential files accessible only to the service account or root. Kubeconfigs are trusted input: client-go can execute a configured credential plugin.

- The Kubernetes API identity needs `patch` on pods in each workload namespace.
- The kubelet identity needs permission to read `/pods`. With fine-grained kubelet authorization, use `get` on `nodes/pods`. Other configurations require `get` on `nodes/proxy`, which grants broader access.
- The process needs access to `/var/lib/kubelet/pod-resources/kubelet.sock`, NVIDIA devices and libraries, and its output directory. The binary always uses that path. If kubelet uses a different `--root-dir`, make the socket available at that path.

Authentication does not grant permissions. Do not assume the kubelet's own client identity can patch workload pods. This feature creates no credentials or RBAC bindings.

Client-go supports kubeconfig bearer tokens, token files, client certificates, and configured credential providers. Prefer rotating file credentials over embedded long-lived credentials. Token-file reload is periodic, so provision a new token before the old token expires. Restart the collector after changes to kubeconfig settings or CA trust. Client certificate files reload through client-go; embedded certificate data does not reload.

Certificate authentication requires both a certificate and its private key. Each can be provided as a file or embedded data. A username and password alone do not satisfy the credential requirement.

Without an explicit kubelet kubeconfig, the transport rereads the projected ServiceAccount token on every request, including retries. This preserves recovery when that token rotates between attempts. The client does not retain tokens or set authentication headers itself.

### Startup and failure handling

Hardware inventory and pod-to-GPU mapping remain enabled. PodResources socket handling is unchanged. The kubelet socket must exist and be accessible when the mapper starts.

The existing 30-second poll period and `--pod-mapper-max-consecutive-failures` limit remain unchanged. The default limit is 10 failed polls. Persistent authentication, authorization, or socket failures remain errors; the collector does not report them as successful mapping.

A missing kubeconfig or credential file is a configuration error. HTTP 401 indicates rejected credentials. HTTP 403 indicates denied permission. TLS errors require correct CA trust and server identity; do not disable verification to work around them.
