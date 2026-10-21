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
	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func attachClaim(t *testing.T, node *corev1.Node, requests ...*v1alpha1.ValidationRequest) cordonownership.Claim {
	t.Helper()
	claim := cordonownership.New(node)
	_, err := cordonownership.Store(node, claim)
	require.NoError(t, err)
	for _, request := range requests {
		request.Annotations[cordonownership.RequestClaimIDAnnotationKey] = claim.ID
	}
	return claim
}

func TestCordonClaim_ReleaseRequiresMatchingUnpreservedIdentity(t *testing.T) {
	cases := []struct {
		name       string
		mutate     func(*corev1.Node, *v1alpha1.ValidationRequest)
		taintsKept bool
	}{
		{"missing node claim", func(n *corev1.Node, _ *v1alpha1.ValidationRequest) {
			delete(n.Annotations, cordonownership.AnnotationKey)
		}, false},
		{"malformed node claim", func(n *corev1.Node, _ *v1alpha1.ValidationRequest) {
			n.Annotations[cordonownership.AnnotationKey] = `{broken`
		}, false},
		{"different claim", func(n *corev1.Node, _ *v1alpha1.ValidationRequest) {
			_, err := cordonownership.Store(n, cordonownership.New(n))
			require.NoError(t, err)
		}, false},
		{"missing request claim", func(_ *corev1.Node, r *v1alpha1.ValidationRequest) {
			delete(r.Annotations, cordonownership.RequestClaimIDAnnotationKey)
		}, false},
		{"empty request claim", func(_ *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Annotations[cordonownership.RequestClaimIDAnnotationKey] = ""
		}, false},
		{"manual ownership", func(_ *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Annotations[annotationCordonOwnership] = cordonPreExisting
		}, false},
		{"unknown ownership", func(_ *corev1.Node, r *v1alpha1.ValidationRequest) {
			r.Annotations[annotationCordonOwnership] = cordonUnknown
		}, false},
		// The session entry belongs to another Node object, so nothing is released.
		{"recreated node", func(n *corev1.Node, _ *v1alpha1.ValidationRequest) { n.UID = "replacement" }, true},
		{"preserved claim", func(n *corev1.Node, _ *v1alpha1.ValidationRequest) {
			claim, _ := cordonownership.Read(n)
			claim.PreserveCordon = true
			_, err := cordonownership.Store(n, claim)
			require.NoError(t, err)
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			node, request, gate := ownershipFixture("claim", cordonOwned)
			attachClaim(t, node, request)
			_, err := updateOwnedValidationSession(node, request, []string{"smoke"})
			require.NoError(t, err)
			tc.mutate(node, request)
			require.NotEmpty(t, ownedReleaseRefusal(node, request))
			_, err = releaseOwnedNodeFromSession(node, request, nil, true, gate)
			require.NoError(t, err)
			require.True(t, node.Spec.Unschedulable)
			require.Equal(t, tc.taintsKept, len(node.Spec.Taints) > 0)
		})
	}
}

func TestCordonClaim_LastAssociationRetiresWithoutDeletionRelease(t *testing.T) {
	for _, succeeded := range []bool{false, true} {
		t.Run(map[bool]string{false: "deleted", true: "succeeded"}[succeeded], func(t *testing.T) {
			node, request, gate := ownershipFixture("last", cordonOwned)
			attachClaim(t, node, request)
			_, err := updateOwnedValidationSession(node, request, []string{"smoke"})
			require.NoError(t, err)
			_, err = releaseOwnedNodeFromSession(node, request, nil, succeeded, gate)
			require.NoError(t, err)
			require.Equal(t, !succeeded, node.Spec.Unschedulable)
			require.NotContains(t, node.Annotations, cordonownership.AnnotationKey)
			_, err = updateOwnedValidationSession(node, request, []string{"smoke"})
			require.NoError(t, err)
			require.NotContains(t, node.Annotations, annotationValidationSession)
		})
	}
}

func TestCordonClaim_DeletedAssociationPreservesPendingSuccessorDecision(t *testing.T) {
	for _, ownership := range []string{cordonOwned, cordonPreExisting, cordonUnknown} {
		t.Run(ownership, func(t *testing.T) {
			node, first, gate := ownershipFixture("first", ownership)
			_, next, _ := ownershipFixture("next", cordonOwned)
			claim := attachClaim(t, node, first, next)
			_, err := updateOwnedValidationSession(node, first, []string{"smoke"})
			require.NoError(t, err)
			node.Annotations[annotationQuarantineValidationHealthEvent] = handoffSource
			_, err = releaseOwnedNodeFromSession(node, first, nil, false, gate)
			require.NoError(t, err)
			current, valid := cordonownership.Read(node)
			require.True(t, valid)
			require.Equal(t, claim.ID, current.ID)
			require.Equal(t, ownership != cordonOwned, current.PreserveCordon)
			require.True(t, node.Spec.Unschedulable)
			_, err = updateOwnedValidationSession(node, next, []string{"smoke"})
			require.NoError(t, err)
			delete(node.Annotations, annotationQuarantineValidationHealthEvent)
			_, err = releaseOwnedNodeFromSession(node, next, nil, true, gate)
			require.NoError(t, err)
			require.Equal(t, ownership != cordonOwned, node.Spec.Unschedulable)
			require.NotContains(t, node.Annotations, cordonownership.AnnotationKey)
		})
	}
}

func TestCordonClaim_EmptyPendingSourceKeepsDeletedGrant(t *testing.T) {
	node, request, gate := ownershipFixture("pending", cordonOwned)
	claim := attachClaim(t, node, request)
	_, err := updateOwnedValidationSession(node, request, []string{"smoke"})
	require.NoError(t, err)
	node.Annotations[annotationQuarantineValidationHealthEvent] = ""
	_, err = releaseOwnedNodeFromSession(node, request, nil, false, gate)
	require.NoError(t, err)
	current, valid := cordonownership.Read(node)
	require.True(t, valid)
	require.Equal(t, claim, current)
	require.True(t, node.Spec.Unschedulable)
}

func TestCordonClaim_OldRequestCannotPoisonNewClaim(t *testing.T) {
	for _, oldClaim := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "different claim"}[oldClaim], func(t *testing.T) {
			node, old, gate := ownershipFixture("old", cordonOwned)
			if oldClaim {
				attachClaim(t, node, old)
			}
			_, err := updateOwnedValidationSession(node, old, []string{"smoke"})
			require.NoError(t, err)
			_, current, _ := ownershipFixture("current", cordonOwned)
			attachClaim(t, node, current)
			original := node.Annotations[cordonownership.AnnotationKey]
			_, err = updateOwnedValidationSession(node, current, []string{"smoke"})
			require.NoError(t, err)
			_, err = updateOwnedValidationSession(node, old, []string{"smoke"})
			require.NoError(t, err)
			require.Equal(t, original, node.Annotations[cordonownership.AnnotationKey])
			_, err = releaseOwnedNodeFromSession(node, old, nil, false, gate)
			require.NoError(t, err)
			require.Equal(t, original, node.Annotations[cordonownership.AnnotationKey])
			entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
			require.NoError(t, err)
			require.Len(t, entries, 1)
			require.False(t, entries[0].PreserveCordon)
			_, err = releaseOwnedNodeFromSession(node, current, nil, true, gate)
			require.NoError(t, err)
			require.False(t, node.Spec.Unschedulable)
			require.NotContains(t, node.Annotations, cordonownership.AnnotationKey)
		})
	}
}

func TestCordonClaim_OldSuccessCannotSupersedeNewFailedRequest(t *testing.T) {
	node, old, gate := ownershipFixture("old-success", cordonOwned)
	attachClaim(t, node, old)
	_, err := updateOwnedValidationSession(node, old, []string{"smoke"})
	require.NoError(t, err)
	_, current, _ := ownershipFixture("current-failed", cordonOwned)
	attachClaim(t, node, current)
	original := node.Annotations[cordonownership.AnnotationKey]
	_, err = updateOwnedValidationSession(node, current, []string{"smoke"})
	require.NoError(t, err)
	_, err = markNodeSessionEntryFailed(node, current.Name)
	require.NoError(t, err)
	_, err = releaseOwnedNodeFromSession(node, old, []string{"smoke"}, true, gate)
	require.NoError(t, err)
	require.True(t, node.Spec.Unschedulable)
	require.Equal(t, original, node.Annotations[cordonownership.AnnotationKey])
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Equal(t, current.Name, entries[0].Name)
	require.True(t, entries[0].Failed)
	require.False(t, entries[0].PreserveCordon)
}
