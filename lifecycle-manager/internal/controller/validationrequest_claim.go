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
	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/commons/pkg/cordonownership"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

func requestClaimID(request *v1alpha1.ValidationRequest) string {
	return request.Annotations[cordonownership.RequestClaimIDAnnotationKey]
}

// A missing claim ID is the legacy protocol. A declared claim must identify
// the current Node, including when preservation prevents automatic release.
func requestClaimMatches(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	id, declared := request.Annotations[cordonownership.RequestClaimIDAnnotationKey]
	if !declared {
		_, present := node.Annotations[cordonownership.AnnotationKey]
		return !present
	}

	claim, valid := cordonownership.Read(node)

	return valid && id != "" && claim.ID == id
}

func claimAllowsRelease(node *corev1.Node, claimID string) bool {
	_, present := node.Annotations[cordonownership.AnnotationKey]
	if claimID == "" {
		return !present
	}

	claim, valid := cordonownership.Read(node)

	return valid && claim.ID == claimID && !claim.PreserveCordon
}

// Preservation belongs to the continuous cordon, not the lifetime of one
// request. Persist it even while a successor exists only as a pending handoff.
func preserveUntrustedSession(node *corev1.Node, entries []sessionEntry) (bool, error) {
	associated := currentClaimSession(node, entries)
	if len(associated) == 0 || sessionOwnsCordon(node, associated) {
		return false, nil
	}

	preserveCurrentClaimSession(node, entries)

	claim, valid := cordonownership.Read(node)
	if !valid || claim.PreserveCordon {
		return false, nil
	}

	claim.PreserveCordon = true

	return cordonownership.Store(node, claim)
}

// Removing the final association retires its grant even when deletion keeps the
// node cordoned. A pending producer handoff still owns an active association.
func retireCompletedClaim(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	if cordonownership.HasAssociations(node) || !requestClaimMatches(node, request) {
		return false
	}

	claim, valid := cordonownership.Read(node)
	if !valid || (!claim.PreserveCordon && claim.ID != requestClaimID(request)) {
		return false
	}

	return cordonownership.Delete(node)
}

func retireReleasedLegacyClaim(node *corev1.Node) bool {
	if node.Spec.Unschedulable || cordonownership.HasAssociations(node) {
		return false
	}

	if _, valid := cordonownership.Read(node); !valid {
		return false
	}

	return cordonownership.Delete(node)
}

func preserveDepartingSession(node *corev1.Node, request *v1alpha1.ValidationRequest,
	entries []sessionEntry) (bool, error) {
	// A late request may remove its old association, but cannot change a newer
	// continuous cordon's claim or the preservation decisions of its requests.
	if !requestClaimMatches(node, request) {
		return false, nil
	}

	if !ownedSessionMatches(node, request) || requestCordonOwnership(node, request) != cordonOwned {
		preserveCurrentClaimSession(node, entries)
	}

	return preserveUntrustedSession(node, entries)
}

func entryUsesCurrentClaim(node *corev1.Node, entry sessionEntry) bool {
	if _, present := node.Annotations[cordonownership.AnnotationKey]; !present {
		return true
	}

	claim, valid := cordonownership.Read(node)

	return valid && entry.ClaimID == claim.ID
}

func currentClaimSession(node *corev1.Node, entries []sessionEntry) []sessionEntry {
	var associated []sessionEntry

	for _, entry := range entries {
		if entryUsesCurrentClaim(node, entry) {
			associated = append(associated, entry)
		}
	}

	return associated
}

func preserveCurrentClaimSession(node *corev1.Node, entries []sessionEntry) {
	for i := range entries {
		if entryUsesCurrentClaim(node, entries[i]) {
			entries[i].PreserveCordon = true
		}
	}
}
