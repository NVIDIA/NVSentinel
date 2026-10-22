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
	"encoding/json"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	"github.com/nvidia/nvsentinel/commons/pkg/quarantinetaints"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func ownershipFixture(name, ownership string) (*corev1.Node, *v1alpha1.ValidationRequest, *v1alpha1.SchedulingGateConfig) {
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "ownership-node", UID: "node-uid"},
		Spec: corev1.NodeSpec{Unschedulable: true, Taints: []corev1.Taint{releaseRaceGateTaint}}}
	request := &v1alpha1.ValidationRequest{ObjectMeta: metav1.ObjectMeta{
		Name: name, UID: types.UID(name + "-uid"), Annotations: map[string]string{annotationCordonOwnership: ownership},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "v1", Kind: "Node", Name: node.Name, UID: node.UID}},
	}, Spec: v1alpha1.ValidationRequestSpec{Nodes: []v1alpha1.NodeSpec{{Name: node.Name}}},
		Status: v1alpha1.ValidationRequestStatus{TestGroups: []v1alpha1.TestGroupStatus{{
			Nodes: []string{node.Name}, Tests: []string{"smoke"}, Phase: v1alpha1.PhaseSucceeded,
			Attempts: []v1alpha1.AttemptStatus{{Phase: v1alpha1.PhaseSucceeded}},
		}}},
	}
	gate := &v1alpha1.SchedulingGateConfig{Cordon: v1alpha1.CordonConfig{Remove: true, OnlyOwned: true},
		Taints: []v1alpha1.TaintConfig{{Key: releaseRaceGateTaint.Key, Effect: releaseRaceGateTaint.Effect, Remove: true}}}
	return node, request, gate
}

func TestOwnedSession_ReleaseRequiresEvidence(t *testing.T) {
	// Ownership gates only the cordon. The configured taints follow the default
	// policy: they are lifted when the session ends without a quarantine, and
	// they stay when the request no longer matches the node's session entry.
	cases := []struct {
		name       string
		mutate     func(*corev1.Node, *v1alpha1.ValidationRequest)
		release    bool
		taintsKept bool
	}{
		{"owned", func(n *corev1.Node, r *v1alpha1.ValidationRequest) {}, true, false},
		{"pre-existing", func(n *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Annotations[annotationCordonOwnership] = cordonPreExisting
		}, false, false},
		{"unknown", func(n *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Annotations[annotationCordonOwnership] = cordonUnknown
		}, false, false},
		{"missing", func(n *corev1.Node, r *v1alpha1.ValidationRequest) { r.Annotations = nil }, false, false},
		{"malformed", func(n *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Annotations[annotationCordonOwnership] = "OWNED"
		}, false, false},
		{"owner missing", func(n *corev1.Node, r *v1alpha1.ValidationRequest) { r.OwnerReferences = nil }, false, false},
		{"request replaced", func(n *corev1.Node, r *v1alpha1.ValidationRequest) { r.UID = "replacement" }, false, true},
		{"node replaced", func(n *corev1.Node, r *v1alpha1.ValidationRequest) { n.UID = "replacement" }, false, true},
		{"no test groups", func(n *corev1.Node, r *v1alpha1.ValidationRequest) { r.Status.TestGroups = nil }, false, false},
		{"no attempt", func(n *corev1.Node, r *v1alpha1.ValidationRequest) { r.Status.TestGroups[0].Attempts = nil }, false, false},
		{"failed attempt", func(n *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Status.TestGroups[0].Attempts[0].Phase = v1alpha1.PhaseFailed
		}, false, false},
		{"quarantined", func(n *corev1.Node, r *v1alpha1.ValidationRequest) {
			n.Annotations[annotationQuarantineHealthEvent] = ""
		}, false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, request, gate := ownershipFixture("request", cordonOwned)
			_, err := registerNodeValidationSession(node, request, []string{"smoke"}, true)
			require.NoError(t, err)
			tc.mutate(node, request)
			refusal := ownedReleaseRefusal(node, request)
			require.Equal(t, tc.release, refusal == "", refusal)
			_, err = releaseNodeFromSession(node, request, nil, true, gate)
			require.NoError(t, err)
			require.Equal(t, !tc.release, node.Spec.Unschedulable)
			require.Equal(t, tc.taintsKept, len(node.Spec.Taints) > 0)
		})
	}
}

func TestOwnedSession_PreserveSurvivesCompletionOrderAndRestart(t *testing.T) {
	for _, first := range []string{"owned", "manual"} {
		t.Run(first, func(t *testing.T) {
			node, owned, gate := ownershipFixture("owned", cordonOwned)
			_, manual, _ := ownershipFixture("manual", cordonPreExisting)
			for _, request := range []*v1alpha1.ValidationRequest{owned, manual} {
				_, err := registerNodeValidationSession(node, request, []string{"smoke"}, true)
				require.NoError(t, err)
			}
			order := []*v1alpha1.ValidationRequest{owned, manual}
			if first == "manual" {
				order = []*v1alpha1.ValidationRequest{manual, owned}
			}
			for _, request := range order {
				// The next controller sees only serialized Kubernetes state.
				data, err := json.Marshal(node)
				require.NoError(t, err)
				node = &corev1.Node{}
				require.NoError(t, json.Unmarshal(data, node))
				_, err = releaseNodeFromSession(node, request, nil, true, gate)
				require.NoError(t, err)
				require.True(t, node.Spec.Unschedulable)
			}
			require.Empty(t, node.Annotations[annotationValidationSession])
		})
	}
}

func TestOwnedSession_DeletePreservesOtherOwnedRequestGrant(t *testing.T) {
	node, first, gate := ownershipFixture("first", cordonOwned)
	_, second, _ := ownershipFixture("second", cordonOwned)
	for _, request := range []*v1alpha1.ValidationRequest{first, second} {
		_, err := registerNodeValidationSession(node, request, []string{"smoke"}, true)
		require.NoError(t, err)
	}
	_, err := releaseNodeFromSession(node, first, nil, false, gate)
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	_, err = releaseNodeFromSession(node, second, nil, true, gate)
	require.NoError(t, err)
	require.False(t, node.Spec.Unschedulable)
	require.Empty(t, node.Spec.Taints)
	require.Empty(t, node.Annotations[annotationValidationSession])
}

func TestOwnedSession_FailedOwnedRequestCanBeSuperseded(t *testing.T) {
	node, failed, gate := ownershipFixture("failed", cordonOwned)
	_, success, _ := ownershipFixture("success", cordonOwned)
	_, err := registerNodeValidationSession(node, failed, []string{"smoke"}, true)
	require.NoError(t, err)
	_, err = markNodeSessionEntryFailed(node, failed.Name)
	require.NoError(t, err)
	_, err = registerNodeValidationSession(node, success, []string{"smoke"}, true)
	require.NoError(t, err)
	_, err = releaseNodeFromSession(node, success, []string{"smoke"}, true, gate)
	require.NoError(t, err)
	require.False(t, node.Spec.Unschedulable)
	require.Empty(t, node.Annotations[annotationValidationSession])
}

func TestOwnedSession_LegacyAndMalformedSessionsRetainCordon(t *testing.T) {
	for _, annotation := range []string{`[{"name":"legacy"}]`, `{broken`} {
		t.Run(annotation, func(t *testing.T) {
			node, request, gate := ownershipFixture("legacy", cordonOwned)
			node.Annotations = map[string]string{annotationValidationSession: annotation}
			_, err := releaseNodeFromSession(node, request, nil, true, gate)
			if annotation == `{broken` {
				require.Error(t, err)
				require.NotEmpty(t, node.Spec.Taints)
			} else {
				// A legacy entry grants no cordon release, but the ended session still
				// lifts the configured taints as the default policy would.
				require.NoError(t, err)
				require.Empty(t, node.Spec.Taints)
			}
			require.True(t, node.Spec.Unschedulable)
		})
	}
}

func TestOwnedSession_RecreatedRequestCannotUpgradeOldGrant(t *testing.T) {
	node, request, gate := ownershipFixture("same-name", cordonOwned)
	_, err := registerNodeValidationSession(node, request, []string{"smoke"}, true)
	require.NoError(t, err)
	request.UID = "replacement-request"
	_, err = registerNodeValidationSession(node, request, []string{"smoke"}, true)
	require.NoError(t, err)
	require.True(t, ownedSessionMatches(node, request))
	_, err = releaseNodeFromSession(node, request, nil, true, gate)
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
}

func TestSessionOwnershipSurvivesEnablingStrictPolicy(t *testing.T) {
	for _, ownership := range []string{cordonOwned, cordonPreExisting, cordonUnknown} {
		t.Run(ownership, func(t *testing.T) {
			node, request, strictGate := ownershipFixture("request", ownership)
			_, err := registerNodeValidationSession(node, request, []string{"smoke"}, false)
			require.NoError(t, err)
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.Equal(t, node.UID, entries[0].NodeUID)
			require.Equal(t, request.UID, entries[0].RequestUID)
			require.Equal(t, ownership, entries[0].CordonOwnership)

			// A restarted controller with strict mode enabled can use the original
			// persisted evidence, without promoting a legacy entry or rerunning tests.
			_, err = releaseNodeFromSession(node, request, nil, true, strictGate)
			require.NoError(t, err)
			require.Equal(t, ownership != cordonOwned, node.Spec.Unschedulable)
		})
	}
}

func TestDefaultRegistrationDoesNotPromoteLegacyIdentity(t *testing.T) {
	node, request, _ := ownershipFixture("legacy", cordonOwned)
	legacy := `[{"name":"legacy","tests":["smoke"]}]`
	node.Annotations = map[string]string{annotationValidationSession: legacy}
	for _, onlyOwned := range []bool{false, true} {
		changed, err := registerNodeValidationSession(node, request, []string{"smoke"}, onlyOwned)
		require.NoError(t, err)
		require.False(t, changed)
		require.Equal(t, legacy, node.Annotations[annotationValidationSession])
	}
}

func TestSessionReleaseRetainsBookkeepingForUnmappedTaints(t *testing.T) {
	for _, onlyOwned := range []bool{false, true} {
		t.Run(fmt.Sprintf("onlyOwned=%t", onlyOwned), func(t *testing.T) {
			node, request, gate := ownershipFixture("request", cordonOwned)
			gate.Cordon.OnlyOwned = onlyOwned
			_, err := registerNodeValidationSession(node, request, []string{"smoke"}, onlyOwned)
			require.NoError(t, err)
			unmapped := corev1.Taint{Key: "test.example/unmapped", Value: "fault", Effect: corev1.TaintEffectNoSchedule}
			preExisting := corev1.Taint{Key: "test.example/operator", Effect: corev1.TaintEffectNoSchedule}
			node.Spec.Taints = append(node.Spec.Taints, unmapped, preExisting)
			records := []map[string]any{
				{"key": releaseRaceGateTaint.Key, "value": releaseRaceGateTaint.Value, "effect": releaseRaceGateTaint.Effect, "preExisting": false},
				{"key": unmapped.Key, "value": unmapped.Value, "effect": unmapped.Effect, "preExisting": false},
				{"key": preExisting.Key, "effect": preExisting.Effect, "preExisting": true},
			}
			value, err := json.Marshal(records)
			require.NoError(t, err)
			node.Annotations[quarantinetaints.AnnotationKey] = string(value)
			_, err = releaseNodeFromSession(node, request, nil, true, gate)
			require.NoError(t, err)
			require.ElementsMatch(t, []corev1.Taint{unmapped, preExisting}, node.Spec.Taints)
			var retained []map[string]any
			require.NoError(t, json.Unmarshal([]byte(node.Annotations[quarantinetaints.AnnotationKey]), &retained))
			require.Len(t, retained, 1)
			require.Equal(t, unmapped.Key, retained[0]["key"])
		})
	}
}
