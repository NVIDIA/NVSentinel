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

package kubernetes

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Status constants for metrics
const (
	StatusSuccess = "success"
	StatusFailed  = "failed"
	// StatusSkipped counts writes that were not made because they would have
	// changed nothing: a batch that leaves the node condition as it is, or a
	// repeat of a fault whose Event is already written.
	StatusSkipped = "skipped"
)

// Operation constants for metrics
const (
	OperationCreate = "create"
	OperationUpdate = "update"
)

// Write kinds for the dropped-write metrics. These name what the write targets,
// unlike OperationCreate/OperationUpdate above, which name the API verb.
const (
	WriteNodeCondition = "node_condition"
	WriteNodeEvent     = "node_event"
)

// Terminal reasons a Kubernetes write is discarded. writeDropReason returns exactly
// one of these, and initMetrics below pre-creates a series for each, so the two must
// stay in step.
const (
	DropReasonShutdown       = "shutdown"
	DropReasonRetryTimeout   = "retry_timeout"
	DropReasonRetryExhausted = "retry_exhausted"
	DropReasonPermanentError = "permanent_error"
)

// dropReasons is every value writeDropReason can return.
var dropReasons = []string{
	DropReasonShutdown,
	DropReasonRetryTimeout,
	DropReasonRetryExhausted,
	DropReasonPermanentError,
}

// prometheus metrics
var (
	droppedWritesCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_dropped_writes_total",
		Help: "Kubernetes writes discarded by operation, reason and health direction, including unattempted " +
			"writes at deadline or shutdown. is_healthy separates a lost clear, which can leave a fault " +
			"standing with nothing to clear it, from a lost set, which the next change re-reports. For " +
			"node_condition one write carries every event grouped for that node, so is_healthy=\"true\" " +
			"means the write contained at least one recovery, not that all of it was recoveries",
	}, []string{"operation", "reason", "is_healthy"})

	droppedBatchesCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_dropped_batches_total",
		Help: "Batches containing discarded Kubernetes writes, counted once per terminal reason",
	}, []string{"reason"})

	nodeConditionUpdateCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_condition_update_total",
		Help: "The total number of node condition updates by status",
	}, []string{"status"})

	// No node_name label: the deployment platform connector writes Events for
	// the whole fleet, and one series per node would be fleet-sized per replica.
	nodeEventOperationsCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_operations_total",
		Help: "The total number of node event operations by type and status",
	}, []string{"operation", "status"})

	nodeConditionUpdateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "k8s_platform_connector_node_condition_update_duration_milliseconds",
		Help:    "Duration of node condition updates in milliseconds",
		Buckets: prometheus.ExponentialBuckets(10, 2, 12),
	})

	nodeEventUpdateCreateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "k8s_platform_connector_node_event_update_create_duration_milliseconds",
		Help:    "Duration of node event updates/creations in milliseconds",
		Buckets: prometheus.ExponentialBuckets(10, 2, 12),
	})
)

func init() {
	initMetrics()
}

// initMetrics creates every label combination these counters can report, at zero.
//
// A CounterVec with no children exports nothing at all: no series, no # HELP, no # TYPE.
// That makes an empty query ambiguous, because "nothing was dropped" and "the component
// is not running, not scraped, or was renamed" look identical, and it means an alert has
// no series to evaluate a rate() against until the first occurrence has already happened,
// which is the event the alert exists to catch.
//
// Every label value here is a compile-time constant, so this is 16 + 4 + 3 + 5 series and
// cannot grow with the fleet.
func initMetrics() {
	for _, operation := range []string{WriteNodeCondition, WriteNodeEvent} {
		for _, reason := range dropReasons {
			for _, isHealthy := range []string{"true", "false"} {
				droppedWritesCounter.WithLabelValues(operation, reason, isHealthy)
			}
		}
	}

	for _, reason := range dropReasons {
		droppedBatchesCounter.WithLabelValues(reason)
	}

	for _, status := range []string{StatusSuccess, StatusFailed, StatusSkipped} {
		nodeConditionUpdateCounter.WithLabelValues(status)
	}

	// Only the pairs the code can actually report. A create is never skipped, so
	// initialising the full cross product would publish a series that can never move.
	for _, pair := range [][2]string{
		{OperationCreate, StatusSuccess},
		{OperationCreate, StatusFailed},
		{OperationUpdate, StatusSuccess},
		{OperationUpdate, StatusFailed},
		{OperationUpdate, StatusSkipped},
	} {
		nodeEventOperationsCounter.WithLabelValues(pair[0], pair[1])
	}
}
