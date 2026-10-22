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

package quarantinetaints

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestPrune_KeepsPresentOwnersUntilValidationEnds(t *testing.T) {
	const owned = `{"key":"example.com/owned","value":"true","effect":"NoSchedule","preExisting":false,"futureField":"keep"}`
	const manual = `{"key":"example.com/manual","value":"true","effect":"NoSchedule","preExisting":true}`
	const removed = `{"key":"example.com/removed","value":"old","effect":"NoSchedule","preExisting":false}`
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{
		AnnotationKey: "[" + owned + "," + manual + "," + removed + "]",
	}}, Spec: corev1.NodeSpec{Taints: []corev1.Taint{
		{Key: "example.com/owned", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "example.com/manual", Value: "true", Effect: corev1.TaintEffectNoSchedule},
		{Key: "example.com/removed", Value: "replacement", Effect: corev1.TaintEffectNoSchedule},
	}}}
	originalTaints := append([]corev1.Taint(nil), node.Spec.Taints...)
	changed, err := Prune(node, true)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, "["+owned+","+manual+"]", node.Annotations[AnnotationKey])
	changed, err = Prune(node, true)
	require.NoError(t, err)
	require.False(t, changed)
	changed, err = Prune(node, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.JSONEq(t, "["+owned+"]", node.Annotations[AnnotationKey])
	require.Equal(t, originalTaints, node.Spec.Taints)
	node.Spec.Taints = node.Spec.Taints[1:]
	changed, err = Prune(node, false)
	require.NoError(t, err)
	require.True(t, changed)
	require.NotContains(t, node.Annotations, AnnotationKey)
}

func TestPrune_EmptyAndMalformedRecords(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		wantError bool
	}{
		{"empty", "", false},
		{"empty-array", "[]", false},
		{"null", "null", false},
		{"malformed", "broken", true},
		{"malformed-record", `["invalid"]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Annotations: map[string]string{AnnotationKey: tc.raw}}}
			changed, err := Prune(node, true)
			if tc.wantError {
				require.Error(t, err)
				require.False(t, changed)
				require.Equal(t, tc.raw, node.Annotations[AnnotationKey])
				return
			}
			require.NoError(t, err)
			require.True(t, changed)
			require.NotContains(t, node.Annotations, AnnotationKey)
			changed, err = Prune(node, true)
			require.NoError(t, err)
			require.False(t, changed)
		})
	}
}
