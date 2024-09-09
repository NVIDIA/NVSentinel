package kubernetes

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// prometheus metrics
var (
	nodeConditionUpdateSuccessCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_condition_update_success_total",
		Help: "The total number of successful node condition updates",
	})
	nodeConditionUpdateFailureCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_condition_update_failed_total",
		Help: "The total number of failed node condition updates",
	})

	nodeEventCreationSuccessCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_creation_success_total",
		Help: "The total number of successful node event creations",
	})

	nodeEventCreationFailureCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_creation_failed_total",
		Help: "The total number of failed node event creations",
	})

	nodeEventUpdateSuccessCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_update_success_total",
		Help: "The total number of successful node event updates",
	})

	nodeEventUpdateFailureCounter = promauto.NewCounter(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_update_failed_total",
		Help: "The total number of failed node event updates",
	})

	nodeConditionUpdateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "k8s_platform_connector_node_condition_update_duration_milliseconds",
		Help:    "Duration of node condition updates in milliseconds",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	})

	nodeEventUpdateCreateDuration = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "k8s_platform_connector_node_event_update_create_duration_milliseconds",
		Help:    "Duration of node event updates/creations in milliseconds",
		Buckets: prometheus.LinearBuckets(0, 10, 500),
	})
)
