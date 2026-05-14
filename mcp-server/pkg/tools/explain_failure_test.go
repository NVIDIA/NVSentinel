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

// TestExplainFailure_RecognizedPattern_NarrativeNamesPattern asserts that a
// node with XID 79 events produces a narrative explicitly naming the
// xid_79_bus_error pattern. Mirrors the construction style of
// NewGPUInventoryHandler. Catches the bug "narrative falls back to 'no
// pattern' even when MatchIncidents returns a hit".
func TestExplainFailure_RecognizedPattern_NarrativeNamesPattern(t *testing.T) {
	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(time.Now().Add(-10*time.Minute), &protos.HealthEvent{
			NodeName:  invTestNode,
			Message:   "GPU has fallen off the bus",
			ErrorCode: []string{"79"},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "GPU-X"},
			},
		}),
	)

	h := tools.NewExplainFailureHandler(r)
	out, err := h.Handle(context.Background(), tools.ExplainFailureInput{Node: invTestNode})

	require.NoError(t, err)
	require.Equal(t, invTestNode, out.Node)
	require.Greater(t, out.EventsConsidered, 0)
	require.NotEmpty(t, out.Incidents)
	require.Contains(t, out.Narrative, "xid_79_bus_error")
	require.Contains(t, out.Narrative, "not your code")
}

// TestExplainFailure_NoMatchingPattern_NarrativeSaysNone asserts that events
// that match no known pattern produce a deterministic "no known pattern"
// narrative and an empty Incidents slice.
func TestExplainFailure_NoMatchingPattern_NarrativeSaysNone(t *testing.T) {
	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(time.Now().Add(-5*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode,
			Message:  "All systems nominal",
		}),
	)

	h := tools.NewExplainFailureHandler(r)
	out, err := h.Handle(context.Background(), tools.ExplainFailureInput{Node: invTestNode})

	require.NoError(t, err)
	require.Empty(t, out.Incidents)
	require.Contains(t, out.Narrative, "No known failure pattern")
}

// TestExplainFailure_OutsideTimeWindow_EventsIgnored asserts that events
// older than SinceMinutes are not considered. Catches the bug "time window
// not applied — every historical event scored".
func TestExplainFailure_OutsideTimeWindow_EventsIgnored(t *testing.T) {
	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(time.Now().Add(-2*time.Hour), &protos.HealthEvent{
			NodeName:  invTestNode,
			Message:   "Old failure",
			ErrorCode: []string{"79"},
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "GPU-X"},
			},
		}),
	)

	h := tools.NewExplainFailureHandler(r)
	out, err := h.Handle(context.Background(), tools.ExplainFailureInput{Node: invTestNode, SinceMinutes: 30})

	require.NoError(t, err)
	require.Equal(t, 0, out.EventsConsidered, "event 2h old must be outside 30min window")
	require.Empty(t, out.Incidents)
}

// TestExplainFailure_GPUUUIDFilter_NarrowsScope asserts that the optional
// GPUUUID input narrows event consideration to events that name that UUID
// in entitiesImpacted.
func TestExplainFailure_GPUUUIDFilter_NarrowsScope(t *testing.T) {
	now := time.Now()
	r := store.NewFakeReader()
	r.SeedNodeEvents(invTestNode,
		at(now.Add(-10*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, ErrorCode: []string{"79"},
			EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-A"}},
		}),
		at(now.Add(-10*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, ErrorCode: []string{"48"},
			EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-B"}},
		}),
	)

	h := tools.NewExplainFailureHandler(r)
	out, err := h.Handle(context.Background(), tools.ExplainFailureInput{Node: invTestNode, GPUUUID: "GPU-A"})

	require.NoError(t, err)
	require.Equal(t, 1, out.EventsConsidered, "only GPU-A events should be considered")
	require.NotEmpty(t, out.Incidents)
	require.Equal(t, "xid_79_bus_error", out.Incidents[0].PatternName)
}

// TestExplainFailure_EmptyNode_ReturnsValidationError asserts the handler
// rejects an empty node argument before any store call.
func TestExplainFailure_EmptyNode_ReturnsValidationError(t *testing.T) {
	h := tools.NewExplainFailureHandler(store.NewFakeReader())

	_, err := h.Handle(context.Background(), tools.ExplainFailureInput{Node: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "node")
}
