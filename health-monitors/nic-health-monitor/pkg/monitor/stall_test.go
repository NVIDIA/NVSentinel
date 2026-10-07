// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0

package monitor

import (
	"context"
	"sync"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/metrics"
)

// capturingClient records every batch it is sent.
type capturingClient struct {
	mu     sync.Mutex
	events []*pb.HealthEvent
}

func (c *capturingClient) HealthEventOccurredV1(
	_ context.Context, in *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.events = append(c.events, in.Events...)

	return &emptypb.Empty{}, nil
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

// stalledGauge reads nic_health_monitor_poll_stalled for one node and category.
func stalledGauge(t *testing.T, node, category string) float64 {
	t.Helper()

	var m dto.Metric
	require.NoError(t, metrics.PollStalled.WithLabelValues(node, category).Write(&m))

	return m.GetGauge().GetValue()
}

func TestEndPoll_FirstPollAfterStart_PublishesOneHealthyBaseline(t *testing.T) {
	m, client, _ := newStallMonitor(t, "baseline-node")

	require.NoError(t, m.RunStateChecks(context.Background()))
	require.NoError(t, m.RunCounterChecks(context.Background()))
	require.NoError(t, m.RunStateChecks(context.Background()))

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
	require.NoError(t, m.RunStateChecks(context.Background()))

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 2, "baseline, then exactly one stall event")
	assert.False(t, events[1].IsHealthy)
	assert.False(t, events[1].IsFatal)
	assert.Equal(t, "NIC state cannot be observed: state poll in flight for 11s", events[1].Message)
	assert.InDelta(t, 1, stalledGauge(t, "stalled-node", "state"), 0)
}

func TestEndPoll_AfterReportedStall_PublishesHealthyAndClearsGauge(t *testing.T) {
	m, client, clock := newStallMonitor(t, "recovering-node")
	require.NoError(t, m.RunStateChecks(context.Background()))

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	m.endPoll(context.Background(), "state")

	events := client.stallEvents()
	require.Len(t, events, 3)
	assert.False(t, events[1].IsHealthy)
	assert.True(t, events[2].IsHealthy)
	assert.InDelta(t, 0, stalledGauge(t, "recovering-node", "state"), 0)

	// Recovery closes the episode, so a later stall is reported again.
	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())
	assert.Len(t, client.stallEvents(), 4)
}

func TestCheckPollStalls_PollWithinDeadline_PublishesNothing(t *testing.T) {
	m, client, clock := newStallMonitor(t, "slow-node")
	require.NoError(t, m.RunStateChecks(context.Background()))

	m.beginPoll("state")
	*clock = clock.Add(9 * time.Second)
	m.checkPollStalls(context.Background())

	assert.Len(t, client.stallEvents(), 1, "only the baseline")
	assert.InDelta(t, 0, stalledGauge(t, "slow-node", "state"), 0)
}

func TestEndPoll_OtherCategoryStillStalled_KeepsStallOpen(t *testing.T) {
	m, client, clock := newStallMonitor(t, "both-node")
	require.NoError(t, m.RunStateChecks(context.Background()))

	m.beginPoll("state")
	m.beginPoll("counter")
	*clock = clock.Add(11 * time.Second)
	m.checkPollStalls(context.Background())

	events := client.stallEvents()
	require.Len(t, events, 2)
	assert.Equal(t,
		"NIC state cannot be observed: counter poll in flight for 11s, state poll in flight for 11s",
		events[1].Message)

	m.endPoll(context.Background(), "counter")
	assert.Len(t, client.stallEvents(), 2, "state is still stalled, so no recovery yet")

	m.endPoll(context.Background(), "state")
	events = client.stallEvents()
	require.Len(t, events, 3)
	assert.True(t, events[2].IsHealthy)
}

func TestEnablePollStallDetection_ZeroDeadline_LeavesItOff(t *testing.T) {
	client := &capturingClient{}
	m := NewNICHealthMonitor("off-node", client, "127.0.0.1:5555", nil, time.Second)
	m.EnablePollStallDetection(0, pb.ProcessingStrategy_STORE_ONLY)

	require.NoError(t, m.RunStateChecks(context.Background()))
	require.NoError(t, m.RunPollStallWatchdog(context.Background()))
	assert.Empty(t, client.stallEvents())
}
