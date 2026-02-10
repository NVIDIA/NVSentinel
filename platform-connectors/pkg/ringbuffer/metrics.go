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

package ringbuffer

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"k8s.io/client-go/util/workqueue"
)

const workqueueLabel = "workqueue"

type prometheusMetricsProvider struct{}

func (prometheusMetricsProvider) NewDepthMetric(name string) workqueue.GaugeMetric {
	return promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_connector_workqueue_depth_" + name,
		Help: "Current depth of Platform connector workqueue",
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewAddsMetric(name string) workqueue.CounterMetric {
	return promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_workqueue_adds_total_" + name,
		Help: "Total number of adds handled by Platform connector workqueue",
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewLatencyMetric(name string) workqueue.HistogramMetric {
	return promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "platform_connector_workqueue_latency_seconds_" + name,
		Help:    "How long an item stays in Platform connector workqueue before being requested",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewWorkDurationMetric(name string) workqueue.HistogramMetric {
	return promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "platform_connector_workqueue_work_duration_seconds_" + name,
		Help:    "How long processing an item from Platform connector workqueue takes",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewRetriesMetric(name string) workqueue.CounterMetric {
	return promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_workqueue_retries_total_" + name,
		Help: "Total number of retries handled by Platform connector workqueue",
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewLongestRunningProcessorSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_connector_workqueue_longest_running_processor_seconds_" + name,
		Help: "How many seconds the longest running processor for Platform connector workqueue has been running",
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewUnfinishedWorkSecondsMetric(name string) workqueue.SettableGaugeMetric {
	return promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "platform_connector_workqueue_unfinished_work_seconds_" + name,
		Help: "The total time in seconds of work in progress in Platform connector workqueue",
	}, []string{workqueueLabel}).WithLabelValues(name)
}
