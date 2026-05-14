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

// TestAnalyzeXID_XID79_PopulatesReportWithPattern asserts the handler reads
// XID-tagged events from the store via EventsByQuery, attributes them to
// nodes/GPUs, and pairs them with the matching donor pattern (xid_79_bus_error
// in this case). Construction style mirrors NewExplainFailureHandler.
// Catches the bug "matched pattern omitted because MatchIncidents not run".
func TestAnalyzeXID_XID79_PopulatesReportWithPattern(t *testing.T) {
	r := store.NewFakeReader()

	now := time.Now()
	r.SetNextQueryResult(
		at(now.Add(-5*time.Minute), &protos.HealthEvent{
			NodeName: "gpu-node-a", ErrorCode: []string{"79"},
			Message: "GPU has fallen off the bus",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "GPU-A1"},
			},
		}),
		at(now.Add(-2*time.Minute), &protos.HealthEvent{
			NodeName: "gpu-node-b", ErrorCode: []string{"79"},
			Message: "GPU bus fall-off",
			EntitiesImpacted: []*protos.Entity{
				{EntityType: "GPU", EntityValue: "GPU-B1"},
			},
		}),
	)

	h := tools.NewAnalyzeXIDHandler(r)
	out, err := h.Handle(context.Background(), tools.AnalyzeXIDInput{XIDCode: 79})

	require.NoError(t, err)
	require.Equal(t, 79, out.XIDCode)
	require.Equal(t, 2, out.EventCount)
	require.ElementsMatch(t, []string{"gpu-node-a", "gpu-node-b"}, out.AffectedNodes)
	require.ElementsMatch(t, []string{"GPU-A1", "GPU-B1"}, out.AffectedGPUs)
	require.NotNil(t, out.Pattern)
	require.Equal(t, "xid_79_bus_error", out.Pattern.PatternName)
	require.True(t, out.Pattern.NotYourCode)
	require.Len(t, out.RecentEvents, 2)
}

// TestAnalyzeXID_UnknownCode_ReturnsEmptyReport asserts the handler succeeds
// with EventCount=0 and Pattern=nil when the store has no events for the
// requested code, rather than erroring.
func TestAnalyzeXID_UnknownCode_ReturnsEmptyReport(t *testing.T) {
	r := store.NewFakeReader() // empty: SetNextQueryResult not called → returns nil slice

	h := tools.NewAnalyzeXIDHandler(r)
	out, err := h.Handle(context.Background(), tools.AnalyzeXIDInput{XIDCode: 999})

	require.NoError(t, err)
	require.Equal(t, 0, out.EventCount)
	require.Empty(t, out.AffectedNodes)
	require.Nil(t, out.Pattern)
}

// TestAnalyzeXID_NodeFilterNarrowsResult asserts the optional Node argument
// narrows the report's event set to that node.
func TestAnalyzeXID_NodeFilterNarrowsResult(t *testing.T) {
	r := store.NewFakeReader()
	r.SetNextQueryResult(
		at(time.Now(), &protos.HealthEvent{NodeName: "node-a", ErrorCode: []string{"79"}}),
		at(time.Now(), &protos.HealthEvent{NodeName: "node-b", ErrorCode: []string{"79"}}),
	)

	h := tools.NewAnalyzeXIDHandler(r)
	out, err := h.Handle(context.Background(), tools.AnalyzeXIDInput{XIDCode: 79, Node: "node-a"})

	require.NoError(t, err)
	require.Equal(t, 1, out.EventCount)
	require.Equal(t, []string{"node-a"}, out.AffectedNodes)
}

// TestAnalyzeXID_InvalidXIDCode_ReturnsValidationError asserts non-positive
// XID codes are rejected before any store call. XID codes are documented
// as positive integers (the lowest meaningful real code is 1).
func TestAnalyzeXID_InvalidXIDCode_ReturnsValidationError(t *testing.T) {
	h := tools.NewAnalyzeXIDHandler(store.NewFakeReader())

	_, err := h.Handle(context.Background(), tools.AnalyzeXIDInput{XIDCode: 0})

	require.Error(t, err)
	require.Contains(t, err.Error(), "xid_code")
}
