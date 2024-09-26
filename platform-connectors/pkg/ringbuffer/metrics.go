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
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	}, []string{workqueueLabel}).WithLabelValues(name)
}

func (prometheusMetricsProvider) NewWorkDurationMetric(name string) workqueue.HistogramMetric {
	return promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "platform_connector_workqueue_work_duration_seconds_" + name,
		Help:    "How long processing an item from Platform connector workqueue takes",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
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
