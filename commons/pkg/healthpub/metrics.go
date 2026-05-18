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

package healthpub

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Counters are registered against the default registry via promauto.
// The `monitor` label is the agent name; `code` on sendsError is a gRPC
// status-code string (bounded cardinality, ~17 values).
var (
	sendsSkippedPCUnavailable = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvsentinel_health_events_publisher_skipped_pc_unavailable_total",
			Help: "Total health-event sends skipped because the platform-connector Unix socket was missing.",
		},
		[]string{"monitor"},
	)

	sendsSuccess = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvsentinel_health_events_publisher_sends_success_total",
			Help: "Total successful health-event sends.",
		},
		[]string{"monitor"},
	)

	sendsError = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvsentinel_health_events_publisher_sends_error_total",
			Help: "Total failed health-event sends after retries exhausted (excluding socket-missing skips).",
		},
		[]string{"monitor", "code"},
	)
)
