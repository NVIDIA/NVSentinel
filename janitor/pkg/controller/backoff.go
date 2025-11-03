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

package controller

import "time"

// getNextRequeueDelay calculates per-resource exponential backoff delay based on consecutive failures.
// This is used with ctrl.Result{RequeueAfter: delay} rather than the controller's built-in rate limiter
// because we need independent backoff per node based on each node's failure history, not global controller
// rate limiting. Each resource (RebootNode/TerminateNode) tracks its own ConsecutiveFailures counter
// and gets its own backoff schedule.
//
// Backoff schedule: 30s, 1m, 2m, 5m (capped at max after 3+ failures)
func getNextRequeueDelay(consecutiveFailures int32) time.Duration {
	delays := []time.Duration{
		30 * time.Second, // First retry after initial failure
		1 * time.Minute,  // Second retry
		2 * time.Minute,  // Third retry
		5 * time.Minute,  // Fourth+ retry (capped)
	}

	// Convert int32 to int for array indexing and cap at maximum if needed
	idx := int(consecutiveFailures)
	if idx >= len(delays) {
		return delays[len(delays)-1] // Cap at maximum
	}

	return delays[idx]
}
