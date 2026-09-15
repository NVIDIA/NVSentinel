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

package central

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Request outcomes for the requests metric, besides the store's own
// (stored, duplicate): deferred is a batch answered Unavailable before any
// write because its node metadata was not all read inside the pipeline
// budget; the caller resends it.
const (
	outcomeFailed   = "failed"
	outcomeRejected = "rejected"
	outcomeDeferred = "deferred"
)

// Reasons a node condition update did not land.
const (
	conditionFailed   = "failed"
	conditionTimedOut = "timeout"
)

// The deployment platform connector's metric families. None of them carries a
// per-pod or per-node label: at fleet scale those are unbounded, so the
// offending identity goes to the log line instead. The signals to alert on
// are request latency, failed writes and condition updates that failed or ran
// out of time.
var (
	// requests counts and times every batch request that passed
	// authentication, by outcome: stored, duplicate (a resent batch whose
	// events were already stored), failed (the datastore write failed, the
	// client will retry) or rejected (the batch itself was invalid). Requests
	// the auth interceptor refuses are logged there and never reach it.
	requests = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "platform_connector_deployment_request_duration_seconds",
		Help:    "Duration of health event batch requests, by outcome",
		Buckets: prometheus.DefBuckets,
	}, []string{"outcome"})

	// conditionUpdateFailures counts node condition updates that did not
	// land although the batch was acknowledged: the update failed after the
	// k8s connector's own retries, or ran past the bounded wait.
	conditionUpdateFailures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_deployment_condition_update_failures_total",
		Help: "Node condition updates that failed or timed out after the batch was acknowledged, by reason",
	}, []string{"reason"})

	// scopeViolations counts batches rejected for naming a node outside the
	// authorizing token's scope, by rejecting check.
	scopeViolations = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "platform_connector_deployment_scope_violations_total",
		Help: "Total batches rejected for naming a node outside the caller token's scope, by class",
	}, []string{"class"})
)
