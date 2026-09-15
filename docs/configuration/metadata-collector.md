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

## Host-path driver access (NRI-mode clusters)

On clusters where GPU Operator is configured for CDI + NRI device injection, a `RuntimeClass` matching `operator.runtimeClass` is often never created. Setting `runtimeClassName` then fails admission, and leaving it unset crash-loops with `NVML: ERROR_LIBRARY_NOT_FOUND`. Requesting `nvidia.com/gpu` works but reserves a GPU for the DaemonSet.

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
- The process needs access to `/var/lib/kubelet/pod-resources/kubelet.sock`, NVIDIA devices and libraries, and its output directory.

Authentication does not grant permissions. Do not assume the kubelet's own client identity can patch workload pods. This feature creates no credentials or RBAC bindings.

Client-go supports kubeconfig bearer tokens, token files, client certificates, and configured credential providers. Prefer rotating file credentials over embedded long-lived credentials. Token-file reload is periodic, so provision a new token before the old token expires. Restart the collector after changes to kubeconfig settings or CA trust. Client certificate files reload through client-go; embedded certificate data does not reload.

### Startup and failure handling

Hardware inventory and pod-to-GPU mapping remain enabled. A missing PodResources socket no longer fails client construction. Each PodResources call waits up to 20 seconds for readiness and honors cancellation. The connection can recover after kubelet restarts.

The existing 30-second poll period and `--pod-mapper-max-consecutive-failures` limit remain unchanged. The default limit is 10 failed polls. Persistent authentication, authorization, or socket failures remain errors; the collector does not report them as successful mapping.

A missing kubeconfig or credential file is a configuration error. HTTP 401 indicates rejected credentials. HTTP 403 indicates denied permission. TLS errors require correct CA trust and server identity; do not disable verification to work around them.
