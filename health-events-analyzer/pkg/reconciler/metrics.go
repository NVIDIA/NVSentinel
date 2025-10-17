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
	totalEventsReceived = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_event_analyzer_events_received_total",
			Help: "Total number of events received from the watcher.",
		},
		[]string{"entity_value"},
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
			Help: "Total number of new fatal events published.",
		},
		[]string{"entity_value"},
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
