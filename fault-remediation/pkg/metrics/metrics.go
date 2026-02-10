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
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// CR Status constants for event processing metrics
const (
	CRStatusCreated = "created"
	CRStatusSkipped = "skipped"
)

var (
	//TODO: evaluate and remove redundant metrics with ctrl-runtime defaults

	// Event Processing Metrics
	TotalEventsReceived = promauto.With(crmetrics.Registry).NewCounter(
		prometheus.CounterOpts{
			Name: "fault_remediation_events_received_total",
			Help: "Total number of events received from the Watcher.",
		},
	)
	EventsProcessed = promauto.With(crmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_remediation_events_processed_total",
			Help: "Total number of remediation events processed by CR creation status.",
		},
		[]string{"cr_status", "node_name"},
	)
	ProcessingErrors = promauto.With(crmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_remediation_processing_errors_total",
			Help: "Total number of errors encountered during event processing.",
		},
		[]string{"error_type", "node_name"},
	)
	TotalUnsupportedRemediationActions = promauto.With(crmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_remediation_unsupported_actions_total",
			Help: "Total number of health events with currently unsupported remediation actions.",
		},
		[]string{"action", "node_name"},
	)

	// Performance Metrics
	EventHandlingDuration = promauto.With(crmetrics.Registry).NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_remediation_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// CR Generation Duration Metrics
	CRGenerationDuration = promauto.With(crmetrics.Registry).NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_remediation_cr_generate_duration_seconds",
			Help:    "Time from drain completion to maintenance CR creation.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// Log Collection Job Metrics
	LogCollectorJobs = promauto.With(crmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_remediation_log_collector_jobs_total",
			Help: "Total number of log collector jobs.",
		},
		[]string{"node_name", "status"},
	)
	LogCollectorJobDuration = promauto.With(crmetrics.Registry).NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fault_remediation_log_collector_job_duration_seconds",
			Help:    "Duration of log collector jobs in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"node_name", "status"},
	)
	LogCollectorErrors = promauto.With(crmetrics.Registry).NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_remediation_log_collector_errors_total",
			Help: "Total number of errors encountered in log collector operations.",
		},
		[]string{"error_type", "node_name"},
	)
)
