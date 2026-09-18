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

// Reasons a batch was refused before the handler saw it. Everything the
// handler itself answers is on platform_connector_request_duration_seconds.
const (
	refusalIndexUnverified = "index_unverified"
	refusalIdempotencyKey  = "idempotency_key"
)

var refusals = promauto.NewCounterVec(prometheus.CounterOpts{
	Name: "platform_connector_deployment_refusals_total",
	Help: "Batches refused before any processing, by reason: index_unverified (this replica has not verified " +
		"the idempotency index; the caller retries) or idempotency_key (the idempotency-key header is missing " +
		"or malformed)",
}, []string{"reason"})
