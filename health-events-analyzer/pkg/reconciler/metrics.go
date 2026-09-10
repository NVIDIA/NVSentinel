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
	labelRuleName               = "rule_name"
	labelNodeName               = "node_name"
	labelEntityType             = "entity_type"
	labelEntityValue            = "entity_value"
	metricNameRuleMatchedEntity = "rule_matched_entity_total"
)

var (
	totalEventsReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
		[]string{"node_name"},
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
		[]string{"rule_name", "node_name"},
	)

	// ruleMatchedEntityTotal counts matches by the entity the rule keyed on.
	// Registered only when ruleMatchedEntityMetricEnabled is set, because
	// entity labels raise cardinality (GPU × GPC × TPC per node).
	// rule_matched_total already reports that a rule fired without it.
	ruleMatchedEntityTotal *prometheus.CounterVec

	mongoQueryExecutionDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mongo_query_execution_duration_seconds",
			Help:    "Histogram of MongoDB pipeline execution durations.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"rule_name"},
	)

	// performance metrics
	eventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "health_event_analyzer_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)
)

// EnableRuleMatchedEntityMetric registers rule_matched_entity_total. Call once at
// startup, before the reconciler runs, when the operator has opted in. Repeat
// calls are ignored so registering twice cannot panic.
func EnableRuleMatchedEntityMetric() {
	if ruleMatchedEntityTotal != nil {
		return
	}

	ruleMatchedEntityTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: metricNameRuleMatchedEntity,
			Help: "Total number of times a rule matched, labeled by the entity it selected on.",
		},
		[]string{labelRuleName, labelNodeName, labelEntityType, labelEntityValue},
	)
}

// recordRuleMatchedEntity records a match against the entity the rule selected
// on. It is a no-op unless EnableRuleMatchedEntityMetric has been called.
func recordRuleMatchedEntity(ruleName, nodeName, entityType, entityValue string) {
	if ruleMatchedEntityTotal == nil {
		return
	}

	ruleMatchedEntityTotal.WithLabelValues(ruleName, nodeName, entityType, entityValue).Inc()
}
