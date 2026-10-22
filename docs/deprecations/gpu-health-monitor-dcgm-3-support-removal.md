# GPU Health Monitor: DCGM 3.x Support Removal

DCGM 3.x support in GPU Health Monitor is deprecated. It will be removed on 30 November 2026.

| Field | Value |
|---|---|
| Status | Deprecated |
| Component | GPU Health Monitor |
| Removal | 30 November 2026 |
| Removal PR | [PR #2102](https://github.com/NVIDIA/NVSentinel/pull/2102) |

## Reason

DCGM 3.x is end-of-life. The [DCGM product lifecycle](https://github.com/NVIDIA/DCGM/blob/master/docs/support_EOL.md) supports each minor release for one year. After that, the release becomes a legacy release, and DCGM creates no more patch releases for it. DCGM 3.3.9 and all earlier releases are legacy releases. The 3.3.x branch has had no updates for about two years, gets no security fixes, and does not support newer GPU platforms.

GPU Health Monitor ships one DaemonSet and one image for each DCGM major version. Each release builds, tests and publishes the 3.x image and DaemonSet for a DCGM version that is no longer supported. After the removal, GPU Health Monitor supports only DCGM 4.x, and new DCGM features need to support only DCGM 4.x.

## Impact

After the removal, NVSentinel will not detect faults on nodes running DCGM 3.x. Nodes running DCGM 4.x are not affected.

## Required Action

Find the nodes that run DCGM 3.x:

```bash
kubectl get nodes -l nvsentinel.dgxc.nvidia.com/dcgm.version=3.x
```

Upgrade DCGM on these nodes to 4.x before you upgrade NVSentinel to the release that removes DCGM 3.x support. The steps depend on `global.dcgm.mode`.

### Operator Service Mode

In `operator-service` mode, the GPU Operator runs DCGM. Make the change in the GPU Operator, not in NVSentinel. Set the GPU Operator `dcgm.version` Helm value to a DCGM 4.x image, or upgrade to a GPU Operator release that ships DCGM 4.x. Labeler reads the new DCGM pod image and changes the node label to `4.x`.

### External Hostengine Mode

In `external-hostengine` mode, you manage the hostengine.

1. Upgrade the hostengine on the node to DCGM 4.x.
2. Change the node label to `4.x`:

   ```bash
   kubectl label node <node-name> nvsentinel.dgxc.nvidia.com/dcgm.version=4.x --overwrite
   ```

The monitor image must match the major version of the hostengine. Change the label immediately after the hostengine upgrade.

### Embedded Mode

In `embedded-mode`, the GPU Health Monitor image contains DCGM. Change the node label to `4.x`, and GPU Health Monitor then runs its DCGM 4.x image on the node:

```bash
kubectl label node <node-name> nvsentinel.dgxc.nvidia.com/dcgm.version=4.x --overwrite
```

## Feedback

If you have any concerns, please provide feedback in [PR #2102](https://github.com/NVIDIA/NVSentinel/pull/2102).
