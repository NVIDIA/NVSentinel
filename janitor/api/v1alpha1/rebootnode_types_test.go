// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
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

package v1alpha1

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestRebootNode_IsRebootInProgress(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   bool
	}{
		{
			name: "signal sent and node not ready",
			conditions: []metav1.Condition{
				{
					Type:   RebootNodeConditionSignalSent,
					Status: metav1.ConditionTrue,
				},
				{
					Type:   RebootNodeConditionNodeReady,
					Status: metav1.ConditionFalse,
				},
			},
			expected: true,
		},
		{
			name: "signal sent but node ready",
			conditions: []metav1.Condition{
				{
					Type:   RebootNodeConditionSignalSent,
					Status: metav1.ConditionTrue,
				},
				{
					Type:   RebootNodeConditionNodeReady,
					Status: metav1.ConditionTrue,
				},
			},
			expected: false,
		},
		{
			name: "signal not sent",
			conditions: []metav1.Condition{
				{
					Type:   RebootNodeConditionSignalSent,
					Status: metav1.ConditionFalse,
				},
				{
					Type:   RebootNodeConditionNodeReady,
					Status: metav1.ConditionFalse,
				},
			},
			expected: false,
		},
		{
			name:       "no conditions",
			conditions: []metav1.Condition{},
			expected:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rn := &RebootNode{
				Status: RebootNodeStatus{
					Conditions: tt.conditions,
				},
			}
			assert.Equal(t, tt.expected, rn.IsRebootInProgress())
		})
	}
}

func TestRebootNode_GetCSPReqRef(t *testing.T) {
	tests := []struct {
		name       string
		conditions []metav1.Condition
		expected   string
	}{
		{
			name: "has CSP ref in signal sent condition",
			conditions: []metav1.Condition{
				{
					Type:    RebootNodeConditionSignalSent,
					Status:  metav1.ConditionTrue,
					Message: "request-id-12345",
				},
			},
			expected: "request-id-12345",
		},
		{
			name:       "no conditions",
			conditions: []metav1.Condition{},
			expected:   "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rn := &RebootNode{
				Status: RebootNodeStatus{
					Conditions: tt.conditions,
				},
			}
			assert.Equal(t, tt.expected, rn.GetCSPReqRef())
		})
	}
}

func TestRebootNode_SetInitialConditions(t *testing.T) {
	t.Run("adds conditions when none exist", func(t *testing.T) {
		rn := &RebootNode{}

		rn.SetInitialConditions()

		hasSignalSent := false
		hasNodeReady := false
		for _, cond := range rn.Status.Conditions {
			if cond.Type == RebootNodeConditionSignalSent {
				hasSignalSent = true
				assert.Equal(t, metav1.ConditionUnknown, cond.Status)
			}
			if cond.Type == RebootNodeConditionNodeReady {
				hasNodeReady = true
				assert.Equal(t, metav1.ConditionUnknown, cond.Status)
			}
		}
		assert.True(t, hasSignalSent)
		assert.True(t, hasNodeReady)
	})
}

func TestRebootNode_SetCondition(t *testing.T) {
	t.Run("adds new condition", func(t *testing.T) {
		rn := &RebootNode{}

		rn.SetCondition(metav1.Condition{
			Type:   RebootNodeConditionSignalSent,
			Status: metav1.ConditionTrue,
		})

		assert.Len(t, rn.Status.Conditions, 1)
		assert.Equal(t, metav1.ConditionTrue, rn.Status.Conditions[0].Status)
	})

	t.Run("updates existing condition", func(t *testing.T) {
		rn := &RebootNode{
			Status: RebootNodeStatus{
				Conditions: []metav1.Condition{
					{
						Type:   RebootNodeConditionSignalSent,
						Status: metav1.ConditionUnknown,
					},
				},
			},
		}

		rn.SetCondition(metav1.Condition{
			Type:   RebootNodeConditionSignalSent,
			Status: metav1.ConditionTrue,
		})

		assert.Len(t, rn.Status.Conditions, 1)
		assert.Equal(t, metav1.ConditionTrue, rn.Status.Conditions[0].Status)
	})
}

func TestRebootNode_SetStartTime(t *testing.T) {
	t.Run("sets start time when nil", func(t *testing.T) {
		rn := &RebootNode{}
		require.Nil(t, rn.Status.StartTime)

		rn.SetStartTime()

		require.NotNil(t, rn.Status.StartTime)
		assert.WithinDuration(t, time.Now(), rn.Status.StartTime.Time, 5*time.Second)
	})

	t.Run("does not overwrite existing start time", func(t *testing.T) {
		originalTime := metav1.NewTime(time.Now().Add(-1 * time.Hour))
		rn := &RebootNode{
			Status: RebootNodeStatus{
				StartTime: &originalTime,
			},
		}

		rn.SetStartTime()

		assert.Equal(t, originalTime.Time, rn.Status.StartTime.Time)
	})
}

func TestRebootNode_SetCompletionTime(t *testing.T) {
	t.Run("sets completion time", func(t *testing.T) {
		rn := &RebootNode{}
		require.Nil(t, rn.Status.CompletionTime)

		rn.SetCompletionTime()

		require.NotNil(t, rn.Status.CompletionTime)
		assert.WithinDuration(t, time.Now(), rn.Status.CompletionTime.Time, 5*time.Second)
	})
}

func TestRebootNode_StatusFields(t *testing.T) {
	t.Run("status fields are initialized to zero values", func(t *testing.T) {
		rn := &RebootNode{}

		// Verify default zero values for status fields
		assert.Equal(t, int32(0), rn.Status.RetryCount)
		assert.Equal(t, int32(0), rn.Status.ConsecutiveFailures)
		assert.Nil(t, rn.Status.StartTime)
		assert.Nil(t, rn.Status.CompletionTime)
		assert.Empty(t, rn.Status.Conditions)
	})

	t.Run("status fields can be set and retrieved independently", func(t *testing.T) {
		rn := &RebootNode{}

		// Set start time
		rn.SetStartTime()
		require.NotNil(t, rn.Status.StartTime)

		// Increment retry count
		rn.Status.RetryCount++
		assert.Equal(t, int32(1), rn.Status.RetryCount)

		// Increment consecutive failures
		rn.Status.ConsecutiveFailures++
		assert.Equal(t, int32(1), rn.Status.ConsecutiveFailures)

		// Set conditions
		rn.SetInitialConditions()
		assert.NotEmpty(t, rn.Status.Conditions)

		// Set completion time
		rn.SetCompletionTime()
		require.NotNil(t, rn.Status.CompletionTime)

		// Verify all fields are set correctly
		assert.Equal(t, int32(1), rn.Status.RetryCount)
		assert.Equal(t, int32(1), rn.Status.ConsecutiveFailures)
		assert.NotNil(t, rn.Status.StartTime)
		assert.NotNil(t, rn.Status.CompletionTime)
		assert.Len(t, rn.Status.Conditions, 2) // SignalSent and NodeReady
	})
}
