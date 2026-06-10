# System Services Monitor

Health monitor for non-DCGM infrastructure failures on NVIDIA GPU nodes: Fabric Manager service health, per-GPU fabric state, and GPU service lifecycle.

Documentation has been moved to the repo-wide `docs/` tree per project convention:

- **Component overview, scope, architecture, check categories** --
  [`docs/system-services-monitor.md`](../../docs/system-services-monitor.md)
- **CLI flags, environment variables, deployment recipes** --
  [`docs/configuration/system-services-monitor.md`](../../docs/configuration/system-services-monitor.md)
- **Architectural decision (split from `gpu-health-monitor`)** --
  [`docs/designs/030-fabric-manager-monitor-scope.md`](../../docs/designs/030-fabric-manager-monitor-scope.md)
- **Pod-level orphan detection for gpu-operator DaemonSets** --
  [`docs/monitoring-critical-operators.md`](../../docs/monitoring-critical-operators.md) (owned by `kubernetes-object-monitor`)

## Build & Test

```sh
make -C health-monitors/system-services-monitor lint-test
make -C health-monitors/system-services-monitor docker-build
```
