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
	"math"
	"time"

	"golang.org/x/exp/maps"
	"k8s.io/klog/v2"
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
		klog.Errorf("Error ensuring circuit breaker config map: %v", err)
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

	klog.V(3).Infof("Adding node %s to current bucket %d", nodeName, currentBucketIndex)
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

	totalNodes, err := b.cfg.GetTotalNodes(ctx)
	if err != nil {
		klog.Errorf("Error getting total nodes: %v", err)
		return false, fmt.Errorf("error getting total nodes: %w", err)
	}

	if totalNodes == 0 {
		klog.Errorf("Total nodes is 0")
		return false, fmt.Errorf("total nodes is 0")
	}

	now := time.Now()

	b.mu.Lock()
	b.slideWindow(now)
	recentCordonedNodes := b.sumBuckets()
	threshold := int(math.Ceil(float64(totalNodes) * b.cfg.TripPercentage / 100))
	shouldTrip := recentCordonedNodes >= threshold

	klog.Infof("Recent Total Cordoned Nodes: %d, Total Nodes: %d, TripPercentage: %f",
		recentCordonedNodes, totalNodes, b.cfg.TripPercentage)

	SetFaultQuarantineBreakerUtilization(float64(recentCordonedNodes) / float64(totalNodes))
	b.mu.Unlock()

	if shouldTrip {
		err := b.ForceState(ctx, StateTripped)
		if err != nil {
			klog.Errorf("Error forcing circuit breaker state to TRIPPED: %v", err)
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
		klog.Errorf("Error writing circuit breaker state: %v", err)
		return err
	}

	klog.Infof("ForceState: %s", s)

	return nil
}

// CurrentState returns the current state of the circuit breaker (CLOSED or TRIPPED).
// This method is thread-safe and provides read-only access to the breaker state.
func (b *slidingWindowBreaker) CurrentState() State {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.state
}
