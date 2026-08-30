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

package reconciler

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	metricLabelNodeName = "node_name"
	metricLabelRuleName = "rule_name"
)

var (
	totalEventsReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
		[]string{metricLabelNodeName},
	)
	totalEventsSuccessfullyProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_events_successfully_processed_total",
			Help: "Total number of events successfully processed.",
		},
	)
	totalEventProcessingError = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_event_processing_errors",
			Help: "Total number of errors encountered during event processing.",
		},
		[]string{"error_type"},
	)

	fatalEventsPublishedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fatal_events_published_total",
			Help: "Total number of times a fatal event is published for an entity.",
		},
		[]string{"entity_value"},
	)

	ruleMatchedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "rule_matched_total",
			Help: "Total number of times a rule matched for a node",
		},
		[]string{metricLabelRuleName, metricLabelNodeName},
	)

	recoveryEventsPublishedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_recovery_events_published_total",
			Help: "Total number of derived healthy transitions published by recovery-enabled rules.",
		},
		[]string{metricLabelRuleName, "scope"},
	)
	recoveryPersistenceTimeoutsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_recovery_persistence_timeouts_total",
			Help: "Total derived transitions not observed in the store before the persistence deadline.",
		},
		[]string{metricLabelRuleName, "state"},
	)

	mongoQueryExecutionDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mongo_query_execution_duration_seconds",
			Help:    "Histogram of MongoDB pipeline execution durations.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{metricLabelRuleName},
	)

	// performance metrics
	eventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "health_event_analyzer_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 12),
		},
	)
)
