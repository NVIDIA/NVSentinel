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

func analyzerSynthesizedEvent(id string, when time.Time) *protos.HealthEvent {
	return &protos.HealthEvent{
		Id:             id,
		NodeName:       invTestNode,
		Agent:          "health-events-analyzer",
		ComponentClass: "GPU",
		CheckName:      "xid_burst_correlation",
		IsFatal:        true,
		IsHealthy:      false,
		Message:        "GPU bus error correlation detected on GPU-X",
		ErrorCode:      []string{"79"},
		EntitiesImpacted: []*protos.Entity{
			{EntityType: "GPU", EntityValue: "GPU-X"},
		},
	}
}

// TestGetIncidentReport_PopulatesReportFromAnalyzerEvent asserts the report
// is composed from the analyzer-synthesized event plus the donated pattern
// matcher's recommendations. The construction mirrors NewExplainFailureHandler
// for the matcher integration. Catches the bug "incident found but
// recommendations missing because MatchIncidents not invoked".
func TestGetIncidentReport_PopulatesReportFromAnalyzerEvent(t *testing.T) {
	now := time.Date(2026, 5, 14, 10, 0, 0, 0, time.UTC)

	r := store.NewFakeReader()
	incidentEvent := analyzerSynthesizedEvent("inc-42", now)
	r.SetNextQueryResult(at(now, incidentEvent))

	r.SeedNodeEvents(invTestNode,
		at(now.Add(-5*time.Minute), &protos.HealthEvent{
			NodeName: invTestNode, Message: "GPU XID 79", ErrorCode: []string{"79"},
			EntitiesImpacted: []*protos.Entity{{EntityType: "GPU", EntityValue: "GPU-X"}},
		}),
		at(now, incidentEvent),
	)

	h := tools.NewGetIncidentReportHandler(r)
	out, err := h.Handle(context.Background(), tools.GetIncidentReportInput{IncidentID: "inc-42"})

	require.NoError(t, err)
	require.Equal(t, "inc-42", out.IncidentID)
	require.NotEmpty(t, out.Title)
	require.Equal(t, "critical", out.Severity, "isFatal events are critical")
	require.Equal(t, invTestNode, out.AffectedNodes[0])
	require.Contains(t, out.AffectedGPUs, "GPU-X")
	require.Equal(t, "xid_79_bus_error", out.RootCauseSummary)
	require.NotEmpty(t, out.RecommendedActions, "RecommendedActions must come from the pattern matcher")
	require.NotEmpty(t, out.RelatedEvents, "raw events on the same node must appear as related")
}

// TestGetIncidentReport_IncidentNotFound_ReturnsError asserts an unknown
// incident id surfaces a clear "not found" error rather than a fabricated
// empty report.
func TestGetIncidentReport_IncidentNotFound_ReturnsError(t *testing.T) {
	r := store.NewFakeReader() // empty: EventsByQuery returns no events

	h := tools.NewGetIncidentReportHandler(r)
	_, err := h.Handle(context.Background(), tools.GetIncidentReportInput{IncidentID: "ghost"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "not found")
}

// TestGetIncidentReport_EmptyIncidentID_ReturnsValidationError asserts an
// empty incident_id is rejected before any store call.
func TestGetIncidentReport_EmptyIncidentID_ReturnsValidationError(t *testing.T) {
	h := tools.NewGetIncidentReportHandler(store.NewFakeReader())

	_, err := h.Handle(context.Background(), tools.GetIncidentReportInput{IncidentID: ""})

	require.Error(t, err)
	require.Contains(t, err.Error(), "incident_id")
}
