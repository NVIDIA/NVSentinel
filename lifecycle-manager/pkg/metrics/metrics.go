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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	StatusSuccess = "success"
	StatusFailure = "failure"

	labelStatus = "status"
)

var (
	// ValidationRequestsTotal tracks the total number of ValidationRequests initiated.
	ValidationRequestsTotal = prometheus.NewCounter(prometheus.CounterOpts{
		Name: "validation_requests_total",
		Help: "Total number of ValidationRequests initiated.",
	})

	// ValidationRequestsCompletedTotal tracks the total number of completed ValidationRequests,
	// labeled by their final status.
	ValidationRequestsCompletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "validation_requests_completed_total",
		Help: "Total number of completed ValidationRequests, labeled by their final status.",
	}, []string{labelStatus})

	// ValidationRequestsDurationSeconds tracks the end-to-end duration of ValidationRequests,
	// from StartTime to CompletionTime, labeled by their final status.
	ValidationRequestsDurationSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "validation_requests_duration_seconds",
		Help:    "The end-to-end duration of ValidationRequests, labeled by their final status.",
		Buckets: prometheus.ExponentialBuckets(10, 2, 10),
	}, []string{labelStatus})
)

func init() {
	metrics.Registry.MustRegister(
		ValidationRequestsTotal,
		ValidationRequestsCompletedTotal,
		ValidationRequestsDurationSeconds,
	)
}
