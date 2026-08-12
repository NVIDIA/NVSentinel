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

type publishFailOnceClient struct {
	calls int
}

func (c *publishFailOnceClient) HealthEventOccurredV1(
	_ context.Context, _ *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.calls++
	if c.calls == 1 {
		return nil, status.Error(codes.InvalidArgument, "injected terminal publish failure")
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

func TestRunChecks_PublishFailureDiscardsAndReemits(t *testing.T) {
	client := &publishFailOnceClient{}
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
