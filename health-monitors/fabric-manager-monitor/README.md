# Fabric Manager Monitor

Health monitor for detecting Fabric Manager, PCIe, NVLink, and GPU infrastructure failures on NVIDIA GPU nodes.

## Problem Statement

The gpu-health-monitor watches DCGM health watches (XID errors, ECC memory, thermals), but several critical infrastructure failure modes exist outside DCGM's visibility:

| Failure Mode | Detection Method | Impact |
|---|---|---|
| Fabric Manager service down | systemd service check via nsenter | NVLink fabric offline, multi-GPU workloads fail |
| PCIe link downtraining | nvidia-smi PCIe query via nsenter | Reduced GPU-host bandwidth, silent performance degradation |
| GPU clock throttling | nvidia-smi clock query via nsenter | Silent throughput loss from thermal/power throttling |
| NVLink CRC errors | DCGM exporter Prometheus metrics | NVLink fabric degradation |
| CUDA context failure | subprocess CUDA validation | GPU completely unusable |

## Architecture

The fabric-manager-monitor follows the same callback pattern as the gpu-health-monitor:

1. **FabricManagerWatcher** runs a polling loop executing all enabled health checks
2. Each check returns `List[CheckResult]` with check name, health status, error codes, and impacted entities
3. Callbacks (e.g. `PlatformConnectorEventProcessor`) receive aggregated results
4. The event processor converts results to protobuf `HealthEvent` messages and sends them via gRPC to the platform-connector Unix domain socket
5. State caching prevents duplicate events -- only state changes are transmitted

```
┌─────────────────────────────────────────────────────────┐
│                  FabricManagerWatcher                    │
│  ┌──────────────┐ ┌───────────┐ ┌────────────────────┐  │
│  │ServiceChecker│ │PCIeChecker│ │NVLinkFabricChecker │  │
│  └──────┬───────┘ └─────┬─────┘ └──────────┬─────────┘  │
│  ┌──────┴───────┐ ┌─────┴─────┐                         │
│  │ ClockChecker │ │CUDAValid. │                         │
│  └──────┬───────┘ └─────┬─────┘                         │
│         └───────┬───────┘                                │
│          List[CheckResult]                               │
└─────────────────┬───────────────────────────────────────┘
                  │
                  ▼
┌─────────────────────────────────────────────────────────┐
│         PlatformConnectorEventProcessor                 │
│  • State caching (only sends on change)                 │
│  • Retry with exponential backoff                       │
│  • gRPC → platform-connector UDS                        │
└─────────────────────────────────────────────────────────┘
```

## Check Categories

| Check | Check Name | Fatal | Entities | Detection |
|---|---|---|---|---|
| Fabric Manager down | `FabricManagerServiceDown` | Yes | NODE | nsenter systemctl show |
| GPU service down | `GpuServiceDown` | No | NODE | nsenter systemctl show |
| PCIe link degraded | `PcieLinkDegraded` | Yes | GPU | nsenter nvidia-smi |
| Clock throttled | `GpuClockThrottled` | No | GPU | nsenter nvidia-smi |
| NVLink degraded | `NvlinkFabricDegraded` | Yes | NODE | DCGM exporter HTTP |
| CUDA validation | `CudaValidationFailed` | Yes | NODE | subprocess torch test |

## Configuration

All options are available as CLI flags:

```
fabric_manager_monitor \
  --platform-connector-socket /run/nvsentinel/platform-connector.sock \
  --port 9101 \
  --poll-interval 30 \
  --boot-grace-period 300 \
  --flap-window 600 \
  --flap-threshold 3 \
  --enable-fabric-check \
  --enable-pcie-check \
  --enable-clock-check \
  --enable-nvlink-check \
  --disable-cuda-validation \
  --dcgm-exporter-url http://localhost:9400 \
  --clock-throttle-ratio 0.85 \
  --processing-strategy EXECUTE_REMEDIATION
```

Environment variables:
- `NODE_NAME` / `HOSTNAME` -- Node name (used as entity in health events)
- `LOG_LEVEL` -- Log level: debug, info, warn, error (default: info)

## Deployment

The fabric-manager-monitor runs as a DaemonSet on GPU nodes, alongside the existing gpu-health-monitor. It requires:

- Host PID namespace access (`hostPID: true`) for nsenter into host systemd
- Platform-connector Unix socket mounted as a volume
- DCGM exporter accessible at the configured URL

## Integration with NVSentinel Remediation

Health events flow through the standard NVSentinel pipeline:

1. **fabric-manager-monitor** detects failure, sends `HealthEvent` to platform-connector
2. **platform-connector** writes event to MongoDB and forwards to fault-quarantine
3. **fault-quarantine** cordons/labels the node based on event severity
4. **fault-remediation** executes the recommended action (RESTART_BM for fatal infra failures)
5. **node-drainer** handles workload migration off the unhealthy node

## False-Positive Mitigations

- **Boot grace period**: Suppresses alerts during node startup (configurable, default 300s)
- **Flap detection**: Tracks service restart frequency to distinguish transient from persistent failures
- **GPU Idle filter**: Clock throttle check ignores benign idle throttle reasons (bitmask 0x1)
- **NVLink correlation**: Bandwidth-zero is only flagged unhealthy when correlated with Fabric Manager being down
- **State caching**: Only state transitions generate events, preventing duplicate alerts
