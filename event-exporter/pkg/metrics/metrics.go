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

const (
	StatusSuccess = "success"
	StatusFailure = "failure"
)

var (
	EventsReceived = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "health_events_exporter_events_received_total",
			Help: "Total number of health events received from the change stream.",
		},
	)

	EventsPublished = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_events_exporter_events_published_total",
			Help: "Total number of events published to the sink.",
		},
		[]string{"status"},
	)

	PublishDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "health_events_exporter_publish_duration_seconds",
			Help:    "Histogram of event publish latency in seconds.",
			Buckets: []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1.0, 2.5, 5.0},
		},
	)

	TransformErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "health_events_exporter_transform_errors_total",
			Help: "Total number of errors transforming events to CloudEvents format.",
		},
	)

	PublishErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "health_events_exporter_publish_errors_total",
			Help: "Total number of errors publishing events to the sink.",
		},
		[]string{"error_type"},
	)

	TokenRefreshErrors = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "health_events_exporter_token_refresh_errors_total",
			Help: "Total number of OIDC token refresh errors.",
		},
	)

	EventBacklogSize = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "health_events_exporter_event_backlog_size",
			Help: "Number of unprocessed events in the backlog.",
		},
	)

	ResumeTokenUpdateTimestamp = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "health_events_exporter_resume_token_update_timestamp",
			Help: "Timestamp of the last resume token update (Unix seconds).",
		},
	)

	BackfillInProgress = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "health_events_exporter_backfill_in_progress",
			Help: "Gauge: 1 during backfill, 0 otherwise.",
		},
	)

	BackfillEventsProcessed = promauto.NewCounter(
		prometheus.CounterOpts{
			Name: "health_events_exporter_backfill_events_processed_total",
			Help: "Total number of events processed during backfill.",
		},
	)

	BackfillDuration = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "health_events_exporter_backfill_duration_seconds",
			Help:    "Histogram of backfill operation duration in seconds.",
			Buckets: []float64{1, 5, 10, 30, 60, 300, 600, 1800, 3600},
		},
	)
)
