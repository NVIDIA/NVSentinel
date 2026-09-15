// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package metrics defines the Prometheus metrics exported by the NVCRE
// Certification Monitor. They are registered on the controller-runtime
// registry so the manager's metrics endpoint serves them.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Label values for the error_type label of SweepErrors.
const (
	ErrListCerts         = "list_certs"
	ErrCompletionTime    = "completion_time"
	ErrConfigMapNotFound = "configmap_not_found"
	ErrConfigMapGet      = "configmap_get"
	ErrConfigMapDecode   = "configmap_decode"
	ErrNodeNotFound      = "node_not_found"
	ErrNodeGet           = "node_get"
)

var (
	factory = promauto.With(crmetrics.Registry)

	// SweepErrors counts the failures hit while sweeping, by kind. Some abort
	// the sweep (list_certs, configmap_get, a failed-nodes configmap_decode);
	// the rest skip the affected cert, category or tuple and continue.
	SweepErrors = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvcre_certification_monitor_sweep_errors_total",
			Help: "Total number of errors hit during reconciliation sweeps over Certification CRs, by error type",
		},
		[]string{"error_type"},
	)

	// SweepDuration observes how long each reconciliation sweep takes.
	SweepDuration = factory.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "nvcre_certification_monitor_sweep_duration_seconds",
			Help:    "Duration of a reconciliation sweep over Certification CRs",
			Buckets: prometheus.DefBuckets,
		},
	)

	// HealthEventsPublished counts health events accepted by platform-connectors.
	HealthEventsPublished = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvcre_certification_monitor_health_events_published_total",
			Help: "Total number of health events published to platform-connectors, by node and health state",
		},
		[]string{"node", "is_healthy"},
	)

	// HealthEventPublishErrors counts health events that failed to publish after retries.
	HealthEventPublishErrors = factory.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvcre_certification_monitor_health_event_publish_errors_total",
			Help: "Total number of health events that could not be published to platform-connectors, by node and health state",
		},
		[]string{"node", "is_healthy"},
	)

	// ActiveFailures is the number of (node, variant, reason) certification
	// failures currently asserted by terminal Certification CRs.
	ActiveFailures = factory.NewGauge(
		prometheus.GaugeOpts{
			Name: "nvcre_certification_monitor_active_failures",
			Help: "Number of (node, variant, reason) certification failures asserted by terminal Certification CRs",
		},
	)

	// MalformedNodeAnnotations is the number of nodes skipped in the last sweep
	// because their cert-failures annotation could not be parsed.
	MalformedNodeAnnotations = factory.NewGauge(
		prometheus.GaugeOpts{
			Name: "nvcre_certification_monitor_malformed_node_annotations",
			Help: "Number of nodes skipped in the last sweep because their cert-failures annotation is unparsable",
		},
	)
)
