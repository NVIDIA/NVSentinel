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
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	dto "github.com/prometheus/client_model/go"
)

// Drain Status constants for event processing metrics
const (
	// labelNode is the node-name label shared by the metrics below.
	labelNode = "node"

	DrainStatusDrained   = "drained"
	DrainStatusCancelled = "cancelled"
	DrainStatusSkipped   = "skipped"

	// ProcessingErrors error_type values for a recorded drain method that cannot be carried out.
	// The method stays as recorded. node-drainer waits and tries again.
	ErrorTypeRecordedCustomDrainUnavailable = "recorded_custom_drain_unavailable"
	ErrorTypeRecordedBuiltInNoPodPolicies   = "recorded_builtin_no_pod_policies"
	ErrorTypeRecordedDrainMethodUnknown     = "recorded_drain_method_unknown"
	ErrorTypeRecordDrainDecision            = "record_drain_decision_error"
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

	// NodeDrainAwaitingCompletionEventTimestamp carries the creation time of the health event
	// behind an in-progress allowCompletion drain, for nodes still waiting on pods.
	// allowCompletion is the only mode with no deadline, so this is what makes "waiting too long"
	// expressible: time() - this > <the longest run those namespaces should take>.
	//
	// A timestamp rather than a 0/1 flag on purpose. The value is re-asserted from the event on
	// every poll, so if the series is removed while another event for the same node is still
	// waiting, the next poll restores the same instant instead of restarting the clock -- a 0/1
	// flag with a `for:` clause would have its timer reset by that gap. The series is removed,
	// not zeroed, when the node stops draining: time() minus zero would read as a very long wait.
	//
	// The value is the event's creation time, not the moment the drain began: it is the same
	// instant the DeleteAfterTimeout deadline measures from, and it can predate the drain itself
	// (a fault quarantined, halted, then re-quarantined hours later reads as hours of waiting on
	// the first poll).
	NodeDrainAwaitingCompletionEventTimestamp = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_drainer_awaiting_completion_event_timestamp_seconds",
			Help: "Unix time of the health event behind an allowCompletion drain that is still " +
				"waiting for pods to finish -- the event's creation time, which can predate the " +
				"drain itself. Absent when nothing is waiting on that node. allowCompletion has no " +
				"timeout, so alert on time() minus this value.",
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

// RecordNodeDrainAwaitingCompletionEventTimestamp publishes the creation time of a health event
// that is still waiting on pods. Several events can be queued for one node while it drains, and
// each one's poll re-asserts its own event's time, so a newer event's polls would otherwise
// overwrite an older wait and hide it from time()-based alerts. Keep the earliest time on the
// series; it is dropped together with the series when the node stops draining. No-op for an
// event that carries no creation time: time.Time's zero value is year 1, and publishing it
// would read as roughly two millennia of waiting and fire every threshold immediately.
func RecordNodeDrainAwaitingCompletionEventTimestamp(nodeName string, createdAt time.Time) {
	if createdAt.IsZero() {
		return
	}

	gauge := NodeDrainAwaitingCompletionEventTimestamp.WithLabelValues(nodeName)

	var metric dto.Metric
	if err := gauge.Write(&metric); err == nil {
		if current := metric.GetGauge().GetValue(); current != 0 && current < float64(createdAt.Unix()) {
			return
		}
	}

	gauge.Set(float64(createdAt.Unix()))
}
