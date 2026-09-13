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

package main

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// podMapperMetrics is the alertable surface for the poll loop: a total to alert on a rising
// rate, and the current streak, which is the value the exit threshold acts on.
//
// The registerer is injected rather than taken from the default so tests get a fresh registry
// instead of sharing counters between cases.
type podMapperMetrics struct {
	failures            prometheus.Counter
	consecutiveFailures prometheus.Gauge
}

func newPodMapperMetrics(reg prometheus.Registerer) *podMapperMetrics {
	factory := promauto.With(reg)

	return &podMapperMetrics{
		failures: factory.NewCounter(prometheus.CounterOpts{
			Name: "metadata_collector_pod_mapper_failures_total",
			Help: "Pod device mapper poll cycles that failed.",
		}),
		consecutiveFailures: factory.NewGauge(prometheus.GaugeOpts{
			Name: "metadata_collector_pod_mapper_consecutive_failures",
			Help: "Consecutive failed pod device mapper poll cycles, reset to 0 by any success. " +
				"The container exits when this reaches --pod-mapper-max-consecutive-failures.",
		}),
	}
}

func (m *podMapperMetrics) recordFailure(consecutive int) {
	m.failures.Inc()
	m.consecutiveFailures.Set(float64(consecutive))
}

func (m *podMapperMetrics) recordSuccess() {
	m.consecutiveFailures.Set(0)
}
