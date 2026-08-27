# Runbook: GPU Hardware Power Brake (`GpuPowerBrakeWatch`)

## Overview

`GpuPowerBrakeWatch` fires when a GPU's clocks-event-reasons mask
(`DCGM_FI_DEV_CLOCKS_EVENT_REASONS`) has the hardware power brake bit (`0x80`) set on
`gpuPowerBrakeMinConsecutivePolls` consecutive polls.

- Failure raises node condition `GpuPowerBrakeWatch=True` with error code `GPU_HW_POWER_BRAKE_VIOLATION`.
- `GPU_HW_POWER_BRAKE_VIOLATION` maps to `CONTACT_SUPPORT` in `dcgmerrorsmapping.csv`, so the event is fatal
  (`isFatal=true`) and the node is cordoned by fault-quarantine, unless `gpuPowerBrakeStoreOnly` is true.
- Condition message resembles:
  `ErrorCode:GPU_HW_POWER_BRAKE_VIOLATION GPU:2 ... GPU 2 hardware power brake asserted for 3 consecutive poll(s) (clocks event reasons mask 0x8c) Recommended Action=CONTACT_SUPPORT;`

An asserted brake means the power delivery path is forcing clocks down. It is a facility, PDU, busbar or
board-level power problem, so a node reboot does not resolve it and the node should not be recycled in the
hope that it clears.

## What this is not

Do not confuse the brake with power capping. Three different things share the same register:

| Bit | Meaning | Actionable |
|---|---|---|
| `0x04` | SW power cap. Normal capping under load. | No |
| `0x40` | HW thermal slowdown. Covered by `GpuThermalMarginWatch`. | Separately |
| `0x80` | **HW power brake.** External assertion from the power delivery path. | **Yes** |

DCGM's own POWER health watch does not report the brake. Its dominant code,
`DCGM_FR_CLOCK_THROTTLE_POWER`, tracks power-capped throttling, maps to `NONE`, and flaps with workload.
Do not use it to confirm or rule out a brake.

## Symptoms

- Reduced throughput on affected GPUs with no thermal or memory fault.
- Clocks pinned below expected boost with the workload otherwise healthy.
- Frequently rack-correlated, since power delivery is shared. A brake appearing on many nodes in one rack at
  the same time points at the facility rather than the boards.

## Procedure

### 1. Confirm the condition and scope

```bash
kubectl get nodes -o json \
  | jq -r '.items[] | select(.status.conditions[]? | select(.type=="GpuPowerBrakeWatch" and .status=="True")) | .metadata.name'
```

### 2. Confirm it on the node itself

```bash
nvidia-smi -q | grep -i -A2 brake
# or, per GPU:
nvidia-smi --query-gpu=index,clocks_event_reasons.hw_power_brake_slowdown --format=csv
```

`clocks_throttle_reasons.active` is the legacy alias, normalized to `clocks_event_reasons.active` in the CSV
header.

### 3. Confirm it in metrics, and check whether it is sustained

If dcgm-exporter is deployed, the raw mask is the authoritative view. PromQL has no bitwise AND, so isolate
the low byte with `% 256` and test its top bit. Do **not** use a bare `>= 128`: that also matches masks such
as `0x100` (`DISPLAY_CLOCKS`) and `0x140`, which do not have the brake bit set.

```promql
(DCGM_FI_DEV_CLOCKS_EVENT_REASONS % 256) >= 128
```

Fraction of a window each GPU spent braked, which distinguishes a transient from a sustained assertion:

```promql
avg by (Hostname, gpu) (avg_over_time(((DCGM_FI_DEV_CLOCKS_EVENT_REASONS % 256) >= bool 128)[1h:1m]))
```

On a DCGM build that predates the rename from clocks-throttle to clocks-event, gpu-health-monitor falls back to
`DCGM_FI_DEV_CLOCK_THROTTLE_REASONS`, and dcgm-exporter names its series after whichever field it is
configured to collect. Substitute that metric name in both queries if the one above returns nothing:

```promql
(DCGM_FI_DEV_CLOCK_THROTTLE_REASONS % 256) >= 128
```

The bit layout is identical across the rename, so only the metric name changes.

A value pinned at 1 is a sustained brake. Values well below 1 suggest load transients, in which case raise
`gpuPowerBrakeMinConsecutivePolls` rather than treating each one as a fault.

### 4. Establish the blast radius before escalating

Group affected GPUs by rack and by position within the node. A brake confined to a subset of GPUs in the
same position on many nodes in one rack (for example GPUs 2 and 3, which on GB200 are the second
Grace-Blackwell superchip) indicates a shared power domain rather than independent board failures.

### 5. Escalate

This is not self-healing and not remediable by reboot. Escalate to the datacenter or hardware team with:

- the affected node and GPU list,
- the observed mask values,
- the fraction of time braked and the onset time,
- whether the pattern is rack-correlated or position-correlated.

### 6. Verify resolution

```bash
kubectl get node <node> -o json | jq '.status.conditions[] | select(.type=="GpuPowerBrakeWatch")'
# Expect: "status": "False", "reason": "GpuPowerBrakeWatchIsHealthy", "message": "No Health Failures"
```

The watch clears on the first poll where the bit is not set, and the internal streak counter resets at the
same time.

## Configuration Reference

See [GPU Health Monitor configuration](../configuration/gpu-health-monitor.md#hardware-power-brake-detection)
for `gpuPowerBrakeMonitoringEnabled`, `gpuPowerBrakeStoreOnly` and `gpuPowerBrakeMinConsecutivePolls`.
