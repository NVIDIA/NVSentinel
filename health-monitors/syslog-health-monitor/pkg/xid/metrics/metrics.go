// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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
	"strconv"

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

// PreInitialize materializes XidCounterMetric at zero for the local node and
// each known XID code so backends that establish a baseline from the first
// ingested sample (e.g. Google Managed Prometheus) do not drop the first real
// occurrence. See https://github.com/NVIDIA/NVSentinel/issues/1196.
//
// Callers typically pass the keys of the embedded NVIDIA XID catalog loaded
// via common.LoadErrorResolutionMap.
//
// Notes on scope:
//   - NVL5 XIDs reported with a subcode suffix (e.g. "145.RLW_SRC_TRACK") are
//     NOT pre-initialized because the subcode space depends on the specific
//     interrupt content and is not enumerable at startup.
//   - XidProcessingErrors is deliberately NOT pre-initialized here. Those
//     counters track internal parser failures; losing the first occurrence in
//     GMP is acceptable and far less impactful than losing the first real XID
//     event.
//
// Calling PreInitialize is idempotent; WithLabelValues(...).Add(0) is a no-op
// on an already-materialized counter.
func PreInitialize(nodeName string, xidCodes []int) {
	for _, code := range xidCodes {
		XidCounterMetric.WithLabelValues(nodeName, strconv.Itoa(code)).Add(0)
	}
}
