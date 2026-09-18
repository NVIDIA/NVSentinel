// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package connectors

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

// fake records calls and answers with a scripted result: an error, a delay,
// or blocking until the context ends.
type fake struct {
	calls atomic.Int32
	err   error
	delay time.Duration
	block bool
	// endedBy records the context error the fake saw when it was cut short.
	endedBy atomic.Pointer[error]
}

func (f *fake) ProcessBatch(ctx context.Context, _ *pb.HealthEvents) error {
	f.calls.Add(1)

	if f.block {
		<-ctx.Done()
		err := ctx.Err()
		f.endedBy.Store(&err)

		return err
	}

	if f.delay > 0 {
		select {
		case <-time.After(f.delay):
		case <-ctx.Done():
			err := ctx.Err()
			f.endedBy.Store(&err)

			return err
		}
	}

	return f.err
}

func batch() *pb.HealthEvents {
	return &pb.HealthEvents{Events: []*pb.HealthEvent{{NodeName: "node-a", CheckName: "check"}}}
}

// TestSet_EveryMemberGetsTheBatch: the happy path hands the batch to every
// member once and succeeds.
func TestSet_EveryMemberGetsTheBatch(t *testing.T) {
	members := []*fake{{}, {}, {}}

	set := Set{members[0], members[1], members[2]}
	require.NoError(t, set.ProcessBatch(context.Background(), batch()))

	for i, m := range members {
		require.EqualValues(t, 1, m.calls.Load(), "member %d", i)
	}
}

// TestSet_EmptyIsANoOp: a set with no members accepts the batch.
func TestSet_EmptyIsANoOp(t *testing.T) {
	require.NoError(t, Set{}.ProcessBatch(context.Background(), batch()))
}

// TestSet_ReportsEveryFailureAndWaitsForTheRest: a failing member does not
// cut the others short. Every member runs to completion on the caller's
// context, and the result names every member that failed.
func TestSet_ReportsEveryFailureAndWaitsForTheRest(t *testing.T) {
	failing := &fake{err: errors.New("primary stepped down")}
	alsoFailing := &fake{err: errors.New("sink unreachable")}
	slow := &fake{delay: 100 * time.Millisecond}

	start := time.Now()
	err := Set{failing, slow, alsoFailing}.ProcessBatch(context.Background(), batch())

	require.ErrorContains(t, err, "primary stepped down")
	require.ErrorContains(t, err, "sink unreachable")
	require.GreaterOrEqual(t, time.Since(start), 100*time.Millisecond, "the slow member was waited for")
	require.EqualValues(t, 1, slow.calls.Load())
	require.Nil(t, slow.endedBy.Load(), "the slow member was not cancelled")
}

// funcConnector adapts a function to the Connector interface.
type funcConnector func(ctx context.Context, he *pb.HealthEvents) error

func (f funcConnector) ProcessBatch(ctx context.Context, he *pb.HealthEvents) error {
	return f(ctx, he)
}

// TestSet_MembersRunAtTheSameTime: each member waits until the other has
// started before it returns. Run one after the other they would wait for the
// context to end and fail; run together they both return.
func TestSet_MembersRunAtTheSameTime(t *testing.T) {
	arrived := make(chan struct{}, 2)
	release := make(chan struct{})

	member := funcConnector(func(ctx context.Context, _ *pb.HealthEvents) error {
		arrived <- struct{}{}

		select {
		case <-release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	go func() {
		<-arrived
		<-arrived
		close(release)
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, Set{member, member}.ProcessBatch(ctx, batch()))
}

// TestSet_CallerCancellationReachesTheMembers: when the caller gives up, the
// members see it and the set reports the caller's context error.
func TestSet_CallerCancellationReachesTheMembers(t *testing.T) {
	blocking := &fake{block: true}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()

	err := Set{blocking}.ProcessBatch(ctx, batch())
	require.ErrorIs(t, err, context.DeadlineExceeded)
}
