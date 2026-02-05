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
)

// Operation constants for metrics
const (
	OperationCreate = "create"
	OperationUpdate = "update"
)

// prometheus metrics
var (
	nodeConditionUpdateCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_condition_update_total",
		Help: "The total number of node condition updates by status",
	}, []string{"status"})

	nodeEventOperationsCounter = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "k8s_platform_connector_node_event_operations_total",
		Help: "The total number of node event operations by type and status",
	}, []string{"node_name", "operation", "status"})

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
