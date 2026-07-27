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

func ptr[T any](v T) *T { return &v }

func TestLambdaNormalizer_Normalize(t *testing.T) {
	const (
		testID      = "abc123"
		testNode    = "node-1"
		testCluster = "test-cluster"
	)

	triggerLimit := 30 * time.Minute
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
			name: "emergency sets scheduledStartTime = now + triggerTimeLimit",
			meta: LambdaEventMetadata{
				ID:               testID,
				Urgency:          UrgencyEmergency,
				Status:           "scheduled",
				NodeName:         testNode,
				ClusterName:      testCluster,
				TriggerTimeLimit: triggerLimit,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				require.NotNil(t, e.ScheduledStartTime)
				// scheduledStartTime must be approximately now + triggerLimit
				lo := time.Now().UTC().Add(triggerLimit - 2*time.Second)
				hi := time.Now().UTC().Add(triggerLimit + 2*time.Second)
				assert.True(t, e.ScheduledStartTime.After(lo) && e.ScheduledStartTime.Before(hi),
					"scheduledStartTime %v should be within 2s of now+%v", *e.ScheduledStartTime, triggerLimit)
				assert.Nil(t, e.ScheduledEndTime, "emergency events have no scheduled end")
				assert.Equal(t, model.StatusDetected, e.Status)
				assert.Equal(t, "NONE", e.RecommendedAction)
			},
		},
		{
			name: "emergency uses 30m fallback when TriggerTimeLimit is zero",
			meta: LambdaEventMetadata{
				ID:               testID,
				Urgency:          UrgencyEmergency,
				Status:           "scheduled",
				NodeName:         testNode,
				ClusterName:      testCluster,
				TriggerTimeLimit: 0,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				require.NotNil(t, e.ScheduledStartTime)
				lo := time.Now().UTC().Add(30*time.Minute - 2*time.Second)
				hi := time.Now().UTC().Add(30*time.Minute + 2*time.Second)
				assert.True(t, e.ScheduledStartTime.After(lo) && e.ScheduledStartTime.Before(hi),
					"scheduledStartTime %v should be within 2s of now+30m", *e.ScheduledStartTime)
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
				TriggerTimeLimit:  triggerLimit,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				require.NotNil(t, e.ScheduledStartTime)
				assert.Equal(t, notBefore.Truncate(time.Second), e.ScheduledStartTime.Truncate(time.Second))
				require.NotNil(t, e.ScheduledEndTime)
				assert.Equal(t, notBeforeDeadline.Truncate(time.Second), e.ScheduledEndTime.Truncate(time.Second))
				assert.Equal(t, notAfter.Format(time.RFC3339), e.Metadata["notAfter"])
			},
		},
		{
			name: "metadata contains urgency and detail",
			meta: LambdaEventMetadata{
				ID:               testID,
				Urgency:          UrgencyEmergency,
				Detail:           "cooling failure",
				Status:           "scheduled",
				NodeName:         testNode,
				ClusterName:      testCluster,
				TriggerTimeLimit: triggerLimit,
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
				ID:               testID,
				Urgency:          UrgencyEmergency,
				Status:           "scheduled",
				NotAfter:         nil,
				NodeName:         testNode,
				ClusterName:      testCluster,
				TriggerTimeLimit: triggerLimit,
			},
			check: func(t *testing.T, e *model.MaintenanceEvent) {
				_, ok := e.Metadata["notAfter"]
				assert.False(t, ok)
			},
		},
		{
			name: "missing metadata returns error",
			meta: LambdaEventMetadata{},
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
		input          string
		wantInternal   model.InternalStatus
		wantCSP        model.ProviderStatus
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
