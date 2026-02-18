// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

// Status constants for metrics
const (
	StatusPassed = "passed"
	StatusFailed = "failed"
)

var (
	// Event Processing Metrics
	TotalEventsReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
	)
	TotalEventsSuccessfullyProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_events_successfully_processed_total",
			Help: "Total number of events successfully processed.",
		},
	)
	ProcessingErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_processing_errors_total",
			Help: "Total number of errors encountered during event processing.",
		},
		[]string{"error_type"},
	)

	// Node Quarantine Metrics
	TotalNodesQuarantined = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_nodes_quarantined_total",
			Help: "Total number of nodes quarantined.",
		},
		[]string{"node"},
	)
	TotalNodesUnquarantined = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_nodes_unquarantined_total",
			Help: "Total number of nodes unquarantined.",
		},
		[]string{"node"},
	)
	TotalNodesManuallyUncordoned = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_nodes_manually_uncordoned_total",
			Help: "Total number of nodes manually uncordoned.",
		},
		[]string{"node"},
	)

	TotalNodesManuallyUntainted = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_nodes_manually_untainted_total",
			Help: "Total number of nodes manually untainted",
		},
		[]string{"node"},
	)
	CurrentQuarantinedNodes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_current_quarantined_nodes",
			Help: "Nodes which are currently quarantined and undergoing breakfix",
		},
		[]string{"node"},
	)

	// Taint and Cordon Metrics
	TaintsApplied = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_taints_applied_total",
			Help: "Total number of taints applied to nodes.",
		},
		[]string{"taint_key", "taint_effect"},
	)
	TaintsRemoved = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_taints_removed_total",
			Help: "Total number of taints removed from nodes.",
		},
		[]string{"taint_key", "taint_effect"},
	)
	CordonsApplied = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_cordons_applied_total",
			Help: "Total number of cordons applied to nodes.",
		},
	)
	CordonsRemoved = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_cordons_removed_total",
			Help: "Total number of cordons removed from nodes.",
		},
	)

	// Ruleset Evaluation Metrics
	RulesetEvaluations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_ruleset_evaluations_total",
			Help: "Total number of ruleset evaluations.",
		},
		[]string{"ruleset", "status"},
	)

	// Performance Metrics
	EventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// Node Quarantine Duration Metrics
	NodeQuarantineDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_node_quarantine_duration_seconds",
			Help:    "Time from health event generation to node quarantine completion.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// Event Processing Metrics
	EventBacklogSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_event_backlog_count",
			Help: "Number of health events which fault quarantine is yet to process.",
		},
	)

	// Circuit Breaker Metrics
	FaultQuarantineBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_breaker_state",
			Help: "State of the fault quarantine breaker.",
		},
		[]string{"state"},
	)
	FaultQuarantineBreakerUtilization = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_breaker_utilization",
			Help: "Utilization of the fault quarantine breaker.",
		},
	)
	FaultQuarantineGetTotalNodesDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_get_total_nodes_duration_seconds",
			Help:    "Duration of getTotalNodesWithRetry calls in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	FaultQuarantineGetTotalNodesErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_get_total_nodes_errors_total",
			Help: "Total number of errors from getTotalNodesWithRetry.",
		},
		[]string{"error_type"},
	)
	FaultQuarantineGetTotalNodesRetryAttempts = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_get_total_nodes_retry_attempts",
			Help:    "Number of retry attempts needed for getTotalNodesWithRetry.",
			Buckets: []float64{0, 1, 2, 3, 5, 10},
		},
	)
)

func SetFaultQuarantineBreakerUtilization(utilization float64) {
	FaultQuarantineBreakerUtilization.Set(utilization)
}

func SetFaultQuarantineBreakerState(state string) {
	FaultQuarantineBreakerState.Reset()
	FaultQuarantineBreakerState.WithLabelValues(state).Set(1)
}
