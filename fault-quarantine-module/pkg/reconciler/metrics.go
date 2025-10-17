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

package reconciler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Event Processing Metrics
	totalEventsReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
	)
	totalEventsSuccessfullyProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_events_successfully_processed_total",
			Help: "Total number of events successfully processed.",
		},
	)
	totalEventsSkipped = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_events_skipped_total",
			Help: "Total number of events received on already cordoned node",
		},
	)
	processingErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_processing_errors_total",
			Help: "Total number of errors encountered during event processing.",
		},
		[]string{"error_type"},
	)

	// Node Quarantine Metrics
	totalNodesQuarantined = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_nodes_quarantined_total",
			Help: "Total number of nodes quarantined.",
		},
		[]string{"node"},
	)
	totalNodesUnquarantined = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_nodes_unquarantined_total",
			Help: "Total number of nodes unquarantined.",
		},
		[]string{"node"},
	)
	currentQuarantinedNodes = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_current_quarantined_nodes",
			Help: "Current number of quarantined nodes.",
		},
		[]string{"node"},
	)

	// Taint and Cordon Metrics
	taintsApplied = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_taints_applied_total",
			Help: "Total number of taints applied to nodes.",
		},
		[]string{"taint_key", "taint_effect"},
	)
	taintsRemoved = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_taints_removed_total",
			Help: "Total number of taints removed from nodes.",
		},
		[]string{"taint_key", "taint_effect"},
	)
	cordonsApplied = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_cordons_applied_total",
			Help: "Total number of cordons applied to nodes.",
		},
	)
	cordonsRemoved = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "fault_quarantine_cordons_removed_total",
			Help: "Total number of cordons removed from nodes.",
		},
	)

	// Ruleset Evaluation Metrics
	rulesetEvaluations = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_ruleset_evaluations_total",
			Help: "Total number of ruleset evaluations.",
		},
		[]string{"ruleset"},
	)
	rulesetPassed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_ruleset_passed_total",
			Help: "Total number of ruleset evaluations that passed.",
		},
		[]string{"ruleset"},
	)
	rulesetFailed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_ruleset_failed_total",
			Help: "Total number of ruleset evaluations that failed.",
		},
		[]string{"ruleset"},
	)

	// Performance Metrics
	eventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
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
)
