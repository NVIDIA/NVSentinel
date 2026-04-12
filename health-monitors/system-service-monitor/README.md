# System Service Monitor

Health monitor for non-DCGM infrastructure failures on NVIDIA GPU nodes: Fabric Manager service health, per-GPU fabric state, CUDA context validity, and GPU service lifecycle.

## Scope

Per [ADR-030](../../docs/designs/030-fabric-manager-monitor-scope.md), this monitor is scoped to signals that DCGM cannot see. PCIe link health, NVLink fabric telemetry, and clock throttling are owned by `gpu-health-monitor` via `pydcgm`.

| Signal | Owner | Rationale |
|---|---|---|
| FM service up/down | **system-service-monitor** | systemd state, not a DCGM field |
| FM flap detection | **system-service-monitor** | restart counting, not DCGM |
| Per-GPU fabric state | **system-service-monitor** | nvidia-smi fabric.state/status |
| CUDA context validity | **system-service-monitor** | active probe, not passive telemetry |
| GPU service lifecycle | **system-service-monitor** | systemd, not DCGM |
| PCIe link health | gpu-health-monitor | DCGM_HEALTH_WATCH_PCIE |
| NVLink bandwidth/errors | gpu-health-monitor | DCGM_HEALTH_WATCH_NVLINK |
| Clock throttling | gpu-health-monitor | DCGM_FI_DEV_CLOCK_THROTTLE_REASONS |

## Architecture

```
+-----------------------------------------------------------+
|                 SystemServiceWatcher                       |
|  +----------------+ +--------------------+ +------------+  |
|  | ServiceChecker | | FabricStateChecker | | CUDAValid. |  |
|  +-------+--------+ +---------+----------+ +------+-----+  |
|          |                    |                    |        |
|          +----------+---------+----+--------------+        |
|                     |              |                        |
|              List[CheckResult]     |                        |
+---------------------+--------------+-----------------------+
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
| CUDA validation | `CudaValidationFailed` | Yes | NODE | subprocess torch test |

### Fabric State Classification

The `FabricStateUnhealthy` check classifies per-GPU fabric failures into three specific states (replacing the former monolithic `FM_UNRESPONSIVE`):

| Error Code | Condition | Meaning |
|---|---|---|
| `FM_NOT_STARTED` | fabric.state == "Not Started" | FM has not begun configuring this GPU |
| `FM_REGISTRATION_STUCK` | fabric.state == "In Progress" | FM may be hung during NVSwitch config |
| `FM_FABRIC_ERROR` | fabric.state == "Completed" && status != "Success" | FM completed but with error |

## Configuration

All options are available as CLI flags:

```
system_service_monitor \
  --platform-connector-socket /run/nvsentinel/platform-connector.sock \
  --port 9101 \
  --poll-interval 30 \
  --boot-grace-period 300 \
  --flap-window 600 \
  --flap-threshold 3 \
  --enable-fabric-check \
  --disable-cuda-validation \
  --processing-strategy EXECUTE_REMEDIATION
```

Environment variables:
- `NODE_NAME` / `HOSTNAME` -- Node name (used as entity in health events)
- `LOG_LEVEL` -- Log level: debug, info, warn, error (default: info)

## Deployment

The system-service-monitor runs as a DaemonSet on GPU nodes, alongside the existing gpu-health-monitor. It requires:

- Host PID namespace access (`hostPID: true`) for nsenter into host systemd
- Platform-connector Unix socket mounted as a volume

## Integration with NVSentinel Remediation

Health events flow through the standard NVSentinel pipeline:

1. **system-service-monitor** detects failure, sends `HealthEvent` to platform-connector
2. **platform-connector** writes event to MongoDB and forwards to fault-quarantine
3. **fault-quarantine** cordons/labels the node based on event severity
4. **fault-remediation** executes the recommended action (RESTART_BM for fatal infra failures)
5. **node-drainer** handles workload migration off the unhealthy node

## False-Positive Mitigations

- **Boot grace period**: Suppresses alerts during node startup (configurable, default 300s)
- **Flap detection**: Tracks service restart frequency to distinguish transient from persistent failures
- **State caching**: Only state transitions generate events, preventing duplicate alerts
