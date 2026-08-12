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
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPollingHealthChecker_InitiallyHealthy(t *testing.T) {
	hc := NewPollingHealthChecker(5 * time.Minute)

	if err := hc.Healthy(context.Background()); err != nil {
		t.Fatalf("expected healthy immediately after creation, got: %v", err)
	}
}

func TestPollingHealthChecker_MarkAliveResetsStale(t *testing.T) {
	var now atomic.Int64
	hc := &PollingHealthChecker{
		stalenessThreshold: 10 * time.Second,
		clock:              func() int64 { return now.Load() },
	}
	now.Store(0)
	hc.lastMonoNano.Store(hc.clock())

	// Advance past threshold.
	now.Store((15 * time.Second).Nanoseconds())
	if err := hc.Healthy(context.Background()); err == nil {
		t.Fatal("expected stale error after 15s with 10s threshold")
	}

	// MarkAlive resets.
	hc.MarkAlive()
	if err := hc.Healthy(context.Background()); err != nil {
		t.Fatalf("expected healthy after MarkAlive, got: %v", err)
	}
}

func TestPollingHealthChecker_StaleAfterThreshold(t *testing.T) {
	var now atomic.Int64
	hc := &PollingHealthChecker{
		stalenessThreshold: 30 * time.Second,
		clock:              func() int64 { return now.Load() },
	}
	now.Store(0)
	hc.lastMonoNano.Store(hc.clock())

	// At exactly threshold: not stale (> is the check, not >=).
	now.Store((30 * time.Second).Nanoseconds())
	if err := hc.Healthy(context.Background()); err != nil {
		t.Fatalf("expected healthy at exact threshold, got: %v", err)
	}

	// One nanosecond past threshold: stale.
	now.Store((30 * time.Second).Nanoseconds() + 1)
	err := hc.Healthy(context.Background())
	if err == nil {
		t.Fatal("expected stale error just past threshold")
	}
	if !strings.Contains(err.Error(), "polling loop stale") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestPollingHealthChecker_StaleErrorContainsDetails(t *testing.T) {
	var now atomic.Int64
	hc := &PollingHealthChecker{
		stalenessThreshold: 1 * time.Minute,
		clock:              func() int64 { return now.Load() },
	}
	now.Store(0)
	hc.lastMonoNano.Store(hc.clock())

	now.Store((90 * time.Second).Nanoseconds())
	err := hc.Healthy(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}

	msg := err.Error()
	if !strings.Contains(msg, "1m30s") {
		t.Errorf("expected elapsed duration in error, got: %s", msg)
	}
	if !strings.Contains(msg, "1m0s") {
		t.Errorf("expected threshold in error, got: %s", msg)
	}
}
