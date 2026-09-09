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

// Drain Status constants for event processing metrics
const (
	// labelNode is the node-name label shared by the metrics below.
	labelNode = "node"

	DrainStatusDrained   = "drained"
	DrainStatusCancelled = "cancelled"
	DrainStatusSkipped   = "skipped"
)

var (
	// Event processing metrics

	// TotalEventsReceived tracks total number of events received from the watcher
	TotalEventsReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
	)

	// TotalEventsReplayed tracks events replayed at startup
	TotalEventsReplayed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_events_replayed_total",
			Help: "Total number of in-progress events replayed at startup.",
		},
	)

	// EventsProcessed tracks events processed by drain outcome and scope
	EventsProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_events_processed_total",
			Help: "Total number of events processed by drain status outcome and drain scope.",
		},
		[]string{"drain_status", labelNode, "drain_scope"},
	)

	// PartialDrains counts partial drains by the entity they targeted. Registered only when
	// partialDrainEntityMetricEnabled is set, because entity_value carries a GPU UUID and some
	// operators will not accept UUIDs as label values. node_drainer_events_processed_total
	// already reports partial against full without it.
	PartialDrains *prometheus.CounterVec

	// ProcessingErrors tracks all errors (event processing and node draining)
	ProcessingErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_processing_errors_total",
			Help: "Total number of errors encountered during event processing and node draining.",
		},
		[]string{"error_type", labelNode},
	)

	// CancelledEvent tracks cancelled drain events
	CancelledEvent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_cancelled_event_total",
			Help: "Total number of cancelled drain events (due to manual uncordon or healthy recovery).",
		}, []string{labelNode, "check_name"},
	)

	// Node draining metrics

	// NodeDrainTimeout tracks node drainer operations in deleteAfterTimeout mode
	NodeDrainTimeout = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_drainer_waiting_for_timeout",
			Help: "Total number of node drainer operations in deleteAfterTimeout mode.",
		},
		[]string{labelNode},
	)

	// NodeDrainTimeoutReached tracks operations that reached timeout and force deleted pods
	NodeDrainTimeoutReached = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_force_delete_pods_after_timeout",
			Help: "Total number of node drainer operations in deleteAfterTimeout mode" +
				"that reached the timeout and force deleted the pods.",
		},
		[]string{labelNode, "namespace"},
	)

	// EventHandlingDuration tracks event handling durations
	EventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "node_drainer_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// PodEvictionDuration tracks time to successfully evict all pods from a node.
	// Exponential buckets from 0.1s to ~3 days (0.1 * 2^22 = 259200s).
	PodEvictionDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "node_drainer_pod_eviction_duration_seconds",
			Help:    "Time from event receipt by node-drainer to successful pod eviction completion.",
			Buckets: prometheus.ExponentialBuckets(0.1, 2, 23),
		},
	)

	// QueueDepth tracks the total number of pending events in the queue
	QueueDepth = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "node_drainer_queue_depth",
			Help: "Total number of pending events in the queue.",
		},
	)

	// QueueItemsAssigned tracks priority decisions made when events enter the ready queue
	QueueItemsAssigned = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_queue_items_assigned_total",
			Help: "Total number of ready queue items assigned by priority and reason.",
		},
		[]string{"priority", "reason"},
	)

	// CustomDrainCRDNotFound tracks failures when custom drain CRD is not found
	CustomDrainCRDNotFound = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_custom_drain_crd_not_found_total",
			Help: "Total number of custom drain operations that failed because the CRD was not found in the cluster.",
		},
		[]string{labelNode},
	)
)

// EnablePartialDrainEntityMetric registers node_drainer_partial_drains_total. Call once at
// startup, before workers run, when the operator has opted in. Repeat calls are ignored so
// registering twice cannot panic.
func EnablePartialDrainEntityMetric() {
	if PartialDrains != nil {
		return
	}

	PartialDrains = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_partial_drains_total",
			Help: "Total number of partial drains by the entity they targeted.",
		},
		[]string{labelNode, "entity_type", "entity_value"},
	)
}

// RecordPartialDrain records a partial drain against the entity it targeted. It is a no-op
// unless EnablePartialDrainEntityMetric has been called.
func RecordPartialDrain(node, entityType, entityValue string) {
	if PartialDrains == nil {
		return
	}

	PartialDrains.WithLabelValues(node, entityType, entityValue).Inc()
}
