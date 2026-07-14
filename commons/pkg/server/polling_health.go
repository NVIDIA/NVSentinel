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

package server

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"
)

// epoch is the process-start reference point for monotonic elapsed time.
// Using time.Since(epoch) preserves Go's monotonic clock reading and is
// immune to wall-clock adjustments (NTP, etc.).
var epoch = time.Now()

// PollingHealthChecker implements HealthChecker for monitors that run a
// periodic polling loop. It reports unhealthy when the loop has not
// completed an iteration within the configured staleness threshold.
//
// All timestamps use the monotonic clock to avoid false positives from
// NTP adjustments.
//
// Usage:
//
//	hc := server.NewPollingHealthChecker(5 * time.Minute)
//	srv := server.NewServer(
//	    server.WithHealthCheck(hc),
//	    server.WithPrometheusMetrics(),
//	)
//
//	// In the polling loop:
//	for {
//	    doWork()
//	    hc.MarkAlive()
//	}
type PollingHealthChecker struct {
	stalenessThreshold time.Duration
	lastMonoNano       atomic.Int64
	// clock returns the current monotonic timestamp in nanoseconds.
	// Defaults to monoNow; overridden in tests for determinism.
	clock func() int64
}

func monoNow() int64 {
	return time.Since(epoch).Nanoseconds()
}

// NewPollingHealthChecker creates a PollingHealthChecker that reports
// unhealthy if MarkAlive has not been called within the given threshold.
// The initial state is alive to allow time for the first poll cycle.
func NewPollingHealthChecker(stalenessThreshold time.Duration) *PollingHealthChecker {
	hc := &PollingHealthChecker{
		stalenessThreshold: stalenessThreshold,
		clock:              monoNow,
	}
	hc.lastMonoNano.Store(hc.clock())

	return hc
}

// MarkAlive records that the polling loop is still running. This method
// is safe to call from any goroutine.
func (h *PollingHealthChecker) MarkAlive() {
	h.lastMonoNano.Store(h.clock())
}

// Healthy implements HealthChecker. It returns nil if MarkAlive was
// called within the staleness threshold, or an error otherwise.
func (h *PollingHealthChecker) Healthy(_ context.Context) error {
	elapsed := time.Duration(h.clock()-h.lastMonoNano.Load()) * time.Nanosecond

	if elapsed > h.stalenessThreshold {
		return fmt.Errorf(
			"polling loop stale: last iteration %s ago, threshold %s",
			elapsed.Truncate(time.Second), h.stalenessThreshold,
		)
	}

	return nil
}
