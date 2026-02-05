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

package healthevents

import (
	"sync"

	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	registerOnce sync.Once

	// quarantineActionsTotal tracks the number of quarantine actions taken.
	quarantineActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "quarantine_controller",
			Name:      "actions_total",
			Help:      "Total number of quarantine actions taken by outcome",
		},
		[]string{"node", "outcome"}, // outcome: success, failed, skipped
	)

	// quarantineLatencySeconds tracks the time taken to quarantine a node.
	quarantineLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "nvsentinel",
			Subsystem: "quarantine_controller",
			Name:      "latency_seconds",
			Help:      "Time taken from HealthEvent creation to node quarantine",
			Buckets:   []float64{0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30},
		},
		[]string{"node"},
	)

	// healthEventsProcessedTotal tracks the number of health events processed.
	healthEventsProcessedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "quarantine_controller",
			Name:      "events_processed_total",
			Help:      "Total number of health events processed",
		},
		[]string{"source", "component_class", "is_fatal"},
	)

	// reconcileErrorsTotal tracks reconciliation errors.
	reconcileErrorsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "quarantine_controller",
			Name:      "reconcile_errors_total",
			Help:      "Total number of reconciliation errors",
		},
		[]string{"error_type"},
	)

	// nodesQuarantined is a gauge showing currently quarantined nodes.
	nodesQuarantined = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Namespace: "nvsentinel",
			Subsystem: "quarantine_controller",
			Name:      "nodes_quarantined",
			Help:      "Number of nodes currently quarantined by NVSentinel",
		},
	)
)

// registerMetrics registers all metrics with the controller-runtime metrics registry.
func registerMetrics() {
	registerOnce.Do(func() {
		metrics.Registry.MustRegister(
			quarantineActionsTotal,
			quarantineLatencySeconds,
			healthEventsProcessedTotal,
			reconcileErrorsTotal,
			nodesQuarantined,
		)
	})
}

// RecordQuarantineLatency records the time from event detection to quarantine.
func RecordQuarantineLatency(node string, seconds float64) {
	quarantineLatencySeconds.WithLabelValues(node).Observe(seconds)
}

// IncrementNodesQuarantined increments the quarantined nodes gauge.
func IncrementNodesQuarantined() {
	nodesQuarantined.Inc()
}

// DecrementNodesQuarantined decrements the quarantined nodes gauge.
func DecrementNodesQuarantined() {
	nodesQuarantined.Dec()
}

// =============================================================================
// Drain Controller Metrics
// =============================================================================

var (
	registerDrainOnce sync.Once

	// drainActionsTotal tracks the number of drain actions taken.
	drainActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "drain_controller",
			Name:      "actions_total",
			Help:      "Total number of drain actions taken by outcome",
		},
		[]string{"node", "outcome"}, // outcome: evicted, failed, skipped, completed
	)

	// drainLatencySeconds tracks the time taken to drain a node.
	drainLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "nvsentinel",
			Subsystem: "drain_controller",
			Name:      "latency_seconds",
			Help:      "Time taken from quarantine to drain completion",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300, 600},
		},
		[]string{"node"},
	)

	// podsEvictedTotal tracks the number of pods evicted.
	podsEvictedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "drain_controller",
			Name:      "pods_evicted_total",
			Help:      "Total number of pods evicted during drain",
		},
		[]string{"node", "namespace"},
	)

	// pdbBlockedTotal tracks evictions blocked by PDBs.
	pdbBlockedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "drain_controller",
			Name:      "pdb_blocked_total",
			Help:      "Total number of evictions blocked by PodDisruptionBudget",
		},
		[]string{"node", "namespace"},
	)
)

// registerDrainMetrics registers drain controller metrics.
func registerDrainMetrics() {
	registerDrainOnce.Do(func() {
		metrics.Registry.MustRegister(
			drainActionsTotal,
			drainLatencySeconds,
			podsEvictedTotal,
			pdbBlockedTotal,
		)
	})
}

// =============================================================================
// TTL Controller Metrics
// =============================================================================

var (
	registerTTLOnce sync.Once

	// ttlDeletionsTotal tracks the number of events deleted by TTL.
	ttlDeletionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "ttl_controller",
			Name:      "deletions_total",
			Help:      "Total number of HealthEvents deleted by TTL controller",
		},
		[]string{"node", "phase"},
	)

	// ttlEventsAgeSeconds tracks the age of events at deletion time.
	ttlEventsAgeSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "nvsentinel",
			Subsystem: "ttl_controller",
			Name:      "event_age_seconds",
			Help:      "Age of HealthEvents at deletion time",
			Buckets:   []float64{3600, 86400, 604800, 2592000, 7776000}, // 1h, 1d, 7d, 30d, 90d
		},
		[]string{"node"},
	)

	// ttlPolicyReloadsTotal tracks policy ConfigMap reloads.
	ttlPolicyReloadsTotal = prometheus.NewCounter(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "ttl_controller",
			Name:      "policy_reloads_total",
			Help:      "Total number of TTL policy reloads",
		},
	)
)

// registerTTLMetrics registers TTL controller metrics.
func registerTTLMetrics() {
	registerTTLOnce.Do(func() {
		metrics.Registry.MustRegister(
			ttlDeletionsTotal,
			ttlEventsAgeSeconds,
			ttlPolicyReloadsTotal,
		)
	})
}

// =============================================================================
// Remediation Controller Metrics
// =============================================================================

var (
	registerRemediationOnce sync.Once

	// remediationActionsTotal tracks successful remediation actions.
	remediationActionsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "remediation_controller",
			Name:      "actions_total",
			Help:      "Total number of remediation actions executed",
		},
		[]string{"node", "strategy"},
	)

	// remediationFailuresTotal tracks failed remediation attempts.
	remediationFailuresTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "remediation_controller",
			Name:      "failures_total",
			Help:      "Total number of failed remediation attempts",
		},
		[]string{"node", "strategy"},
	)

	// remediationLatencySeconds tracks remediation execution latency.
	remediationLatencySeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "nvsentinel",
			Subsystem: "remediation_controller",
			Name:      "latency_seconds",
			Help:      "Latency of remediation actions",
			Buckets:   []float64{1, 5, 10, 30, 60, 120, 300, 600},
		},
		[]string{"node", "strategy"},
	)

	// rebootJobsCreatedTotal tracks reboot jobs created.
	rebootJobsCreatedTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "remediation_controller",
			Name:      "reboot_jobs_created_total",
			Help:      "Total number of reboot jobs created",
		},
		[]string{"node"},
	)
)

// registerRemediationMetrics registers remediation controller metrics.
func registerRemediationMetrics() {
	registerRemediationOnce.Do(func() {
		metrics.Registry.MustRegister(
			remediationActionsTotal,
			remediationFailuresTotal,
			remediationLatencySeconds,
			rebootJobsCreatedTotal,
		)
	})
}
