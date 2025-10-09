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

// Package breaker provides types and interfaces for the fault quarantine circuit breaker.
package breaker

import (
	"context"
	"sync"
	"time"
)

// State represents the current state of the circuit breaker
type State string

const (
	// StateClosed indicates the breaker is allowing operation
	StateClosed State = "CLOSED"
	// StateTripped indicates the breaker is blocking operations
	StateTripped State = "TRIPPED"
)

type CircuitBreaker interface {
	// AddCordonEvent records a node cordoning event in the sliding window
	AddCordonEvent(nodeName string)
	// IsTripped checks if the breaker should prevent further cordoning
	IsTripped(ctx context.Context) (bool, error)
	// ForceState manually sets the breaker state (CLOSED or TRIPPED)
	ForceState(ctx context.Context, s State) error
	// CurrentState returns the current breaker state
	CurrentState() State
}

// Config holds the configuration parameters for the sliding window circuit breaker.
// It defines the time window, trip threshold, and optional persistence hooks.
type Config struct {
	// Window defines the sliding time window over which cordon events are counted.
	// Default: 5 minutes. Events older than this window are automatically discarded.
	Window time.Duration

	// TripPercentage is the fraction of total nodes that, if exceeded by recent cordon
	// events within Window, will trip the breaker (e.g., 50 for 50%).
	// Default: 50 (50% of nodes).
	TripPercentage float64

	// GetTotalNodes returns the current total number of nodes in the cluster.
	// Used to compute the dynamic trip threshold as total_nodes * TripPercentage.
	// This allows the threshold to adapt to cluster scaling events.
	GetTotalNodes func(ctx context.Context) (int, error)

	// MaxRetries is the maximum number of retry attempts when GetTotalNodes returns 0
	// Default: 10 retries (allows ~30 seconds for cache sync with exponential backoff)
	MaxRetries int

	// InitialRetryDelay is the base delay for the first retry attempt
	// Default: 100ms, exponentially increases with each retry
	InitialRetryDelay time.Duration

	// MaxRetryDelay caps the maximum delay between retry attempts
	// Default: 5 seconds (prevents excessive delays)
	MaxRetryDelay time.Duration

	// EnsureConfigMap creates/initializes the ConfigMap for state persistence
	EnsureConfigMap func(ctx context.Context, initial State) error
	// ReadStateFn reads the persisted breaker state from ConfigMap
	ReadStateFn func(ctx context.Context) (State, error)
	// WriteStateFn persists the current breaker state to ConfigMap
	WriteStateFn func(ctx context.Context, s State) error
}

// slidingWindowBreaker implements CircuitBreaker using a ring buffer approach.
// It tracks unique nodes that have been cordoned in time-based buckets and automatically
// expires old events as the window slides forward.
type slidingWindowBreaker struct {
	// cfg holds the breaker configuration (window size, trip ratio, etc.)
	cfg Config
	// mu protects all mutable fields from concurrent access
	mu sync.RWMutex

	// Ring buffer implementation for sliding window
	// bucketSize defines the duration represented by each bucket (fixed at 1 second)
	bucketSize time.Duration
	// buckets holds per-bucket counts of recent cordon events forming a ring buffer
	// For a 5-minute window, this contains 300 buckets (5 * 60 seconds)
	buckets []int
	// startTime marks the time corresponding to buckets[0]; used to advance the ring
	// as time progresses, ensuring the window slides correctly
	startTime time.Time

	// Node tracking for unique cordon events within the sliding window
	// nodeToIndex maps node name to the bucket index where it was last cordoned
	nodeToIndex map[string]int
	// indexToNodes maps bucket index to a set of node names cordoned in that bucket
	indexToNodes map[int]map[string]bool

	// state is the current breaker state (CLOSED or TRIPPED)
	// Can be manually forced via ForceState() or automatically set by IsTripped()
	state State
}
