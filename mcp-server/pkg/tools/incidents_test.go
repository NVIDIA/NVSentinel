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
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	protos "github.com/nvidia/nvsentinel/data-models/pkg/protos"
	"github.com/nvidia/nvsentinel/mcp-server/pkg/tools"
	"github.com/nvidia/nvsentinel/store-client/pkg/datastore"
)

func eventWithCodes(codes ...string) datastore.HealthEventWithStatus {
	return at(time.Now(), &protos.HealthEvent{
		NodeName:  invTestNode,
		ErrorCode: codes,
	})
}

func eventWithMessage(msg string) datastore.HealthEventWithStatus {
	return at(time.Now(), &protos.HealthEvent{
		NodeName: invTestNode,
		Message:  msg,
	})
}

// TestMatchIncidents_XID79_MatchesBusErrorPattern asserts the pattern matcher
// identifies XID 79 as xid_79_bus_error and surfaces hardware-failure
// recommendations. The pattern matcher is consumed by Task 12 (explain_failure)
// and Task 13 (get_incident_report) — its inputs mirror the events
// NewGPUInventoryHandler reads from the store. Catches the bug "XID code
// extraction silently drops numeric strings from ErrorCode".
func TestMatchIncidents_XID79_MatchesBusErrorPattern(t *testing.T) {
	got := tools.MatchIncidents([]datastore.HealthEventWithStatus{
		eventWithCodes("79"),
	})

	require.NotEmpty(t, got)

	var found *tools.Incident
	for i := range got {
		if got[i].PatternName == "xid_79_bus_error" {
			found = &got[i]
			break
		}
	}

	require.NotNil(t, found, "xid_79_bus_error not in matched incidents")
	require.Greater(t, found.Confidence, 0.0)
	require.True(t, found.NotYourCode)
	require.NotEmpty(t, found.Recommendations)
	require.Contains(t, found.Evidence[0], "79")
}

// TestMatchIncidents_NVLinkSignals_BothXIDAndMessageBoostConfidence asserts
// that a pattern with BOTH its XID code and one of its message phrases
// scores higher than a pattern with only one indicator present. Catches the
// bug "confidence not affected by message-substring evidence".
func TestMatchIncidents_NVLinkSignals_BothXIDAndMessageBoostConfidence(t *testing.T) {
	bothPresent := tools.MatchIncidents([]datastore.HealthEventWithStatus{
		at(time.Now(), &protos.HealthEvent{
			NodeName:  invTestNode,
			ErrorCode: []string{"74"},
			Message:   "NVLink down detected on link 3",
		}),
	})

	xidOnly := tools.MatchIncidents([]datastore.HealthEventWithStatus{
		eventWithCodes("74"),
	})

	pickNVLink := func(in []tools.Incident) *tools.Incident {
		for i := range in {
			if in[i].PatternName == "nvlink_failure" {
				return &in[i]
			}
		}

		return nil
	}

	bothMatch := pickNVLink(bothPresent)
	xidOnlyMatch := pickNVLink(xidOnly)

	require.NotNil(t, bothMatch, "nvlink_failure should match when both XID 74 and 'NVLink' message present")
	require.NotNil(t, xidOnlyMatch, "nvlink_failure should match on XID 74 alone")
	require.Greater(t, bothMatch.Confidence, xidOnlyMatch.Confidence, "confidence must rise when more indicators match")
}

// TestMatchIncidents_OOMKilledMessage_MatchesSoftwareOOM asserts that a
// pure message-substring pattern (no XID required) matches.
func TestMatchIncidents_OOMKilledMessage_MatchesSoftwareOOM(t *testing.T) {
	got := tools.MatchIncidents([]datastore.HealthEventWithStatus{
		eventWithMessage("Container OOMKilled by Linux OOM killer"),
	})

	require.NotEmpty(t, got)

	var found *tools.Incident
	for i := range got {
		if got[i].PatternName == "software_oom" {
			found = &got[i]
			break
		}
	}

	require.NotNil(t, found)
	require.False(t, found.NotYourCode, "software_oom is a user-code issue")
}

// TestMatchIncidents_NoSignals_ReturnsEmpty asserts events with neither XID
// codes nor matchable messages produce an empty incident slice (not an
// "unknown" placeholder).
func TestMatchIncidents_NoSignals_ReturnsEmpty(t *testing.T) {
	got := tools.MatchIncidents([]datastore.HealthEventWithStatus{
		eventWithMessage("All systems nominal"),
	})

	require.Empty(t, got)
}

// TestMatchIncidents_SortsByConfidenceDescending asserts the returned slice
// is ordered with the highest-confidence match first, so callers can pick
// incidents[0] as the primary diagnosis.
func TestMatchIncidents_SortsByConfidenceDescending(t *testing.T) {
	got := tools.MatchIncidents([]datastore.HealthEventWithStatus{
		at(time.Now(), &protos.HealthEvent{
			NodeName:  invTestNode,
			ErrorCode: []string{"79"},
			Message:   "GPU bus fall-off, thermal event observed",
		}),
	})

	require.GreaterOrEqual(t, len(got), 2, "both xid_79_bus_error and thermal_cascade should match this event")

	for i := 1; i < len(got); i++ {
		require.GreaterOrEqual(t, got[i-1].Confidence, got[i].Confidence,
			"incidents must be sorted by confidence descending: pos %d (%.2f) >= pos %d (%.2f)",
			i-1, got[i-1].Confidence, i, got[i].Confidence)
	}
}
