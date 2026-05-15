// Copyright 2026 k8s-gpu-mcp-server contributors
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

package mcp

import "github.com/prometheus/client_golang/prometheus"

// Prometheus label keys, hoisted to constants so the goconst linter does not
// flag the repeated string literals in each metric's label list.
const (
	labelTool   = "tool"
	labelStatus = "status"
)

// Prometheus metrics for the MCP server. Tool handlers call RecordRequest
// (and may directly manipulate ActiveRequests for in-flight gauges); the
// default registry serves them via commons/pkg/server's /metrics endpoint on
// the metrics port, so consumers do not need to know they live in this
// package.
var (
	// RequestsTotal counts tool invocations, partitioned by tool name and
	// status ("ok", "error", "denied", etc. — handlers choose).
	RequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "mcp_server_requests_total",
			Help: "Total number of MCP tool requests processed, partitioned by tool name and status.",
		},
		[]string{labelTool, labelStatus},
	)

	// RequestDuration observes per-tool latency. Buckets are tuned for
	// in-cluster MCP traffic: most reads finish under 100ms, slow store
	// queries up to a few seconds.
	RequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "mcp_server_request_duration_seconds",
			Help:    "Latency of MCP tool requests in seconds, partitioned by tool name.",
			Buckets: []float64{0.01, 0.05, 0.1, 0.5, 1, 5},
		},
		[]string{labelTool},
	)

	// ActiveRequests is the count of currently in-flight tool invocations
	// per tool. Handlers Inc on entry and Dec on exit (deferred).
	ActiveRequests = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "mcp_server_active_requests",
			Help: "Number of MCP tool requests currently in flight, partitioned by tool name.",
		},
		[]string{labelTool},
	)
)

// RecordRequest is the canonical accessor handlers should use after a tool
// invocation completes. It increments the request counter for (tool, status)
// and observes the elapsed duration on the per-tool histogram.
func RecordRequest(tool, status string, durationSeconds float64) {
	RequestsTotal.WithLabelValues(tool, status).Inc()
	RequestDuration.WithLabelValues(tool).Observe(durationSeconds)
}

func init() {
	prometheus.MustRegister(RequestsTotal, RequestDuration, ActiveRequests)
}
