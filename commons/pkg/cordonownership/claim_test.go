// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cordonownership

import (
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestInheritable_RequiresActiveOwnedAssociation(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		want        bool
	}{
		{"detached historical claim", nil, false},
		{"quarantine", map[string]string{QuarantineAnnotationKey: `[{"checkName":"gpu"}]`}, true},
		{"pending successor", map[string]string{PendingHandoffAnnotationKey: `[{"checkName":"gpu","tests":["quick"]}]`}, true},
		{"matching session", map[string]string{ValidationSessionAnnotationKey: `[{"name":"a","nodeUID":"node-1","requestUID":"a-1","claimID":"claim-1","cordonOwnership":"owned"}]`}, true},
		{"different node", map[string]string{ValidationSessionAnnotationKey: `[{"nodeUID":"node-2","requestUID":"a-1","claimID":"claim-1","cordonOwnership":"owned"}]`}, false},
		{"different claim", map[string]string{ValidationSessionAnnotationKey: `[{"nodeUID":"node-1","requestUID":"a-1","claimID":"claim-2","cordonOwnership":"owned"}]`}, false},
		{"legacy session", map[string]string{ValidationSessionAnnotationKey: `[{"name":"a"}]`}, false},
		{"missing request UID", map[string]string{ValidationSessionAnnotationKey: `[{"nodeUID":"node-1","claimID":"claim-1","cordonOwnership":"owned"}]`}, false},
		{"preserved session overrides quarantine", map[string]string{
			QuarantineAnnotationKey: `[{"checkName":"gpu"}]`, ValidationSessionAnnotationKey: `[{"nodeUID":"node-1","requestUID":"a-1","claimID":"claim-1","cordonOwnership":"owned","preserveCordon":true}]`}, false},
		{"manual session overrides handoff", map[string]string{
			PendingHandoffAnnotationKey: `[{"checkName":"gpu"}]`, ValidationSessionAnnotationKey: `[{"nodeUID":"node-1","requestUID":"a-1","claimID":"claim-1","cordonOwnership":"pre-existing"}]`}, false},
		{"malformed session", map[string]string{ValidationSessionAnnotationKey: "broken"}, false},
		{"empty session", map[string]string{ValidationSessionAnnotationKey: "[]"}, false},
		{"null session", map[string]string{ValidationSessionAnnotationKey: "null"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node-1", Annotations: tt.annotations},
				Spec: corev1.NodeSpec{Unschedulable: true}}
			claim := Claim{ID: "claim-1", NodeUID: node.UID}
			_, err := Store(node, claim)
			require.NoError(t, err)
			_, valid := Inheritable(node)
			require.Equal(t, tt.want, valid)
		})
	}
}

func TestInheritable_EmptyOrMalformedAssociationDoesNotReviveClaim(t *testing.T) {
	for _, key := range []string{QuarantineAnnotationKey, PendingHandoffAnnotationKey} {
		for _, raw := range []string{"", " ", "[]", "null", "{}", `{"events":{}}`, "broken"} {
			t.Run(key+raw, func(t *testing.T) {
				node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node-1", Annotations: map[string]string{key: raw}},
					Spec: corev1.NodeSpec{Unschedulable: true}}
				_, err := Store(node, New(node))
				require.NoError(t, err)
				require.True(t, HasAssociations(node), "ambiguous metadata keeps cleanup conservative")
				_, valid := Inheritable(node)
				require.False(t, valid, "ambiguous metadata cannot authorize inheritance")
			})
		}
	}
}

func TestInheritable_InvalidOrPreservedClaimRetainsCordon(t *testing.T) {
	for _, raw := range []string{"", "null", "{}", "bad-json",
		`{"id":"claim-1","nodeUID":"old-node"}`,
		`{"id":"claim-1","nodeUID":"node-1","preserveCordon":true}`} {
		t.Run(raw, func(t *testing.T) {
			node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node-1", Annotations: map[string]string{
				AnnotationKey: raw, PendingHandoffAnnotationKey: "pending",
			}}, Spec: corev1.NodeSpec{Unschedulable: true}}
			_, valid := Inheritable(node)
			require.False(t, valid)
			require.True(t, node.Spec.Unschedulable)
		})
	}
}

func TestClaim_IdentityLifecycle(t *testing.T) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{UID: "node-1"}}
	claim := New(node)
	require.NotEmpty(t, claim.ID)
	require.NotEqual(t, claim.ID, New(node).ID)
	changed, err := Store(node, claim)
	require.NoError(t, err)
	require.True(t, changed)
	changed, err = Store(node, claim)
	require.NoError(t, err)
	require.False(t, changed)
	read, valid := Read(node)
	require.True(t, valid)
	require.Equal(t, claim, read)
	node.Annotations[PendingHandoffAnnotationKey] = "pending"
	_, valid = Inheritable(node)
	require.False(t, valid, "a schedulable node has no continuous cordon to inherit")
	node.UID = "node-2"
	_, valid = Read(node)
	require.False(t, valid)
	_, err = Store(node, claim)
	require.Error(t, err)
	require.True(t, Delete(node))
	require.False(t, Delete(node))
}
