# Deploying NVSentinel on SUSE Linux Enterprise and RKE2

This guide covers the settings to check when installing NVSentinel on SUSE Linux Enterprise Server (including SL Micro) with RKE2. It was validated on SUSE 16.0-family nodes, amd64 and arm64 with 64 KB-page kernels, running RKE2 v1.36 and GPU Operator.

## GPU Operator mode

RKE2 documents two GPU Operator setups ([RKE2 GPU Operator docs](https://docs.rke2.io/add-ons/gpu_operators)):

- **Default (non-NRI)**: GPU Operator creates the `nvidia` RuntimeClass. metadata-collector's default `runtimeClassName: nvidia` works.
- **NRI plugin mode** (`cdi.nriPluginEnabled: true`): there is no `nvidia` RuntimeClass. Set `metadata-collector.nriPlugin.enabled: true` and add the NVSentinel namespace to the toolkit's `NRI_MANAGEMENT_CDI_DEVICE_NAMESPACES`. See [GPU Operator NRI plugin mode](../configuration/metadata-collector.md#gpu-operator-nri-plugin-mode).

## SELinux

SUSE Linux Enterprise Server 16 enables SELinux in enforcing mode by default, and RKE2's RPM install enables RKE2 SELinux support (`rke2-selinux`, `RKE2_SELINUX=true`). On those nodes, set:

```yaml
global:
  seLinuxOptions:
    type: spc_t
```

Without it, platform-connectors cannot create `/var/run/nvsentinel.sock`, so no monitor can publish health events, and nic-health-monitor's init container cannot read its state directory. To check a node:

```bash
sestatus | grep "Current mode"                # enforcing?
grep selinux /etc/rancher/rke2/config.yaml    # selinux: true (or RKE2_SELINUX=true in the service environment)
ps -eZ | grep platform-connectors             # container_t means the pod is confined
```

Set `spc_t` only when all three checks apply: it disables SELinux confinement for these pods.

Nodes with SELinux disabled need nothing; the setting is ignored there.

## Driver installed on the host (SL Micro)

The labeler sets `nvsentinel.dgxc.nvidia.com/driver.installed=true` when it finds a GPU Operator driver pod, including the SUSE precompiled driver container. When the driver is installed on the host instead, as SUSE documents for SL Micro (GPU Operator `driver.enabled: false`), there is no driver pod. Set:

```yaml
labeler:
  assumeDriverInstalled: true
```

Otherwise, no GPU-node DaemonSet schedules.

## DCGM

gpu-health-monitor needs a DCGM hostengine on each GPU node. GPU Operator on RKE2 often runs dcgm-exporter without the standalone hostengine (`dcgm.enabled: false`), so choose one of the following options.

### Option 1: Standalone DCGM from GPU Operator (recommended)

Enable the standalone hostengine in the GPU Operator values:

```yaml
dcgm:
  enabled: true
```

GPU Operator then deploys the `nvidia-dcgm` DaemonSet and Service. NVSentinel's default `operator-service` mode connects to `nvidia-dcgm.gpu-operator.svc:5555`, and labeler derives the `nvsentinel.dgxc.nvidia.com/dcgm.version` node label from the DCGM pod image, so no NVSentinel settings are needed. If GPU Operator runs in a namespace other than `gpu-operator`, set `global.dcgm.service.endpoint` to match. See [GPU Health Monitor configuration](../configuration/gpu-health-monitor.md).

### Option 2: Embedded hostengine

If you cannot enable the standalone hostengine, use gpu-health-monitor `embedded-mode`, which runs the hostengine in-process:

```yaml
global:
  dcgm:
    mode: embedded-mode
gpu-health-monitor:
  runtimeClassName: nvidia   # non-NRI clusters
```

and label each GPU node with its DCGM major version, as `embedded-mode` requires:

```bash
kubectl label node <node> nvsentinel.dgxc.nvidia.com/dcgm.version=4.x
```

## System journal

syslog-health-monitor reads the host journal from `/var/log/journal`. SUSE Linux Enterprise Server keeps the journal persistent by default. If a node uses a volatile journal (`/run/log/journal` only), make it persistent:

```bash
mkdir -p /var/log/journal && systemctl restart systemd-journald
```

## Datastore on arm64

Both datastore images, `bitnamilegacy/postgresql:16.4` and `percona/percona-server-mongodb:8.0`, start on arm64 nodes with 64 KB-page kernels. MongoDB logs advisory transparent-hugepage tuning warnings at start-up.

## Host diagnostics

`sos` and `supportconfig` are both installed on SUSE Linux Enterprise Server 16. Log-collector currently runs `sos` only on GCP and AWS, so collect host diagnostics on other nodes manually, for example with `supportconfig`.
