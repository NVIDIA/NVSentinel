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

package datastore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

// TestShouldSkipUnchangedEvent covers the CSP-poll dedup decision used inside
// UpsertMaintenanceEvent to short-circuit re-upserts when the CSP hasn't
// changed the event since we last stored it. This prevents the "every poll
// overwrites QUARANTINE_TRIGGERED back to DETECTED and re-fires the trigger
// engine" behaviour that otherwise happens for Lambda events still visible in
// the API.
func TestShouldSkipUnchangedEvent(t *testing.T) {
	const stamp = "2026-07-28T16:32:36.509041Z"

	tests := []struct {
		name     string
		existing *model.MaintenanceEvent
		incoming *model.MaintenanceEvent
		want     bool
	}{
		{
			name: "identical providerLastUpdated: skip",
			existing: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			incoming: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			want: true,
		},
		{
			name: "different providerLastUpdated: do not skip",
			existing: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: "2026-07-28T15:00:00Z"},
			},
			incoming: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			want: false,
		},
		{
			name: "incoming has no providerLastUpdated: do not skip",
			existing: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			incoming: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{"urgency": "emergency"},
			},
			want: false,
		},
		{
			name: "stored has no providerLastUpdated (legacy doc): do not skip",
			existing: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{"urgency": "emergency"},
			},
			incoming: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			want: false,
		},
		{
			name: "stored has nil metadata: do not skip",
			existing: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: nil,
			},
			incoming: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			want: false,
		},
		{
			name: "nil existing (not found): do not skip",
			existing: nil,
			incoming: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			want: false,
		},
		{
			name: "nil incoming: do not skip",
			existing: &model.MaintenanceEvent{
				EventID:  "evt-1",
				Metadata: map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			incoming: nil,
			want:     false,
		},
		{
			// Guards against a CSP that transitions an event to
			// cancelled/completed without bumping providerLastUpdated —
			// without this check we'd skip the write and never dispatch HEALTHY.
			name: "same providerLastUpdated but CSPStatus changed: do not skip",
			existing: &model.MaintenanceEvent{
				EventID:   "evt-1",
				CSPStatus: "scheduled",
				Metadata:  map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			incoming: &model.MaintenanceEvent{
				EventID:   "evt-1",
				CSPStatus: "canceled",
				Metadata:  map[string]string{model.ProviderLastUpdatedKey: stamp},
			},
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, shouldSkipUnchangedEvent(tc.existing, tc.incoming))
		})
	}
}

// TestHasProviderLastUpdated verifies the guard used to decide whether we need
// to fetch the existing doc at all — if the incoming event carries no
// providerLastUpdated (e.g. a CSP that hasn't opted into dedup, or an event
// without a last_updated timestamp), we skip the pre-write read.
func TestHasProviderLastUpdated(t *testing.T) {
	tests := []struct {
		name  string
		event *model.MaintenanceEvent
		want  bool
	}{
		{
			name: "metadata has providerLastUpdated",
			event: &model.MaintenanceEvent{
				Metadata: map[string]string{model.ProviderLastUpdatedKey: "2026-07-28T16:32:36Z"},
			},
			want: true,
		},
		{
			name: "metadata present but no providerLastUpdated",
			event: &model.MaintenanceEvent{
				Metadata: map[string]string{"urgency": "emergency"},
			},
			want: false,
		},
		{
			name:  "nil metadata",
			event: &model.MaintenanceEvent{},
			want:  false,
		},
		{
			name:  "nil event",
			event: nil,
			want:  false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, hasProviderLastUpdated(tc.event))
		})
	}
}
