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

package overrides

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	overridesApplied = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "nvsentinel_overrides_applied_total",
		Help: "Total number of overrides applied by rule and field",
	}, []string{"rule_name", "field"})

	evaluationErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "nvsentinel_override_evaluation_errors_total",
		Help: "Total number of override rule evaluation errors",
	})
)
