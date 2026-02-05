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

package crdpublisher

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// healthEventsCreatedTotal tracks the number of HealthEvent CRDs created.
	healthEventsCreatedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "crd_publisher",
			Name:      "health_events_created_total",
			Help:      "Total number of HealthEvent CRDs created",
		},
		[]string{"node", "source"},
	)

	// healthEventsFailedTotal tracks failed CRD creations.
	healthEventsFailedTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "crd_publisher",
			Name:      "health_events_failed_total",
			Help:      "Total number of failed HealthEvent CRD creations",
		},
		[]string{"node", "source", "reason"},
	)

	// debouncedEventsTotal tracks events that were debounced.
	debouncedEventsTotal = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "nvsentinel",
			Subsystem: "crd_publisher",
			Name:      "debounced_events_total",
			Help:      "Total number of events debounced",
		},
		[]string{"node"},
	)
)
