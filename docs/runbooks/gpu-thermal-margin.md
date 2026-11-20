# Runbook: GPU Thermal Margin / HW Slowdown Violations (`GpuThermalMarginWatch`)

## Overview

`GpuThermalMarginWatch` monitors the live thermal margin of each GPU (DCGM field 153, `DCGM_FI_DEV_GPU_TEMP_LIMIT`). It compares this margin with the per-SKU hardware-slowdown T.Limit offset that the metadata-collector publishes (NVML field 194, `FI_DEV_TEMPERATURE_SLOWDOWN_TLIMIT`). When the margin of a GPU stays below its slowdown threshold for `gpuTempLimitMinConsecutivePolls` consecutive polls (3 by default), the fatal health event for GpuThermalMarginWatch occurs. In this case, the GPU has been at or past the temperature at which the hardware engages thermal slowdown.

**Key points:**

- By default, [`gpuTempLimitStoreOnly`](../configuration/gpu-health-monitor.md#gputemplimitstoreonly) is `true`. With this value, gpu-health-monitor sends the health event with `processingStrategy=STORE_ONLY`. NVSentinel stores the health event and exports it as a metric, but it does not set a node condition or cordon the node.
- If `gpuTempLimitStoreOnly` is `false`, a failure raises the node condition `GpuThermalMarginWatch=True`, with reason `GpuThermalMarginWatchIsNotHealthy` and error code `GPU_TEMP_HW_SLOWDOWN_VIOLATION`.
- `GPU_TEMP_HW_SLOWDOWN_VIOLATION` maps to `CONTACT_SUPPORT` in `dcgmerrorsmapping.csv`. Thus, the health event is fatal (`isFatal=true`). If `gpuTempLimitStoreOnly` is `false`, fault-quarantine quarantines the node.
- The check requires the feature flag to be enabled and a per-GPU `slowdown_tlimit_c` value in the metadata file of the node. If either is missing, the check silently skips the GPU (no false positive).
- This check is different from `DCGM_FR_CLOCK_THROTTLE_THERMAL`, which maps to `NONE` and is non-fatal workload throttling.

## Symptoms

- If `gpuTempLimitStoreOnly` is `false`, the node condition `GpuThermalMarginWatch` is present with `Status: True`.
- The condition message is similar to: `ErrorCode:GPU_TEMP_HW_SLOWDOWN_VIOLATION GPU:0 PCI:0000:00:00.0 GPU_UUID:GPU-... GPU 0 thermal margin 3°C below HW slowdown T.Limit (slowdown=-2°C) Recommended Action=CONTACT_SUPPORT;`
- If `gpuTempLimitStoreOnly` is `false`, fault-quarantine cordons the node shortly after the condition appears.
- gpu-health-monitor logs contain `thermal margin ...°C below HW slowdown T.Limit`.

## Procedure

### 1. Confirm the Node Condition

If `gpuTempLimitStoreOnly` is `true` (the default), the node has no `GpuThermalMarginWatch` condition. In this case, find the GPU index in the `gpu_id` label of the `dcgm_health_active_events{event_type="GpuThermalMarginWatch"}` series with the value 1. Then go to step 2.

```bash
kubectl get node {NODE_NAME} -o json | jq '.status.conditions[] | select(.type=="GpuThermalMarginWatch")'
```

A live violation shows `"status": "True"`, `"reason": "GpuThermalMarginWatchIsNotHealthy"` and a message. The message names the GPU index, the observed margin and the slowdown threshold. Record the GPU index, the PCI address and the UUID for the steps below.

### 2. Confirm It Is a Real Thermal Event (Not a Bad Threshold)

The check compares the live margin (field 153) with the slowdown threshold (`slowdown_tlimit_c` from the metadata). A violation means `margin_c < slowdown_threshold`. Verify the live GPU temperature and margin directly on the node:

```bash
# Live margin to the slowdown limit (field 153) and current temps
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- dcgmi dmon -e 153,150,140 -c 1

# Or via nvidia-smi on the node
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- nvidia-smi -q -d TEMPERATURE -i {GPU_INDEX}
```

- If the GPU is truly hot (a low or negative margin, and a temperature that crosses the slowdown limit), this is a real thermal fault. Go to step 5.
- If the GPU is cool and the margin is healthy but the condition still fired, there are two possible causes. One is transient environmental conditions, for example the ambient temperature. The other is a hardware issue. To verify, go to step 5. Run the related dcgmproftester and nvidia-smi commands to decide the next steps.

### 3. Inspect the Per-GPU Slowdown Threshold (Metadata)

gpu-health-monitor reads the threshold for each GPU from the metadata file of the node. The metadata-collector fills this file from NVML field 194.

```bash
# Via the gpu-health-monitor pod, which mounts the metadata file
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- cat /var/lib/nvsentinel/gpu_metadata.json | jq '.gpus[] | {gpu_id, slowdown_tlimit_c}'
```

- `slowdown_tlimit_c` is a small per-SKU offset (for example, H100 = `-2`). An unrealistic value (such as a large positive number) causes false positives. Check the metadata-collector logs on that node:

  ```bash
  kubectl logs -n nvsentinel {METADATA_COLLECTOR_POD} | grep -i "slowdown TLIMIT"
  ```

- If `slowdown_tlimit_c` is absent for a GPU, the check is not active for that GPU. The check logs `missing slowdown TLIMIT threshold metadata` and increments `gpu_temp_limit_slowdown_threshold_missing`. It does not raise a condition.

### 4. Check the Watcher's Own Health Metrics

```bash
kubectl exec -n nvsentinel {GPU_MONITOR_POD} -- curl -s localhost:2112/metrics | grep -E 'gpu_temp_limit_(margin_blank|slowdown_threshold_missing)|dcgm_field_153|nvsentinel_feature_flag_enabled'
```

- A rising `gpu_temp_limit_margin_blank` means that field 153 returns blank or unavailable values (DCGM does not report the margin). The check skips the GPU. To investigate DCGM, use the [GPU Monitor DCGM Connectivity Failures](./gpu-monitor-dcgm-failures.md) runbook.
- A rising `gpu_temp_limit_slowdown_threshold_missing` means that the metadata threshold is missing (step 3).
- A rising `dcgm_api_failures{error_name="dcgm_field_153_get_since"}` means DCGM read errors.
- A value of `1` for `nvsentinel_feature_flag_enabled{flag="store_only_mode"}` means that all of gpu-health-monitor is in observe-only mode, including this check. No metric shows [`gpuTempLimitStoreOnly`](../configuration/gpu-health-monitor.md#gputemplimitstoreonly). This value puts only this check in observe-only mode, and its default is `true`.

### 5. Remediate a Real Thermal Fault

A real `GPU_TEMP_HW_SLOWDOWN_VIOLATION` (`CONTACT_SUPPORT`) shows that the GPU reaches its hardware slowdown temperature. If `gpuTempLimitStoreOnly` is `false`, NVSentinel has already cordoned the node. Next, do these steps:

1. If remediation has not already drained the node, drain the remaining workloads from the node.
2. Reproduce the thermal fault under load to confirm it, with the DCGM CUDA load generator from NVIDIA. The `nvidia-dcgm` pod includes the `dcgmproftester` binary (for example `/usr/bin/dcgmproftester12`). Use the versioned binary that is installed on the node:

   ```bash
   kubectl exec -n gpu-operator {NVIDIA_DCGM_POD} -- \
     dcgmproftester12 --no-dcgm-validation --max-processes 0 -t 1004 -d 900
   ```

   If your DCGM image has a different version (for example `dcgmproftester13`), replace `dcgmproftester12` with the installed binary.

   - `-t 1004` — `DCGM_FI_PROF_PIPE_TENSOR_ACTIVE`: sustained Tensor-Core (HMMA/IMMA) GEMM load.
   - `--no-dcgm-validation` — generate load only. The test runs even on a node that is already cordoned or unhealthy.
   - `--max-processes 0` — test all GPUs in parallel.
   - `-d 900` — run for 900 s (15 min), which is enough to reach thermal equilibrium.

   If `GpuThermalMarginWatch` fires again under load (the field 153 margin drops below the per-SKU slowdown threshold), the thermal fault is real. Escalate to the applicable team for a cooling inspection (blocked air channel, failing fan or poor seating). If the margin stays healthy for the full run, the `slowdown_tlimit_c` threshold is possibly stale or incorrect (step 3).

3. While the load runs, capture per-GPU thermal telemetry in a second shell. Use it to see which GPU throttles and why, and to correlate the values:

   ```bash
   # Sample every 1s to a CSV for the whole run; drop --loop/--filename for a one-shot snapshot
   kubectl exec -n gpu-operator {NVIDIA_DCGM_POD} -- \
     nvidia-smi \
       --query-gpu=serial,name,timestamp,index,temperature.gpu,temperature.memory,temperature.gpu.tlimit,power.draw,clocks.current.sm,clocks_throttle_reasons.active,utilization.gpu \
       --format=csv --loop=1 --filename=/tmp/thermal_{NODE_NAME}.csv
   ```

   - `temperature.gpu.tlimit` — live margin (°C) to the HW slowdown limit. This is the same quantity as DCGM field 153. Watch this value: a margin that shrinks toward 0 is the violation.
   - `clocks_throttle_reasons.active` — bitmask of the reasons for the clock caps. A HW-slowdown or thermal bit that is set under load confirms real hardware thermal throttling, not a stale threshold.
   - `temperature.memory` / `power.draw` / `clocks.current.sm` — values that confirm the result: HBM temperature, power compared to TDP, and whether the SM clocks decrease.
   - `temperature.gpu` / `utilization.gpu` — core temperature, and proof that the load reached the GPU.
   - `--query-gpu` and `--format` take double dashes. `clocks_throttle_reasons.active` is the legacy alias that the driver accepts. The driver normalizes it to `clocks_event_reasons.active` in the CSV header.

### 6. Verify Resolution

After you resolve the thermal condition (fix the cooling or correct the threshold), verify the result. If `gpuTempLimitStoreOnly` is `true` (the default), the node has no `GpuThermalMarginWatch` condition. Thus, verify that the `dcgm_health_active_events{event_type="GpuThermalMarginWatch"}` series of the GPU from step 1 has the value 0. If `gpuTempLimitStoreOnly` is `false`, run this command:

```bash
kubectl get node {NODE_NAME} -o json | jq '.status.conditions[] | select(.type=="GpuThermalMarginWatch")'
# Expect: "status": "False", "reason": "GpuThermalMarginWatchIsHealthy", "message": "No Health Failures"
```

The condition clears automatically when the live margin is at or above the slowdown threshold again on the next poll. If NVSentinel cordoned the node, let the normal quarantine and uncordon flow complete. The [Cordoned Nodes](./cordoned-nodes.md) runbook describes this flow.

## Configuration Reference

You configure the check through the `gpu-health-monitor` Helm chart. The chart renders the `[dcgmfieldsmonitoring]` section of `config.ini`:

- Enable the check: Helm value `dcgmFieldsMonitoring.gpuTempLimitMonitoringEnabled` renders `gputemplimitmonitoringenabled`.
- Consecutive polls required to fail: Helm value `dcgmFieldsMonitoring.gpuTempLimitMinConsecutivePolls` renders `gputemplimitminconsecutivepolls`.

The per-GPU threshold (`slowdown_tlimit_c`) is not a Helm value. The metadata-collector collects it at runtime and writes it to `/var/lib/nvsentinel/gpu_metadata.json`.
