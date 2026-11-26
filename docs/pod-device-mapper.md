# Pod Device Mapper

## Overview

The pod device mapper records the devices of each pod as the `dgxc.nvidia.com/devices` annotation on the pod. A partial drain in the node-drainer reads this annotation to evict only the pods that use a failed device, for example one GPU, instead of every pod on the node.

An annotation lists each device under the way the pod got it, with the ID that health events use for the device. A pod with one GPU from the NVIDIA device plugin gets:

```text
dgxc.nvidia.com/devices: {"devices":{"nvidia.com/gpu":["GPU-8f6d..."]}}
```

The mapper records the devices that `global.partialRemediationEntityTypes` lists. By default, these are NVIDIA GPUs, from the device plugin or from the NVIDIA DRA driver.

## How It Works

To create the annotation, the mapper does these steps on each node:

1. To find all pods that run on the node, the mapper calls the Kubelet /pods HTTPS endpoint.
2. To find the devices that are allocated to each pod, the mapper uses the Kubelet PodResourcesLister gRPC service. It records only the devices that `global.partialRemediationEntityTypes` lists. By default, these are the NVIDIA GPUs:
   - Device-plugin allocations (by default `nvidia.com/gpu` and `nvidia.com/pgpu`): the mapper records them under the resource name, with the device IDs that the plugin reported. For the NVIDIA device plugin, these IDs are GPU UUIDs.
   - DRA allocations (by default the `gpu.nvidia.com` driver of GPU Operator GPUCluster mode): they arrive as a driver, a pool, and a device name such as `gpu-1`. The mapper finds that device in the ResourceSlices of its node, and records the value of the configured attribute under the driver name. For NVIDIA GPUs, the attribute is `uuid`, so the mapper records the GPU UUID. The kubelet reports DRA allocations only with the `KubeletPodResourcesDynamicResources` feature gate. This feature gate is enabled by default from Kubernetes 1.34.
   - If a DRA device of a pod is not in the ResourceSlices yet, or has no such attribute, the pod gets no annotation, and an existing one is removed. An annotation that lists only some devices of a pod would make a partial drain miss that pod. Without an annotation, the partial drain waits instead.
3. If the devices of a pod change, the mapper updates the annotation on the pod object.
4. The mapper runs this logic every 30 seconds, to continually update the mapping for new and existing pods.

## Where It Runs

The mapper runs in the `pod-device-mapper` DaemonSet, which the metadata-collector Helm chart renders next to the [Metadata Collector](metadata-collector.md). It does not use the GPU driver or any vendor library, so it runs on the pod network without privileges, and it starts on a node before the GPU driver is ready. By default it runs on the NVIDIA GPU nodes. To record devices on other nodes, select them with `metadata-collector.podMapper.nodeSelector`.

## Configuration

See [Pod Device Mapper Configuration](configuration/pod-device-mapper.md).
