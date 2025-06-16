// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// --- Main Monitor Metrics ---

var (
	// CSP Client Metrics
	CSPEventsReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_csp_events_received_total",
			Help: "Total number of raw events received from CSP API/source.",
		},
		[]string{"csp"}, // gcp, aws
	)
	CSPPollingDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "csp_health_monitor_csp_polling_duration_seconds",
			Help:    "Duration of CSP polling cycles.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"csp"}, // gcp, aws
	)
	CSPAPIErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_csp_api_errors_total",
			Help: "Total number of errors encountered during CSP API calls.",
		},
		[]string{"csp", "error_type"}, // gcp/aws, connection/parse/rate_limit etc.
	)
	CSPAPIDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "csp_health_monitor_csp_api_polling_duration_seconds",
			Help:    "Duration of CSP API polling cycles.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"csp", "api"}, // gcp, aws, describe_events, describe_affected_entities, describe_event_details etc
	)
	CSPMonitorErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_csp_monitor_errors_total",
			Help: "Total number of errors initializing or starting CSP monitors.",
		},
		[]string{"csp", "error_type"}, // gcp/aws, init_error/start_error
	)

	CSPEventsByTypeUnsupported = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_csp_events_by_type_unsupported_total",
			Help: "Total number of raw CSP events received, partitioned by event type code.",
		},
		[]string{
			"csp",        // gcp, aws
			"event_type", // AWS_EC2_PERSISTENT_INSTANCE_RETIREMENT_SCHEDULED etc.
		},
	)

	// Normalization Metrics
	MainEventsToNormalize = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_events_to_normalize_total",
			Help: "Total number of events passed to the normalizer.",
		},
		[]string{"csp"}, // gcp, aws
	)
	MainNormalizationErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_normalization_errors_total",
			Help: "Total number of errors during event normalization.",
		},
		[]string{"csp"}, // gcp, aws
	)

	// Event Processor Metrics
	MainEventsReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_events_received_total",
			Help: "Total number of normalized events received by the main processor.",
		},
		[]string{"csp"}, // gcp, aws
	)
	MainEventsProcessedSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_events_processed_success_total",
			Help: "Total number of events successfully processed by the main processor (mapped & stored).",
		},
		[]string{"csp"}, // gcp, aws
	)
	MainProcessingErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_processing_errors_total",
			Help: "Total number of errors during event processing (mapping, storing).",
		},
		[]string{"csp", "error_type"}, // gcp/aws, mapping/datastore_upsert
	)
	MainEventProcessingDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "csp_health_monitor_main_event_processing_duration_seconds",
			Help:    "Duration of processing a single event (mapping + storing).",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"csp"}, // gcp, aws
	)

	// Datastore Metrics (Main Monitor - primarily Upserts)
	MainDatastoreUpsertAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_datastore_upsert_attempts_total",
			Help: "Total number of attempts to upsert maintenance events.",
		},
		[]string{"csp"}, // gcp, aws
	)
	MainDatastoreUpsertSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_datastore_upsert_success_total",
			Help: "Total number of successful maintenance event upserts.",
		},
		[]string{"csp"}, // gcp, aws
	)
	MainDatastoreUpsertErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_main_datastore_upsert_errors_total",
			Help: "Total number of errors during maintenance event upserts.",
		},
		[]string{"csp"}, // gcp, aws
	)
)

// --- Quarantine Trigger Engine (Sidecar) Metrics ---

var (
	TriggerPollCycles = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_poll_cycles_total",
			Help: "Total number of polling cycles executed by the trigger engine.",
		},
	)
	TriggerPollErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_poll_errors_total",
			Help: "Total number of errors during a trigger engine poll cycle (e.g., DB query failed).",
		},
	)

	// Datastore Metrics (Trigger Engine - Queries and Updates)
	TriggerDatastoreQueryDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "csp_health_monitor_trigger_datastore_query_duration_seconds",
			Help:    "Duration of datastore queries performed by the trigger engine.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"query_type"}, // quarantine, healthy, last_timestamp_gcp, last_timestamp_aws
	)
	TriggerDatastoreQueryErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_datastore_query_errors_total",
			Help: "Total number of errors during datastore queries by the trigger engine.",
		},
		[]string{"query_type"}, // quarantine, healthy
	)
	TriggerDatastoreUpdateErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_datastore_update_errors_total",
			Help: "Total number of errors updating event status after trigger.",
		},
		[]string{"trigger_type"}, // quarantine, healthy
	)

	// Triggering Metrics
	TriggerEventsFound = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_events_found_total",
			Help: "Total number of events found potentially needing a trigger.",
		},
		[]string{"trigger_type"}, // quarantine, healthy
	)
	TriggerAttempts = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_attempts_total",
			Help: "Total number of trigger attempts made (sending event via UDS).",
		},
		[]string{"trigger_type"}, // quarantine, healthy
	)
	TriggerSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_success_total",
			Help: "Total number of successful triggers (UDS send OK, DB status updated).",
		},
		[]string{"trigger_type"}, // quarantine, healthy
	)
	TriggerFailures = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_failures_total",
			Help: "Total number of failed trigger attempts (mapping error, UDS send error, DB update error).",
		},
		[]string{"trigger_type", "failure_reason"}, // quarantine/healthy, mapping/uds/db_update
	)

	// UDS Metrics
	TriggerUDSSendDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "csp_health_monitor_trigger_uds_send_duration_seconds",
			Help:    "Duration of sending health events via UDS.",
			Buckets: prometheus.DefBuckets, // Consider smaller buckets if UDS is fast
		},
	)
	TriggerUDSSendErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_trigger_uds_send_errors_total",
			Help: "Total number of errors encountered when sending events via UDS.",
		},
	)

	// Node Readiness Metrics
	NodeNotReadyTimeout = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_node_not_ready_timeout_total",
			Help: "Total number of nodes that remained not ready after the timeout period.",
		},
		[]string{"node_name"}, // Track which specific nodes are having issues
	)
	NodeReadinessMonitoringStarted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "csp_health_monitor_node_readiness_monitoring_started_total",
			Help: "Total number of times background node readiness monitoring was started.",
		},
		[]string{"node_name"}, // Track which nodes are being monitored
	)
)
