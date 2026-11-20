# Deploying NVSentinel on SUSE Linux Enterprise and RKE2

This guide covers the settings to check when you install NVSentinel on SUSE Linux Enterprise Server (including SL Micro) with RKE2. The guide was validated on SUSE 16.0-family nodes (amd64, and arm64 with 64 KB-page kernels) that run RKE2 v1.36 and GPU Operator.

## GPU Operator mode

RKE2 documents two GPU Operator setups:

- **Default (non-NRI)**: GPU Operator creates the `nvidia` RuntimeClass. The metadata-collector default `runtimeClassName: nvidia` works.
- **NRI plugin mode** (`cdi.nriPluginEnabled: true`): there is no `nvidia` RuntimeClass. Set `metadata-collector.nriPlugin.enabled: true`. Also add the NVSentinel namespace to the toolkit's `NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES`. See [GPU Operator NRI plugin mode](../configuration/metadata-collector.md#gpu-operator-nri-plugin-mode).

## SELinux

SUSE Linux Enterprise Server 16 enables SELinux in enforcing mode by default. The RPM install of RKE2 enables RKE2 SELinux support (`rke2-selinux`, `RKE2_SELINUX=true`). On those nodes, set:

```yaml
global:
  seLinuxOptions:
    type: spc_t
```

Without this setting, platform-connectors cannot create `/var/run/nvsentinel.sock`, so no monitor can publish health events. Also, the init container of nic-health-monitor cannot read its state directory. To check a node, use these commands:

```bash
sestatus | grep "Current mode"                # enforcing?
grep selinux /etc/rancher/rke2/config.yaml    # selinux: true (or RKE2_SELINUX=true in the service environment)
ps -eZ | grep platform-connectors             # container_t means the pod is confined
```

If all three checks apply, set `spc_t`. Do not set it in other cases, because it disables SELinux confinement for these pods.

Nodes with SELinux disabled do not need this setting. On those nodes, the setting has no effect.

## Driver installed on the host (SL Micro)

The labeler sets `nvsentinel.dgxc.nvidia.com/driver.installed=true` when it finds a GPU Operator driver pod, including the SUSE precompiled driver container. For SL Micro, SUSE documents a driver that is installed on the host (GPU Operator `driver.enabled: false`). If the driver is installed on the host, there is no driver pod. In this case, set:

```yaml
labeler:
  assumeDriverInstalled: true
```

If you do not set this value, no GPU-node DaemonSet schedules.

## DCGM

gpu-health-monitor needs a DCGM hostengine on each GPU node. GPU Operator on RKE2 often runs dcgm-exporter without the standalone hostengine (`dcgm.enabled: false`). Thus, select one of the following options.

### Option 1: Standalone DCGM from GPU Operator (recommended)

Enable the standalone hostengine in the GPU Operator values:

```yaml
dcgm:
  enabled: true
```

GPU Operator then deploys the `nvidia-dcgm` DaemonSet and Service. The default `operator-service` mode of NVSentinel connects to `nvidia-dcgm.gpu-operator.svc:5555`. The labeler derives the `nvsentinel.dgxc.nvidia.com/dcgm.version` node label from the DCGM pod image. Thus, you do not need to change NVSentinel settings. If GPU Operator runs in a namespace other than `gpu-operator`, set `global.dcgm.service.endpoint` to match. See [GPU Health Monitor configuration](../configuration/gpu-health-monitor.md).

### Option 2: Embedded hostengine

If you cannot enable the standalone hostengine, use the gpu-health-monitor `embedded-mode`. This mode runs the hostengine in-process:

```yaml
global:
  dcgm:
    mode: embedded-mode
gpu-health-monitor:
  runtimeClassName: nvidia   # non-NRI clusters
```

Then label each GPU node with its DCGM major version, because `embedded-mode` requires this label:

```bash
kubectl label node <node> nvsentinel.dgxc.nvidia.com/dcgm.version=4.x
```

## System journal

syslog-health-monitor reads the host journal from `/var/log/journal`. SUSE Linux Enterprise Server keeps the journal persistent by default. If a node uses a volatile journal (`/run/log/journal` only), make it persistent:

```bash
mkdir -p /var/log/journal && systemctl restart systemd-journald
```

## Datastore on arm64

Both datastore images, `bitnamilegacy/postgresql:16.4` and `percona/percona-server-mongodb:8.0`, start on arm64 nodes with 64 KB-page kernels. At start-up, MongoDB logs advisory warnings about transparent-hugepage tuning.

## Host diagnostics

`sos` and `supportconfig` are both installed on SUSE Linux Enterprise Server 16. Log-collector runs `sos` only on GCP and AWS. On other nodes, collect host diagnostics manually, for example with `supportconfig`.
