# ADR-074: Compatibility — DCGM Versions Supported by NVSentinel

Status: Proposed (tracking issue [#2160](https://github.com/NVIDIA/NVSentinel/issues/2160)). Maintainer acceptance is pending.

## Context

### What the DCGM version in NVSentinel depends on

In the default `operator-service` mode ([ADR-044](044-dcgm-source-modes.md)), NVSentinel does not run the DCGM hostengine (`nv-hostengine`). The GPU Operator runs it and sets its version. Three NVSentinel components are DCGM clients of that hostengine:

| Component | DCGM call | Version pinned in |
|---|---|---|
| GPU health monitor (GHM) | Python bindings: health watches and field reads | `health-monitors/gpu-health-monitor/Dockerfile` |
| Preflight `preflight-dcgm-diag` | Python bindings: `group.action.RunDiagnostic` | `preflight-checks/dcgm-diag/Dockerfile` and `Makefile` |
| Lifecycle-manager validation test `dcgm-diag-test` | `dcgmi diag --run 2` | `config.tests.dcgm-diag-test.image` in the `lifecycle-manager` chart values |

Each client works only if its version fits the hostengine version. NVSentinel and the GPU Operator have separate releases, so they upgrade at different times.

ADR-044 has two more modes for GHM:

- `external-hostengine`: the cluster operator runs `nv-hostengine` on each node and sets its version. This ADR applies in the same way as in `operator-service` mode.
- `embedded-mode`: GHM runs the hostengine from its own image, so GHM and its hostengine always have the same version. This ADR does not apply to GHM in this mode.

Preflight and the Lifecycle-manager validation test do not read `global.dcgm.mode`. By default, they connect to the GPU Operator DCGM service. In each mode, this ADR applies to the hostengine that they connect to. They cannot use the embedded hostengine of GHM, because it listens only on the loopback address of the GHM pod.

## Decision

NVSentinel supports a DCGM client whose version is <= nv-hostengine(DCGM) version deployed. This applies to each DCGM client in NVSentinel: GHM, preflight and the Lifecycle-manager validation test. Each combination of a hostengine version and a client version has one status:

| Status | When |
|---|---|
| **Supported** | The hostengine is the same version as the client, or newer, with the same major version. |
| **No guarantee** | The client is newer than the hostengine. This applies also if the pair worked in our tests. |

- **Upgrade the hostengine first, then NVSentinel.** For a rollback, use the opposite order. With this order, each pair stays supported during the rollout.
- Supported means that NVSentinel takes responsibility for the pair. It does not mean that DCGM guarantees the pair. NVSentinel does not test each supported pair.

### Compatibility Matrix

Each NVSentinel release in this table is supported on the hostengines in the last column. Each row is the first release with a change in a DCGM version.

| NVSentinel release | GHM (`-dcgm-4.x` image) | Preflight | Lifecycle manager (validation test) | Supported hostengines |
|---|---|---|---|---|
| v0.1.0 | 4.2.0 | — | — | 4.2.0 and newer |
| v0.9.0 | 4.2.0 | 4.2.3 | — | 4.2.3 and newer |
| v0.10.0 | 4.4.2 | 4.2.3 | — | 4.4.2 and newer |
| v1.4.0 | 4.5.2 | 4.2.3 | — | 4.5.2 and newer |
| v1.9.0 | 4.5.2 | 4.5.2 | — | 4.5.2 and newer |
| v1.22.0 | 4.5.2 | 4.5.2 | 4.5.2 | 4.5.2 and newer |
| v1.24.0 to v1.32.0 | 4.7.0 | 4.7.0 | 4.7.0 | 4.7.0 and newer |

- "—" means that the component is not in that release.
- "and newer" means newer DCGM 4 versions only. GHM has images only for DCGM 3 and DCGM 4 hostengines ([ADR-044](044-dcgm-source-modes.md)).
- Preflight v0.10.0 to v1.8.x: the Dockerfile default is 4.4.2, but the `docker-publish` target in the Makefile passes 4.2.3.
- The GHM `-dcgm-3.x` image uses DCGM 3.3.7 in all releases. DCGM 3 is out of scope. See [Notes](#notes).

## Implementation

### When you change a DCGM version

| Change | What to do before the rollout |
|---|---|
| Hostengine upgrade: a GPU Operator upgrade, or an upgrade of an external hostengine | Do not roll out a hostengine that is older than an NVSentinel client. Upgrade the hostengine before NVSentinel. If the new hostengine is not tested, run the tests with the current NVSentinel release |
| New DCGM version in GHM, preflight or the Lifecycle-manager validation image | The new version is supported only on hostengines of the same version or newer. A newer client can make the oldest supported hostengine of the release newer. Run the tests. Add the new versions to the [Compatibility](#compatibility-matrix) table, and put the test results in the PR description. Change all pins of one component in one PR |
| Custom DCGM image or Helm override by an operator | Keep the hostengine the same version as the client, or newer. Other combinations have no guarantee |

For a DCGM version change in NVSentinel, read the DCGM changelog for each release since the current one. Look for changed request versions and new environment gates. In the same PR, add the new versions to the [Compatibility](#compatibility-matrix) table, and put the test results in the PR description. Give the oldest supported hostengine version in the release notes.

### Testing

- **Test on real GPUs.** The CI E2E tests and Tilt use a simulated hostengine (`nvsentinel-fake-dcgm:4.2.0`). They cannot find a version mismatch.
- **Test the healthy case.** After the upgrade, no node stays cordoned.
- **Use a test cluster.** The tests inject faults. Each fault can quarantine and reboot the node.

In GPU Operator ClusterPolicy mode, set the hostengine version with `.spec.dcgm.version` in the ClusterPolicy. Before you test, wait until each DCGM pod in the cluster runs the new version. The tests can select any GPU node.

| Component | Test | Pass |
|---|---|---|
| Lifecycle-manager `dcgmi`, quick check | Run `dcgmi diag --run 2` from the validation image on a GPU node (command below) | Exit code 0. A mismatch gives `(-12) API version mismatch`, exit code 244 |
| GHM | Read the GHM log and the node conditions. Then, on one node, run the GHM fault commands below: inject InfoROM and row remap, read the conditions, and clear the faults. Wait until the node is uncordoned and any reboot is complete. Then inject XID 95 | No "Error getting DCGM handle" or "API version mismatch" in the log. `GpuDcgmConnectivityFailure` is `False`. InfoROM and row remap make the `GpuInforomWatch` and `GpuMemWatch` conditions unhealthy, and the conditions become healthy after the clear. XID 95 gives a quarantine, a reboot and a successful validation. The reboot clears the injected XID |
| Preflight | Run a GPU pod in a namespace with the label `nvsentinel.nvidia.com/preflight=enabled` | The `preflight-dcgm-diag` log has a `Diagnostic summary` line with `passed` above 0, `failed` and `warned` 0, then a `DCGM diagnostic check passed` line, and no `Failed to send health event` line. At least one event was sent, and each event is healthy. No `DCGM_ST_VER_MISMATCH` event |

Quick check for the Lifecycle-manager validation image. Replace `<tag>` with the tag of the image, for example `4.7.0-ubuntu24.04`. In GPU Operator GPUCluster (DRA) mode, replace `nvidia-dcgm` with `nvidia-dcgm-dra` in the host name:

```bash
kubectl run dcgm-version-check --rm -i --restart=Never \
  --image=nvcr.io/nvidia/cloud-native/dcgm:<tag> \
  --overrides='{"apiVersion":"v1","spec":{"nodeName":"<gpu-node>"}}' \
  --command -- dcgmi diag --host nvidia-dcgm.gpu-operator.svc:5555 --run 2
```

GHM fault commands. Use a node that NVSentinel manages. Run them in the GHM pod on the test node. Replace `<dcgm-host>` with the hostengine that GHM uses, for example `nvidia-dcgm.gpu-operator.svc`. InfoROM and row remap can quarantine and reboot the node before the clear. Clear the faults after the conditions are unhealthy:

```bash
dcgmi test --host <dcgm-host>:5555 --inject --gpuid 0 -f 84 -v 0    # InfoROM fault
dcgmi test --host <dcgm-host>:5555 --inject --gpuid 0 -f 395 -v 1   # row remap fault
dcgmi test --host <dcgm-host>:5555 --inject --gpuid 0 -f 84 -v 1    # clear InfoROM
dcgmi test --host <dcgm-host>:5555 --inject --gpuid 0 -f 395 -v 0   # clear row remap
dcgmi test --host <dcgm-host>:5555 --inject --gpuid 0 -f 230 -v 95  # XID 95
```

## Consequences

### Positive

- Operators compare their hostengine version with the client versions and get one status.
- Each DCGM version change states its reason and updates the [Compatibility](#compatibility-matrix) table.

### Negative

- Each DCGM change needs GPU test time on each hostengine version that the change affects. CI does not cover it.
- A client that is newer than its hostengine has no guarantee, also if the pair works in our tests. A client upgrade must wait for the hostengine upgrade.

### Mitigations

- If a problem occurs, roll back in the opposite order of the upgrade: NVSentinel first, then the hostengine.
- Test each new hostengine release with the current NVSentinel release before the rollout.

## Alternatives Considered

### Support each pair that passes the tests

With this option, the test results are the support statement. A client that is newer than its hostengine is supported when its tests pass.

**Rejected** because: a passing test shows that a pair works today, but DCGM does not promise it. A newer client can send a request version that an older hostengine does not know, and the hostengine rejects it with `(-12) API version mismatch`. A test result for one pair does not cover the next client version, so NVSentinel cannot take responsibility for these pairs.

## Notes

- Out of scope: DCGM 3. Its support in GHM is deprecated, with removal planned for 30 November 2026 ([deprecation notice](../deprecations/gpu-health-monitor-dcgm-3-support-removal.md)).

## References

- Issue [#2160](https://github.com/NVIDIA/NVSentinel/issues/2160): the DCGM upgrade request that this ADR answers.
- [ADR-026](026-preflight-checks.md), [ADR-044](044-dcgm-source-modes.md), [ADR-049](049-node-validation.md).
- [Validation configuration](../configuration/validation.md), [Preflight configuration](../configuration/preflight.md).
- DCGM changelog: <https://docs.nvidia.com/datacenter/dcgm/latest/release-notes/changelog.html>
- DCGM `nv-hostengine` reference: <https://docs.nvidia.com/datacenter/dcgm/latest/reference/command-line-reference/nv-hostengine.html>
- DCGM lifecycle document: <https://github.com/NVIDIA/DCGM/blob/master/docs/support_EOL.md>
