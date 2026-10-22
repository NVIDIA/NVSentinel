# GPU Health Monitor: DCGM 3.x Support Removal

DCGM 3.x support in GPU Health Monitor is deprecated. It will be removed no later than v1.32.0.

| Field | Value |
|---|---|
| Status | Deprecated |
| Component | GPU Health Monitor |
| Feedback window closes | 9 November 2026 |
| Removal | No later than v1.32.0 |

## Reason

DCGM 3.x is end-of-life. The [DCGM product lifecycle](https://github.com/NVIDIA/DCGM/blob/master/docs/support_EOL.md) supports each minor release for one year. After that, the release becomes a legacy release, and DCGM creates no more patch releases for it. DCGM 3.3.9 and all earlier releases are legacy releases. The 3.3.x branch has had no updates for about two years, gets no security fixes, and does not support newer GPU platforms.

GPU Health Monitor ships one DaemonSet and one image for each DCGM major version. Each release builds, tests and publishes the 3.x image and DaemonSet for a DCGM version that is no longer supported. After the removal, GPU Health Monitor supports only DCGM 4.x, and new DCGM features need to support only DCGM 4.x.

## Impact

After the removal, a node that runs DCGM 3.x runs no GPU Health Monitor pod, and nothing reports an error. NVSentinel does not detect GPU faults on that node. Nodes that run DCGM 4.x are not affected.

## Required Action

1. Find the nodes that run DCGM 3.x:

   ```bash
   kubectl get nodes -l nvsentinel.dgxc.nvidia.com/dcgm.version=3.x
   ```

2. Upgrade DCGM on these nodes to 4.x before you upgrade to the release that removes DCGM 3.x support. In `operator-service` mode, upgrade the DCGM component of the GPU Operator.
3. If you cannot upgrade DCGM, stay on the last NVSentinel release that supports DCGM 3.x, and raise an objection.

## Objections

Before the feedback window closes, open a [GitHub issue](https://github.com/NVIDIA/NVSentinel/issues/new/choose) if your deployment depends on DCGM 3.x. Give the DCGM 3.x version, the environment, and the reason that the nodes cannot move to DCGM 4.x.

The maintainers decide whether an objection blocks the removal. If it does, they extend the feedback window or keep DCGM 3.x support.

## Timeline

| Step | Release | Expected date |
|---|---|---|
| The feedback window closes | — | 9 November 2026 |
| The removal ships, if there is no blocking objection | No later than v1.32.0 | No later than 16 November 2026 |

The GPU Health Monitor configuration page shows a [deprecation warning](../configuration/gpu-health-monitor.md#dcgm-version-selection) until the removal ships.
