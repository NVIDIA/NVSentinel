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
	"slices"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const (
	annotationCordonOwnership = "nvsentinel.nvidia.com/cordon-ownership"
	cordonOwned               = "owned"
	cordonPreExisting         = "pre-existing"
	cordonUnknown             = "unknown"
)

func (r *ValidationRequestReconciler) onlyOwnedCordon() bool {
	gate := r.Config.Validation.Spec.SchedulingGate
	return gate != nil && gate.Cordon.OnlyOwned
}

func requestNodeIdentityConflicts(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	for _, owner := range request.OwnerReferences {
		if owner.APIVersion == "v1" && owner.Kind == "Node" &&
			(owner.Name != node.Name || owner.UID != node.UID) {
			return true
		}
	}

	return false
}

func requestCordonOwnership(node *corev1.Node, request *v1alpha1.ValidationRequest) string {
	if node.UID == "" || request.UID == "" || requestNodeIdentityConflicts(node, request) {
		return cordonUnknown
	}

	if !hasMatchingNodeOwner(node, request) || !claimAllowsRelease(node, requestClaimID(request)) {
		return cordonUnknown
	}

	switch request.Annotations[annotationCordonOwnership] {
	case cordonOwned:
		return cordonOwned
	case cordonPreExisting:
		return cordonPreExisting
	default:
		return cordonUnknown
	}
}

func hasMatchingNodeOwner(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	return slices.ContainsFunc(request.OwnerReferences, func(owner metav1.OwnerReference) bool {
		return owner.APIVersion == "v1" && owner.Kind == "Node" &&
			owner.Name == node.Name && owner.UID == node.UID
	})
}

func sessionOwnsCordon(node *corev1.Node, entries []sessionEntry) bool {
	return len(entries) > 0 && !slices.ContainsFunc(entries, func(entry sessionEntry) bool {
		return entry.NodeUID != node.UID || entry.NodeUID == "" || entry.RequestUID == "" ||
			entry.CordonOwnership != cordonOwned || entry.PreserveCordon || !claimAllowsRelease(node, entry.ClaimID)
	})
}

func ownedSessionMatches(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	if requestNodeIdentityConflicts(node, request) {
		return false
	}

	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false
	}

	return slices.ContainsFunc(entries, func(entry sessionEntry) bool {
		return entry.Name == request.Name && entry.RequestUID != "" && entry.RequestUID == request.UID &&
			entry.NodeUID != "" && entry.NodeUID == node.UID && entry.ClaimID == requestClaimID(request) &&
			requestClaimMatches(node, request)
	})
}

func requestValidatedNode(request *v1alpha1.ValidationRequest, nodeName string, entries []sessionEntry) bool {
	for _, entry := range entries {
		if entry.Name != request.Name || entry.RequestUID != request.UID || len(entry.Tests) == 0 {
			continue
		}

		return !slices.ContainsFunc(entry.Tests, func(test string) bool {
			return !slices.ContainsFunc(request.Status.TestGroups, func(group v1alpha1.TestGroupStatus) bool {
				return group.Phase == v1alpha1.PhaseSucceeded && slices.Contains(group.Nodes, nodeName) &&
					slices.Contains(group.Tests, test) && len(group.Attempts) > 0 &&
					group.Attempts[len(group.Attempts)-1].Phase == v1alpha1.PhaseSucceeded
			})
		})
	}

	return false
}

func updateOwnedValidationSession(node *corev1.Node, request *v1alpha1.ValidationRequest,
	tests []string) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	if !canRegisterOwnedRequest(node, request) {
		return false, nil
	}

	incoming := sessionEntry{
		Name: request.Name, Tests: tests, NodeUID: node.UID, RequestUID: request.UID,
		CordonOwnership: requestCordonOwnership(node, request), ClaimID: requestClaimID(request),
	}

	entries = upsertOwnedSession(entries, incoming)

	preserved, err := preserveUntrustedSession(node, entries)
	if err != nil {
		return false, err
	}

	changed, err := storeOwnedSession(node, entries)

	return changed || preserved, err
}

func upsertOwnedSession(entries []sessionEntry, incoming sessionEntry) []sessionEntry {
	for i := range entries {
		if entries[i].Name != incoming.Name {
			continue
		}

		if entries[i].NodeUID != incoming.NodeUID || entries[i].RequestUID != incoming.RequestUID ||
			entries[i].ClaimID != incoming.ClaimID {
			// A replaced object cannot inherit its namesake's release decision.
			incoming.PreserveCordon = true
			entries[i] = incoming
		} else if incoming.CordonOwnership != cordonOwned {
			entries[i].PreserveCordon = true
		}

		return entries
	}

	return append(entries, incoming)
}

func storeOwnedSession(node *corev1.Node, entries []sessionEntry) (bool, error) {
	if len(entries) == 0 {
		_, exists := node.Annotations[annotationValidationSession]
		delete(node.Annotations, annotationValidationSession)

		return exists, nil
	}

	value, err := marshalSessionAnnotation(entries)
	if err != nil {
		return false, err
	}

	if node.Annotations[annotationValidationSession] == value {
		return false, nil
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[annotationValidationSession] = value

	return true, nil
}

func releaseOwnedNodeFromSession(node *corev1.Node, request *v1alpha1.ValidationRequest,
	supersedeTests []string, succeeded bool, gate *v1alpha1.SchedulingGateConfig) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	// Legacy entries have no release grant but can still be cleaned up.
	matches := func(entry sessionEntry) bool {
		return ownedSessionTargetsRequest(entry, node, request)
	}
	if !slices.ContainsFunc(entries, matches) {
		return false, nil
	}

	release := succeeded && ownedSessionCanRelease(node, request, entries)

	preserved, err := preserveDepartingSession(node, request, entries)
	if err != nil {
		return false, err
	}

	remaining := removeOwnedSessionEntries(entries, matches, node, supersedeTests,
		succeeded && requestClaimMatches(node, request), requestClaimID(request))

	changed, err := storeOwnedSession(node, remaining)
	if err != nil {
		return false, err
	}

	released := finishOwnedSessionRelease(node, request, len(remaining) == 0, release, gate)

	return changed || released || preserved, nil
}

// ownedReleaseRefusal explains why a successful request cannot release the
// cordon. It returns "" when ownership permits the release.
func ownedReleaseRefusal(node *corev1.Node, request *v1alpha1.ValidationRequest) string {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return "validation session annotation is malformed"
	}

	if _, quarantined := node.Annotations[annotationQuarantineHealthEvent]; quarantined {
		return "node is quarantined again"
	}

	if hasPendingValidationHandoff(node) {
		return "a newer validation handoff is pending on the node"
	}

	return ownedRequestRefusal(node, request, entries)
}

func ownedRequestRefusal(node *corev1.Node, request *v1alpha1.ValidationRequest, entries []sessionEntry) string {
	switch {
	case requestNodeIdentityConflicts(node, request) || !hasMatchingNodeOwner(node, request):
		return "request is not bound to this Node object"
	case !claimAllowsRelease(node, requestClaimID(request)):
		return "cordon claim is missing, replaced, or preserved"
	case requestCordonOwnership(node, request) != cordonOwned:
		return "request cordon ownership is " + requestOwnershipLabel(request)
	case !ownedSessionMatches(node, request):
		return "session entry does not match the request identity"
	case !sessionOwnsCordon(node, entries):
		return "session holds entries without proven ownership"
	case !requestValidatedNode(request, node.Name, entries):
		return "no successful test attempt covers this node"
	default:
		return ""
	}
}

func requestOwnershipLabel(request *v1alpha1.ValidationRequest) string {
	value, present := request.Annotations[annotationCordonOwnership]
	if !present {
		return "missing"
	}

	switch value {
	case cordonOwned, cordonPreExisting, cordonUnknown:
		return value
	default:
		return "malformed"
	}
}

func ownedSessionTargetsRequest(entry sessionEntry, node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	return entry.Name == request.Name &&
		(entry.RequestUID == request.UID || entry.RequestUID == "") &&
		(entry.NodeUID == node.UID || entry.NodeUID == "")
}

func ownedSessionCanRelease(node *corev1.Node, request *v1alpha1.ValidationRequest, entries []sessionEntry) bool {
	return sessionOwnsCordon(node, entries) &&
		ownedSessionMatches(node, request) && requestCordonOwnership(node, request) == cordonOwned &&
		requestValidatedNode(request, node.Name, entries)
}

func removeOwnedSessionEntries(entries []sessionEntry, matches func(sessionEntry) bool,
	node *corev1.Node, supersedeTests []string, succeeded bool, claimID string) []sessionEntry {
	return slices.DeleteFunc(slices.Clone(entries), func(entry sessionEntry) bool {
		return matches(entry) || (succeeded && supersedeTests != nil && entry.Failed &&
			entry.NodeUID == node.UID && entry.ClaimID == claimID && sameTests(entry.Tests, supersedeTests))
	})
}

func finishOwnedSessionRelease(node *corev1.Node, request *v1alpha1.ValidationRequest,
	sessionEnded, release bool, gate *v1alpha1.SchedulingGateConfig) bool {
	changed := false

	if node.Annotations[annotationActiveValidationRequest] == request.Name {
		delete(node.Annotations, annotationActiveValidationRequest)

		changed = true
	}

	_, quarantined := node.Annotations[annotationQuarantineHealthEvent]
	if sessionEnded && !quarantined && !hasPendingValidationHandoff(node) {
		if release {
			changed = applySchedulingGateRelease(node, gate) || changed
		} else {
			// Ownership gates only the cordon. The configured taints are lifted when
			// the session ends, exactly as the default release policy lifts them.
			changed = releaseConfiguredTaints(node, gate) || changed
		}
	}

	changed = retireCompletedClaim(node, request) || changed

	return changed
}
