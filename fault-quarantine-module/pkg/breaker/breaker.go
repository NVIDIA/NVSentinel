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

// Package breaker implements a sliding window circuit breaker for fault quarantine.
// It prevents excessive node cordoning that could destabilize Kubernetes clusters
// by tracking cordon events over time and blocking further operations when thresholds are exceeded.
//
// The implementation uses a ring buffer to efficiently track events within a sliding time window,
// providing predictable performance and memory usage regardless of cluster activity levels.
package breaker

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"time"

	"golang.org/x/exp/maps"
)

const (
	resultError = "error"
)

// NewSlidingWindowBreaker creates a new sliding window circuit breaker for fault quarantine.
// It prevents cordoning more than a specified percentage of nodes within a time window.
// The breaker uses a ring buffer with 1-second granularity to track unique cordoned nodes.
func NewSlidingWindowBreaker(ctx context.Context, cfg Config) (CircuitBreaker, error) {
	numBuckets := int((cfg.Window + time.Second - 1) / time.Second)
	b := &slidingWindowBreaker{
		cfg:          cfg,
		bucketSize:   time.Second,
		buckets:      make([]int, numBuckets),
		startTime:    time.Now(),
		state:        StateClosed,
		nodeToIndex:  make(map[string]int),
		indexToNodes: make(map[int]map[string]bool),
	}

	// Initialize indexToNodes for all buckets
	for i := range numBuckets {
		b.indexToNodes[i] = make(map[string]bool)
	}

	err := cfg.EnsureConfigMap(ctx, StateClosed)
	if err != nil {
		slog.Error("Error ensuring circuit breaker config map", "error", err)
		return nil, fmt.Errorf("error ensuring circuit breaker config map: %w", err)
	}

	if s, err := cfg.ReadStateFn(ctx); err == nil && (s == StateClosed || s == StateTripped) {
		b.state = s
	}

	return b, nil
}

// slideWindowToCurrentTimeLocked advances the ring buffer to the current time by shifting buckets.
// This method must be called with the mutex locked. It calculates elapsed time since
// the last update and shifts the ring buffer accordingly, clearing old buckets and node mappings.
func (b *slidingWindowBreaker) slideWindow(now time.Time) {
	elapsed := now.Sub(b.startTime)
	if elapsed <= 0 {
		return
	}

	steps := int(elapsed / b.bucketSize)
	if steps >= len(b.buckets) {
		// If we've elapsed more than the entire window, clear everything
		for i := range b.buckets {
			b.buckets[i] = 0
		}

		maps.Clear(b.nodeToIndex)

		for i := range b.indexToNodes {
			maps.Clear(b.indexToNodes[i])
		}

		b.startTime = now.Truncate(b.bucketSize)

		return
	}

	for range steps {
		// Clean up node mappings for the bucket being shifted out (bucket 0)
		if expiredNodes, ok := b.indexToNodes[0]; ok {
			for nodeName := range expiredNodes {
				delete(b.nodeToIndex, nodeName)
			}
		}

		// Shift ring buffer by one bucket
		copy(b.buckets, b.buckets[1:])
		b.buckets[len(b.buckets)-1] = 0

		// Shift node mappings
		for i := range len(b.indexToNodes) - 1 {
			b.indexToNodes[i] = b.indexToNodes[i+1]
		}

		b.indexToNodes[len(b.indexToNodes)-1] = make(map[string]bool)

		// Update all node indices (decrement by 1)
		for nodeName, index := range b.nodeToIndex {
			b.nodeToIndex[nodeName] = index - 1
		}

		b.startTime = b.startTime.Add(b.bucketSize)
	}
}

// AddCordonEvent records a new node cordoning event in the sliding window.
// It advances the ring buffer to the current time and tracks the node uniquely
// within the sliding window. This method is thread-safe.
func (b *slidingWindowBreaker) AddCordonEvent(nodeName string) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := time.Now()
	b.slideWindow(now)

	currentBucketIndex := len(b.buckets) - 1

	// Check if this node was already cordoned in the current window
	if oldIndex, exists := b.nodeToIndex[nodeName]; exists {
		// Node was already cordoned in this window, remove from old bucket
		if oldBucketNodes, ok := b.indexToNodes[oldIndex]; ok {
			delete(oldBucketNodes, nodeName)

			b.buckets[oldIndex]--
		}
	}

	slog.Debug("Adding node to current bucket",
		"node", nodeName,
		"bucket", currentBucketIndex)
	// Add node to current bucket
	b.nodeToIndex[nodeName] = currentBucketIndex
	b.indexToNodes[currentBucketIndex][nodeName] = true
	b.buckets[currentBucketIndex]++
}

// sumBucketsLocked calculates the total number of cordon events across all buckets
// in the sliding window. This method must be called with the mutex locked.
// Returns the sum of all bucket values representing recent cordon events.
func (b *slidingWindowBreaker) sumBuckets() int {
	sum := 0

	for _, v := range b.buckets {
		sum += v
	}

	return sum
}

// IsTripped checks if the circuit breaker should prevent further node cordoning.
// It returns true if:
// 1. The breaker is already in TRIPPED state, OR
// 2. Recent cordon events exceed the configured threshold (TripPercentage * total nodes)
// The method automatically trips the breaker if the threshold is exceeded.
func (b *slidingWindowBreaker) IsTripped(ctx context.Context) (bool, error) {
	b.mu.RLock()
	if b.state == StateTripped {
		b.mu.RUnlock()
		return true, nil
	}
	b.mu.RUnlock()

	totalNodes, err := b.getTotalNodesWithRetry(ctx)
	if err != nil {
		slog.Error("Failed to get total nodes after retries", "error", err)
		return false, fmt.Errorf("failed to get total nodes after retries: %w", err)
	}

	if totalNodes == 0 {
		slog.Error("Total nodes is still 0 after all retry attempts - cluster may have no GPU nodes")
		return false, fmt.Errorf("total nodes is 0 after retries")
	}

	now := time.Now()

	b.mu.Lock()
	b.slideWindow(now)
	recentCordonedNodes := b.sumBuckets()
	threshold := int(math.Ceil(float64(totalNodes) * b.cfg.TripPercentage / 100))
	shouldTrip := recentCordonedNodes >= threshold

	slog.Debug("Recent cordoned nodes status",
		"recentCordonedNodes", recentCordonedNodes,
		"totalNodes", totalNodes,
		"tripPercentage", b.cfg.TripPercentage)

	SetFaultQuarantineBreakerUtilization(float64(recentCordonedNodes) / float64(totalNodes))
	b.mu.Unlock()

	if shouldTrip {
		err := b.ForceState(ctx, StateTripped)
		if err != nil {
			slog.Error("Error forcing circuit breaker state to TRIPPED", "error", err)
			return true, fmt.Errorf("error forcing circuit breaker state to TRIPPED: %w", err)
		}

		SetFaultQuarantineBreakerState(StateTripped)

		return true, nil
	}

	SetFaultQuarantineBreakerState(StateClosed)

	return false, nil
}

// ForceState manually sets the circuit breaker state to CLOSED or TRIPPED.
// This bypasses the normal threshold checking and directly controls the breaker state.
// If a WriteStateFn is configured, it persists the state change. This method is thread-safe.
func (b *slidingWindowBreaker) ForceState(ctx context.Context, s State) error {
	b.mu.Lock()
	b.state = s
	b.mu.Unlock()

	if err := b.cfg.WriteStateFn(ctx, s); err != nil {
		slog.Error("Error writing circuit breaker state", "error", err)
		return fmt.Errorf("failed to write circuit breaker state %s: %w", s, err)
	}

	slog.Info("ForceState changed", "state", s)

	return nil
}

// CurrentState returns the current state of the circuit breaker (CLOSED or TRIPPED).
// This method is thread-safe and provides read-only access to the breaker state.
func (b *slidingWindowBreaker) CurrentState() State {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.state
}

// getTotalNodesWithRetry gets the total number of nodes with retry logic and exponential backoff.
// This handles NodeInformer cache sync delays that can cause GetTotalNodes to temporarily return 0.
func (b *slidingWindowBreaker) getTotalNodesWithRetry(ctx context.Context) (int, error) {
	startTime := time.Now()

	var result string

	var errorType string

	defer func() {
		duration := time.Since(startTime).Seconds()
		faultQuarantineGetTotalNodesDuration.WithLabelValues(result).Observe(duration)

		if errorType != "" {
			faultQuarantineGetTotalNodesErrors.WithLabelValues(errorType).Inc()
		}
	}()

	maxRetries, initialDelay, maxDelay := b.getRetryConfig()

	var lastErr error

	for attempt := 0; attempt <= maxRetries; attempt++ {
		totalNodes, err := b.cfg.GetTotalNodes(ctx)

		if err != nil {
			result = resultError
			errorType = "api_error"

			return b.handleGetTotalNodesError(err, attempt, maxRetries)
		}

		if totalNodes > 0 {
			result = "success"

			faultQuarantineGetTotalNodesRetryAttempts.Observe(float64(attempt))

			return b.handleSuccessfulNodeCount(totalNodes, attempt)
		}

		// Store error for final return (only last value is used)
		//nolint:staticcheck // SA4006: intermediate values overwritten, only final used
		lastErr = b.handleZeroNodes(attempt, maxRetries)

		if attempt < maxRetries {
			if err := b.performRetryDelay(ctx, attempt, maxRetries, initialDelay, maxDelay); err != nil {
				result = resultError
				errorType = "context_cancelled"

				return 0, fmt.Errorf("context cancelled during GetTotalNodes retry: %w", err)
			}
		}
	}

	// All retries exhausted
	if err := b.logRetriesExhausted(maxRetries, initialDelay, maxDelay); err != nil {
		return 0, fmt.Errorf("error logging retries exhausted: %w", err)
	}

	result = resultError
	errorType = "zero_nodes"

	return 0, lastErr
}

// getRetryConfig extracts and validates retry configuration with defaults
func (b *slidingWindowBreaker) getRetryConfig() (int, time.Duration, time.Duration) {
	maxRetries := b.cfg.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 10 // Default: 10 retries
	}

	initialDelay := b.cfg.InitialRetryDelay
	if initialDelay <= 0 {
		initialDelay = 100 * time.Millisecond // Default: 100ms
	}

	maxDelay := b.cfg.MaxRetryDelay
	if maxDelay <= 0 {
		maxDelay = 5 * time.Second // Default: 5 seconds
	}

	return maxRetries, initialDelay, maxDelay
}

// handleGetTotalNodesError handles API errors from GetTotalNodes
func (b *slidingWindowBreaker) handleGetTotalNodesError(err error, attempt, maxRetries int) (int, error) {
	slog.Error("GetTotalNodes failed on attempt",
		"attempt", attempt+1,
		"maxAttempts", maxRetries+1,
		"error", err)

	return 0, fmt.Errorf("GetTotalNodes failed: %w", err)
}

// handleSuccessfulNodeCount handles the success case when nodes > 0
func (b *slidingWindowBreaker) handleSuccessfulNodeCount(totalNodes, attempt int) (int, error) {
	if attempt > 0 {
		slog.Info("Circuit breaker retry successful",
			"totalNodes", totalNodes,
			"attempts", attempt+1)
	}

	return totalNodes, nil
}

// handleZeroNodes handles the case when GetTotalNodes returns 0
func (b *slidingWindowBreaker) handleZeroNodes(attempt, maxRetries int) error {
	lastErr := fmt.Errorf("GetTotalNodes returned 0 nodes (likely NodeInformer cache not synced yet)")

	if attempt == 0 {
		slog.Info("Circuit breaker starting retries: NodeInformer cache may not be synced yet",
			"maxRetries", maxRetries)
	}

	return lastErr
}

// performRetryDelay calculates and performs the exponential backoff delay
func (b *slidingWindowBreaker) performRetryDelay(ctx context.Context, attempt, maxRetries int,
	initialDelay, maxDelay time.Duration) error {
	delay := b.calculateBackoffDelay(attempt, initialDelay, maxDelay)

	slog.Debug("Circuit breaker retry; got 0 nodes, retrying (NodeInformer cache may still be syncing)",
		"attempt", attempt+1,
		"maxRetries", maxRetries,
		"delay", delay)

	select {
	case <-ctx.Done():
		return fmt.Errorf("context cancelled during retry: %w", ctx.Err())
	case <-time.After(delay):
	}

	return nil
}

// calculateBackoffDelay calculates exponential backoff delay with overflow protection
func (b *slidingWindowBreaker) calculateBackoffDelay(attempt int,
	initialDelay, maxDelay time.Duration) time.Duration {
	if attempt > 30 || attempt < 0 { // Prevent overflow for very large or negative attempts
		return maxDelay
	}

	// Safe conversion: attempt is guaranteed to be [0, 30] at this point
	safeAttempt := uint(attempt)          //nolint:gosec // Range validated above
	multiplier := int64(1 << safeAttempt) // 2^attempt as integer
	delay := time.Duration(int64(initialDelay) * multiplier)

	if delay > maxDelay || delay < 0 { // Check for overflow
		delay = maxDelay
	}

	return delay
}

// logRetriesExhausted logs a summary when all retries are exhausted and crashes the pod.
// It attempts to get the actual node count for accurate error context.
// Returns an error if unable to get the node count.
func (b *slidingWindowBreaker) logRetriesExhausted(maxRetries int, initialDelay, maxDelay time.Duration) error {
	// Get the actual node count from the last attempt to provide accurate error context
	ctx := context.Background()
	actualNodes, err := b.cfg.GetTotalNodes(ctx)

	if err != nil {
		slog.Error(
			"Circuit breaker: All retry attempts exhausted; failed to get node count from Kubernetes API; pod will restart",
			"maxRetries", maxRetries,
			"error", err,
			"initialDelay", initialDelay,
			"totalClusterNodes", actualNodes,
			"maxDelay", maxDelay)

		return nil
	}

	return fmt.Errorf("error getting total nodes after %d retries", maxRetries)
}
