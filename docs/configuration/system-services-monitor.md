# System Services Monitor Configuration

CLI flags, environment variables, and per-environment recipes for `system_services_monitor`. For component overview, scope, and architecture, see [`docs/system-services-monitor.md`](../system-services-monitor.md).

## CLI Flags

All options are available as `system_services_monitor` CLI flags:

```
system_services_monitor \
  --platform-connector-socket /run/nvsentinel/platform-connector.sock \
  --port 9101 \
  --poll-interval 30 \
  --boot-grace-period 300 \
  --flap-window 600 \
  --flap-threshold 3 \
  --enable-fabric-check \
  --processing-strategy EXECUTE_REMEDIATION
```

| Flag | Default | Purpose |
|---|---|---|
| `--platform-connector-socket` | _(required)_ | Unix socket path for the gRPC connection to platform-connector |
| `--port` | `9101` | Prometheus metrics HTTP server port |
| `--poll-interval` | `30` | Seconds between check cycles |
| `--node-name` | `$NODE_NAME` then `$HOSTNAME` | Node name used as health-event entity |
| `--boot-grace-period` | `300` | Seconds after startup to suppress unhealthy alerts |
| `--flap-window` | `600` | Seconds window for counting service restarts |
| `--flap-threshold` | `3` | Restart count within `--flap-window` to flag flapping |
| `--enable-fabric-check / --disable-fabric-check` | enabled | Enable Fabric Manager service check |
| `--processing-strategy` | `EXECUTE_REMEDIATION` | One of `EXECUTE_REMEDIATION`, `STORE_ONLY` |
| `--verbose` | off | Enable debug logging |

## Environment Variables

- `NODE_NAME` / `HOSTNAME` -- Node name (used as entity in health events) when `--node-name` is not set
- `LOG_LEVEL` -- Log level: `debug`, `info`, `warn`, `error` (default: `info`); overridden when `--verbose` is set

## GKE Container-Optimized OS (COS) Recipe

On COS, Fabric Manager runs inside a gpu-operator DaemonSet pod rather than as a host systemd unit. Disable the host-systemd FM check and rely on `nvidia-smi fabric.state` for FM status:

```
system_services_monitor \
  --platform-connector-socket /run/nvsentinel/platform-connector.sock \
  --disable-fabric-check \
  --processing-strategy EXECUTE_REMEDIATION
```

Pod-level orphan / unhealthy detection (CrashLoopBackOff, stuck init containers) for the gpu-operator DaemonSet is configured in the [`kubernetes-object-monitor`](../monitoring-critical-operators.md), not here.
