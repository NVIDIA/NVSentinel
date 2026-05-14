// Copyright 2026 k8s-gpu-mcp-server contributors
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

package tools_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/store"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
)

// TestGetGPUTimeline_OrdersEventsAscendingByTimestamp asserts the timeline
// is ordered oldest-to-newest regardless of insertion order. Construction
// mirrors NewExplainFailureHandler in time-window handling. Catches the bug
// "result returned in insertion order, not chronological".
func TestGetGPUTimeline_OrdersEventsAscendingByTimestamp(t *testing.T) {
	now := time.Now()

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(now.Add(-3*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, ComponentClass: "GPU", IsHealthy: false,
			Message: "third",
		}),
		at(now.Add(-30*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, ComponentClass: "GPU", IsHealthy: true,
			Message: "first",
		}),
		at(now.Add(-15*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, ComponentClass: "GPU", IsHealthy: false,
			Message: "second",
		}),
	)

	h := tools.NewGetGPUTimelineHandler(r)
	out, err := h.Handle(context.Background(), tools.GetGPUTimelineInput{Node: invTestNode, SinceMinutes: 60})

	require.NoError(t, err)
	require.Len(t, out.Events, 3)
	require.Equal(t, "first", out.Events[0].Summary)
	require.Equal(t, "second", out.Events[1].Summary)
	require.Equal(t, "third", out.Events[2].Summary)

	for i := 1; i < len(out.Events); i++ {
		require.True(t, !out.Events[i-1].Timestamp.After(out.Events[i].Timestamp),
			"timeline must be ascending: pos %d (%s) <= pos %d (%s)",
			i-1, out.Events[i-1].Timestamp, i, out.Events[i].Timestamp)
	}
}

// TestGetGPUTimeline_OutsideTimeWindow_EventsExcluded asserts events older
// than SinceMinutes are filtered out. Catches the bug "time window ignored".
func TestGetGPUTimeline_OutsideTimeWindow_EventsExcluded(t *testing.T) {
	now := time.Now()

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(now.Add(-2*time.Hour), &protos.HealthEvent{NodeName: invTestNode, Message: "old"}),
		at(now.Add(-5*time.Minute), &protos.HealthEvent{NodeName: invTestNode, Message: "recent"}),
	)

	h := tools.NewGetGPUTimelineHandler(r)
	out, err := h.Handle(context.Background(), tools.GetGPUTimelineInput{Node: invTestNode, SinceMinutes: 30})

	require.NoError(t, err)
	require.Len(t, out.Events, 1)
	require.Equal(t, "recent", out.Events[0].Summary)
}

// TestGetGPUTimeline_GPUUUIDFilter_NarrowsToOneGPU asserts the optional
// GPUUUID input excludes events that don't mention that GPU.
func TestGetGPUTimeline_GPUUUIDFilter_NarrowsToOneGPU(t *testing.T) {
	now := time.Now()

	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(now.Add(-5*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, Message: "GPU-A event",
			EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-A"}},
		}),
		at(now.Add(-3*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, Message: "GPU-B event",
			EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-B"}},
		}),
	)

	h := tools.NewGetGPUTimelineHandler(r)
	out, err := h.Handle(context.Background(), tools.GetGPUTimelineInput{Node: invTestNode, GPUUUID: "GPU-A"})

	require.NoError(t, err)
	require.Len(t, out.Events, 1)
	require.Equal(t, "GPU-A event", out.Events[0].Summary)
}

// TestGetGPUTimeline_EmptyNode_ReturnsValidationError asserts the handler
// validates the required node argument.
func TestGetGPUTimeline_EmptyNode_ReturnsValidationError(t *testing.T) {
	h := tools.NewGetGPUTimelineHandler(store.NewFakeReader())

	_, err := h.Handle(context.Background(), tools.GetGPUTimelineInput{Node: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "node")
}
