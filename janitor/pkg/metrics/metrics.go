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
	"github.com/prometheus/client_golang/prometheus/testutil"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// Action types for metrics labeling
const (
	ActionTypeReboot    = "reboot"
	ActionTypeTerminate = "terminate"
	ActionTypeLock      = "lock"
	ActionTypeUnlock    = "unlock"
)

// Status values for action metrics
const (
	StatusStarted   = "started"
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
)

var (
	// actionsCount tracks the total number of actions by type and status
	actionsCount = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "janitor_actions_count",
			Help: "Total number of janitor actions by type and status",
		},
		[]string{"action_type", "status", "node"},
	)

	// actionMTTRHistogram tracks the time taken to complete actions
	actionMTTRHistogram = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "janitor_action_mttr_seconds",
			Help:    "Time from CR creation to action completion",
			Buckets: prometheus.ExponentialBuckets(10, 2, 10), // Log-scale buckets for MTTR
		},
		[]string{"action_type"},
	)

	GPUResetRequestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpu_reset_requests_total",
		Help: "Total number of GPU reset requests initiated.",
	}, []string{"node"})

	GPUResetRequestsCompletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpu_reset_requests_completed_total",
		Help: "Total number of completed GPU reset requests, labeled by their final status.",
	}, []string{"node", "status"})

	GPUResetDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name: "gpu_reset_duration_seconds",
		Help: "The end-to-end duration of GPU reset workflows, from controller acknowledgment to completion, " +
			"labeled by their final status.",
		Buckets: prometheus.LinearBuckets(30, 30, 10),
	}, []string{"node", "status"})

	GPUResetPendingRequests = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gpu_reset_pending_requests",
		Help: "The number of GPU reset requests currently pending (e.g., waiting due to resource contention).",
	}, []string{"node"})

	GPUResetActiveRequests = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "gpu_reset_active_requests",
		Help: "The number of GPU reset requests currently in progress.",
	}, []string{"node"})

	GPUResetFailureReasonsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "gpu_reset_failure_reasons_total",
		Help: "Total number of GPU reset failures, labeled by the specific reason.",
	}, []string{"node", "reason"})
)

// ActionMetrics provides a centralized interface for recording action metrics
type ActionMetrics struct{}

// NewActionMetrics creates a new ActionMetrics instance and registers the metrics
func NewActionMetrics() *ActionMetrics {
	// Register metrics with the controller-runtime metrics registry
	metrics.Registry.MustRegister(
		actionsCount,
		actionMTTRHistogram,
		GPUResetRequestsTotal,
		GPUResetRequestsCompletedTotal,
		GPUResetDurationSeconds,
		GPUResetPendingRequests,
		GPUResetActiveRequests,
		GPUResetFailureReasonsTotal,
	)

	return &ActionMetrics{}
}

// IncActionCount increments the action count for the given action type, status, and node
func (m *ActionMetrics) IncActionCount(actionType, status, node string) {
	actionsCount.With(prometheus.Labels{
		"action_type": actionType,
		"status":      status,
		"node":        node,
	}).Inc()
}

// RecordActionMTTR records the completion time for an action
func (m *ActionMetrics) RecordActionMTTR(actionType string, duration time.Duration) {
	actionMTTRHistogram.With(prometheus.Labels{
		"action_type": actionType,
	}).Observe(duration.Seconds())
}

// GetActionsCountValue returns the current value of the actions counter for testing purposes
func (m *ActionMetrics) GetActionsCountValue(actionType, status, node string) float64 {
	return testutil.ToFloat64(
		actionsCount.WithLabelValues(actionType, status, node),
	)
}

// GlobalMetrics is the global metrics instance for easy access across controllers
var GlobalMetrics *ActionMetrics

// Initialize the global metrics instance
func init() {
	GlobalMetrics = NewActionMetrics()
}

// IncActionCount is a convenience function to increment action count using the global instance
func IncActionCount(actionType, status, node string) {
	GlobalMetrics.IncActionCount(actionType, status, node)
}

// RecordActionMTTR is a convenience function to record MTTR using the global instance
func RecordActionMTTR(actionType string, duration time.Duration) {
	GlobalMetrics.RecordActionMTTR(actionType, duration)
}
