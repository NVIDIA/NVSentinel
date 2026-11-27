// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package metrics holds the Prometheus metrics of nvhv-health-monitor.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

const (
	labelNode  = "node"
	labelCode  = "code"
	labelCheck = "check"
	labelFatal = "is_fatal"
)

var (
	// Unreachable is 1 while the monitor's calls to NVHV are failing and 0
	// once NVHV answers.
	Unreachable = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nvhv_health_monitor_unreachable",
		Help: "1 while calls to NVHV are failing, 0 while NVHV answers",
	}, []string{labelNode})

	// EvaluateFailures counts failed calls to NVHV by gRPC status code.
	EvaluateFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nvhv_health_monitor_evaluate_failures_total",
		Help: "Total number of failed NVHV Evaluate calls, by gRPC status code",
	}, []string{labelNode, labelCode})

	// EventsPublished counts health events delivered to the platform connector.
	EventsPublished = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nvhv_health_monitor_events_published_total",
		Help: "Total number of health events delivered to the platform connector",
	}, []string{labelNode, labelCheck, labelFatal})

	// PublishFailures counts batches the platform connector did not accept.
	PublishFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nvhv_health_monitor_publish_failures_total",
		Help: "Total number of event batches that failed to publish",
	}, []string{labelNode})
)
