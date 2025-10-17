// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	XidCounterMetric = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "syslog_health_monitor_xid_errors",
			Help: "Total number of XID found",
		},
		[]string{"node", "err_code"},
	)

	XidProcessingErrors = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "syslog_health_monitor_xid_processing_errors",
			Help: "Total number of errors encountered during XID processing",
		},
		[]string{"error_type", "node"},
	)

	XidProcessingLatency = promauto.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "syslog_health_monitor_xid_processing_latency_seconds",
			Help:    "Histogram of XID processing latency",
			Buckets: prometheus.DefBuckets,
		},
	)
)
