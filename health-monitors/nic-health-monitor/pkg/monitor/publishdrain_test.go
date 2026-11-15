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
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/health-monitors/nic-health-monitor/pkg/checks"
)

// hangingClient holds a send until its context ends or the test releases it,
// standing in for a platform connector that wedged mid-send.
type hangingClient struct {
	release chan struct{}
}

func (c *hangingClient) HealthEventOccurredV1(
	ctx context.Context, _ *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-c.release:
		return &emptypb.Empty{}, nil
	}
}

// wedgingClient wedges every publish until its context ends, counting the
// sends it was handed.
type wedgingClient struct {
	publishes int
}

func (c *wedgingClient) HealthEventOccurredV1(
	ctx context.Context, _ *pb.HealthEvents, _ ...grpc.CallOption,
) (*emptypb.Empty, error) {
	c.publishes++

	<-ctx.Done()

	return nil, ctx.Err()
}

// TestCheckPollStalls_CancelledContextStillPublishesStall: the stall event is
// the only record that the node was blind, and liveness lands SIGTERM on
// this monitor precisely while that event is still trying to deliver, so
// the publish must go out on the drain window instead of dying on the
// cancelled context.
func TestCheckPollStalls_CancelledContextStillPublishesStall(t *testing.T) {
	m, client, clock := newStallMonitor(t, "cancelled-node")
	publishBaseline(t, m)

	m.beginPoll("state")
	*clock = clock.Add(11 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.armPublishDrain()
	m.checkPollStalls(ctx)

	events := client.stallEvents()
	require.Len(t, events, 2, "baseline, then the stall event delivered on the drain window")
	assert.False(t, events[1].IsHealthy)
}

// TestRunChecks_CancelledContextStillPublishes: a poll that completes after
// SIGTERM carries the transition that ended the stall, so its batch goes
// out on the drain window and the check commits instead of leaving the
// next container to re-detect the recovery.
func TestRunChecks_CancelledContextStillPublishes(t *testing.T) {
	client := &capturingClient{}
	check := &stagedTestCheck{}
	m := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	// Through the real registration: once the cancellation lands, the AfterFunc
	// callback and the poll race to arm the deadline, and both orders must
	// deliver.
	m.ArmPublishDrain(ctx)
	cancel()

	require.NoError(t, m.RunStateChecks(ctx))

	assert.True(t, check.committed, "delivered on the drain window, so the transition is consumed")
	assert.Len(t, client.events, 1)
}

// TestRunChecks_CancelledBeforeTheArmStillPublishes: the AfterFunc callback
// runs in its own goroutine, so a poll can reach the publish with the
// cancellation already landed and the deadline still unarmed. The publish
// path must arm the window itself in that order — handing back the
// cancelled parent would drop the batch for good, the exact loss the drain
// exists to prevent.
func TestRunChecks_CancelledBeforeTheArmStillPublishes(t *testing.T) {
	client := &capturingClient{}
	check := &stagedTestCheck{}
	m := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// The deadline stays unarmed: this publish stands in for one that the
	// AfterFunc callback has not reached yet.

	require.NoError(t, m.RunStateChecks(ctx))

	assert.True(t, check.committed, "the publish path armed the window when it saw the cancellation")
	assert.Len(t, client.events, 1)
}

// TestRunChecks_WedgedPublishIsBoundedByDrainWindow: detaching from the
// cancellation must not turn a wedged connector into an unbounded wait at
// shutdown; the publish ends with the window and the transition stays
// staged for the next poll.
func TestRunChecks_WedgedPublishIsBoundedByDrainWindow(t *testing.T) {
	client := &hangingClient{release: make(chan struct{})}
	defer close(client.release)

	check := &stagedTestCheck{}
	m := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)
	m.publishDrain = 50 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.armPublishDrain()

	start := time.Now()
	require.NoError(t, m.RunStateChecks(ctx))

	assert.Less(t, time.Since(start), 5*time.Second,
		"the drain window bounds how long shutdown waits on one publish")
	assert.False(t, check.committed, "undelivered, so the transition stays staged")
	assert.Equal(t, 1, check.discardCalls)
}

// TestRunChecks_ShutdownPublishesShareOneWindow: the drain is one deadline
// armed at the cancellation, not a fresh window per publish. Two checks
// with events during shutdown must together cost one window — the loops
// can start another poll after the cancellation, so per-publish windows
// would stack past the pod's termination grace period.
func TestRunChecks_ShutdownPublishesShareOneWindow(t *testing.T) {
	client := &wedgingClient{}
	first, second := &stagedTestCheck{}, &stagedTestCheck{}
	m := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{first, second}, time.Second)
	m.publishDrain = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	m.armPublishDrain()

	start := time.Now()
	require.NoError(t, m.RunStateChecks(ctx))

	assert.Less(t, time.Since(start), 160*time.Millisecond,
		"two shutdown publishes share one window, not one each")
	assert.Equal(t, 1, client.publishes,
		"the first publish spent the window; the second never reached the wire")
	assert.Equal(t, 1, first.discardCalls, "both publishes were attempted")
	assert.Equal(t, 1, second.discardCalls, "the second failed fast on the spent window")
	assert.False(t, first.committed)
	assert.False(t, second.committed)
}

// TestRunChecks_LiveContextIsNotCappedByDrainWindow: the drain bounds only
// publishes that start after the cancellation. A steady-state publish
// keeps the caller's context, so a direct-mode batch that waits out a slow
// connector inside its retry window still delivers.
func TestRunChecks_LiveContextIsNotCappedByDrainWindow(t *testing.T) {
	release := make(chan struct{})
	client := &hangingClient{release: release}

	// The connector answers well after the drain window has passed.
	go func() {
		time.Sleep(150 * time.Millisecond)
		close(release)
	}()

	check := &stagedTestCheck{}
	m := NewNICHealthMonitor("node1", client, "127.0.0.1:5555",
		[]checks.TransactionalCheck{check}, time.Second)
	m.publishDrain = 50 * time.Millisecond

	start := time.Now()
	require.NoError(t, m.RunStateChecks(context.Background()))

	assert.True(t, check.committed, "delivered on the live context, not the drain window")
	assert.Greater(t, time.Since(start), m.publishDrain,
		"the publish outlived the drain window, so nothing capped it")
}
