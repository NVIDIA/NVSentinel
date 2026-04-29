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

package gpufallen

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Counter metric for GPU fallen off bus errors
	gpuFallenCounterMetric = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "syslog_health_monitor_gpu_fallen_errors",
			Help: "Total number of GPU fallen off bus errors detected",
		},
		[]string{"node"},
	)
)

// PreInitialize materializes gpuFallenCounterMetric at zero for the local node
// so backends that establish a baseline from the first ingested sample (e.g.
// Google Managed Prometheus) do not drop the first real occurrence.
//
// Calling PreInitialize is idempotent; WithLabelValues(...).Add(0) is a no-op
// on an already-materialized counter.
func PreInitialize(nodeName string) {
	gpuFallenCounterMetric.WithLabelValues(nodeName).Add(0)
}
