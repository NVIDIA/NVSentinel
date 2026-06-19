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

package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

// ExtRR observability metrics. All three share the
// nvsentinel_external_remediation_ namespace so a single grep against the
// metrics endpoint surfaces the entire signal set. Schema per ADR-040.

// Phase label values for ExtRRTotal.
const (
	ExtRRPhaseCreated          = "created"
	ExtRRPhaseReleased         = "released"
	ExtRRPhaseExternalResponse = "external_response"
	ExtRRPhaseClosed           = "closed"
)

// Result label values for ExtRRTotal / ExtRRAgeSeconds. ExtRRResultNone is the
// sentinel used for phases that don't carry an outcome (e.g. phase=created),
// kept as an explicit constant so empty-string label values aren't a stringly-
// typed surprise to dashboard authors.
const (
	ExtRRResultSuccess         = "success"
	ExtRRResultFailure         = "failure"
	ExtRRResultOperatorDeleted = "operator_deleted"
	ExtRRResultNone            = ""
)

// Open-state label values for ExtRROpen.
const (
	ExtRROpenStateAwaiting = "awaiting"
	ExtRROpenStateFailed   = "failed"
)

var (
	// ExtRRTotal counts lifecycle transitions:
	//
	//   created           — reconciler initialised a fresh ExtRR.
	//   released          — NVSentinelOwnershipReleased flipped. result=success|failure.
	//   external_response — ExternalRemediationComplete observed. result=success|failure.
	//   closed            — cleanup ran. result=success (Complete=True) | operator_deleted.
	ExtRRTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nvsentinel_external_remediation_total",
		Help: "Lifecycle transitions of ExternalRemediationRequest objects, labeled by phase and outcome.",
	}, []string{"phase", "result"})

	// ExtRROpen tracks currently-open ExtRRs by substate (awaiting external
	// response vs. external reported failure but operator hasn't acted).
	ExtRROpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nvsentinel_external_remediation_open",
		Help: "Currently-open ExternalRemediationRequest objects by node, action, and substate.",
	}, []string{"node", "recommended_action", "state"})

	// ExtRRAgeSeconds records creation-to-close age. Buckets span seconds
	// (healthy auto-cleanup) through hours (operator forgot about it).
	ExtRRAgeSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nvsentinel_external_remediation_age_seconds",
		Help:    "Age of an ExternalRemediationRequest at close time.",
		Buckets: prometheus.ExponentialBuckets(10, 4, 8), // 10s, 40s, 160s, ~10m, ~43m, ~3h, ~11h, ~46h
	}, []string{"recommended_action", "result"})
)

// IncExtRRTotal increments ExtRRTotal. Pass ExtRRResultNone for phases that
// don't carry an outcome (phase=created).
func (m *ActionMetrics) IncExtRRTotal(phase, result string) {
	ExtRRTotal.With(prometheus.Labels{
		"phase":  phase,
		"result": result,
	}).Inc()
}

// AdjustExtRROpen adjusts ExtRROpen by delta (+1 entering the state, -1
// leaving). The reconciler keeps emission in one place per branch to avoid
// double-counting across re-reconciles.
func (m *ActionMetrics) AdjustExtRROpen(node, recommendedAction, state string, delta float64) {
	ExtRROpen.With(prometheus.Labels{
		"node":               node,
		"recommended_action": recommendedAction,
		"state":              state,
	}).Add(delta)
}

func (m *ActionMetrics) ObserveExtRRAge(recommendedAction, result string, ageSeconds float64) {
	ExtRRAgeSeconds.With(prometheus.Labels{
		"recommended_action": recommendedAction,
		"result":             result,
	}).Observe(ageSeconds)
}

// registerExtRRMetrics registers the series with controller-runtime's metrics
// registry. Called from NewActionMetrics so the standard endpoint exposes them.
func registerExtRRMetrics() {
	metrics.Registry.MustRegister(
		ExtRRTotal,
		ExtRROpen,
		ExtRRAgeSeconds,
	)
}
