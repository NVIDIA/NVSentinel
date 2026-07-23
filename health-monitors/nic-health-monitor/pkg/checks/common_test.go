// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package checks

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/nvidia/nvsentinel/data-models/pkg/protos"
)

func TestEnsureClearPrecedesBatch_BackdatesAgainstClockSteps(t *testing.T) {
	// Each event stamps its own time.Now(), so a backward wall-clock
	// step between constructing the clear and the events that follow it
	// could invert the intended ordering. The normalization pass must
	// restore a strict clear-first ordering.
	clearEvt := NewBaselineClearEvent("node1", InfiniBandStateCheckName, "clear", pb.ProcessingStrategy_EXECUTE_REMEDIATION)
	fatal := NewHealthEvent("node1", InfiniBandStateCheckName, "fatal",
		PortEntities("mlx5_0", 1), true, false, pb.RecommendedAction_REPLACE_VM,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION)

	// Simulate the clock stepping backwards after the clear was built:
	// the fatal ends up with an OLDER timestamp than the clear.
	fatal.GeneratedTimestamp = timestamppb.New(clearEvt.GeneratedTimestamp.AsTime().Add(-10 * time.Second))

	events := []*pb.HealthEvent{clearEvt, fatal}
	EnsureClearPrecedesBatch(events)

	require.True(t,
		events[0].GeneratedTimestamp.AsTime().Before(events[1].GeneratedTimestamp.AsTime()),
		"the clear must be strictly older than every other event after normalization")
}

func TestEnsureClearPrecedesBatch_NoOpWhenAlreadyOrdered(t *testing.T) {
	clearEvt := NewBaselineClearEvent("node1", InfiniBandStateCheckName, "clear", pb.ProcessingStrategy_EXECUTE_REMEDIATION)
	healthy := NewHealthEvent("node1", InfiniBandStateCheckName, "healthy",
		PortEntities("mlx5_0", 1), false, true, pb.RecommendedAction_NONE,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION)

	before := clearEvt.GeneratedTimestamp.AsTime()

	EnsureClearPrecedesBatch([]*pb.HealthEvent{clearEvt, healthy})

	assert.Equal(t, before, clearEvt.GeneratedTimestamp.AsTime(),
		"an already-ordered batch must not be rewritten")
}

func TestEnsureClearPrecedesBatch_IgnoresNonClearBatches(t *testing.T) {
	// Batches whose first event carries entities (i.e., not a
	// check-scoped clear) must be left untouched.
	fatal := NewHealthEvent("node1", InfiniBandStateCheckName, "fatal",
		PortEntities("mlx5_0", 1), true, false, pb.RecommendedAction_REPLACE_VM,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION)
	healthy := NewHealthEvent("node1", InfiniBandStateCheckName, "healthy",
		PortEntities("mlx5_1", 1), false, true, pb.RecommendedAction_NONE,
		pb.ProcessingStrategy_EXECUTE_REMEDIATION)

	before := fatal.GeneratedTimestamp.AsTime()

	EnsureClearPrecedesBatch([]*pb.HealthEvent{fatal, healthy})

	assert.Equal(t, before, fatal.GeneratedTimestamp.AsTime())
}
