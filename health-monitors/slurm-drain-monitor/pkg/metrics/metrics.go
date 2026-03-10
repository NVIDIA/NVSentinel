// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	ExternalDrainsDetected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "slurm_drain_monitor_external_drains_detected_total",
			Help: "Total number of external Slurm drains detected by pattern",
		},
		[]string{"pattern_name"},
	)

	HealthEventsPublishErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "slurm_drain_monitor_health_events_publish_errors_total",
			Help: "Errors publishing health events to Platform Connector via gRPC",
		},
		[]string{"error_type"},
	)

	ReconciliationErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "slurm_drain_monitor_reconciliation_errors_total",
			Help: "Controller reconciliation errors",
		},
		[]string{"error_type"},
	)

	ParseMatches = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "slurm_drain_monitor_parse_matches_total",
			Help: "Total number of parse matches by pattern name",
		},
		[]string{"pattern_name"},
	)
)
