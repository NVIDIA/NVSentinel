// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/nvidia/nvsentinel/commons/pkg/healthpub"
	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
)

// pollCategories are the polling loops the stall watchdog tracks.
var pollCategories = []string{"state", "counter"}

// pollStall reports a poll that stops completing. When the host stalls the
// sysfs reads a poll makes, the loop freezes and liveness restarts the
// container, repeatedly, without anything saying that NIC state cannot be
// observed. The watchdog publishes that as an event before liveness acts.
//
// One node-level event covers both loops: unhealthy once any category has
// been in flight past the deadline, healthy once none is. The first poll to
// complete after start publishes a healthy baseline, which closes a stall
// left open by a liveness restart.
type pollStall struct {
	deadline time.Duration
	strategy pb.ProcessingStrategy
	now      func() time.Time

	mu        sync.Mutex
	started   map[string]time.Time
	reported  bool
	baselined bool

	// publishMu serializes stall publications so a recovery can never be
	// delivered ahead of the stall it clears.
	publishMu sync.Mutex
}

// EnablePollStallDetection turns on the poll stall watchdog. A deadline of
// zero or less leaves it off. Call before the polling loops start.
func (m *NICHealthMonitor) EnablePollStallDetection(deadline time.Duration, strategy pb.ProcessingStrategy) {
	if deadline <= 0 {
		return
	}

	m.stall = &pollStall{
		deadline: deadline,
		strategy: strategy,
		now:      time.Now,
		started:  map[string]time.Time{},
	}

	for _, category := range pollCategories {
		metrics.PollStalled.WithLabelValues(m.nodeName, category).Set(0)
	}

	slog.Info("NIC poll stall detection enabled", "deadline", deadline, "processing_strategy", strategy.String())
}

// RunPollStallWatchdog checks for stalled polls until ctx is cancelled. It
// returns at once when stall detection is off.
func (m *NICHealthMonitor) RunPollStallWatchdog(ctx context.Context) error {
	if m.stall == nil {
		return nil
	}

	interval := min(time.Second, m.stall.deadline/4)
	ticker := time.NewTicker(interval)

	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			m.checkPollStalls(ctx)
		}
	}
}

// beginPoll records that a poll of category is in flight.
func (m *NICHealthMonitor) beginPoll(category string) {
	if m.stall == nil {
		return
	}

	m.stall.mu.Lock()
	m.stall.started[category] = m.stall.now()
	m.stall.mu.Unlock()
}

// endPoll records that a poll of category completed, and publishes the
// healthy event when it ends a reported stall or is the first poll since
// start.
func (m *NICHealthMonitor) endPoll(ctx context.Context, category string) {
	s := m.stall
	if s == nil {
		return
	}

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	s.mu.Lock()
	delete(s.started, category)
	metrics.PollStalled.WithLabelValues(m.nodeName, category).Set(0)

	stalled := s.stalledLocked()
	needHealthy := len(stalled) == 0 && (s.reported || !s.baselined)
	s.mu.Unlock()

	if !needHealthy {
		return
	}

	evt := checks.NewHealthEvent(m.nodeName, checks.PollStallCheckName,
		"NIC polls are completing", nil, false, true, pb.RecommendedAction_NONE, s.strategy)
	if !m.publishStallEvent(ctx, evt) {
		return
	}

	s.mu.Lock()
	s.reported = false
	s.baselined = true
	s.mu.Unlock()
}

// checkPollStalls publishes the stall event once any category has been in
// flight past the deadline.
func (m *NICHealthMonitor) checkPollStalls(ctx context.Context) {
	s := m.stall

	s.publishMu.Lock()
	defer s.publishMu.Unlock()

	s.mu.Lock()
	stalled := s.stalledLocked()

	for category := range stalled {
		metrics.PollStalled.WithLabelValues(m.nodeName, category).Set(1)
	}

	if len(stalled) == 0 || s.reported {
		s.mu.Unlock()
		return
	}
	s.mu.Unlock()

	evt := checks.NewHealthEvent(m.nodeName, checks.PollStallCheckName,
		stallMessage(stalled), nil, false, false, pb.RecommendedAction_NONE, s.strategy)
	if !m.publishStallEvent(ctx, evt) {
		return
	}

	s.mu.Lock()
	s.reported = true
	s.mu.Unlock()
}

// stalledLocked returns how long each category has been in flight, for those
// past the deadline. The caller holds s.mu.
func (s *pollStall) stalledLocked() map[string]time.Duration {
	now := s.now()
	stalled := map[string]time.Duration{}

	for category, start := range s.started {
		if elapsed := now.Sub(start); elapsed >= s.deadline {
			stalled[category] = elapsed
		}
	}

	return stalled
}

// stallMessage names the stalled categories in a stable order.
func stallMessage(stalled map[string]time.Duration) string {
	categories := make([]string, 0, len(stalled))
	for category := range stalled {
		categories = append(categories, category)
	}

	sort.Strings(categories)

	parts := make([]string, 0, len(categories))
	for _, category := range categories {
		parts = append(parts, fmt.Sprintf("%s poll in flight for %s", category, stalled[category].Round(time.Second)))
	}

	return "NIC state cannot be observed: " + strings.Join(parts, ", ")
}

// publishStallEvent publishes one stall event and reports whether it is
// settled. A failure is retried on the next check or poll; a permanent
// rejection counts as settled, as it does for check events.
func (m *NICHealthMonitor) publishStallEvent(ctx context.Context, evt *pb.HealthEvent) bool {
	batch := &pb.HealthEvents{Version: 1, Events: []*pb.HealthEvent{evt}}
	if err := m.pub.Publish(ctx, batch); err != nil {
		if errors.Is(err, healthpub.ErrPublishRejected) {
			slog.Error("Platform connector rejected NIC poll stall event for good; dropping it",
				"is_healthy", evt.IsHealthy, "error", err)

			return true
		}

		slog.Error("Failed to send NIC poll stall event", "is_healthy", evt.IsHealthy, "error", err)

		return false
	}

	m.logSentEvents(checks.PollStallCheckName, batch.Events)

	return true
}
