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

package breaker

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	faultQuarantineBreakerState = promauto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_breaker_state",
			Help: "State of the fault quarantine breaker.",
		},
		[]string{"state"},
	)
	faultQuarantineBreakerUtilization = promauto.NewGauge(
		prometheus.GaugeOpts{
			Name: "fault_quarantine_breaker_utilization",
			Help: "Utilization of the fault quarantine breaker.",
		},
	)
	faultQuarantineGetTotalNodesDuration = promauto.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_get_total_nodes_duration_seconds",
			Help:    "Duration of getTotalNodesWithRetry calls in seconds.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"result"},
	)
	faultQuarantineGetTotalNodesErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "fault_quarantine_get_total_nodes_errors_total",
			Help: "Total number of errors from getTotalNodesWithRetry.",
		},
		[]string{"error_type"},
	)
	faultQuarantineGetTotalNodesRetryAttempts = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "fault_quarantine_get_total_nodes_retry_attempts",
			Help:    "Number of retry attempts needed for getTotalNodesWithRetry.",
			Buckets: []float64{0, 1, 2, 3, 5, 10},
		},
	)
)

func SetFaultQuarantineBreakerUtilization(utilization float64) {
	faultQuarantineBreakerUtilization.Set(utilization)
}

func SetFaultQuarantineBreakerState(state State) {
	faultQuarantineBreakerState.Reset()
	faultQuarantineBreakerState.WithLabelValues(string(state)).Set(1)
}
