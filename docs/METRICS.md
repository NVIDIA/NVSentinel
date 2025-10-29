# NVSentinel Metrics

This document outlines all Prometheus metrics exposed by NVSentinel components.

## Table of Contents

- [Fault Quarantine Module](#fault-quarantine-module)
- [Fault Remediation Module](#fault-remediation-module)
- [Health Events Analyzer](#health-events-analyzer)
- [Labeler Module](#labeler-module)
- [Node Drainer Module](#node-drainer-module)
- [Janitor](#janitor)
- [Platform Connectors](#platform-connectors)
- [Health Monitors](#health-monitors)
  - [CSP Health Monitor](#csp-health-monitor)
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

## Fault Remediation Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_remediation_events_received_total` | Counter | - | Total number of events received from the watcher |
| `fault_remediation_events_successfully_processed_total` | Counter | - | Total number of events successfully processed |
| `fault_remediation_processing_errors_total` | Counter | `error_type`, `node_name` | Total number of errors encountered during event processing |
| `fault_remediation_unsupported_actions_total` | Counter | `action`, `node_name` | Total number of health events with currently unsupported remediation actions |

### Log Collection Job Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_remediation_log_collector_jobs_total` | Counter | `node_name`, `status` | Total number of log collector jobs |
| `fault_remediation_log_collector_job_duration_seconds` | Histogram | `node_name`, `status` | Duration of log collector jobs in seconds |

---

## Health Events Analyzer

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `health_event_analyzer_events_received_total` | Counter | `entity_value` | Total number of events received from the watcher |
| `health_event_analyzer_events_successfully_processed_total` | Counter | - | Total number of events successfully processed |
| `health_event_analyzer_event_processing_errors` | Counter | `error_type` | Total number of errors encountered during event processing |
| `fatal_events_published_total` | Counter | `entity_value` | Total number of new fatal events published |
| `health_event_analyzer_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

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

## Node Drainer Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_events_received_total` | Counter | - | Total number of events received from the watcher |
| `node_drainer_events_replayed_total` | Counter | - | Total number of in-progress events replayed at startup |
| `node_drainer_events_successfully_processed_total` | Counter | - | Total number of events successfully processed |
| `node_drainer_processing_errors_total` | Counter | `error_type` | Total number of errors encountered during event processing |
| `node_drainer_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

### Health Event Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_healthy_event_total` | Counter | `node`, `check_name` | Total number of healthy events |
| `node_drainer_healthy_event_with_node_drain_cancel_total` | Counter | - | Total number of healthy events that led to the cancellation of node draining |
| `node_drainer_unhealthy_event_total` | Counter | `node`, `check_name` | Total number of unhealthy events |

### Node Draining Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_node_drain_successful_total` | Counter | `node` | Total number of successful node drainings |
| `node_drainer_node_drain_errors_total` | Counter | `error_type`, `node` | Total number of errors encountered while draining a node |
| `node_drainer_node_drain_status` | Gauge | `node` | Shows if a node is currently being drained (1) or not (0) |
| `node_drainer_waiting_for_timeout` | Gauge | `node` | Total number of node drainer operations in deleteAfterTimeout mode |
| `node_drainer_force_delete_pods_after_timeout` | Counter | `node`, `namespace` | Total number of node drainer operations that reached timeout and force deleted pods |
| `node_drainer_queue_depth` | Gauge | `node` | Number of pending events in the queue for each node |

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

### CSP Health Monitor

#### Main Monitor Metrics - CSP Client

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_csp_events_received_total` | Counter | `csp` | Total number of raw events received from CSP API/source. CSP values: `gcp`, `aws` |
| `csp_health_monitor_csp_polling_duration_seconds` | Histogram | `csp` | Duration of CSP polling cycles |
| `csp_health_monitor_csp_api_errors_total` | Counter | `csp`, `error_type` | Total number of errors encountered during CSP API calls. Error types: `connection`, `parse`, `rate_limit`, etc. |
| `csp_health_monitor_csp_api_polling_duration_seconds` | Histogram | `csp`, `api` | Duration of CSP API polling cycles. API values: `describe_events`, `describe_affected_entities`, `describe_event_details`, etc. |
| `csp_health_monitor_csp_monitor_errors_total` | Counter | `csp`, `error_type` | Total number of errors initializing or starting CSP monitors. Error types: `init_error`, `start_error` |
| `csp_health_monitor_csp_events_by_type_unsupported_total` | Counter | `csp`, `event_type` | Total number of raw CSP events received, partitioned by event type code (e.g., AWS_EC2_PERSISTENT_INSTANCE_RETIREMENT_SCHEDULED) |

#### Main Monitor Metrics - Normalization

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_main_events_to_normalize_total` | Counter | `csp` | Total number of events passed to the normalizer |
| `csp_health_monitor_main_normalization_errors_total` | Counter | `csp` | Total number of errors during event normalization |

#### Main Monitor Metrics - Event Processor

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_main_events_received_total` | Counter | `csp` | Total number of normalized events received by the main processor |
| `csp_health_monitor_main_events_processed_success_total` | Counter | `csp` | Total number of events successfully processed by the main processor (mapped & stored) |
| `csp_health_monitor_main_processing_errors_total` | Counter | `csp`, `error_type` | Total number of errors during event processing. Error types: `mapping`, `datastore_upsert` |
| `csp_health_monitor_main_event_processing_duration_seconds` | Histogram | `csp` | Duration of processing a single event (mapping + storing) |

#### Main Monitor Metrics - Datastore

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_main_datastore_upsert_attempts_total` | Counter | `csp` | Total number of attempts to upsert maintenance events |
| `csp_health_monitor_main_datastore_upsert_success_total` | Counter | `csp` | Total number of successful maintenance event upserts |
| `csp_health_monitor_main_datastore_upsert_errors_total` | Counter | `csp` | Total number of errors during maintenance event upserts |

#### Quarantine Trigger Engine (Sidecar) Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_trigger_poll_cycles_total` | Counter | - | Total number of polling cycles executed by the trigger engine |
| `csp_health_monitor_trigger_poll_errors_total` | Counter | - | Total number of errors during a trigger engine poll cycle (e.g., DB query failed) |

#### Trigger Engine - Datastore Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_trigger_datastore_query_duration_seconds` | Histogram | `query_type` | Duration of datastore queries performed by the trigger engine. Query types: `quarantine`, `healthy`, `last_timestamp_gcp`, `last_timestamp_aws` |
| `csp_health_monitor_trigger_datastore_query_errors_total` | Counter | `query_type` | Total number of errors during datastore queries by the trigger engine |
| `csp_health_monitor_trigger_datastore_update_errors_total` | Counter | `trigger_type` | Total number of errors updating event status after trigger |

#### Trigger Engine - Triggering Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_trigger_events_found_total` | Counter | `trigger_type` | Total number of events found potentially needing a trigger. Trigger types: `quarantine`, `healthy` |
| `csp_health_monitor_trigger_attempts_total` | Counter | `trigger_type` | Total number of trigger attempts made (sending event via UDS) |
| `csp_health_monitor_trigger_success_total` | Counter | `trigger_type` | Total number of successful triggers (UDS send OK, DB status updated) |
| `csp_health_monitor_trigger_failures_total` | Counter | `trigger_type`, `failure_reason` | Total number of failed trigger attempts. Failure reasons: `mapping`, `uds`, `db_update` |

#### Trigger Engine - UDS Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_trigger_uds_send_duration_seconds` | Histogram | - | Duration of sending health events via UDS |
| `csp_health_monitor_trigger_uds_send_errors_total` | Counter | - | Total number of errors encountered when sending events via UDS |

#### Trigger Engine - Node Readiness Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_node_not_ready_timeout_total` | Counter | `node_name` | Total number of nodes that remained not ready after the timeout period |
| `csp_health_monitor_node_readiness_monitoring_started_total` | Counter | `node_name` | Total number of times background node readiness monitoring was started |

---

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

All NVSentinel components expose Prometheus metrics on a metrics endpoint (typically `:8080/metrics`). The metrics can be scraped by Prometheus using standard scrape configurations.

### Helm Chart Configuration

To enable Prometheus integration via the Helm chart:

```bash
helm install nvsentinel ./distros/kubernetes/nvsentinel \
  --set prometheus.enabled=true
```

### Annotation-based Discovery

Components can be configured to include Prometheus scrape annotations:

```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "8080"
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
