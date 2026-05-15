# System Services Monitor

Health monitor for non-DCGM infrastructure failures on NVIDIA GPU nodes: Fabric Manager service health, per-GPU fabric state, and GPU service lifecycle.

## Scope

Per [ADR-030](designs/030-fabric-manager-monitor-scope.md), this monitor is scoped to signals that DCGM cannot see. PCIe link health, NVLink fabric telemetry, and clock throttling are owned by `gpu-health-monitor` via `pydcgm`.

| Signal | Owner | Rationale |
|---|---|---|
| FM service up/down | **system-services-monitor** | systemd state, not a DCGM field |
| FM flap detection | **system-services-monitor** | restart counting, not DCGM |
| Per-GPU fabric state | **system-services-monitor** | nvidia-smi fabric.state/status |
| GPU service lifecycle | **system-services-monitor** | systemd, not DCGM |
| PCIe link health | gpu-health-monitor | DCGM_HEALTH_WATCH_PCIE |
| NVLink bandwidth/errors | gpu-health-monitor | DCGM_HEALTH_WATCH_NVLINK |
| Clock throttling | gpu-health-monitor | DCGM_FI_DEV_CLOCK_THROTTLE_REASONS |

## Architecture

```
+-----------------------------------------------------------+
|                 SystemServiceWatcher                       |
|  +----------------+ +--------------------+                 |
|  | ServiceChecker | | FabricStateChecker |                 |
|  +-------+--------+ +---------+----------+                 |
|          |                    |                            |
|          +----------+---------+                            |
|                     |                                      |
|              List[CheckResult]                             |
+---------------------+--------------------------------------+
                      |
                      v
+-----------------------------------------------------------+
|          PlatformConnectorEventProcessor                   |
|  - State caching (only sends on change)                    |
|  - Retry with exponential backoff                          |
|  - gRPC -> platform-connector UDS                          |
+-----------------------------------------------------------+
```

## Check Categories

| Check | Check Name | Fatal | Entities | Detection |
|---|---|---|---|---|
| Fabric Manager down | `FabricManagerServiceDown` | Yes | NODE | nsenter systemctl show |
| GPU service down | `GpuServiceDown` | No | NODE | nsenter systemctl show |
| Fabric state unhealthy | `FabricStateUnhealthy` | Yes | GPU | nsenter nvidia-smi fabric.state |

### Fabric State Classification

The `FabricStateUnhealthy` check classifies per-GPU fabric failures into three specific states (replacing the former monolithic `FM_UNRESPONSIVE`):

| Error Code | Condition | Meaning |
|---|---|---|
| `FM_NOT_STARTED` | fabric.state == "Not Started" | FM has not begun configuring this GPU |
| `FM_REGISTRATION_STUCK` | fabric.state == "In Progress" | FM may be hung during NVSwitch config |
| `FM_FABRIC_ERROR` | fabric.state == "Completed" && status != "Success" | FM completed but with error |

## Deployment

The system-services-monitor runs as a DaemonSet on GPU nodes, alongside the existing gpu-health-monitor. It requires:

- Host PID namespace access (`hostPID: true`) for nsenter into host systemd
- Platform-connector Unix socket mounted as a volume

## Integration with NVSentinel Remediation

Health events flow through the standard NVSentinel pipeline:

1. **system-services-monitor** detects failure, sends `HealthEvent` to platform-connector
2. **platform-connector** writes event to MongoDB and forwards to fault-quarantine
3. **fault-quarantine** cordons/labels the node based on event severity
4. **fault-remediation** executes the recommended action (RESTART_BM for fatal infra failures)
5. **node-drainer** handles workload migration off the unhealthy node

## False-Positive Mitigations

- **Boot grace period**: Suppresses alerts during node startup (configurable, default 300s)
- **Flap detection**: Tracks service restart frequency to distinguish transient from persistent failures
- **State caching**: Only state transitions generate events, preventing duplicate alerts

## GKE Container-Optimized OS (COS) on Pod-Level Detection

On GKE with Container-Optimized OS, Fabric Manager does not run as a host systemd service. The NVIDIA GPU Operator manages the full driver stack -- including Fabric Manager -- inside gpu-operator driver pods, so `nsenter ... systemctl show nvidia-fabricmanager` will always report inactive on the host.

In this topology, FM health is inferred from two complementary sources:

1. **Per-GPU fabric state (`nvidia-smi fabric.state`)** -- The `FabricStateChecker` runs the same on COS because it queries the GPU driver directly. If FM is down or stuck inside the gpu-operator pod, per-GPU fabric state surfaces it as Not Started, In Progress, or error.

2. **gpu-operator pod health** -- Pod-level orphan / unhealthy detection (CrashLoopBackOff, stuck Pending init, ImagePullBackOff) is owned by the [`kubernetes-object-monitor`](monitoring-critical-operators.md). Configure a policy in that monitor's `policies` list to watch DaemonSet pods in the `gpu-operator` namespace; it will emit a health event and cordon the node when a driver pod is unhealthy.

See [`docs/configuration/system-services-monitor.md`](configuration/system-services-monitor.md) for CLI flags and the COS deployment recipe.
