# Pod Device Mapper Configuration

## Overview

The pod device mapper writes the `dgxc.nvidia.com/devices` annotation on each pod that has a device of [`global.partialRemediationEntityTypes`](node-drainer.md#partial-drain). Partial drains in the node-drainer read this annotation to find the pods that use a failed device. See [Pod Device Mapper](../pod-device-mapper.md) for how it works.

The mapper runs in the `pod-device-mapper` DaemonSet. The metadata-collector chart renders it next to the `nvidia-metadata-collector` DaemonSet of the NVIDIA discovery, so its values are under `metadata-collector`. `global.metadataCollector.enabled: false` turns off both. The mapper needs no GPU driver and no vendor settings. It runs on the pod network, without privileges and with all capabilities dropped. It runs as root only because the kubelet's PodResources socket belongs to root.

These values of the chart apply to both DaemonSets: `logLevel`, `podAnnotations`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `updateStrategy` and `metrics.enabled`, with their `global` overrides. `runtimeClassName`, `nriPlugin`, `extraEnv`, `additionalVolumeMounts`, `additionalHostVolumes` and `metrics.port` apply only to the NVIDIA discovery.

## Where it runs

By default, the mapper runs on the nodes with the `nvidia.com/gpu.present: "true"` label, together with `global.nodeSelector` or `nodeSelector`. Unlike the NVIDIA discovery, it does not wait for the `nvsentinel.dgxc.nvidia.com/driver.installed` label: until the driver is ready, no pod on the node has a GPU, so the mapper writes no annotations.

To record devices on other nodes, select those nodes. Helm adds your labels to the default, so remove the default with `null`:

```yaml
metadata-collector:
  podMapper:
    nodeSelector:
      nvidia.com/gpu.present: null
      example.com/accelerator: "true"
```

An `affinity` that you set applies to the mapper. The default affinity of the NVIDIA discovery, which follows GPU driver upgrades, does not.

## Image and resources

```yaml
metadata-collector:
  podMapper:
    image:
      repository: ghcr.io/nvidia/nvsentinel/pod-device-mapper
      pullPolicy: IfNotPresent
      tag: ""   # defaults to global.image.tag, then to the chart appVersion
    resources: {}   # when empty, the mapper uses metadata-collector.resources
```

## Metrics

The mapper serves its metrics on `global.metricsPort`, the port that the namespace NetworkPolicy allows and the PodMonitor scrapes, like the other NVSentinel pods on the pod network. `metadata-collector.metrics.enabled: false` turns the endpoint off. The metric names start with `metadata_collector_pod_mapper_`, as before ADR-072. See [Metrics](../METRICS.md).

## Kubelet Host

Sets the `KUBELET_HOST` environment variable, which tells the mapper where to reach the kubelet `/pods` endpoint.

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

The mapper runs on the pod network, where `localhost` is the pod itself. So `kubeletHost: {}` and a loopback value (`localhost`, `127.0.0.1`, `::1`) also give the node IP. An explicit `--kubelet-kubeconfig` fully overrides this value.

## Kubelet Root Directory

The mapper maps pods to devices through the kubelet PodResources socket. Kubelet creates that socket under its `--root-dir`. Thus, the chart mounts `<kubeletRootDir>/pod-resources` from the host at the fixed path that the mapper reads. If a distribution runs kubelet from a non-default directory, set this value to the kubelet `--root-dir`. For example, k0s uses `/var/lib/k0s/kubelet`:

```yaml
global:
  kubeletRootDir: /var/lib/kubelet   # default
```

If the value does not match kubelet, one of these occurs:

- The pod stays in `ContainerCreating` with a `FailedMount` event, because the host directory does not exist.
- The container logs `Pod device mapper failed` with the error `got an error creating Kubelet gRPC client: stat /var/lib/kubelet/pod-resources/kubelet.sock: no such file or directory` and exits, because the host directory exists but kubelet does not use it.

## Failure Tolerance

The number of consecutive failed poll cycles that the mapper tolerates before the container exits with a non-zero code.

```yaml
metadata-collector:
  podMapper:
    maxConsecutiveFailures: 10
```

The poll period is 30 seconds. Thus, the default tolerates five minutes of failures. This time is long enough to outlast a credential rotation or a kubelet restart. It is also short enough to fail visibly when the mapper has a real fault. The minimum is `1`, which exits on the first failed poll. This value sets the `--pod-mapper-max-consecutive-failures` flag that [Startup and failure handling](#startup-and-failure-handling) describes.

## Recorded devices

The pod device mapper records the devices that [`global.partialRemediationEntityTypes`](node-drainer.md#partial-drain) lists, the same value that the node-drainer reads. The chart renders it into the `pod-device-mapper` ConfigMap, which the mapper reads at start. A change to the value restarts the mapper pods.

- For a device-plugin entry, the mapper records the device IDs that the kubelet reports under each of the `resourceNames`.
- For a DRA entry, the kubelet reports each allocated device with its driver, pool and device name. The mapper finds that device in the ResourceSlices of its node, and records the value of the `entityValueAttribute` attribute under the `driver` name. A driver can name its own attributes without its domain, so `uuid` and `gpu.nvidia.com/uuid` are the same attribute of the `gpu.nvidia.com` driver. The attribute must have a string value. Only ResourceSlices that set `spec.nodeName` to the node are read.

For a full GPU, the NVIDIA DRA driver sets the `uuid` attribute to the GPU UUID from NVML. This is the same value that the GPU health monitor reports as `GPU_UUID`, so the default entry matches the events. For other NVIDIA DRA devices, `uuid` has a different value, so partial drains for `GPU_UUID` do not match them: a MIG device has the UUID of the MIG device (`MIG-...`), and a GPU for VFIO passthrough has a UUID that the driver derives from the PCI address.

The mapper lists ResourceSlices only when a pod on the node has a DRA device that it has not seen before. It lists only the ResourceSlices of its node and of that driver, from the cache of the API server. It keeps each value until it restarts. For this, the chart gives the `metadata-collector` ClusterRole, which the mapper uses, `list` permission on ResourceSlices when an entity type has a DRA entry.

To change the `entityValueAttribute` of a driver, follow [Changing entityValueAttribute](node-drainer.md#changing-entityvalueattribute): until the mapper writes the new values, the annotations hold values of the old attribute.

If the mapper cannot find the value of a DRA device, the pod gets no annotation, and an existing annotation is removed. The mapper tries again on the next poll. If the mapper cannot read the ResourceSlices, for example because it has no permission, it still updates the annotations of the other pods, and the poll counts as failed. A lasting failure then reaches the [failure limit](#failure-tolerance). An annotation that lists only some devices of a pod would make a partial drain miss that pod. Without an annotation, the partial drain waits instead.

## Disabling the Pod Mapper

Partial drains in the node-drainer need the annotation that the mapper writes. Disable the mapper only where it cannot work. For example, the mapper cannot work when the pods that the kubelet reports are not the pods that the mapper sees through the Kubernetes API.

```yaml
metadata-collector:
  podMapper:
    enabled: false
```

The chart then does not render the `pod-device-mapper` DaemonSet. The NVIDIA discovery still writes the GPU metadata file. Pods get no device annotations. Thus, keep `node-drainer.partialDrainEnabled` set to `false`, because a partial drain fails for a GPU pod without the annotation. Full drains still detect GPU pods from their container limits and DRA claims. The value is compared as a string, so a quoted `"false"` also turns the mapper off.

## Upgrade from a single metadata-collector DaemonSet

Before ADR-072, one `metadata-collector` DaemonSet ran both the NVIDIA discovery and the mapper. The upgrade creates the `nvidia-metadata-collector` and `pod-device-mapper` DaemonSets and deletes the `metadata-collector` DaemonSet. Its pods stop on all nodes together, without a rolling update. Until the new mapper pod of a node is ready, the node has no mapper, and a partial drain there waits for the missing annotations. The GPU health monitors keep reading the existing `gpu_metadata.json` until the new discovery pod writes it again.

The `metadata-collector` ServiceAccount and ClusterRole stay with the mapper. The NVIDIA discovery uses the namespace's default ServiceAccount, without a token. A query that filters metrics by the `metadata-collector` pod name no longer matches, and the mapper metrics move from `metrics.port` on the node to `global.metricsPort` on the pod.

## Host-native authentication

If the mapper runs outside a pod, run the `pod-device-mapper` binary with two explicit kubeconfigs:

```bash
NODE_NAME=gpu-node-1 pod-device-mapper \
  --kubeconfig=/etc/nvsentinel/pod-device-mapper/apiserver.kubeconfig \
  --kubelet-kubeconfig=/etc/nvsentinel/pod-device-mapper/kubelet.kubeconfig \
  --config=/etc/nvsentinel/pod-device-mapper/config.toml
```

Until ADR-072, the mapper ran inside the `metadata-collector` binary, with the same flags. `metadata-collector` still accepts these flags and ignores them, so an existing host setup keeps collecting GPU metadata, but it needs the `pod-device-mapper` binary for the annotations.

`--config` is required. It names a TOML file with the [recorded devices](#recorded-devices), in the form that the chart renders into the `config.toml` key of the `pod-device-mapper` ConfigMap:

```toml
[partialRemediationEntityTypes.GPU_UUID]
  scopedActions = ["COMPONENT_RESET"]

  [[partialRemediationEntityTypes.GPU_UUID.devices]]
    resourceNames = ["nvidia.com/gpu", "nvidia.com/pgpu"]

  [[partialRemediationEntityTypes.GPU_UUID.devices]]
    deviceClasses = ["gpu.nvidia.com"]
    driver = "gpu.nvidia.com"
    entityValueAttribute = "uuid"
```

Set `NODE_NAME` to the name of the Kubernetes node. The mapper needs it to read the ResourceSlices of the node for DRA devices. If an entity type has a DRA entry, as the default does, the mapper does not start without it. In the chart, the DaemonSet sets it from the node of the pod.

| Flag | Purpose | Default |
|---|---|---|
| `--kubeconfig` | Kubernetes API endpoint, trust, and credentials for pod annotation updates | In-cluster configuration |
| `--kubelet-kubeconfig` | Kubelet HTTPS endpoint, trust, and credentials for `/pods` | `KUBELET_HOST:10250` and the projected ServiceAccount token |
| `--config` | TOML configuration file | None. Required |
| `--pod-mapper-max-consecutive-failures` | [Failure tolerance](#failure-tolerance) | `10` |
| `--metrics-port` | Port of the Prometheus metrics endpoint. `0` turns it off | `2112` |

If `KUBELET_HOST` is not set, the default is `localhost`. An explicit kubelet kubeconfig overrides that variable. The mapper does not use the API server credentials for the kubelet. It does not load `KUBECONFIG` or a home-directory kubeconfig implicitly.

Explicit configurations must use HTTPS, verify server certificates, and provide credentials. Set the kubelet server to an address in its serving certificate, such as `https://gpu-node.example:10250`. Provide the correct CA and, if needed, `tls-server-name`.

### Credentials and permissions

Provision credentials at node runtime. Keep kubeconfig and credential files accessible only to the service account or root. Kubeconfigs are trusted input, because client-go can run a configured credential plugin.

- The Kubernetes API identity needs `patch` on pods in each workload namespace. If an entity type has a DRA entry, it also needs `list` on `resourceslices` in the `resource.k8s.io` API group.
- The kubelet identity needs permission to read `/pods`. With fine-grained kubelet authorization, use `get` on `nodes/pods`. Other configurations require `get` on `nodes/proxy`, which grants broader access.
- The process needs access to `/var/lib/kubelet/pod-resources/kubelet.sock`. The binary always uses that path. If kubelet uses a different `--root-dir`, make the socket available at that path.

Authentication does not grant permissions. Do not assume that the client identity of the kubelet can patch workload pods. This feature creates no credentials or RBAC bindings.

Client-go supports kubeconfig bearer tokens, token files, client certificates, and configured credential providers. Prefer rotating file credentials to embedded long-lived credentials. Token-file reload is periodic, so provision a new token before the old token expires. Restart the mapper after changes to kubeconfig settings or CA trust. Client certificate files reload through client-go. Embedded certificate data does not reload.

Certificate authentication requires both a certificate and its private key. You can provide each as a file or as embedded data. A username and password alone do not satisfy the credential requirement.

Without an explicit kubelet kubeconfig, the transport reads the projected ServiceAccount token again on every request, also on retries. Thus, recovery continues to work when that token rotates between attempts. The client does not keep tokens or set authentication headers itself.

### Startup and failure handling

The kubelet socket must exist and be accessible when the mapper starts.

The 30-second poll period and the `--pod-mapper-max-consecutive-failures` limit also apply. The default limit is 10 failed polls. Persistent authentication, authorization, or socket failures stay errors. The mapper does not report them as successful mapping.

A missing kubeconfig or credential file is a configuration error. HTTP 401 indicates rejected credentials. HTTP 403 indicates denied permission. To correct a TLS error, configure the correct CA trust and server identity. Do not disable verification to bypass TLS errors.
