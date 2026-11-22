# NVSentinel Metrics

This document lists all Prometheus metrics that NVSentinel components expose.

## Table of Contents

- [Fault Quarantine Module](#fault-quarantine-module)
- [Node Drainer Module](#node-drainer-module)
- [Fault Remediation Module](#fault-remediation-module)
- [Labeler Module](#labeler-module)
- [Janitor](#janitor)
- [Platform Connectors](#platform-connectors)
- [Health Events Analyzer](#health-events-analyzer)
- [Event Exporter](#event-exporter)
- [Lifecycle Manager](#lifecycle-manager)
- [Metadata Collector](#metadata-collector)
- [Commons](#commons)
- [Health Monitors](#health-monitors)
  - [Health Event Publisher](#health-event-publisher)
  - [GPU Health Monitor](#gpu-health-monitor)
  - [NIC Health Monitor](#nic-health-monitor)
  - [Syslog Health Monitor](#syslog-health-monitor)
  - [CSP Health Monitor](#csp-health-monitor)
  - [NVCRE Certification Monitor](#nvcre-certification-monitor)
  - [Kubernetes Object Monitor](#kubernetes-object-monitor)
  - [Slurm Drain Monitor](#slurm-drain-monitor)
- [Change Stream Metrics](#change-stream-metrics)

---

## Fault Quarantine Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_events_received_total` | Counter | - | Total number of health events received from the watcher |
| `fault_quarantine_events_successfully_processed_total` | Counter | - | Total number of health events successfully processed |
| `fault_quarantine_processing_errors_total` | Counter | `error_type` | Total number of errors during event processing |
| `fault_quarantine_cold_start_events_total` | Counter | `result` | Health events examined during startup recovery, labeled `processed`, `skipped`, `superseded`, `invalid`, or `failed` |
| `fault_quarantine_cold_start_duration_seconds` | Histogram | - | Startup recovery duration. Buckets: ExponentialBuckets(start=0.1s, factor=2, count=18), max ~3.6 hours |
| `fault_quarantine_event_backlog_count` | Gauge | - | Number of health events that fault quarantine has not processed. The gauge reports `-1` from registration until the process records its first event checkpoint. The count uses this checkpoint as its baseline, so no count exists before then. On PostgreSQL, the change-stream watcher does not support the count, so the gauge stays `-1` even after the first checkpoint |
| `fault_quarantine_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

### Node Quarantine Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_nodes_quarantined_total` | Counter | `node` | Total number of nodes quarantined |
| `fault_quarantine_nodes_unquarantined_total` | Counter | `node` | Total number of nodes unquarantined |
| `fault_quarantine_nodes_manually_uncordoned_total` | Counter | `node` | Total number of manual uncordons of nodes |
| `fault_quarantine_nodes_manually_untainted_total` | Counter | `node` | Total number of manual taint removals for nodes |
| `fault_quarantine_nodes_pending_cancellation` | Gauge | - | Nodes that wait until fault quarantine marks their health events `Cancelled` after a manual uncordon, a manual untaint or a dry-run switch. If the gauge stays above zero, the cancellation keeps failing |
| `fault_quarantine_current_quarantined_nodes` | Gauge | `node` | Current number of quarantined nodes |
| `fault_quarantine_dry_run_actions_total` | Counter | `action` | Total number of quarantine and unquarantine actions skipped by dry run |

### Taint and Cordon Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_taints_applied_total` | Counter | `taint_key`, `taint_effect` | Total number of taints applied to nodes |
| `fault_quarantine_taints_removed_total` | Counter | `taint_key`, `taint_effect` | Total number of taints removed from nodes |
| `fault_quarantine_labels_applied_total` | Counter | `label_key` | Total number of quarantine labels applied to nodes |
| `fault_quarantine_labels_removed_total` | Counter | `label_key` | Total number of quarantine labels removed from nodes |
| `fault_quarantine_cordons_applied_total` | Counter | - | Total number of cordons applied to nodes |
| `fault_quarantine_cordons_removed_total` | Counter | - | Total number of cordons removed from nodes |
| `fault_quarantine_node_quarantine_duration_seconds` | Histogram | - | Time from health event generation to node quarantine completion. Buckets: Prometheus DefBuckets |
| `fault_quarantine_node_remediation_duration_seconds` | Histogram | `recommended_action` | End-to-end node remediation time: from `generatedTimestamp` (of the original unhealthy event) to the node unquarantine. Fault quarantine emits it on both an automatic unquarantine (through a healthy event) and a manual uncordon. Buckets: ExponentialBuckets(start=10s, factor=1.5, count=27), max ~4.4 days |
| `fault_quarantine_node_remediation_duration_excluding_drain_seconds` | Histogram | `recommended_action` | Remediation time excluding the node-drainer duration: `(unquarantineTime - generatedTimestamp) - (drainFinishTimestamp - quarantineFinishTimestamp)`. Fault quarantine emits it only when both `quarantineFinishTimestamp` and `drainFinishTimestamp` are present in the original event document. Buckets: ExponentialBuckets(start=10s, factor=1.5, count=19), max ~4.1 hours |

### Ruleset Evaluation Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_ruleset_evaluations_total` | Counter | `ruleset`, `status` | Total number of ruleset evaluations. Status values: `passed`, `failed` |

### Circuit Breaker Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_quarantine_breaker_state` | Gauge | `state` | State of the fault quarantine breaker |
| `fault_quarantine_breaker_utilization` | Gauge | - | Fraction of GPU nodes cordoned in the sliding window of the circuit breaker |
| `fault_quarantine_breaker_threshold_nodes` | Gauge | `bound` | Cordoned-node count that trips the circuit breaker. The label shows which configured bound produced this count: `percentage`, `maxNodes`, `fleetSize` when clamped, or `noGPUNodes` when the circuit breaker counts no GPU nodes. In that last case, a node cordoned in the window that is still in the cluster trips the breaker |
| `fault_quarantine_get_total_nodes_duration_seconds` | Histogram | `result` | Duration in seconds of each read of the GPU node count for the circuit breaker |
| `fault_quarantine_get_total_nodes_errors_total` | Counter | `error_type` | Total number of errors when fault quarantine reads the GPU node count for the circuit breaker |

---

## Node Drainer Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_events_received_total` | Counter | - | Total number of health events received from the watcher |
| `node_drainer_events_replayed_total` | Counter | - | Total number of in-progress health events replayed at startup |
| `node_drainer_events_processed_total` | Counter | `drain_status`, `node`, `drain_scope` | Total number of health events processed, by drain status outcome and drain scope. Drain status values: `drained`, `cancelled`, `skipped` |
| `node_drainer_cancelled_event_total` | Counter | `node`, `check_name` | Total number of cancelled drain events (because of a manual uncordon or a healthy recovery) |
| `node_drainer_processing_errors_total` | Counter | `error_type`, `node` | Total number of errors during event processing and node draining |
| `node_drainer_queue_items_assigned_total` | Counter | `priority`, `reason` | Total number of ready queue items assigned, by priority and reason |
| `node_drainer_custom_drain_crd_not_found_total` | Counter | `node` | Total number of custom drain operations that failed because the CRD did not exist in the cluster |
| `node_drainer_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |
| `node_drainer_queue_depth` | Gauge | - | Total number of pending health events in the queue |
| `node_drainer_pod_eviction_duration_seconds` | Histogram | - | Time from when node-drainer receives the health event to the successful completion of the pod eviction. Buckets: Exponential (0.1s, factor 2, 23 buckets, up to ~3 days) |

### Node Draining Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `node_drainer_waiting_for_timeout` | Gauge | `node` | Shows if the node-drainer operation waits for the timeout before force deletion (1=waiting, 0=not waiting) |
| `node_drainer_awaiting_completion_event_timestamp_seconds` | Gauge | `node` | Unix time of the health event behind an `allowCompletion` drain that still waits for pods. This mode has no deadline, so alert on `time() -` this value. For its caveats, see [AllowCompletion Drain Metric](configuration/node-drainer.md#allowcompletion-drain-metric). |
| `node_drainer_force_delete_pods_after_timeout` | Counter | `node`, `namespace` | Total number of node-drainer operations that reached the timeout and force deleted pods |
| `node_drainer_partial_drains_total` | Counter | `node`, `entity_type`, `entity_value` | Total number of partial drains, by the entity that they targeted. Disabled by default. To enable it, set [`node-drainer.partialDrainEntityMetricEnabled`](configuration/node-drainer.md#partial-drain-entity-metric) to `true` |

---

## Fault Remediation Module

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_remediation_events_received_total` | Counter | - | Total number of health events received from the watcher |
| `fault_remediation_events_processed_total` | Counter | `cr_status`, `node_name` | Total number of remediation events processed, by CR creation status. CR status values: `created`, `skipped` |
| `fault_remediation_processing_errors_total` | Counter | `error_type`, `node_name` | Total number of errors during event processing |
| `fault_remediation_unsupported_actions_total` | Counter | `action`, `node_name` | Total number of health events with unsupported remediation actions |
| `fault_remediation_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |
| `fault_remediation_cr_generate_duration_seconds` | Histogram | - | Time from drain completion (or quarantine completion, if the drain timestamp is not available) to maintenance CR creation. Buckets: Prometheus DefBuckets |

### Log Collector Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `fault_remediation_log_collector_jobs_total` | Counter | `node_name`, `status` | Total number of log collector jobs. Status values: `success`, `failure`, `timeout` |
| `fault_remediation_log_collector_job_duration_seconds` | Histogram | `node_name`, `status` | Duration of log collector jobs in seconds |
| `fault_remediation_log_collector_errors_total` | Counter | `error_type`, `node_name` | Total number of errors in log collector operations |

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
| `labeler_device_count_current` | Gauge | `node`, `class` | Current device count observed for a node and device class |
| `labeler_device_count_expected` | Gauge | `class`, `partition` | Expected device count for a device class and hardware partition |
| `labeler_device_count_source` | Gauge | `node`, `class`, `source` | Source that counted the devices of a node for a device class (1 for the selected source) |
| `labeler_device_count_skipped_updates_total` | Counter | `class`, `reason` | Total number of skipped device-count label updates |
| `labeler_node_labels_skipped_managed_total` | Counter | - | Node label reconcile attempts skipped because the node is opted out of NVSentinel management |

---

## Janitor

### Action Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `janitor_actions_count` | Counter | `action_type`, `status`, `node` | Total number of janitor actions by type and status. Action types: `reboot`, `terminate`. Status values: `started`, `succeeded`, `failed` |
| `janitor_action_mttr_seconds` | Histogram | `action_type` | Time from CR creation to action completion (Mean Time To Repair). Uses exponential buckets (10, 2, 10) for log-scale MTTR measurement |

### GPU Reset Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `gpu_reset_requests_total` | Counter | `node` | Total number of GPU reset requests started |
| `gpu_reset_requests_completed_total` | Counter | `node`, `status` | Total number of completed GPU reset requests, labeled by their final status |
| `gpu_reset_duration_seconds` | Histogram | `node`, `status` | The end-to-end duration of GPU reset workflows, from controller acknowledgment to completion, labeled by their final status |
| `gpu_reset_active_requests` | Gauge | `node` | The number of GPU reset requests in progress |
| `gpu_reset_failure_reasons_total` | Counter | `node`, `reason` | Total number of GPU reset failures, labeled by the specific reason |
| `janitor_ttl_deletions_total` | Counter | `kind` | Total number of CRs that the TTL reconciler deleted, labeled by kind |

### External Remediation Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `nvsentinel_external_remediation_total` | Counter | `phase`, `result` | Lifecycle transitions of ExternalRemediationRequest objects, labeled by phase and outcome |
| `nvsentinel_external_remediation_open` | Gauge | `node`, `recommended_action`, `state` | Open ExternalRemediationRequest objects, by node, action, and substate |
| `nvsentinel_external_remediation_age_seconds` | Histogram | `recommended_action`, `result` | Age of an ExternalRemediationRequest at close time |

---

## Platform Connectors

### Health Event Authentication Metrics

These metrics track the node-binding interceptor. The interceptor stops a caller on one node from submitting health events that name another node. See [Health Event Authentication](platform-connectors.md#health-event-node-binding).

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `platform_connector_auth_decisions_total` | Counter | `decision` | Health event batches, by the node scope granted to the caller. Values: `node_local`, `cross_node` |
| `platform_connector_auth_node_claim_total` | Counter | `result` | Whether the token of an authenticated caller carried a node claim. `verified`: the token carried a node claim, and the claim named this node. The deployment platform connector has no node of its own, so it takes the claim as the node of the caller. `absent`: the token carried no node claim, so the platform connector pinned the caller to this node. A caller without a token gets the same scope. The deployment platform connector instead rejects a caller whose token has no node claim. The reason is `node_claim_absent` or `unbound_token`. If the identity is on the cross-node list, the reason is `cross_node_claim_absent` or `unbound_cross_node_token`. |
| `platform_connector_auth_violations_total` | Counter | `reason` | Health event batches that violated the node-binding rule. Under `global.platformConnectorAuth.mode: enforce` (default), the platform connector rejects these batches. Under `mode: audit`, it records them here but accepts them. Reasons: `node_mismatch` (a node-local caller named a different node). `node_claim_mismatch` (the token was issued on another node; this reason applies to every caller, allowlisted or not). `unbound_cross_node_token` (an allowlisted caller presented a token bound to no pod, for example from `kubectl create token`). `cross_node_claim_absent` (the token is pod-bound, but the pod was never scheduled). `missing_node_name` (the event has no node name, and the platform connector was not able to stamp one). `token_invalid` (TokenReview rejected the token). `malformed_credentials` (the authorization header was duplicated or did not use the Bearer scheme). Three reasons occur only on the deployment platform connector, because it has no local node to use instead: `token_missing` (no token at all). `unbound_token` (a token bound to no pod). `node_claim_absent` (a token bound to a pod that never scheduled, so there is no node to pin its events to). Three more reasons mean that the validator **reached no verdict**, not that the platform connector rejected the caller: `validator_unavailable` (the API server was unreachable). `validator_timeout` (the caller stopped waiting, or its deadline passed). `validator_error` (an unexpected failure, typically in the RBAC permission of this component to create TokenReviews). With `failOpenOnUnavailable: true`, `validator_unavailable` and `validator_timeout` still increment this counter. However, the platform connector then gives the caller a degraded node-local scope instead of rejecting the request. This also applies in `mode: enforce`, because an outage says nothing about the credential of the caller. That scope is a guess, not a verified identity. In either mode, the platform connector accepts and stamps an event with a blank or matching node name as usual. For an event that names a different node, the result depends on `mode`. Under `enforce`, the platform connector refuses the event as retryable `Unavailable`, and does not count it as `node_mismatch`. The reason is that the caller can be an allowlisted cross-node publisher that the platform connector did not verify because of the outage. Under `audit`, the platform connector records that same `Unavailable` and then accepts the event, like any other audited violation. So it forwards the event with whatever node name the event carried. `missing_node_name` is the one violation that `mode: audit` does not accept. The platform connector always rejects a blank node name from a caller that resolved to **verified** cross-node scope, in both modes. The reason is that nothing downstream can handle it. This rule does not cover a caller that has a degraded node-local scope under `failOpenOnUnavailable`. The platform connector never verified the identity of this caller as cross-node. So it stamps a blank name from this caller, as it does for any other node-local caller. When you alert on suspected credential abuse, exclude the three no-verdict reasons. A control-plane outage increments them for every in-flight request. Without the exclusion, an outage looks identical to an attack. The selector to alert on is below this table. |

The alerting selector for suspected credential abuse, without the three no-verdict reasons:

```promql
sum(rate(platform_connector_auth_violations_total{reason=~"node_mismatch|token_invalid|node_claim_mismatch|unbound_cross_node_token|malformed_credentials|cross_node_claim_absent|missing_node_name|token_missing|unbound_token|node_claim_absent"}[5m]))
```

### Kubernetes Connector Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `k8s_platform_connector_node_condition_update_total` | Counter | `status` | Total number of node condition updates, by status. Status values: `success`, `failed`, `skipped`. The platform connector skips an update that does not change the node conditions. An example is the repeat of a fault that the node already shows |
| `k8s_platform_connector_node_event_operations_total` | Counter | `operation`, `status` | Total number of node event operations, by type and status. Operation values: `create`, `update`. Status values: `success`, `failed`, `skipped`. The platform connector skips a repeat of a fault if it wrote the Event of that fault less than 10 minutes ago. Later repeats refresh the Event |
| `k8s_platform_connector_node_condition_update_duration_milliseconds` | Histogram | - | Duration of node condition updates in milliseconds. Uses linear buckets (0, 10, 500) |
| `k8s_platform_connector_node_event_update_create_duration_milliseconds` | Histogram | - | Duration of node event updates and creations in milliseconds. Uses linear buckets (0, 10, 500) |
| `k8s_platform_connector_dropped_writes_total` | Counter | `operation`, `reason`, `is_healthy` | Kubernetes writes that the platform connector discarded, including writes that it did not attempt at a deadline or at shutdown. `is_healthy` separates a lost clear from a lost set. A lost clear can leave a fault in place with nothing left to clear it. The next change reports a lost set again. For `operation="node_condition"`, one write carries every event grouped for that node. So `is_healthy="true"` means that the write contained at least one recovery, not that all of it was recoveries |
| `k8s_platform_connector_dropped_batches_total` | Counter | `reason` | Batches that contain discarded Kubernetes writes, counted one time for each terminal reason. This metric intentionally has no label for the health direction, because one batch can contain both |

### Prometheus Connector Metrics

Every health monitor publishes through the platform connector, so this one metric covers `gpu-health-monitor`, `syslog-health-monitor`, `nic-health-monitor`, `csp-health-monitor`, `kubernetes-object-monitor` and `health-events-analyzer` without per-monitor instrumentation. Enable it with `platformConnector.promConnector.enabled` (default `false`, like the other optional connectors).

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `health_events_total` | Counter | `node`, `agent`, `check_name`, `recommended_action`, `is_fatal`, `is_healthy` | Health events that the platform connector received. `recommended_action` is the enum name, for example `NONE`, `CONTACT_SUPPORT` or `RESTART_VM`. A `CUSTOM` action reports as `CUSTOM`. The label does not expand `customRecommendedAction`, because that value is operator-defined and unbounded |

`node` carries the **event's own** `nodeName`, not the node of the connector pod that received the event. This difference is important. The DaemonSet monitors publish over the node-local socket, so the two nodes are the same. But `health-events-analyzer` is a Deployment, and its derived events describe other nodes. Also, an allowlisted cross-node publisher can name any node. An explicit label makes every agent correct without a `kube_pod_info` join.

The metric does not include `errorCode`, because its values are unbounded. Suffixed XIDs such as `145.RLW_SRC_TRACK` can grow the series set without limit. The size of the fleet limits the values of `node`.

The metric keeps `is_fatal`, although producers derive it as `recommendedAction != NONE`. This redundancy is intentional. A producer that disagrees with that derivation becomes visible, not silent.

Useful queries:

```promql
# Actionable event rate by action, fleet-wide
sum by (recommended_action) (rate(health_events_total{recommended_action!="NONE"}[5m]))

# Which agent and check is producing the actionable events
sum by (agent, check_name) (rate(health_events_total{recommended_action!="NONE",is_healthy="false"}[1h]))

# Per node, correct for every agent including health-events-analyzer
sum by (node) (rate(health_events_total{recommended_action!="NONE"}[1h]))
```

### Platform Connector Request Metrics

Both roles of the platform connector, the node-local DaemonSet and the deployment platform connector, expose these metrics for the batches that reach the request handler. Alert on `failed` outcomes and on best-effort failures. The deployment platform connector (`--mode=deployment`) also exposes the metrics above for its Kubernetes connector and for the node-binding interceptor. `platform_connector_auth_violations_total` counts the rejections of the interceptor, by the reasons listed there. The deployment platform connector also exposes the refusals below, which it answers before the handler sees the batch.

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `platform_connector_request_duration_seconds` | Histogram | `outcome` | Duration of health event batch requests that reached the handler, by outcome. `ok`: acknowledged. `rejected`: the batch is invalid. `failed`: the connector returned an error while it processed the batch, and the caller retries. Both roles expose this metric. On the node-local DaemonSet, the connector is the set of ring buffers, which always accept. So a batch is `ok` after it is queued, unless it is invalid. On the deployment platform connector, when config.json enables a store connector, the write of that store connector decides the outcome. The node update and the sink are best effort and never fail a batch. So without a store, every batch that passed validation is `ok`. |
| `platform_connector_health_events_received_total` | Counter | `processing_strategy`, `is_healthy` | The total number of health events that the platform connector has received, by processing strategy and event health |
| `platform_connector_deployment_refusals_total` | Counter | `reason` | Batches refused before the handler saw them, for one of two reasons. `index_unverified`: this replica has a store connector and has not verified the idempotency index; the caller retries. `idempotency_key`: the idempotency-key header is missing or malformed |
| `platform_connector_store_batches_total` | Counter | `outcome` | Batches that either role wrote to the datastore: `stored`, or `duplicate` (a resend or a retried batch whose events already existed). The platform connector treats a duplicate like a store |
| `platform_connector_best_effort_failures_total` | Counter | `connector`, `reason` | Deployment platform connector only. Work that a best-effort connector (`kubernetes` node conditions and Events, `grpcsink` forwards) did not finish, although the platform connector acknowledged the batch. Reasons: `failed` or `timeout`. The node shows the old state until the next report from the monitor |

### Workqueue Metrics

These metrics track the performance of the internal ring buffer workqueue:

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `platform_connector_workqueue_depth_{name}` | Gauge | `workqueue` | Current depth of the platform connector workqueue |
| `platform_connector_workqueue_adds_total_{name}` | Counter | `workqueue` | Total number of adds that the platform connector workqueue handled |
| `platform_connector_workqueue_latency_seconds_{name}` | Histogram | `workqueue` | How long an item stays in the platform connector workqueue before it is requested. Uses linear buckets (0, 10, 500) |
| `platform_connector_workqueue_work_duration_seconds_{name}` | Histogram | `workqueue` | How long the processing of an item from the platform connector workqueue takes. Uses linear buckets (0, 10, 500) |
| `platform_connector_workqueue_retries_total_{name}` | Counter | `workqueue` | Total number of retries that the platform connector workqueue handled |
| `platform_connector_workqueue_longest_running_processor_seconds_{name}` | Gauge | `workqueue` | How many seconds the longest-running processor of the platform connector workqueue has run |
| `platform_connector_workqueue_unfinished_work_seconds_{name}` | Gauge | `workqueue` | The total time in seconds of work in progress in the platform connector workqueue |

**Note:** At runtime, the actual workqueue name replaces `{name}` in the metric names.

### gRPC Sink Connector Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `grpc_sink_platform_connector_send_total` | Counter | `status` | The total number of health event sends to the gRPC sink by status |
| `grpc_sink_platform_connector_send_duration_milliseconds` | Histogram | - | Duration of gRPC sink sends in milliseconds |
| `grpc_sink_platform_connector_retry_total` | Counter | - | The total number of retried health event sends to the gRPC sink |

### Transformer Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `nvsentinel_overrides_applied_total` | Counter | `rule_name`, `field` | Total number of overrides applied by rule and field |
| `nvsentinel_override_evaluation_errors_total` | Counter | - | Total number of errors in the evaluation of override rules |
| `nvsentinel_platform_connector_dedup_store_and_analyse_total` | Counter | `check`, `err_code` | Total number of duplicate health events that deduplication marked STORE_AND_ANALYSE |
| `nvsentinel_platform_connector_metadata_skip_label_gated_total` | Counter | - | Total health events gated to STORE_ONLY because the target node carries the configured skip label |
| `nvsentinel_platform_connector_metadata_lookup_failures_total` | Counter | - | Total metadata lookup failures where the event proceeded ungated (fail-open) |

---

## Health Events Analyzer

### Event Processing Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `health_event_analyzer_events_received_total` | Counter | `node_name` | Total number of health events received from the watcher |
| `health_event_analyzer_events_successfully_processed_total` | Counter | - | Total number of health events successfully processed |
| `health_event_analyzer_event_processing_errors` | Counter | `error_type` | Total number of errors during event processing |
| `fatal_event_publishing_errors` | Counter | `error_type` | Total number of errors that occurred when the analyzer published health events to UDS |
| `fatal_events_published_total` | Counter | `entity_value` | Total number of times the analyzer published a fatal health event for an entity |
| `recovery_events_published_total` | Counter | `rule_name`, `node_name` | Total number of recovery health events that the platform connector accepted, including republishes that wait for storage |
| `rule_matched_total` | Counter | `rule_name`, `node_name` | Total number of times a rule matched for a node |
| `rule_matched_entity_total` | Counter | `rule_name`, `node_name`, `entity_type`, `entity_value` | Total number of times a rule matched, labeled by the entity that it selected on. The analyzer records it when ruleMatchedEntityMetricEnabled is set |
| `health_event_analyzer_rule_skipped_total` | Counter | `rule_name` | Total number of rule evaluations skipped because the when expression of the rule was false |
| `health_event_analyzer_rule_when_errors_total` | Counter | `rule_name` | Total number of times the when expression of a rule failed to evaluate. The analyzer evaluated the rule anyway |
| `mongo_query_execution_duration_seconds` | Histogram | `rule_name` | Histogram of the execution durations of MongoDB pipelines |
| `health_event_analyzer_event_handling_duration_seconds` | Histogram | - | Histogram of event handling durations |

---

## Event Exporter

### Event Export Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `health_events_exporter_events_received_total` | Counter | - | Total number of health events received from the change stream |
| `health_events_exporter_events_published_total` | Counter | `status` | Total number of health events published to the sink. Status values: `success`, `failure` |
| `health_events_exporter_publish_duration_seconds` | Histogram | - | Histogram of event publish latency in seconds |
| `health_events_exporter_transform_errors_total` | Counter | - | Total number of errors when the exporter transforms health events to the CloudEvents format |
| `health_events_exporter_publish_errors_total` | Counter | `error_type` | Total number of errors when the exporter publishes health events to the sink |
| `health_events_exporter_token_refresh_errors_total` | Counter | - | Total number of OIDC token refresh errors |
| `health_events_exporter_events_filtered_total` | Counter | - | Total number of health events that the exporter did not publish, because the filter expression evaluated to false |
| `health_events_exporter_filter_errors_total` | Counter | - | Total number of filter evaluation errors. The exporter exports the event anyway (the metric counts events that bypassed the filter) |
| `health_events_exporter_resume_token_update_timestamp` | Gauge | - | Timestamp of the last resume token update (Unix seconds) |
| `health_events_exporter_backfill_in_progress` | Gauge | - | 1 during backfill, 0 otherwise |
| `health_events_exporter_backfill_events_processed_total` | Counter | - | Total number of events processed during backfill |
| `health_events_exporter_backfill_duration_seconds` | Histogram | - | Histogram of backfill operation duration in seconds |

---

## Lifecycle Manager

### Validation Request Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `validation_requests_total` | Counter | - | Total number of ValidationRequests started |
| `validation_requests_completed_total` | Counter | `status` | Total number of completed ValidationRequests, labeled by their final status. Status values: `success`, `failure` |
| `validation_requests_duration_seconds` | Histogram | `status` | The end-to-end duration of ValidationRequests, labeled by their final status |
| `new_node_validation_batches_total` | Counter | `status` | Total number of new node validation batch flush attempts, labeled by status |
| `new_node_validation_batch_size` | Histogram | - | Number of nodes included in each new node validation batch that succeeded |

---

## Metadata Collector

### Collection Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `metadata_collector_collection_completed_timestamp_seconds` | Gauge | - | Unix time when GPU metadata collection completed, or 0 while the collection still runs. Collection can block on a stuck GPU driver. So a value of 0 long after the process start means that the collection has stalled |

### Pod Mapper Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `metadata_collector_pod_mapper_failures_total` | Counter | - | Pod device mapper poll cycles that failed |
| `metadata_collector_pod_mapper_consecutive_failures` | Gauge | - | Consecutive failed poll cycles of the pod device mapper. Any success resets this value to 0. The container exits when this value reaches `--pod-mapper-max-consecutive-failures` |
| `metadata_collector_pod_mapper_last_success_timestamp_seconds` | Gauge | - | Unix time of the last successful poll of the pod device mapper, or 0 before the first one. After a successful poll, each device annotation on the node is current or removed |

---

## Commons

### Feature Flag and State Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `nvsentinel_feature_flag_enabled` | Gauge | `service`, `flag` | Reports whether a feature flag is enabled (1) or disabled (0) |
| `nvsentinel_state_transition_unexpected_total` | Counter | `from_state`, `to_state`, `node` | Total number of unexpected state transitions detected (race conditions or bugs) |

---

## Health Monitors

### Health Event Publisher

The Go health monitors send their health events through the shared publishing client (`commons/pkg/healthpub`). The `monitor` label names the monitor. The socket rows apply to the node-local path. The direct-mode rows apply only when a monitor publishes to the deployment platform connector. A monitor does this when `HEALTH_PUBLISH_TARGET` is set. The Helm value `publishTo: deployment` sets this variable.

| Metric Name                                                       | Type    | Labels              | Description                                                                                                                                                                       |
|-------------------------------------------------------------------|---------|---------------------|-----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------|
| `nvsentinel_health_events_publisher_skipped_pc_unavailable_total` | Counter | `monitor`           | Socket-path sends skipped because the node-local platform connector socket was missing                                                                                            |
| `nvsentinel_health_events_publisher_sends_success_total`          | Counter | `monitor`           | Batches that the platform connector acknowledged, on either path                                                                                                                  |
| `nvsentinel_health_events_publisher_sends_error_total`            | Counter | `monitor`, `code`   | Socket-path sends that failed after retries, by gRPC status code                                                                                                                  |
| `nvsentinel_health_events_publisher_dropped_total`                | Counter | `monitor`, `reason` | Direct-mode batches that the publisher dropped: `rejected` (the server refuses the batch permanently, or it exceeds the 4 MiB message limit), `retry_window_exhausted`, `shutdown` or `withdrawn`. The last reason means that the caller stopped waiting: its context ended or its timeout expired, so the publisher dropped the batch |
| `nvsentinel_health_events_publisher_retries_total`                | Counter | `monitor`           | Direct-mode send attempts that failed, and that the publisher retried in the retry window of the batch                                                                            |

---

### GPU Health Monitor

These metrics track the GPU health events that the monitor detects through DCGM (Data Center GPU Manager):

| Metric Name                                             | Type      | Labels                             | Description                                                                                               |
|---------------------------------------------------------|-----------|------------------------------------|-----------------------------------------------------------------------------------------------------------|
| `dcgm_health_events_publish_time_to_grpc_channel`       | Histogram | `operation_name`                   | Amount of time spent in publishing DCGM health events on the gRPC channel                                 |
| `health_events_insertion_to_uds_succeed_total`          | Counter   | -                                  | Total number of successful insertions of health events to UDS                                             |
| `health_events_insertion_to_uds_error_total`            | Counter   | -                                  | Total number of failed insertions of health events to UDS                                                 |
| `health_events_insertion_skipped_pc_unavailable_total`  | Counter   | -                                  | Sends skipped because the node-local platform connector socket was missing                                |
| `health_events_direct_publish_succeed_total`            | Counter   | -                                  | Batches that the deployment platform connector acknowledged (direct mode)                                 |
| `health_events_direct_publish_dropped_total`            | Counter   | `reason`                           | Direct-mode batches that the monitor dropped. The reasons are the same as for `dropped_total` of the Go publisher |
| `health_events_direct_publish_retries_total`            | Counter   | -                                  | Direct-mode send attempts that failed, and that the monitor retried                                       |
| `dcgm_connectivity_consecutive_observations`           | Gauge     | `result`                           | Consecutive DCGM connectivity observations by result                                                      |
| `dcgm_health_active_events`                             | Gauge     | `event_type`, `gpu_id`, `error_code`, `is_fatal`, `processing_strategy` | Total number of active GPU health events at any given time. `is_fatal` is `true` or `false`, the same as the `isFatal` that gpu-health-monitor sends (`recommendedAction` is not `NONE`). Use it to exclude informational events from alerts. The value comes from the gpu-health-monitor error-code mapping. `processing_strategy` is the processing strategy that gpu-health-monitor sends with the event, for example `EXECUTE_REMEDIATION` or `STORE_ONLY`. The global `processingStrategy` sets it, and a per-check setting can change it to `STORE_ONLY`, for example for `GpuThermalMarginWatch`, `GpuPowerBrakeWatch`, `GpuUptimeLimitWatch` and `GpuDcgmUnresponsive`. When the value is `STORE_ONLY`, no NVSentinel module acts on the event. When the value is `EXECUTE_REMEDIATION`, gpu-health-monitor requests action, but the platform connector can still change the strategy. For example, it sets `STORE_ONLY` for a node with the `nvsentinel.dgxc.nvidia.com/managed=false` label. Like `is_fatal`, this label shows what gpu-health-monitor sends, not the final decision. |
| `dcgm_health_active_switch_events`                      | Gauge     | `event_type`, `switch_id`, `error_code` | Total number of active NVSwitch health events at any given time. It is separate from `dcgm_health_active_events`, because switch IDs and GPU IDs are distinct sequences that both start at 0 |
| `dcgm_health_check_suppressed_incidents_total`          | Counter   | `error_code`                       | Number of DCGM health check incidents suppressed because of a non-actionable error code                   |
| `dcgm_health_check_debounced_incidents_total`           | Counter   | `error_code`, `gpu_id`             | Number of DCGM health check polls where the monitor withheld an error code for a GPU. The error code had not persisted for its configured minimum number of consecutive polls |
| `dcgm_api_latency`                                      | Histogram | `operation_name`                   | Amount of time spent calling DCGM APIs                                                                    |
| `dcgm_reconcile_time`                                   | Histogram | -                                  | Amount of time spent running a single DCGM reconcile loop                                                 |
| `number_of_health_watches`                              | Gauge     | -                                  | Number of DCGM health watches available                                                                   |
| `number_of_fields`                                      | Gauge     | -                                  | Number of `DCGM_FI_DEV*` constants exposed by the installed DCGM Python bindings                            |
| `callback_failures_total`                               | Counter   | `class_name`, `func_name`          | Number of times a callback function has thrown an exception                                               |
| `callback_success_total`                                | Counter   | `class_name`, `func_name`          | Number of times a callback function has successfully completed                                            |
| `dcgm_api_failures_total`                               | Counter   | `error_name`                       | Number of DCGM API errors                                                                                 |
| `dcgm_health_check_unknown_system_skipped_total`        | Counter   | -                                  | Number of DCGM health check incidents skipped because of an unrecognized system value                     |
| `dcgm_probe_hangs_total`                                | Counter   | `operation_name`                   | Number of DCGM probes that exceeded the watchdog deadline without returning                               |
| `gpu_temp_limit_margin_blank_total`                     | Counter   | -                                  | Number of poll cycles where DCGM field 153 (T.Limit) returned a blank value                                |
| `gpu_temp_limit_slowdown_threshold_missing_total`       | Counter   | -                                  | Number of poll cycles where slowdown T.Limit metadata from NVML field 194 is missing                      |
| `gpu_power_brake_reasons_blank_total`                   | Counter   | -                                  | Number of GPU polls where DCGM clocks-event-reasons was missing or blank, so the monitor cannot evaluate GpuPowerBrakeWatch for that poll |

The `number_of_fields` and `number_of_health_watches` gauges describe the binding inventory. They do not confirm active watches, hardware support, successful polling or valid samples. A field can appear in the inventory while its monitor is disabled.

---

### NIC Health Monitor

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `nic_health_monitor_devices_discovered` | Gauge | `node`, `check` | Number of NIC devices discovered in sysfs |
| `nic_health_monitor_health_events_sent_total` | Counter | `node`, `check`, `is_fatal` | Total number of health events sent to the platform connector |
| `nic_health_monitor_poll_cycle_duration_seconds` | Histogram | `node`, `category` | Duration of each poll cycle in seconds |
| `nic_health_monitor_poll_cycle_last_completed_timestamp_seconds` | Gauge | `node`, `category` | The monitor sets this Unix timestamp at startup and advances it after each completed poll cycle (`state` or `counter`). To measure how long the category has had no completed poll, use `time() - metric`. This includes a stall in the first poll. |
| `nic_health_monitor_state_check_errors_total` | Counter | `node`, `check`, `device`, `port` | Total number of state check error events |
| `nic_health_monitor_port_state_blips_total` | Counter | `node`, `check`, `device`, `port` | Ports that turned unhealthy and recovered before the port-state hold-down reported them |
| `nic_health_monitor_counter_threshold_breaches_total` | Counter | `node`, `counter`, `device`, `port`, `is_fatal` | Total number of counter threshold breaches detected |
| `nic_health_monitor_first_poll_deferred_total` | Counter | `node`, `check` | Polls deferred before the first evaluation because devices were unreadable |

---

### Syslog Health Monitor

The syslog health monitor tracks the GPU-related errors that it detects in system logs.

#### XID Error Metrics

XID (GPU Error ID) errors are NVIDIA GPU driver errors:

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_xid_errors` | Counter | `node`, `err_code` | Total number of XID errors found |
| `syslog_health_monitor_xid_processing_errors` | Counter | `error_type`, `node` | Total number of errors during XID processing |
| `syslog_health_monitor_xid_processing_latency_seconds` | Histogram | - | Histogram of XID processing latency |
| `syslog_health_monitor_cancellations_emitted_total` | Counter | `check`, `source_error_code`, `target_error_code` | Total number of synthetic cancellation health events that configured rules emitted |
| `syslog_health_monitor_xid_empty_driver_version_total` | Counter | `node` | Total number of XID decode requests sent with an empty driver version |

#### SXID Error Metrics

SXID errors are NVSwitch-related errors:

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_sxid_errors` | Counter | `node`, `err_code`, `link`, `nvswitch` | Total number of SXID errors found |

#### GPU Fallen Off Bus Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_gpu_fallen_errors` | Counter | `node` | Total number of GPU fallen off bus errors detected |

#### NIC Driver Error Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `syslog_health_monitor_nic_driver_errors` | Counter | `node`, `event`, `severity` | Total number of NIC driver or firmware errors detected in kernel logs |

---

### CSP Health Monitor

The CSP health monitor tracks cloud provider maintenance events and node health issues.

#### CSP Client Metrics

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `csp_health_monitor_csp_events_received_total` | Counter | `csp` | Total number of raw events received from the CSP API or source |
| `csp_health_monitor_csp_polling_duration_seconds` | Histogram | `csp` | Duration of CSP polling cycles |
| `csp_health_monitor_csp_api_errors_total` | Counter | `csp`, `error_type` | Total number of errors during CSP API calls |
| `csp_health_monitor_csp_api_polling_duration_seconds` | Histogram | `csp`, `api` | Duration of CSP API polling cycles |
| `csp_health_monitor_csp_monitor_errors_total` | Counter | `csp`, `error_type` | Total number of errors during the initialization or start of CSP monitors |
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
| `csp_health_monitor_trigger_events_found_total` | Counter | `trigger_type` | Total number of events found that possibly need a trigger |
| `csp_health_monitor_trigger_attempts_total` | Counter | `trigger_type` | Total number of trigger attempts made |
| `csp_health_monitor_trigger_success_total` | Counter | `trigger_type` | Total number of successful triggers |
| `csp_health_monitor_trigger_failures_total` | Counter | `trigger_type`, `failure_reason` | Total number of failed trigger attempts |
| `csp_health_monitor_trigger_datastore_query_duration_seconds` | Histogram | `query_type` | Duration of datastore queries performed by the trigger engine |
| `csp_health_monitor_trigger_datastore_query_errors_total` | Counter | `query_type` | Total number of errors during datastore queries |
| `csp_health_monitor_trigger_datastore_update_errors_total` | Counter | `trigger_type` | Total number of errors when the trigger engine updates the event status after a trigger |
| `csp_health_monitor_trigger_uds_send_duration_seconds` | Histogram | - | Duration of sending health events through UDS |
| `csp_health_monitor_trigger_uds_send_errors_total` | Counter | - | Total number of errors when sending health events through UDS |
| `csp_health_monitor_node_not_ready_timeout_total` | Counter | `node_name` | Total number of nodes that remained not ready after the timeout period |
| `csp_health_monitor_node_readiness_monitoring_started_total` | Counter | `node_name` | Total number of times the monitor started background node readiness monitoring |

---

### NVCRE Certification Monitor

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `nvcre_certification_monitor_sweep_errors_total` | Counter | `error_type` | Total number of errors during reconciliation sweeps over Certification CRs, by error type |
| `nvcre_certification_monitor_sweep_duration_seconds` | Histogram | - | Duration of a reconciliation sweep over Certification CRs |
| `nvcre_certification_monitor_health_events_published_total` | Counter | `node`, `is_healthy` | Total number of health events published to the platform connector, by node and health state |
| `nvcre_certification_monitor_health_event_publish_errors_total` | Counter | `node`, `is_healthy` | Total number of health events that the monitor failed to publish to the platform connector, by node and health state |
| `nvcre_certification_monitor_active_failures` | Gauge | - | Number of (node, variant, reason) certification failures asserted by terminal Certification CRs |
| `nvcre_certification_monitor_malformed_node_annotations` | Gauge | - | Number of nodes skipped in the last sweep because their cert-failures annotation is unparsable |

---

### Kubernetes Object Monitor

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `k8s_object_monitor_policy_matches_total` | Counter | `policy_name`, `node`, `resource_kind` | Total number of times a policy matched (predicate=true), which shows the detected issues by node |
| `k8s_object_monitor_policy_evaluation_errors_total` | Counter | `policy_name`, `error_type` | Policy evaluation errors |
| `k8s_object_monitor_health_events_publish_errors_total` | Counter | `policy_name`, `error_type` | Errors when publishing health events to the platform connector through gRPC |
| `k8s_object_monitor_reconciliation_errors_total` | Counter | `resource_kind`, `error_type` | Controller reconciliation errors |

---

### Slurm Drain Monitor

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `slurm_drain_monitor_external_drains_detected_total` | Counter | `pattern_name` | Total number of external Slurm drains detected by pattern |
| `slurm_drain_monitor_parse_matches_total` | Counter | `pattern_name` | Total number of parse matches by pattern name |
| `slurm_drain_monitor_reconciliation_errors_total` | Counter | `error_type` | Controller reconciliation errors |
| `slurm_drain_monitor_health_events_publish_errors_total` | Counter | `error_type` | Errors when publishing health events to the platform connector through gRPC |

---

## Change Stream Metrics

`store-client` emits these metrics, so they appear on every module that reads the change stream, on both the MongoDB and PostgreSQL providers. These modules are `event-exporter`, `fault-quarantine`, `health-events-analyzer`, `node-drainer`, and `fault-remediation`. The `client` label is the name of the consumer.

`datastore_connected` is the one exception in this table. Only `fault-quarantine`, `node-drainer` and `fault-remediation` export it, and it carries no `client` label.

| Metric Name | Type | Labels | Description |
|------------|------|--------|-------------|
| `change_stream_lag_seconds` | Gauge | `client` | Seconds since this consumer last had evidence that it was caught up with its own change stream. The evidence is an empty batch or the server-side timestamp of the last event that it read. The series is absent until the consumer observes one of these |
| `change_stream_lag_known` | Gauge | `client` | 1 when `change_stream_lag_seconds` is available for this consumer, 0 before then |
| `change_stream_resume_token_recoveries_total` | Counter | `client`, `phase` | Number of times the watcher deleted a stale or invalid resume token and restarted the stream from the current position |
| `change_stream_reopens_total` | Counter | `client`, `result` | Attempts to reopen the stream after a runtime failure. `success`: a disconnect that the watcher recovered from when it reopened the stream. `failure`: one attempt that did not succeed. `exhausted`: the recovery budget ran out. The watcher then stops, and the process is expected to exit so that the pod restarts. Use this value together with `kube_pod_container_status_restarts_total` |
| `datastore_connected` | Gauge | none | PostgreSQL watcher readiness. It is `1` while LISTEN is connected and subscribed, or after polling succeeds, unless a database read has failed. After a failed read, it stays `0` until another read succeeds. An active subscription can report ready before its first poll finishes, and while a multi-batch catch-up still drains. Readiness does not mean that the catch-up is complete. It also does not guarantee the replay of rows whose notifications the watcher missed before the subscription. The watcher reads poll batches in `(changed_at, id)` order. The cursor advances only after channel delivery or filter consumption. PostgreSQL assigns `BIGSERIAL` IDs and `changed_at DEFAULT NOW()` before a transaction commits. So a long-running transaction can become visible after a poll has advanced past its sort position. A later catch-up cannot find that row from the forward cursor. If the watcher missed its NOTIFY while it was disconnected, the watcher can skip the row. If LISTEN is active, its NOTIFY can fetch the row by ID and deliver it after newer rows. So the watcher does not guarantee the global commit order. A TRIPPED fault quarantine breaker halts event dequeuing independently. So this gauge can stay `1` while fault quarantine is deliberately stopped. Other watchers stay ready after they are registered, unless they expose a readiness state. This metric does not measure processing progress. `kube_pod_status_ready` reports the readiness result with pod labels |

Each consumer measures lag against its **own filtered stream**. So a module whose pipeline admits nothing still reports zero lag while it is caught up.

### Suggested alerts

```yaml
# A consumer is behind. Tune the threshold per fleet; no lag_known qualifier is needed,
# because the series is absent rather than zero while lag is unknown.
- alert: ChangeStreamConsumerBehind
  expr: change_stream_lag_seconds > 900
  for: 10m

# A watcher that never started, or is wedged before its first read. The grace period covers
# normal startup, which closes in milliseconds on a live stream.
- alert: ChangeStreamLagUnknown
  expr: change_stream_lag_known == 0
  for: 10m
```

### What zero lag does not tell you

In each of these cases, a consumer can be behind while the gauge reads zero.

1. **Replication lag.** The watcher opens the MongoDB stream with `SecondaryPreferred` and no max staleness. So an empty batch means "caught up with the secondary I am reading", not with the primary.
2. **Data skipped after a resume-token recovery.** When a stored token is too old for the oplog, the watcher reopens the stream from the current position. So the lag reads near zero exactly when the watcher skipped the most events. Read `change_stream_lag_seconds` together with `change_stream_resume_token_recoveries_total`. A lag of zero is reassuring only if the recoveries counter has not moved.
3. **Durable position.** This gauge measures the progress of the watcher against its own stream. It does not measure whether the watcher persisted its position. A consumer that reads an event and crashes before it records the event as processed (`MarkProcessed`) looks healthy here, because the read did occur.

---

## Metrics Configuration

### Scraping Metrics

All NVSentinel components expose Prometheus metrics on a metrics endpoint (typically `:2112/metrics`). Prometheus can scrape the metrics with standard scrape configurations.

### Helm Chart Configuration

The NVSentinel Helm chart automatically creates a `PodMonitor` resource for Prometheus Operator integration:

```bash
helm install nvsentinel ./distros/kubernetes/nvsentinel \
  --namespace nvsentinel --create-namespace
```

Prometheus uses the PodMonitor to scrape all NVSentinel component pods on their metrics endpoints (`/metrics` on port `metrics`).

### Annotation-based Discovery

You can configure components to include Prometheus scrape annotations:

```yaml
annotations:
  prometheus.io/scrape: "true"
  prometheus.io/port: "2112"
  prometheus.io/path: "/metrics"
```

---

## Metric Types Reference

- **Counter**: A cumulative metric that only increases or resets to zero on restart
- **Gauge**: A metric that can arbitrarily increase and decrease
- **Histogram**: Samples observations and counts them in configurable buckets
- **Summary**: Similar to a histogram, but calculates configurable quantiles over a sliding time window

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
