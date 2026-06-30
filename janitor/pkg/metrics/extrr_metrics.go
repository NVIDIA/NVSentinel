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

// ExtRR observability metrics. Schema per ADR-040.

const (
	ExtRRPhaseCreated          = "created"
	ExtRRPhaseReleased         = "released"
	ExtRRPhaseExternalResponse = "external_response"
	ExtRRPhaseClosed           = "closed"
)

const (
	ExtRRResultSuccess         = "success"
	ExtRRResultOperatorDeleted = "operator_deleted"
	// ExtRRResultNodeNotFound is recorded when the apply path fails because
	// the target Node doesn't exist at reconcile time. Terminal — no retry.
	ExtRRResultNodeNotFound = "node_not_found"
	// ExtRRResultNone is the explicit sentinel for phases without an outcome.
	ExtRRResultNone = ""
)

const (
	ExtRROpenStateAwaiting = "awaiting"
)

var (
	// ExtRRTotal phases:
	//   created           — fresh ExtRR initialised.
	//   released          — NVSentinelOwnershipReleased transitioned:
	//                       result=success (taint applied) or
	//                       result=node_not_found (target node absent at apply time).
	//   external_response — ExternalRemediationComplete=True observed. result=success.
	//   closed            — cleanup ran. result=success | operator_deleted.
	ExtRRTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "nvsentinel_external_remediation_total",
		Help: "Lifecycle transitions of ExternalRemediationRequest objects, labeled by phase and outcome.",
	}, []string{"phase", "result"})

	ExtRROpen = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "nvsentinel_external_remediation_open",
		Help: "Currently-open ExternalRemediationRequest objects by node, action, and substate.",
	}, []string{"node", "recommended_action", "state"})

	ExtRRAgeSeconds = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "nvsentinel_external_remediation_age_seconds",
		Help:    "Age of an ExternalRemediationRequest at close time.",
		Buckets: prometheus.ExponentialBuckets(10, 4, 8), // 10s, 40s, 160s, ~10m, ~43m, ~3h, ~11h, ~46h
	}, []string{"recommended_action", "result"})
)

func (m *ActionMetrics) IncExtRRTotal(phase, result string) {
	ExtRRTotal.With(prometheus.Labels{
		"phase":  phase,
		"result": result,
	}).Inc()
}

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

func registerExtRRMetrics() {
	metrics.Registry.MustRegister(
		ExtRRTotal,
		ExtRROpen,
		ExtRRAgeSeconds,
	)
}
