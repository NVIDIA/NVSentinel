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
package event

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/nvidia/nvsentinel/health-monitors/csp-health-monitor/pkg/model"
)

//go:fix inline
func ptr[T any](v T) *T { return new(v) }

func TestLambdaNormalizer_Normalize(t *testing.T) {
	const (
		testID      = "abc123"
		testNode    = "node-1"
		testCluster = "test-cluster"
	)

	notBefore := time.Now().UTC().Add(2 * time.Hour)
	notBeforeDeadline := notBefore.Add(4 * time.Hour)
	notAfter := notBefore.Add(1 * time.Hour)

	n := &LambdaNormalizer{}

	tests := []struct {
		name    string
		meta    LambdaEventMetadata
		check   func(t *testing.T, e *model.MaintenanceEvent)
		wantErr bool
	}{
		{
			name: "emergency leaves scheduledStartTime and scheduledEndTime nil",
			meta: LambdaEventMetadata{
				ID:          testID,
				Urgency:     UrgencyEmergency,
				Status:      "scheduled",
				NodeName:    testNode,
				ClusterName: testCluster,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				// Emergency events rely on FindEmergencyEventsToTriggerQuarantine
				// (which filters on metadata.urgency and has no time-window check),
				// not on the scheduledStartTime range query. Both time fields must
				// stay nil so we don't pollute downstream dashboards / notifier
				// with a synthetic "scheduled in 30 min" value.
				assert.Nil(t, e.ScheduledStartTime, "emergency events must not synthesize scheduledStartTime")
				assert.Nil(t, e.ScheduledEndTime, "emergency events have no scheduled end")
				assert.Equal(t, model.MetadataUrgencyEmergency, e.Metadata["urgency"],
					"metadata.urgency must match what FindEmergencyEventsToTriggerQuarantine filters on")
				assert.Equal(t, model.StatusDetected, e.Status)
				assert.Equal(t, model.TypeUnscheduled, e.MaintenanceType,
					"emergency events are unplanned; MaintenanceType should be UNSCHEDULED")
				assert.Equal(t, model.CSPLambda, e.CSP)
				assert.Equal(t, "NONE", e.RecommendedAction)
			},
		},
		{
			name: "emergency with populated not_before still leaves scheduledStartTime nil",
			meta: LambdaEventMetadata{
				ID:          testID,
				Urgency:     UrgencyEmergency,
				Status:      "scheduled",
				NotBefore:   &notBefore, // API sometimes returns this; must be ignored for emergency
				NodeName:    testNode,
				ClusterName: testCluster,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				assert.Nil(t, e.ScheduledStartTime)
				assert.Nil(t, e.ScheduledEndTime)
			},
		},
		{
			name: "critical_with_deadline uses not_before and not_before_deadline",
			meta: LambdaEventMetadata{
				ID:                testID,
				Urgency:           UrgencyCriticalWithDeadline,
				Status:            "scheduled",
				NotBefore:         &notBefore,
				NotBeforeDeadline: &notBeforeDeadline,
				NotAfter:          &notAfter,
				NodeName:          testNode,
				ClusterName:       testCluster,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				require.NotNil(t, e.ScheduledStartTime)
				assert.Equal(t, notBefore.Truncate(time.Second), e.ScheduledStartTime.Truncate(time.Second))
				require.NotNil(t, e.ScheduledEndTime)
				assert.Equal(t, notBeforeDeadline.Truncate(time.Second), e.ScheduledEndTime.Truncate(time.Second))
				assert.Equal(t, notAfter.Format(time.RFC3339), e.Metadata["notAfter"])
				assert.Equal(t, model.TypeScheduled, e.MaintenanceType,
					"non-emergency events remain SCHEDULED")
			},
		},
		{
			name: "metadata contains urgency and detail",
			meta: LambdaEventMetadata{
				ID:          testID,
				Urgency:     UrgencyEmergency,
				Detail:      "cooling failure",
				Status:      "scheduled",
				NodeName:    testNode,
				ClusterName: testCluster,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				assert.Equal(t, UrgencyEmergency, e.Metadata["urgency"])
				assert.Equal(t, "cooling failure", e.Metadata["detail"])
				_, hasNotAfter := e.Metadata["notAfter"]
				assert.False(t, hasNotAfter, "notAfter should be absent when nil")
			},
		},
		{
			name: "notAfter absent when nil",
			meta: LambdaEventMetadata{
				ID:          testID,
				Urgency:     UrgencyEmergency,
				Status:      "scheduled",
				NotAfter:    nil,
				NodeName:    testNode,
				ClusterName: testCluster,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				_, ok := e.Metadata["notAfter"]
				assert.False(t, ok)
			},
		},
		{
			name: "LastUpdated is written into metadata.providerLastUpdated",
			meta: LambdaEventMetadata{
				ID:          testID,
				Urgency:     UrgencyCriticalWithDeadline,
				Status:      "scheduled",
				NotBefore:   &notBefore,
				NodeName:    testNode,
				ClusterName: testCluster,
				LastUpdated: new(time.Date(2026, 7, 28, 16, 32, 36, 509041000, time.UTC)),
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				got, ok := e.Metadata[model.ProviderLastUpdatedKey]
				require.True(t, ok, "expected metadata[%q] to be set", model.ProviderLastUpdatedKey)
				assert.Equal(t, "2026-07-28T16:32:36.509041Z", got)
			},
		},
		{
			name: "LastUpdated nil leaves providerLastUpdated unset",
			meta: LambdaEventMetadata{
				ID:          testID,
				Urgency:     UrgencyCriticalWithDeadline,
				Status:      "scheduled",
				NotBefore:   &notBefore,
				NodeName:    testNode,
				ClusterName: testCluster,
				LastUpdated: nil,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				_, ok := e.Metadata[model.ProviderLastUpdatedKey]
				assert.False(t, ok, "providerLastUpdated should be absent when LastUpdated is nil")
			},
		},
		{
			name:  "missing metadata returns error",
			meta:  LambdaEventMetadata{},
			check: func(_ *testing.T, _ *model.MaintenanceEvent) {},
			// Normalize with zero meta — caught by empty ID check first, but also
			// exercising the path when additionalInfo is omitted entirely.
			wantErr: true,
		},
		{
			name: "empty event ID returns error",
			meta: LambdaEventMetadata{
				ID:      "",
				Urgency: UrgencyEmergency,
				Status:  "scheduled",
			},
			wantErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := n.Normalize(nil, tc.meta)
			if tc.wantErr {
				assert.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.NotNil(t, got)
			tc.check(t, got)
		})
	}
}

func TestMapLambdaStatus(t *testing.T) {
	tests := []struct {
		input           string
		wantInternal    model.InternalStatus
		wantCSP         model.ProviderStatus
		wantActualStart bool
		wantActualEnd   bool
	}{
		{"scheduled", model.StatusDetected, "scheduled", false, false},
		{"in_progress", model.StatusMaintenanceOngoing, "in_progress", true, false},
		{"completed", model.StatusMaintenanceComplete, "completed", false, true},
		{"canceled", model.StatusCancelled, "canceled", false, false},
		{"unknown_value", model.StatusDetected, "unknown_value", false, false},
	}

	for _, tc := range tests {
		t.Run(tc.input, func(t *testing.T) {
			internal, csp, start, end := mapLambdaStatus(tc.input)
			assert.Equal(t, tc.wantInternal, internal)
			assert.Equal(t, tc.wantCSP, csp)
			assert.Equal(t, tc.wantActualStart, start != nil, "actualStartTime presence")
			assert.Equal(t, tc.wantActualEnd, end != nil, "actualEndTime presence")
		})
	}
}
