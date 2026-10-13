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
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
)

// capturingClient records every batch it is sent, or fails every send while
// failing is set.
type capturingClient struct {
	mu      sync.Mutex
	events  []*pb.HealthEvent
	failing bool
}

func (c *capturingClient) HealthEventOccurredV1(
	_ context.Context, in *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.failing {
		return nil, status.Error(codes.Internal, "platform connector unavailable")
	}

	c.events = append(c.events, in.Events...)

	return &emptypb.Empty{}, nil
}

func (c *capturingClient) setFailing(failing bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.failing = failing
}

func (c *capturingClient) stallEvents() []*pb.HealthEvent {
	c.mu.Lock()
	defer c.mu.Unlock()

	var out []*pb.HealthEvent

	for _, e := range c.events {
		if e.CheckName == checks.PollStallCheckName {
			out = append(out, e)
		}
	}

	return out
}

// newStallMonitor returns a monitor with no checks, stall detection at a 10s
// deadline, and a clock the test controls.
func newStallMonitor(t *testing.T, node string) (*NICHealthMonitor, *capturingClient, *time.Time) {
	t.Helper()

	client := &capturingClient{}
	m := NewNICHealthMonitor(node, client, "127.0.0.1:5555", nil, time.Second)
	m.EnablePollStallDetection(10*time.Second, pb.ProcessingStrategy_STORE_ONLY)

	clock := time.Unix(1_000_000, 0)
	m.stall.now = func() time.Time { return clock }

	return m, client, &clock
}

// publishBaseline completes a state poll and runs the watchdog once, which
// publishes the healthy baseline.
func publishBaseline(t *testing.T, m *NICHealthMonitor) {
	t.Helper()

	require.NoError(t, m.RunStateChecks(context.Background()))
	m.checkPollStalls(context.Background())
}

func TestCheckPollStalls_FirstCompletedPoll_PublishesOneHealthyBaseline(t *testing.T) {
	m, client, _ := newStallMonitor(t, "baseline-node")

	m.checkPollStalls(context.Background())
	assert.Empty(t, client.stallEvents(), "no baseline before any poll has completed")

	require.NoError(t, m.RunStateChecks(context.Background()))
	assert.Empty(t, client.stallEvents(), "polling loops never publish stall events")

	m.checkPollStalls(context.Background())
	require.NoError(t, m.RunCounterChecks(context.Background()))
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 1, "one baseline per process start, not per poll")
	assert.True(t, events[0].IsHealthy)
	assert.False(t, events[0].IsFatal)
	assert.Equal(t, pb.RecommendedAction_NONE, events[0].RecommendedAction)
	assert.Equal(t, pb.ProcessingStrategy_STORE_ONLY, events[0].ProcessingStrategy)
	assert.Equal(t, checks.ComponentClass, events[0].ComponentClass)
}

func TestCheckPollStalls_PollPastDeadline_PublishesUnhealthyOnce(t *testing.T) {
	m, client, clock := newStallMonitor(t, "stalled-node")
	publishBaseline(t, m)

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 2, "baseline, then exactly one stall event")
	assert.False(t, events[1].IsHealthy)
	assert.False(t, events[1].IsFatal)
	assert.Equal(t, "NIC state cannot be observed: state poll in flight for 11s", events[1].Message)
}

func TestCheckPollStalls_AfterReportedStallEnds_PublishesHealthyAndClearsGauge(t *testing.T) {
	m, client, clock := newStallMonitor(t, "recovering-node")
	publishBaseline(t, m)

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	m.endPoll("state")
	assert.Len(t, client.stallEvents(), 2, "the poll does not publish the recovery itself")

	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 3)
	assert.False(t, events[1].IsHealthy)
	assert.True(t, events[2].IsHealthy)

	// Recovery closes the episode, so a later stall is reported again.
	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	assert.Len(t, client.stallEvents(), 4)
}

func TestCheckPollStalls_PollWithinDeadline_PublishesNothing(t *testing.T) {
	m, client, clock := newStallMonitor(t, "slow-node")
	publishBaseline(t, m)

	m.beginPoll("state")
	*clock = clock.Add(9 * time.Second)
	m.checkPollStalls(context.Background())

	assert.Len(t, client.stallEvents(), 1, "only the baseline")
}

func TestCheckPollStalls_OtherCategoryStillStalled_KeepsStallOpen(t *testing.T) {
	m, client, clock := newStallMonitor(t, "both-node")
	publishBaseline(t, m)

	m.beginPoll("state")
	m.beginPoll("counter")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 2)
	assert.Equal(t,
		"NIC state cannot be observed: counter poll in flight for 11s, state poll in flight for 11s",
		events[1].Message)

	m.endPoll("counter")
	m.checkPollStalls(context.Background())
	assert.Len(t, client.stallEvents(), 2, "state is still stalled, so no recovery yet")

	m.endPoll("state")
	m.checkPollStalls(context.Background())
	events = client.stallEvents()
	require.Len(t, events, 3)
	assert.True(t, events[2].IsHealthy)
}

func TestCheckPollStalls_StallPublishFailsAndPollCompletes_ReportsStallThenRecovery(t *testing.T) {
	m, client, clock := newStallMonitor(t, "unreported-node")
	publishBaseline(t, m)

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	client.setFailing(true)
	m.checkPollStalls(context.Background())
	assert.Len(t, client.stallEvents(), 1, "the stall publish failed")

	m.endPoll("state")
	client.setFailing(false)
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 3, "the stall is still reported after it ended, then cleared")
	assert.False(t, events[1].IsHealthy)
	assert.Equal(t, "NIC state cannot be observed: state poll in flight for 11s", events[1].Message)
	assert.True(t, events[2].IsHealthy)

	m.checkPollStalls(context.Background())
	assert.Len(t, client.stallEvents(), 3)
}

func TestCheckPollStalls_PublishWaitingOnServer_ReportsNoStall(t *testing.T) {
	m, client, clock := newStallMonitor(t, "waiting-node")
	publishBaseline(t, m)

	waiting := true
	m.stall.waitingOnServer = func() bool { return waiting }

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	assert.Len(t, client.stallEvents(), 1, "a poll waiting on the platform connector is not a NIC stall")

	waiting = false
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 2, "still in flight once the wait ends, so it is reported")
	assert.False(t, events[1].IsHealthy)
}

func TestEnablePollStallDetection_ZeroDeadline_LeavesItOff(t *testing.T) {
	client := &capturingClient{}
	m := NewNICHealthMonitor("off-node", client, "127.0.0.1:5555", nil, time.Second)
	m.EnablePollStallDetection(0, pb.ProcessingStrategy_STORE_ONLY)

	require.NoError(t, m.RunStateChecks(context.Background()))
	require.NoError(t, m.RunPollStallWatchdog(context.Background()))
	assert.Empty(t, client.stallEvents())
}
