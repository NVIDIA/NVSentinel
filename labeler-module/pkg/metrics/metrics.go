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

// Status constants for metrics
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
)

var (
	// EventsProcessed tracks the total number of pod events processed
	EventsProcessed = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "labeler_events_processed_total",
			Help: "Total number of pod events processed.",
		},
		[]string{"status"},
	)

	// NodeUpdateFailures tracks the total number of node update failures during reconciliation
	NodeUpdateFailures = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "labeler_node_update_failures_total",
			Help: "Total number of node update failures during reconciliation.",
		},
	)

	// EventHandlingDuration tracks the histogram of event handling durations
	EventHandlingDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "labeler_event_handling_duration_seconds",
			Help:    "Histogram of event handling durations.",
			Buckets: prometheus.DefBuckets,
		},
	)

	// NodeUpdateDuration tracks the histogram of node update operation durations
	NodeUpdateDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "labeler_node_update_duration_seconds",
			Help:    "Histogram of node update operation durations.",
			Buckets: prometheus.DefBuckets,
		},
	)
)
