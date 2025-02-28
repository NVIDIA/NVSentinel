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

	nodeEventCreationSuccessCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_creation_success_total",
		Help: "The total number of successful node event creations",
	}, []string{"node_name"})

	nodeEventCreationFailureCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_creation_failed_total",
		Help: "The total number of failed node event creations",
	}, []string{"node_name"})

	nodeEventUpdateSuccessCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_update_success_total",
		Help: "The total number of successful node event updates",
	}, []string{"node_name"})

	nodeEventUpdateFailureCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_update_failed_total",
		Help: "The total number of failed node event updates",
	}, []string{"node_name"})

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
