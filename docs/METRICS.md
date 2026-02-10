# NVSentinel Metrics

This document outlines all Prometheus metrics exposed by NVSentinel components.

## Table of Contents

- [Fault Quarantine Module](#fault-quarantine)
- [Node Drainer Module](#node-drainer)
- [Fault Remediation Module](#fault-remediation)
- [Labeler Module](#labeler)
- [Janitor](#janitor)
- [Platform Connectors](#platform-connectors)
- [Health Monitors](#health-monitors)
  - [GPU Health Monitor](#gpu-health-monitor)
  - [Syslog Health Monitor](#syslog-health-monitor)
  - [CSP Health Monitor](#csp-health-monitor)

---

## Fault Quarantine Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_events_received_total` | Counter | - | Total number of events received from the watcher |
| `fault_quarantine_events_successfully_processed_total` | Counter | - | Total number of events successfully processed |
| `fault_quarantine_processing_errors_total` | Counter | `error_type` | Total number of errors encountered during event processing |
| `fault_quarantine_event_backlog_count` | Gauge | - | Number of health events which fault quarantine is yet to process |
| `fault_quarantine_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

### Node Quarantine Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_nodes_quarantined_total` | Counter | `node` | Total number of nodes quarantined |
| `fault_quarantine_nodes_unquarantined_total` | Counter | `node` | Total number of nodes unquarantined |
| `fault_quarantine_nodes_manually_uncordoned_total` | Counter | `node` | Total number of manually uncordons for nodes |
| `fault_quarantine_current_quarantined_nodes` | Gauge | `node` | Current number of quarantined nodes |

### Taint and Cordon Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_taints_applied_total` | Counter | `taint_key`, `taint_effect` | Total number of taints applied to nodes |
| `fault_quarantine_taints_removed_total` | Counter | `taint_key`, `taint_effect` | Total number of taints removed from nodes |
| `fault_quarantine_cordons_applied_total` | Counter | - | Total number of cordons applied to nodes |
| `fault_quarantine_cordons_removed_total` | Counter | - | Total number of cordons removed from nodes |
| `fault_quarantine_node_quarantine_duration_seconds` | Histogram | - | Time from health event generation to node quarantine completion. Buckets: Prometheus DefBuckets |

### Ruleset Evaluation Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_ruleset_evaluations_total` | Counter | `ruleset`, `status` | Total number of ruleset evaluations. Status values: `passed`, `failed` |

### Circuit Breaker Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_breaker_state` | Gauge | `state` | State of the fault quarantine breaker |
| `fault_quarantine_breaker_utilization` | Gauge | - | Utilization of the fault quarantine breaker |
| `fault_quarantine_get_total_nodes_duration_seconds` | Histogram | `result` | Duration of getTotalNodesWithRetry calls in seconds |
| `fault_quarantine_get_total_nodes_errors_total` | Counter | `error_type` | Total number of errors from getTotalNodesWithRetry |
| `fault_quarantine_get_total_nodes_retry_attempts` | Histogram | - | Number of retry attempts needed for getTotalNodesWithRetry (buckets: 0, 1, 2, 3, 5, 10) |

---

## Node Drainer Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_events_received_total` | Counter | - | Total number of events received from the watcher |
| `node_drainer_events_replayed_total` | Counter | - | Total number of in-progress events replayed at startup |
| `node_drainer_events_processed_total` | Counter | `drain_status`, `node` | Total number of events processed by drain status outcome. Drain status values: `drained`, `cancelled`, `skipped` |
| `node_drainer_cancelled_event_total` | Counter | `node`, `check_name` | Total number of cancelled drain events (due to manual uncordon or healthy recovery) |
| `node_drainer_processing_errors_total` | Counter | `error_type`, `node` | Total number of errors encountered during event processing and node draining |
| `node_drainer_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |
| `node_drainer_queue_depth` | Gauge | - | Total number of pending events in the queue |
| `node_drainer_pod_eviction_duration_seconds` | Histogram | - | Time from event receipt by node-drainer to successful pod eviction completion. Buckets: Exponential (0.1s, factor 2, 23 buckets, up to ~3 days) |

### Node Draining Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_waiting_for_timeout` | Gauge | `node` | Shows if node drainer operation is waiting for timeout before force deletion (1=waiting, 0=not waiting) |
| `node_drainer_force_delete_pods_after_timeout` | Counter | `node`, `namespace` | Total number of node drainer operations that reached timeout and force deleted pods |

---

## Fault Remediation Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_remediation_events_received_total` | Counter | - | Total number of events received from the watcher |
| `fault_remediation_events_processed_total` | Counter | `cr_status`, `node_name` | Total number of remediation events processed by CR creation status. CR status values: `created`, `skipped` |
| `fault_remediation_processing_errors_total` | Counter | `error_type`, `node_name` | Total number of errors encountered during event processing |
| `fault_remediation_unsupported_actions_total` | Counter | `action`, `node_name` | Total number of health events with currently unsupported remediation actions |
| `fault_remediation_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |
| `fault_remediation_cr_generate_duration_seconds` | Histogram | - | Time from drain completion (or quarantine completion if drain timestamp unavailable) to maintenance CR creation. Buckets: Prometheus DefBuckets |

### Log Collector Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_remediation_log_collector_jobs_total` | Counter | `node_name`, `status` | Total number of log collector jobs. Status values: `success`, `failure`, `timeout` |
| `fault_remediation_log_collector_job_duration_seconds` | Histogram | `node_name`, `status` | Duration of log collector jobs in seconds |
| `fault_remediation_log_collector_errors_total` | Counter | `error_type`, `node_name` | Total number of errors encountered in log collector operations |

### File Server Metrics

#### HTTP Request Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `http_response_count_total` | Counter | `method`, `status`, `app` | Total HTTP responses by method and status code |
| `http_request_duration_seconds` | Histogram | `method`, `status` | HTTP request duration in seconds |

#### Log Rotation Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fileserver_log_rotation_successful_total` | Counter | - | Total successful log cleanup operations |
| `fileserver_log_rotation_failed_total` | Counter | - | Total failed log cleanup operations |
| `fileserver_disk_space_free_bytes` | Gauge | - | Free disk space in bytes |

---

## Labeler Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `labeler_events_processed_total` | Counter | `status` | Total number of pod events processed. Status values: `success`, `failed` |
| `labeler_node_update_failures_total` | Counter | - | Total number of node update failures during reconciliation |
| `labeler_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

---

## Janitor

### Action Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `janitor_actions_count` | Counter | `action_type`, `status`, `node` | Total number of janitor actions by type and status. Action types: `reboot`, `terminate`. Status values: `started`, `succeeded`, `failed` |
| `janitor_action_mttr_seconds` | Histogram | `action_type` | Time from CR creation to action completion (Mean Time To Repair). Uses exponential buckets (10, 2, 10) for log-scale MTTR measurement |

---

## Platform Connectors

### Kubernetes Connector Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `k8s_platform_connector_node_condition_update_total` | Counter | `status` | Total number of node condition updates by status. Status values: `success`, `failed` |
| `k8s_platform_connector_node_event_operations_total` | Counter | `node_name`, `operation`, `status` | Total number of node event operations by type and status. Operation values: `create`, `update`. Status values: `success`, `failed` |
| `k8s_platform_connector_node_condition_update_duration_milliseconds` | Histogram | - | Duration of node condition updates in milliseconds. Uses linear buckets (0, 10, 500) |
| `k8s_platform_connector_node_event_update_create_duration_milliseconds` | Histogram | - | Duration of node event updates/creations in milliseconds. Uses linear buckets (0, 10, 500) |

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
| `health_events_insertion_to_uds_error` | Counter | - | Total number of failed insertions of health events to UDS |
| `dcgm_health_active_events` | Gauge | `event_type`, `gpu_id`, `severity` | Total number of active health events at any given time by severity. Severity values: `fatal`, `non_fatal` |

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

### CSP Health Monitor

The CSP health monitor tracks cloud provider maintenance events and node health issues.

#### CSP Client Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_csp_events_received_total` | Counter | `csp` | Total number of raw events received from CSP API/source |
| `csp_health_monitor_csp_polling_duration_seconds` | Histogram | `csp` | Duration of CSP polling cycles |
| `csp_health_monitor_csp_api_errors_total` | Counter | `csp`, `error_type` | Total number of errors encountered during CSP API calls |
| `csp_health_monitor_csp_api_polling_duration_seconds` | Histogram | `csp`, `api` | Duration of CSP API polling cycles |
| `csp_health_monitor_csp_monitor_errors_total` | Counter | `csp`, `error_type` | Total number of errors initializing or starting CSP monitors |
| `csp_health_monitor_csp_events_by_type_unsupported_total` | Counter | `csp`, `event_type` | Total number of raw CSP events received, partitioned by event type code |

#### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_main_events_to_normalize_total` | Counter | `csp` | Total number of events passed to the normalizer |
| `csp_health_monitor_main_normalization_errors_total` | Counter | `csp` | Total number of errors during event normalization |
| `csp_health_monitor_main_events_received_total` | Counter | `csp` | Total number of normalized events received by the main processor |
| `csp_health_monitor_main_events_processed_success_total` | Counter | `csp` | Total number of events successfully processed |
| `csp_health_monitor_main_processing_errors_total` | Counter | `csp`, `error_type` | Total number of errors during event processing |
| `csp_health_monitor_main_event_processing_duration_seconds` | Histogram | `csp` | Duration of processing a single event |

#### Datastore Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_main_datastore_upsert_attempts_total` | Counter | `csp` | Total number of attempts to upsert maintenance events |
| `csp_health_monitor_main_datastore_upsert_total` | Counter | `csp`, `status` | Total number of maintenance event upserts by status. Status values: `success`, `failed` |

#### Trigger Engine Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_trigger_poll_cycles_total` | Counter | - | Total number of polling cycles executed by the trigger engine |
| `csp_health_monitor_trigger_poll_errors_total` | Counter | - | Total number of errors during a trigger engine poll cycle |
| `csp_health_monitor_trigger_events_found_total` | Counter | `trigger_type` | Total number of events found potentially needing a trigger |
| `csp_health_monitor_trigger_attempts_total` | Counter | `trigger_type` | Total number of trigger attempts made |
| `csp_health_monitor_trigger_success_total` | Counter | `trigger_type` | Total number of successful triggers |
| `csp_health_monitor_trigger_failures_total` | Counter | `trigger_type`, `failure_reason` | Total number of failed trigger attempts |
| `csp_health_monitor_trigger_datastore_query_duration_seconds` | Histogram | `query_type` | Duration of datastore queries performed by the trigger engine |
| `csp_health_monitor_trigger_datastore_query_errors_total` | Counter | `query_type` | Total number of errors during datastore queries |
| `csp_health_monitor_trigger_datastore_update_errors_total` | Counter | `trigger_type` | Total number of errors updating event status after trigger |
| `csp_health_monitor_trigger_uds_send_duration_seconds` | Histogram | - | Duration of sending health events via UDS |
| `csp_health_monitor_trigger_uds_send_errors_total` | Counter | - | Total number of errors encountered when sending events via UDS |
| `csp_health_monitor_node_not_ready_timeout_total` | Counter | `node_name` | Total number of nodes that remained not ready after the timeout period |
| `csp_health_monitor_node_readiness_monitoring_started_total` | Counter | `node_name` | Total number of times background node readiness monitoring was started |

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
