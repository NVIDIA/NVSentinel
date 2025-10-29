# NVSentinel Metrics

This document outlines all Prometheus metrics exposed by NVSentinel components.

## Table of Contents

- [Fault Quarantine Module](#fault-quarantine-module)
- [Labeler Module](#labeler-module)
- [Janitor](#janitor)
- [Platform Connectors](#platform-connectors)
- [Health Monitors](#health-monitors)
  - [GPU Health Monitor](#gpu-health-monitor)
  - [Syslog Health Monitor](#syslog-health-monitor)

---

## Fault Quarantine Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_events_received_total` | Counter | - | Total number of events received from the watcher |
| `fault_quarantine_events_successfully_processed_total` | Counter | - | Total number of events successfully processed |
| `fault_quarantine_events_skipped_total` | Counter | - | Total number of events received on already cordoned node |
| `fault_quarantine_processing_errors_total` | Counter | `error_type` | Total number of errors encountered during event processing |
| `fault_quarantine_event_backlog_count` | Gauge | - | Number of health events which fault quarantine is yet to process |
| `fault_quarantine_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

### Node Quarantine Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_nodes_quarantined_total` | Counter | `node` | Total number of nodes quarantined |
| `fault_quarantine_nodes_unquarantined_total` | Counter | `node` | Total number of nodes unquarantined |
| `fault_quarantine_current_quarantined_nodes` | Gauge | `node` | Current number of quarantined nodes |

### Taint and Cordon Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_taints_applied_total` | Counter | `taint_key`, `taint_effect` | Total number of taints applied to nodes |
| `fault_quarantine_taints_removed_total` | Counter | `taint_key`, `taint_effect` | Total number of taints removed from nodes |
| `fault_quarantine_cordons_applied_total` | Counter | - | Total number of cordons applied to nodes |
| `fault_quarantine_cordons_removed_total` | Counter | - | Total number of cordons removed from nodes |

### Ruleset Evaluation Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_ruleset_evaluations_total` | Counter | `ruleset` | Total number of ruleset evaluations |
| `fault_quarantine_ruleset_passed_total` | Counter | `ruleset` | Total number of ruleset evaluations that passed |
| `fault_quarantine_ruleset_failed_total` | Counter | `ruleset` | Total number of ruleset evaluations that failed |

### Circuit Breaker Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_breaker_state` | Gauge | `state` | State of the fault quarantine breaker |
| `fault_quarantine_breaker_utilization` | Gauge | - | Utilization of the fault quarantine breaker |
| `fault_quarantine_get_total_nodes_duration_seconds` | Histogram | `result` | Duration of getTotalNodesWithRetry calls in seconds |
| `fault_quarantine_get_total_nodes_errors_total` | Counter | `error_type` | Total number of errors from getTotalNodesWithRetry |
| `fault_quarantine_get_total_nodes_retry_attempts` | Histogram | - | Number of retry attempts needed for getTotalNodesWithRetry (buckets: 0, 1, 2, 3, 5, 10) |

---

## Labeler Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `labeler_events_processed_total` | Counter | `status` | Total number of pod events processed. Status values: `success`, `failed` |
| `labeler_node_update_failures_total` | Counter | - | Total number of node update failures during reconciliation |
| `labeler_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |
| `labeler_node_update_duration_seconds` | Histogram | - | Histogram of node update operation durations |

---

## Janitor

### Action Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `janitor_actions_count` | Counter | `action_type`, `status`, `node` | Total number of janitor actions by type and status. Action types: `reboot`, `terminate`. Status values: `started`, `succeeded`, `failed` |
| `janitor_action_mttr_seconds` | Histogram | `action_type` | Time taken to complete janitor actions (Mean Time To Repair). Uses exponential buckets (10, 2, 10) for log-scale MTTR measurement |

---

## Platform Connectors

### Server Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `platform_connector_health_events_received_total` | Counter | - | The total number of health events that the platform connector has received |

### Workqueue Metrics

These metrics track the internal ring buffer workqueue performance:

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `platform_connector_workqueue_depth_<name>` | Gauge | `workqueue` | Current depth of Platform connector workqueue |
| `platform_connector_workqueue_adds_total_<name>` | Counter | `workqueue` | Total number of adds handled by Platform connector workqueue |
| `platform_connector_workqueue_latency_seconds_<name>` | Histogram | `workqueue` | How long an item stays in Platform connector workqueue before being requested. Uses linear buckets (0, 10, 500) |
| `platform_connector_workqueue_work_duration_seconds_<name>` | Histogram | `workqueue` | How long processing an item from Platform connector workqueue takes. Uses linear buckets (0, 10, 500) |
| `platform_connector_workqueue_retries_total_<name>` | Counter | `workqueue` | Total number of retries handled by Platform connector workqueue |
| `platform_connector_workqueue_longest_running_processor_seconds_<name>` | Gauge | `workqueue` | How many seconds the longest running processor for Platform connector workqueue has been running |
| `platform_connector_workqueue_unfinished_work_seconds_<name>` | Gauge | `workqueue` | The total time in seconds of work in progress in Platform connector workqueue |

**Note:** `<name>` in the metric names is replaced with the actual workqueue name at runtime.

---

## Health Monitors

### GPU Health Monitor

These metrics track GPU health events detected via DCGM (Data Center GPU Manager):

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `dcgm_health_events_publish_time_to_grpc_channel` | Histogram | `operation_name` | Amount of time spent in publishing DCGM health events on the gRPC channel |
| `health_events_insertion_to_uds_succeed` | Counter | - | Total number of successful insertions of health events to UDS |
| `health_events_insertion_to_uds_error` | Gauge | - | Error in insertions of health events to UDS |
| `dcgm_health_active_non_fatal_health_events` | Gauge | `event_type`, `gpu_id` | Total number of active non-fatal health events at any given time |
| `dcgm_health_active_fatal_health_events` | Gauge | `event_type`, `gpu_id` | Total number of active fatal health events at any given time |

---

### Syslog Health Monitor

The syslog health monitor tracks GPU-related errors detected from system logs.

#### XID Error Metrics

XID (GPU Error ID) errors are NVIDIA GPU driver errors:

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_xid_errors` | Counter | `node`, `err_code` | Total number of XID errors found |
| `syslog_health_monitor_xid_processing_errors` | Counter | `error_type`, `node` | Total number of errors encountered during XID processing |
| `syslog_health_monitor_xid_processing_latency_seconds` | Histogram | - | Histogram of XID processing latency |

#### SXID Error Metrics

SXID errors are NVSwitch-related errors:

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_sxid_errors` | Counter | `node`, `err_code`, `link`, `nvswitch` | Total number of SXID errors found |

#### GPU Fallen Off Bus Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_gpu_fallen_errors` | Counter | `node` | Total number of GPU fallen off bus errors detected |

---

## Metrics Configuration

### Scraping Metrics

All NVSentinel components expose Prometheus metrics on a metrics endpoint (typically `:2112/metrics`). The metrics can be scraped by Prometheus using standard scrape configurations.

### Helm Chart Configuration

The NVSentinel Helm chart automatically creates a `PodMonitor` resource for Prometheus Operator integration:

```bash
helm install nvsentinel ./distros/kubernetes/nvsentinel \
  --namespace nvsentinel --create-namespace
```

The PodMonitor is configured to scrape all NVSentinel component pods on their metrics endpoints (`/metrics` on port `metrics`).

### Annotation-based Discovery

Components can be configured to include Prometheus scrape annotations:

```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "2112"
  prometheus.io/path: "/metrics"
```

---

## Metric Types Reference

- **Counter**: A cumulative metric that only increases or resets to zero on restart
- **Gauge**: A metric that can arbitrarily go up and down
- **Histogram**: Samples observations and counts them in configurable buckets
- **Summary**: Similar to histogram but calculates configurable quantiles over a sliding time window

---

## Common Label Values

### Status Labels
- `success` / `failed` - Operation outcome
- `started` / `succeeded` / `failed` - Action lifecycle status

### Action Types
- `reboot` - Node reboot action
- `terminate` - Node termination action

### CSP Labels
- `gcp` - Google Cloud Platform
- `aws` - Amazon Web Services

### Trigger Types
- `quarantine` - Node quarantine trigger
- `healthy` - Node healthy trigger
