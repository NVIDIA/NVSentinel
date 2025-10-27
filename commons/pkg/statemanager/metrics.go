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

package statemanager

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	// Metric to track unexpected state transitions (for observability, not enforcement)
	stateTransitionUnexpected = promauto.NewCounterVec(
		prometheus.CounterOpts{
			Name: "nvsentinel_state_transition_unexpected_total",
			Help: "Total number of unexpected state transitions detected (race conditions or bugs)",
		},
		[]string{"from_state", "to_state", "node"},
	)
)
