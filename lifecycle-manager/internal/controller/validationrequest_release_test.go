// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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

package controller

import (
	"testing"

	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func TestReleaseBlockedError_Message_NamesEveryViolationInOrder(t *testing.T) {
	tests := []struct {
		name       string
		violations []readinessViolation
		want       string
	}{
		{
			name:       "single node",
			violations: []readinessViolation{{node: "node-a", criterion: "not-under-quarantine"}},
			want:       `node "node-a" failed readiness criterion "not-under-quarantine"`,
		},
		{
			name: "several nodes keep request order",
			violations: []readinessViolation{
				{node: "node-b", criterion: "gpu-allocatable"},
				{node: "node-a", criterion: "not-under-quarantine"},
			},
			want: `node "node-b" failed readiness criterion "gpu-allocatable"; ` +
				`node "node-a" failed readiness criterion "not-under-quarantine"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := &releaseBlockedError{violations: tt.violations}
			require.Equal(t, tt.want, err.message())
			require.Equal(t, "validation release blocked: "+tt.want, err.Error())
		})
	}
}

func TestSetReleaseBlockedCondition_RecordsReasonAndMessage(t *testing.T) {
	vr := &v1alpha1.ValidationRequest{ObjectMeta: metav1.ObjectMeta{Name: "vr", Generation: 3}}
	blocked := &releaseBlockedError{violations: []readinessViolation{{node: "node-a", criterion: "node-ready"}}}

	setReleaseBlockedCondition(vr, blocked)

	cond := meta.FindStatusCondition(vr.Status.Conditions, conditionTypeReleaseBlocked)
	require.NotNil(t, cond)
	require.Equal(t, metav1.ConditionTrue, cond.Status)
	require.Equal(t, string(v1alpha1.FailureReasonNodeReadinessViolation), cond.Reason)
	require.Equal(t, `node "node-a" failed readiness criterion "node-ready"`, cond.Message)
	require.Equal(t, int64(3), cond.ObservedGeneration)
	require.False(t, cond.LastTransitionTime.IsZero())

	// A second evaluation replaces the message instead of adding a condition.
	setReleaseBlockedCondition(vr, &releaseBlockedError{
		violations: []readinessViolation{{node: "node-a", criterion: "not-under-quarantine"}},
	})
	require.Len(t, vr.Status.Conditions, 1)
	require.Equal(t, `node "node-a" failed readiness criterion "not-under-quarantine"`, vr.Status.Conditions[0].Message)
}
