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

	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/metrics"
)

// nodeCountResult is the result label on the get_total_nodes duration metric.
type nodeCountResult string

const (
	nodeCountSuccess nodeCountResult = "success"
	nodeCountError   nodeCountResult = "error"
)

// NewSlidingWindowBreaker creates a new sliding window circuit breaker for fault quarantine.
// It prevents cordoning more than a specified percentage of nodes within a time window.
// The breaker uses a ring buffer with 1-second granularity to track unique cordoned nodes.
func NewSlidingWindowBreaker(ctx context.Context, cfg Config) (CircuitBreaker, error) {
	if err := cfg.validateBounds(); err != nil {
		return nil, fmt.Errorf("validate circuit breaker bounds: %w", err)
	}

	// One bucket per second of the window: a non-positive window makes zero or a negative bucket count,
	// which panics when the ring buffer is allocated or indexed on the first cordon event.
	if cfg.Window <= 0 {
		return nil, fmt.Errorf("circuit breaker window must be a positive duration, got %s", cfg.Window)
	}

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

	err := cfg.K8sClient.EnsureCircuitBreakerConfigMap(ctx, cfg.ConfigMapName, cfg.ConfigMapNamespace, StateClosed)
	if err != nil {
		slog.ErrorContext(ctx, "Error ensuring circuit breaker config map", "error", err)
		return nil, fmt.Errorf("error ensuring circuit breaker config map: %w", err)
	}

	state, err := cfg.K8sClient.ReadCircuitBreakerState(ctx, cfg.ConfigMapName, cfg.ConfigMapNamespace)
	if err == nil {
		if state == StateClosed || state == StateTripped {
			b.state = state
		}
	}

	// A restored trip returns from IsTripped before any evaluation, so report the
	// state now or a latched breaker exports no series at all.
	metrics.SetFaultQuarantineBreakerState(string(b.state))

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

		clear(b.nodeToIndex)

		for i := range b.indexToNodes {
			clear(b.indexToNodes[i])
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

// Bound names the configured limit that produced the effective threshold. It appears in
// logs and as the label on fault_quarantine_breaker_threshold_nodes.
type Bound string

const (
	// boundNone is the zero value, reported when no bound is configured.
	boundNone       Bound = ""
	boundPercentage Bound = "percentage"
	boundMaxNodes   Bound = "maxNodes"
	// boundFleetSize is reported when a configured bound exceeded the fleet size and was
	// clamped to it.
	boundFleetSize Bound = "fleetSize"
	// boundMaxCordonedNodes is reported when the standing count of quarantined nodes, rather
	// than the rate inside Window, is what tripped the breaker.
	boundMaxCordonedNodes Bound = "maxCordonedNodes"
	// boundMaxCordonedPercentage is the percentage form of boundMaxCordonedNodes.
	boundMaxCordonedPercentage Bound = "maxCordonedPercentage"
	// boundNoGPUNodes is reported when no GPU nodes are counted, where any recent cordon trips
	// the breaker.
	boundNoGPUNodes Bound = "noGPUNodes"
)

// resolveThreshold turns a percentage bound and an absolute bound into a single node count,
// along with the bound that produced it. Shared by the rate pair and the standing pair so both
// resolve identically: when both are configured the lower one binds, so growing the fleet
// cannot silently raise the effective limit. Returns boundNone when neither is configured.
//
// Scaling a percentage above 100 is clamped to the fleet size here because the int conversion
// is the unsafe part, not because the result is unreachable; callers decide separately whether
// an absolute bound above the fleet size should be clamped.
func resolveThreshold(
	totalNodes int, percentage float64, percentageBound Bound, maxNodes int, maxNodesBound Bound,
) (int, Bound) {
	threshold, bound := 0, boundNone

	if percentage > 0 {
		// Compared as a float before converting: a percentage large enough to exceed
		// math.MaxInt makes the int conversion implementation-defined per the Go spec, and
		// a negative result would slip past the comparisons above and trip immediately.
		scaled := math.Ceil(float64(totalNodes) * percentage / 100)
		if scaled > float64(totalNodes) {
			threshold, bound = totalNodes, boundFleetSize
		} else {
			threshold, bound = int(scaled), percentageBound
		}
	}

	if maxNodes > 0 && (threshold == 0 || maxNodes < threshold) {
		threshold = maxNodes
		bound = maxNodesBound
	}

	return threshold, bound
}

// tripThreshold returns the recent-cordon count that trips the breaker for the given GPU
// node count, along with the bound that produced it.
func (b *slidingWindowBreaker) tripThreshold(totalNodes int) (int, Bound) {
	threshold, bound := resolveThreshold(totalNodes, b.cfg.TripPercentage, boundPercentage,
		b.cfg.TripMaxNodes, boundMaxNodes)

	// A threshold above the fleet size can never be reached, which would turn either bound
	// into an off switch for the breaker. Clamp so a too-large value degrades to "every node"
	// instead of "never trip"; the bound label reports when this happens. Safe here because
	// the window counter only ever counts cordon events this breaker recorded for GPU nodes,
	// so it cannot exceed the GPU fleet size.
	if threshold > totalNodes {
		threshold = totalNodes
		bound = boundFleetSize
	}

	return threshold, bound
}

// standingThreshold returns the standing quarantined-node count that trips the breaker for the
// given GPU node count, along with the bound that produced it. Resolved like tripThreshold but
// from the standing pair.
//
// Deliberately NOT clamped to the fleet size, unlike tripThreshold. The quarantined count comes
// from the annotation index over every watched node, while totalNodes counts only GPU-labelled
// ones (see NodeInformer.GetNodeCounts, which keeps watching all nodes so a quarantined node
// whose GPU label is temporarily absent stays recoverable). The count can therefore legitimately
// exceed totalNodes, and clamping would trip the breaker spuriously whenever it did.
func (b *slidingWindowBreaker) standingThreshold(totalNodes int) (int, Bound) {
	return resolveThreshold(totalNodes, b.cfg.TripMaxCordonedPercentage, boundMaxCordonedPercentage,
		b.cfg.TripMaxCordonedNodes, boundMaxCordonedNodes)
}

// standingBoundReached reports whether the number of nodes NVSentinel currently holds
// quarantined has reached the given standing threshold, and returns that count for logging.
// The threshold and bound come from standingThreshold.
//
// The window bounds limit the rate of cordoning, not the total, so a rate that stays under
// the threshold still cordons without limit given enough windows. This bound is the only
// one that caps how many nodes are held at once. Opt-in: boundNone skips the lookup entirely,
// so a deployment configuring neither standing bound makes no extra API call.
func (b *slidingWindowBreaker) standingBoundReached(
	ctx context.Context, threshold int, bound Bound,
) (int, bool, error) {
	if bound == boundNone {
		return -1, false, nil
	}

	cordonedNodes, err := b.cfg.K8sClient.GetCordonedNodes(ctx)
	if err != nil {
		return -1, false, fmt.Errorf("failed to get cordoned node count: %w", err)
	}

	return cordonedNodes, cordonedNodes >= threshold, nil
}

// IsTripped checks if the circuit breaker should prevent further node cordoning.
// It returns true if:
// 1. The breaker is already in TRIPPED state, OR
// 2. Recent cordon events reach the configured threshold
// The threshold is the lower of the configured percentage of GPU nodes and TripMaxNodes,
// clamped to the fleet size. The breaker trips when the recent-cordon count reaches it.
func (b *slidingWindowBreaker) IsTripped(ctx context.Context) (bool, error) {
	b.mu.RLock()

	if b.state == StateTripped {
		b.mu.RUnlock()

		return true, nil
	}

	b.mu.RUnlock()

	totalNodes, err := b.getTotalNodes(ctx)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to get total nodes", "error", err)

		return false, fmt.Errorf("failed to get total nodes: %w", err)
	}

	if totalNodes == 0 {
		return b.isTrippedByRecentCordons(ctx)
	}

	now := time.Now()

	b.mu.Lock()

	b.slideWindow(now)
	recentCordonedNodes := b.sumBuckets()
	threshold, bindingBound := b.tripThreshold(totalNodes)
	// A threshold of 0 means no window bound is configured. Comparing against it with >=
	// would trip on every evaluation with no cordons at all, so the rate check only
	// applies when a window bound actually exists.
	shouldTrip := bindingBound != boundNone && recentCordonedNodes >= threshold

	b.mu.Unlock()

	// Only consulted when the window bounds have not already tripped. The standing bound can
	// add a reason to trip but must never remove one: a lookup failure here would otherwise
	// return early and leave a breaker unlatched that the window bounds had already earned.
	cordonedNodes := -1

	if !shouldTrip {
		standingLimit, standingBound := b.standingThreshold(totalNodes)

		var reached bool

		cordonedNodes, reached, err = b.standingBoundReached(ctx, standingLimit, standingBound)
		if err != nil {
			slog.ErrorContext(ctx, "Failed to get cordoned node count", "error", err)

			return false, err
		}

		if reached {
			threshold, bindingBound = standingLimit, standingBound
			shouldTrip = true
		}
	}

	slog.DebugContext(ctx, "Recent cordoned nodes status",
		"recentCordonedNodes", recentCordonedNodes,
		"cordonedNodes", cordonedNodes,
		"totalNodes", totalNodes,
		"tripPercentage", b.cfg.TripPercentage,
		"tripMaxNodes", b.cfg.TripMaxNodes,
		"tripMaxCordonedNodes", b.cfg.TripMaxCordonedNodes,
		"tripMaxCordonedPercentage", b.cfg.TripMaxCordonedPercentage,
		"threshold", threshold,
		"bindingBound", bindingBound)

	metrics.SetFaultQuarantineBreakerUtilization(float64(recentCordonedNodes) / float64(totalNodes))
	metrics.SetFaultQuarantineBreakerThresholdNodes(float64(threshold), string(bindingBound))

	if shouldTrip {
		err := b.ForceState(ctx, StateTripped)
		if err != nil {
			slog.ErrorContext(ctx, "Error forcing circuit breaker state to TRIPPED", "error", err)
			return true, fmt.Errorf("error forcing circuit breaker state to TRIPPED: %w", err)
		}

		return true, nil
	}

	metrics.SetFaultQuarantineBreakerState(string(StateClosed))

	return false, nil
}

// ForceState manually sets the circuit breaker state to CLOSED or TRIPPED.
// This bypasses the normal threshold checking and directly controls the breaker state.
// If a WriteStateFn is configured, it persists the state change. This method is thread-safe.
func (b *slidingWindowBreaker) ForceState(ctx context.Context, s State) error {
	b.mu.Lock()
	b.state = s
	b.mu.Unlock()

	metrics.SetFaultQuarantineBreakerState(string(s))

	err := b.cfg.K8sClient.WriteCircuitBreakerState(
		ctx, b.cfg.ConfigMapName, b.cfg.ConfigMapNamespace, s)
	if err != nil {
		slog.ErrorContext(ctx, "Error writing circuit breaker state", "error", err)
		return fmt.Errorf("error writing circuit breaker state: %w", err)
	}

	slog.InfoContext(ctx, "ForceState changed", "state", s)

	return nil
}

// CurrentState returns the current state of the circuit breaker (CLOSED or TRIPPED).
// This method is thread-safe and provides read-only access to the breaker state.
func (b *slidingWindowBreaker) CurrentState() State {
	b.mu.RLock()
	defer b.mu.RUnlock()

	return b.state
}

func (b *slidingWindowBreaker) GetCursorMode(ctx context.Context) (CursorMode, error) {
	return b.cfg.K8sClient.ReadCursorMode(ctx, b.cfg.ConfigMapName, b.cfg.ConfigMapNamespace)
}

func (b *slidingWindowBreaker) SetCursorMode(ctx context.Context, mode CursorMode) error {
	return b.cfg.K8sClient.WriteCursorMode(ctx, b.cfg.ConfigMapName, b.cfg.ConfigMapNamespace, mode)
}

// getTotalNodes reads the GPU node count from the synced NodeInformer cache.
func (b *slidingWindowBreaker) getTotalNodes(ctx context.Context) (int, error) {
	startTime := time.Now()
	result := nodeCountSuccess

	defer func() {
		metrics.FaultQuarantineGetTotalNodesDuration.WithLabelValues(string(result)).Observe(time.Since(startTime).Seconds())
	}()

	totalNodes, err := b.cfg.K8sClient.GetTotalNodes(ctx)
	if err != nil {
		result = nodeCountError

		metrics.FaultQuarantineGetTotalNodesErrors.WithLabelValues("api_error").Inc()

		return 0, fmt.Errorf("GetTotalNodes failed: %w", err)
	}

	return totalNodes, nil
}

// isTrippedByRecentCordons is called when no GPU nodes are counted. It trips if the window
// holds any cordon, since that means gpuNodeLabel does not match the nodes being
// quarantined. Otherwise the breaker stays CLOSED.
func (b *slidingWindowBreaker) isTrippedByRecentCordons(ctx context.Context) (bool, error) {
	b.mu.Lock()
	b.slideWindow(time.Now())
	recentCordonedNodes := b.sumBuckets()
	b.mu.Unlock()

	metrics.SetFaultQuarantineBreakerThresholdNodes(1, string(boundNoGPUNodes))

	if recentCordonedNodes == 0 {
		metrics.SetFaultQuarantineBreakerUtilization(0)
		metrics.SetFaultQuarantineBreakerState(string(StateClosed))

		return false, nil
	}

	slog.ErrorContext(ctx, "Circuit breaker: nodes were cordoned but no GPU nodes are counted; tripping",
		"recentCordonedNodes", recentCordonedNodes)

	if err := b.ForceState(ctx, StateTripped); err != nil {
		return true, fmt.Errorf("error forcing circuit breaker state to TRIPPED: %w", err)
	}

	return true, nil
}
