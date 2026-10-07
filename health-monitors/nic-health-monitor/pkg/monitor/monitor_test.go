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

	"github.com/prometheus/client_golang/prometheus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
)

// publishFailOnceClient fails the first call with the given status.
type publishFailOnceClient struct {
	calls int
	code  codes.Code
}

func (c *publishFailOnceClient) HealthEventOccurredV1(
	_ context.Context, _ *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.calls++
	if c.calls == 1 {
		return nil, status.Error(c.code, "injected publish failure")
	}

	return &emptypb.Empty{}, nil
}

type stagedTestCheck struct {
	prepareCalls int
	commitCalls  int
	discardCalls int
	committed    bool
	pending      bool
}

func (c *stagedTestCheck) Name() string { return checks.InfiniBandStateCheckName }

func (c *stagedTestCheck) Run() ([]*pb.HealthEvent, error) {
	events, err := c.Prepare()
	if err == nil {
		c.Commit()
	}

	return events, err
}

func (c *stagedTestCheck) Prepare() ([]*pb.HealthEvent, error) {
	c.prepareCalls++
	c.pending = true
	if c.committed {
		return nil, nil
	}

	return []*pb.HealthEvent{{
		Version:        1,
		Agent:          checks.AgentName,
		ComponentClass: checks.ComponentClass,
		CheckName:      c.Name(),
		NodeName:       "node1",
		IsFatal:        true,
	}}, nil
}

func (c *stagedTestCheck) Commit() {
	if !c.pending {
		return
	}

	c.commitCalls++
	c.committed = true
	c.pending = false
}

func (c *stagedTestCheck) Discard() {
	if c.pending {
		c.discardCalls++
	}

	c.pending = false
}

type blockingPollCheck struct {
	block   bool
	started chan struct{}
	release chan struct{}
}

func (c *blockingPollCheck) Name() string                    { return checks.InfiniBandStateCheckName }
func (c *blockingPollCheck) Run() ([]*pb.HealthEvent, error) { return c.Prepare() }
func (c *blockingPollCheck) Prepare() ([]*pb.HealthEvent, error) {
	if c.block {
		close(c.started)
		<-c.release
	}

	return nil, nil
}
func (c *blockingPollCheck) Commit()  {}
func (c *blockingPollCheck) Discard() {}

func TestRunChecks_PollCompletionTimestampWaitsForBlockedCheck(t *testing.T) {
	const node = "blocked-poll-node"
	check := &blockingPollCheck{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(check.release) }) }
	defer release()

	monitor := NewNICHealthMonitor(node, &publishFailOnceClient{}, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)

	readTimestamp := func() float64 {
		families, err := prometheus.DefaultGatherer.Gather()
		require.NoError(t, err)
		for _, family := range families {
			if family.GetName() != "nic_health_monitor_poll_cycle_last_completed_timestamp_seconds" {
				continue
			}
			for _, metric := range family.GetMetric() {
				labels := map[string]string{}
				for _, label := range metric.GetLabel() {
					labels[label.GetName()] = label.GetValue()
				}
				if labels["node"] == node && labels["category"] == "state" {
					return metric.GetGauge().GetValue()
				}
			}
		}
		t.Fatalf("missing completed-poll timestamp for node %q", node)
		return 0
	}

	startupTimestamp := readTimestamp()
	assert.Greater(t, startupTimestamp, float64(0),
		"the timestamp must be exported before the first poll completes")

	check.block = true
	done := make(chan error, 1)
	go func() { done <- monitor.RunStateChecks(context.Background()) }()

	select {
	case <-check.started:
	case <-time.After(time.Second):
		t.Fatal("poll did not reach the blocking check")
	}

	assert.Equal(t, startupTimestamp, readTimestamp(),
		"the timestamp must not advance while a check is blocked")
	release()
	require.NoError(t, <-done)
	assert.Greater(t, readTimestamp(), startupTimestamp,
		"the timestamp must advance once the poll completes")
}

// TestRunChecks_PublishFailureDiscardsAndReemits: a failure the server may
// not repeat (here an expired token) leaves the transition staged, so the
// next tick prepares and sends it again.
func TestRunChecks_PublishFailureDiscardsAndReemits(t *testing.T) {
	client := &publishFailOnceClient{code: codes.Unauthenticated}
	check := &stagedTestCheck{}
	monitor := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)

	require.NoError(t, monitor.RunStateChecks(context.Background()))
	assert.False(t, check.committed)
	assert.Equal(t, 1, check.discardCalls)
	assert.Equal(t, 0, check.commitCalls)

	require.NoError(t, monitor.RunStateChecks(context.Background()))
	assert.True(t, check.committed)
	assert.Equal(t, 2, check.prepareCalls)
	assert.Equal(t, 1, check.commitCalls)
	assert.Equal(t, 2, client.calls)

	// Once committed, a zero-event poll still commits its latest observation.
	require.NoError(t, monitor.RunStateChecks(context.Background()))
	assert.Equal(t, 2, check.commitCalls)
}

// TestRunChecks_PermanentRejectionConsumesTheTransition: a batch the server
// refuses for good would be refused again on every tick, so the transition is
// committed and not re-emitted.
func TestRunChecks_PermanentRejectionConsumesTheTransition(t *testing.T) {
	client := &publishFailOnceClient{code: codes.InvalidArgument}
	check := &stagedTestCheck{}
	monitor := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)

	require.NoError(t, monitor.RunStateChecks(context.Background()))
	assert.True(t, check.committed, "the rejected transition is consumed")
	assert.Equal(t, 1, check.commitCalls)
	assert.Equal(t, 0, check.discardCalls)
	assert.Equal(t, 1, client.calls)

	// The next poll has nothing new to say; the rejected batch is not re-sent.
	require.NoError(t, monitor.RunStateChecks(context.Background()))
	assert.Equal(t, 1, client.calls, "no re-emit of a rejected batch")
}
