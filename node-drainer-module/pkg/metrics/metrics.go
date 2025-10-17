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

	// TotalEventsSuccessfullyProcessed tracks successfully processed events
	TotalEventsSuccessfullyProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_events_successfully_processed_total",
			Help: "Total number of events successfully processed.",
		},
	)

	// TotalEventProcessingError tracks errors during event processing
	TotalEventProcessingError = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_processing_errors_total",
			Help: "Total number of errors encountered during event processing.",
		},
		[]string{"error_type"},
	)

	// Health event metrics

	// HealthyEvent tracks healthy events by node and check name
	HealthyEvent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_healthy_event_total",
			Help: "Total number of healthy events.",
		}, []string{"node", "check_name"},
	)

	// HealthyEventWithContextCancellation tracks healthy events that canceled node draining
	HealthyEventWithContextCancellation = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_healthy_event_with_node_drain_cancel_total",
			Help: "Total number of healthy events that led to the cancellation of node draining",
		},
	)

	// UnhealthyEvent tracks unhealthy events by node and check name
	UnhealthyEvent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_unhealthy_event_total",
			Help: "Total number of unhealthy events.",
		}, []string{"node", "check_name"},
	)

	// Node draining metrics

	// NodeDrainSuccess tracks successful node drainings
	NodeDrainSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_node_drain_successful_total",
			Help: "Total number of successful node drainings.",
		}, []string{"node"},
	)

	// NodeDrainError tracks errors while draining nodes
	NodeDrainError = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_node_drain_errors_total",
			Help: "Total number of errors encountered while draining a node.",
		},
		[]string{"error_type", "node"},
	)

	// NodeDrainTimeout tracks node drainer operations in deleteAfterTimeout mode
	NodeDrainTimeout = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_drainer_waiting_for_timeout",
			Help: "Total number of node drainer operations in deleteAfterTimeout mode.",
		},
		[]string{"node"},
	)

	// NodeDrainTimeoutReached tracks operations that reached timeout and force deleted pods
	NodeDrainTimeoutReached = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_force_delete_pods_after_timeout",
			Help: "Total number of node drainer operations in deleteAfterTimeout mode" +
				"that reached the timeout and force deleted the pods.",
		},
		[]string{"node", "namespace"},
	)

	// EventHandlingDuration tracks event handling durations
	EventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "node_drainer_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// NodeDrainStatus tracks which nodes are currently being drained (1 = draining, 0 = not draining)
	NodeDrainStatus = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_drainer_node_drain_status",
			Help: "Shows if a node is currently being drained (1) or not (0).",
		},
		[]string{"node"},
	)

	// NodeQueueDepth tracks the number of pending events per node
	NodeQueueDepth = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "node_drainer_queue_depth",
			Help: "Number of pending events in the queue for each node.",
		},
		[]string{"node"},
	)
)
