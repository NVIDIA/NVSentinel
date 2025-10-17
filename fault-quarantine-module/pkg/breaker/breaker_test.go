// Copyright (c) 2024, NVIDIA CORPORATION.  All rights reserved.
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

package breaker

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func newTestBreaker(t *testing.T, totalNodes int, tripPercentage float64, window time.Duration, opts ...func(*Config)) CircuitBreaker {
	t.Helper()
	ctx := context.Background()
	callsEnsure := 0
	callsWrite := 0
	cfg := Config{
		Window:         window,
		TripPercentage: tripPercentage,
		GetTotalNodes: func(context.Context) (int, error) {
			return totalNodes, nil
		},
		EnsureConfigMap: func(context.Context, State) error {
			callsEnsure++
			return nil
		},
		ReadStateFn:  func(context.Context) (State, error) { return StateClosed, nil },
		WriteStateFn: func(context.Context, State) error { callsWrite++; return nil },
	}
	for _, opt := range opts {
		opt(&cfg)
	}
	b, err := NewSlidingWindowBreaker(ctx, cfg)
	if err != nil {
		t.Fatalf("failed to create breaker: %v", err)
	}
	// Ensure constructor hook was called once
	if callsEnsure != 1 {
		t.Fatalf("expected EnsureConfigMap to be called once, got %d", callsEnsure)
	}
	// Use callsWrite to avoid unused warning via ForceState in tests
	_ = callsWrite
	return b
}

func TestDoesNotTripBelowThreshold(t *testing.T) {
	b := newTestBreaker(t, 10, 50, 1*time.Second)
	ctx := context.Background()

	// threshold = int(10*0.5) = 5, must exceed to trip
	for i := 0; i < 5; i++ {
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}
	if _, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	}
}

func TestTripsWhenAboveThreshold(t *testing.T) {
	b := newTestBreaker(t, 10, 50, 1*time.Second)
	ctx := context.Background()

	for i := 0; i < 6; i++ { // exceed threshold 5
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if !tripped {
		t.Fatalf("breaker should trip when above threshold")
	}
}

func TestForceStateOverridesComputation(t *testing.T) {
	b := newTestBreaker(t, 10, 50, 1*time.Second)
	ctx := context.Background()

	if err := b.ForceState(ctx, StateTripped); err != nil {
		t.Fatalf("force trip failed: %v", err)
	}
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if !tripped {
		t.Fatalf("breaker should report tripped after ForceState(StateTripped)")
	}

	if err := b.ForceState(ctx, StateClosed); err != nil {
		t.Fatalf("force close failed: %v", err)
	}
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if tripped {
		t.Fatalf("breaker should not be tripped after ForceState(StateClosed)")
	}
}

func TestWindowExpiryResetsCounts(t *testing.T) {
	b := newTestBreaker(t, 10, 50, 1*time.Second)
	ctx := context.Background()

	for i := 0; i < 6; i++ { // exceed threshold
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if !tripped {
		t.Fatalf("breaker should trip when above threshold")
	}

	// Close it again so we can test reset via window advance
	if err := b.ForceState(ctx, StateClosed); err != nil {
		t.Fatalf("force close failed: %v", err)
	}

	// Wait for window to roll over; buckets are 1s granularity
	time.Sleep(1100 * time.Millisecond)

	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if tripped {
		t.Fatalf("breaker should not trip after window expiry with no new events")
	}
}

func TestInitializeFromReadState(t *testing.T) {
	ctx := context.Background()
	// Start with TRIPPED in persisted state
	b, err := NewSlidingWindowBreaker(ctx, Config{
		Window:         1 * time.Second,
		TripPercentage: 50,
		GetTotalNodes: func(context.Context) (int, error) {
			return 10, nil
		},
		EnsureConfigMap: func(context.Context, State) error { return nil },
		ReadStateFn:     func(context.Context) (State, error) { return StateTripped, nil },
		WriteStateFn:    func(context.Context, State) error { return nil },
	})
	if err != nil {
		t.Fatalf("failed to create breaker: %v", err)
	}
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if !tripped {
		t.Fatalf("breaker should trip when above threshold")
	}
	if got := b.CurrentState(); got != StateTripped {
		t.Fatalf("expected initial state TRIPPED, got %s", got)
	} else if got != StateTripped {
		t.Fatalf("expected initial state TRIPPED, got %s", got)
	}
}

func TestFlappingNodeDoesNotMultiplyCount(t *testing.T) {
	b := newTestBreaker(t, 10, 50, 5*time.Second) // 5-second window, 50% threshold = 5 nodes
	ctx := context.Background()

	// Add the same node multiple times (simulating flapping)
	for i := 0; i < 10; i++ {
		b.AddCordonEvent("flapping-node")
	}

	// Should not trip because it's only 1 unique node, not 6
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if tripped {
		t.Fatalf("breaker should not trip for single flapping node")
	}

	// Add 5 more unique nodes (total 6 unique nodes)
	for i := 0; i < 5; i++ {
		b.AddCordonEvent(fmt.Sprintf("node%d", i))
	}

	// Now should trip because we have 6 unique nodes
	if tripped, err := b.IsTripped(ctx); err != nil {
		t.Fatalf("error checking if breaker should trip: %v", err)
	} else if !tripped {
		t.Fatalf("breaker should trip with 6 unique nodes (exceeds 5 threshold)")
	}
}
