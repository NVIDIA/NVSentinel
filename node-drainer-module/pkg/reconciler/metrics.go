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
	// event processing metrics
	totalEventsReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
	)
	totalEventsSuccessfullyProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_events_successfully_processed_total",
			Help: "Total number of events successfully processed.",
		},
	)
	totalEventProcessingError = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_processing_errors_total",
			Help: "Total number of errors encountered during event processing.",
		},
		[]string{"error_type"},
	)

	// health event metrics
	healthyEvent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_healthy_event_total",
			Help: "Total number of healthy events.",
		}, []string{"node", "check_name"},
	)
	healthyEventWithContextCancellation = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "node_drainer_healthy_event_with_node_drain_cancel_total",
			Help: "Total number of healthy events that led to the cancellation of node draining",
		},
	)

	unhealthyEvent = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_unhealthy_event_total",
			Help: "Total number of unhealthy events.",
		}, []string{"node", "check_name"},
	)

	// node draining metrics
	nodeDrainSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_node_drain_successful_total",
			Help: "Total number of successful node drainings.",
		}, []string{"node"},
	)

	nodeDrainError = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "node_drainer_node_drain_errors_total",
			Help: "Total number of errors encountered while draining a node.",
		},
		[]string{"error_type", "node"},
	)

	// performance metrics
	eventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "node_drainer_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)
)
