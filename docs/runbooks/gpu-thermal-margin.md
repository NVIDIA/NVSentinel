# Runbook: GPU Thermal Margin / HW Slowdown Violations (`GpuThermalMarginWatch`)

## Overview

`GpuThermalMarginWatch` monitors each GPU's live thermal margin (DCGM field 153, `DCGM_FI_DEV_GPU_TEMP_LIMIT`) against the per-SKU hardware-slowdown T.Limit offset published by the metadata-collector (NVML field 194, `FI_DEV_TEMPERATURE_SLOWDOWN_TLIMIT`). When a GPU's margin drops below its slowdown threshold, the GPU is at or past the temperature at which the hardware engages thermal slowdown, the fatal event for GpuThermalMarginWatch occurs. The feature is described in [ADR-042: GPU Thermal Margin](../designs/042-gpu-temp-limit-field-monitoring.md).

**Key points:**

- Failure raises node condition `GpuThermalMarginWatch=True` with reason `GpuThermalMarginWatchIsNotHealthy` and error code `GPU_TEMP_HW_SLOWDOWN_VIOLATION`.
- `GPU_TEMP_HW_SLOWDOWN_VIOLATION` maps to `CONTACT_SUPPORT` in `dcgmerrorsmapping.csv`, so the event is fatal (`isFatal=true`) and the node is cordoned/quarantined by fault-quarantine.
- The check requires both the feature flag enabled and a per-GPU `slowdown_tlimit_c` value in the node's metadata file. If either is missing, the GPU is silently skipped (no false positive).
- This is distinct from `DCGM_FR_CLOCK_THROTTLE_THERMAL` (mapped to `NONE`, non-fatal workload throttling).

## Symptoms

- Node condition `GpuThermalMarginWatch` present with `Status: True`.
- Condition message resembles: `ErrorCode:GPU_TEMP_HW_SLOWDOWN_VIOLATION GPU:0 PCI:0000:00:00.0 GPU_UUID:GPU-... GPU 0 thermal margin 3°C below HW slowdown T.Limit (slowdown=-2°C) Recommended Action=CONTACT_SUPPORT;`
- Node cordoned by fault-quarantine shortly after the condition appears.
- gpu-health-monitor logs contain `thermal margin ...°C below HW slowdown T.Limit`.

## Procedure

### 1. Confirm the Node Condition

```bash
kubectl get node {NODE_NAME} -o json | jq '.status.conditions[] | select(.type=="GpuThermalMarginWatch")'
```

A live violation shows `"status": "True"`, `"reason": "GpuThermalMarginWatchIsNotHealthy"`, and a message naming the GPU index, the observed margin, and the slowdown threshold. Note the GPU index and PCI/UUID for the steps below.

### 2. Confirm It Is a Real Thermal Event (Not a Bad Threshold)

The check compares the live margin (field 153) against the slowdown threshold (`slowdown_tlimit_c` from metadata). A violation means `margin_c < slowdown_threshold`. Verify the live GPU temperature and margin directly on the node:

```bash
# Live margin to the slowdown limit (field 153) and current temps
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- dcgmi dmon -e 153,150,140 -c 1

# Or via nvidia-smi on the node
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- nvidia-smi -q -d TEMPERATURE -i {GPU_INDEX}
```

- If the GPU is genuinely hot (low/negative margin, temperature crossing the slowdown limit), this is a real thermal fault. Go to step 5.
- If the GPU is cool and the margin is healthy but the condition still fired, this could indicate transient environmental conditions (ambient temp etc.) or a hardware issue. To verify, go to step 5 and run the respective dcgmproftester and nvidia-smi-related commands to decide the next steps.

### 3. Inspect the Per-GPU Slowdown Threshold (Metadata)

The threshold is read per GPU from the node's metadata file, populated by the metadata-collector from NVML field 194.

```bash
# Via the gpu-health-monitor pod, which mounts the metadata file
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- cat /var/lib/nvsentinel/gpu_metadata.json | jq '.gpus[] | {gpu_id, slowdown_tlimit_c}'
```

- `slowdown_tlimit_c` is a small per-SKU offset (for example, H100 = `-2`). A wildly wrong value (such as a large positive number) will cause false positives. Check the metadata-collector logs on that node:

  ```bash
  kubectl logs -n nvsentinel {METADATA_COLLECTOR_POD} | grep -i "slowdown TLIMIT"
  ```

- If `slowdown_tlimit_c` is absent for a GPU, the check is not active for it. The watcher logs `missing slowdown TLIMIT threshold metadata` and increments `gpu_temp_limit_slowdown_threshold_missing`, and no condition is raised.

### 4. Check the Watcher's Own Health Metrics

```bash
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- curl -s localhost:2112/metrics | grep -E 'gpu_temp_limit_(margin_blank|slowdown_threshold_missing)|dcgm_field_153|nvsentinel_feature_flag_enabled'
```

- `gpu_temp_limit_margin_blank` rising means field 153 is returning blank or unavailable values (DCGM not reporting margin); the GPU is skipped. Investigate DCGM using the [GPU Monitor DCGM Connectivity Failures](./gpu-monitor-dcgm-failures.md) runbook.
- `gpu_temp_limit_slowdown_threshold_missing` rising means the metadata threshold is missing (step 3).
- `dcgm_api_failures{dcgm_field="dcgm_field_153_get_latest"}` rising means DCGM read errors.
- `nvsentinel_feature_flag_enabled{flag="gpu_temp_limit_store_only"}` set to `1` means the check is in observe-only mode (see "Observe-only / dry-run mode" below).

### 5. Remediate a Real Thermal Fault

A genuine `GPU_TEMP_HW_SLOWDOWN_VIOLATION` (`CONTACT_SUPPORT`) indicates the GPU is reaching its hardware slowdown temperature. NVSentinel will already have cordoned the node. Next:

1. Drain remaining workloads off the node if remediation has not already done so.
2. Reproduce/confirm the thermal fault under load with NVIDIA's DCGM CUDA load generator. The `dcgmproftester` binary ships in the `nvidia-dcgm` pod (for example `/usr/bin/dcgmproftester12`); use whichever versioned binary is installed on the node:

   ```bash
   kubectl exec -n gpu-operator {NVIDIA_DCGM_POD} -- \
     dcgmproftester12 --no-dcgm-validation --max-processes 0 -t 1004 -d 900
   ```

   Replace `dcgmproftester12` with the installed binary if your DCGM image ships a different version (for example `dcgmproftester13`).

   - `-t 1004` — `DCGM_FI_PROF_PIPE_TENSOR_ACTIVE`: sustained Tensor-Core (HMMA/IMMA) GEMM load.
   - `--no-dcgm-validation` — generate load only; runs even on an already-cordoned/unhealthy node.
   - `--max-processes 0` — test all GPUs in parallel.
   - `-d 900` — run for 900 s (15 min), enough to reach thermal equilibrium.

   If `GpuThermalMarginWatch` re-fires under load (field 153 margin drops below the per-SKU slowdown threshold), the thermal fault is genuine — escalate to the respective team for cooling inspection (blocked air channel, failing fan, poor seating). If the margin stays healthy throughout, suspect a stale or incorrect `slowdown_tlimit_c` threshold (step 3).

3. While the load runs (in a second shell), capture per-GPU thermal telemetry to see which GPU throttles and why and try to correlate them:

   ```bash
   # Sample every 1s to a CSV for the whole run; drop --loop/--filename for a one-shot snapshot
   kubectl exec -n gpu-operator {NVIDIA_DCGM_POD} -- \
     nvidia-smi \
       --query-gpu=serial,name,timestamp,index,temperature.gpu,temperature.memory,temperature.gpu.tlimit,power.draw,clocks.current.sm,clocks_throttle_reasons.active,utilization.gpu \
       --format=csv --loop=1 --filename=/tmp/thermal_{NODE_NAME}.csv
   ```

   - `temperature.gpu.tlimit` — live margin (°C) to the HW slowdown limit, the same quantity as DCGM field 153. This is the value to watch: shrinking toward 0 is the violation.
   - `clocks_throttle_reasons.active` — bitmask of why clocks are capped; a HW-slowdown/thermal bit set under load confirms genuine hardware thermal throttling rather than a stale threshold.
   - `temperature.memory` / `power.draw` / `clocks.current.sm` — corroborate: HBM temp, power vs TDP, and whether SM clocks are being pulled down.
   - `temperature.gpu` / `utilization.gpu` — core temp and that the load actually landed on the GPU.
   - `--query-gpu` / `--format` take double dashes; `clocks_throttle_reasons.active` is the legacy alias accepted by the driver and normalized to `clocks_event_reasons.active` in the CSV header.

### 6. Verify Resolution

After the thermal condition is resolved (cooling fixed, or threshold corrected):

```bash
kubectl get node {NODE_NAME} -o json | jq '.status.conditions[] | select(.type=="GpuThermalMarginWatch")'
# Expect: "status": "False", "reason": "GpuThermalMarginWatchIsHealthy", "message": "No Health Failures"
```

The condition clears automatically once the live margin returns to at or above the slowdown threshold on the next poll. If the node was cordoned by NVSentinel, allow the normal quarantine and uncordon flow to complete, as described in the [Cordoned Nodes](./cordoned-nodes.md) runbook.

## Configuration Reference

The check is configured through the `gpu-health-monitor` Helm chart, which renders the `[dcgmfieldsmonitoring]` section of `config.ini`:

- Enable the check: Helm value `dcgmFieldsMonitoring.gpuTempLimitMonitoringEnabled` renders `gputemplimitmonitoringenabled`.

The per-GPU threshold (`slowdown_tlimit_c`) is not a Helm value. It is collected at runtime by the metadata-collector and written to `/var/lib/nvsentinel/gpu_metadata.json`.
