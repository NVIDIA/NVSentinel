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
	"crypto/sha256"
	"fmt"

	corev1 "k8s.io/api/core/v1"

	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

const (
	annotationQuarantineValidationHealthEvent = "quarantineValidationHealthEvent"
	annotationValidationSourceHash            = "nvsentinel.nvidia.com/validation-source-hash"
)

type validationHandoffPendingError struct {
	nodeName string
}

func (e *validationHandoffPendingError) Error() string {
	return fmt.Sprintf("node %q has a pending validation handoff", e.nodeName)
}

func hasPendingValidationHandoff(node *corev1.Node) bool {
	_, pending := node.Annotations[annotationQuarantineValidationHealthEvent]
	return pending
}

func canRegisterOwnedHandoff(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	_, usesHandoff := request.Annotations[annotationValidationSourceHash]
	return !usesHandoff || hasPendingValidationHandoff(node) || ownedSessionMatches(node, request)
}

func canRegisterOwnedRequest(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	return ownedRegistrationRefusal(node, request) == ""
}

// ownedRegistrationRefusal explains why a request cannot join the node session
// yet. It returns "" when registration may proceed.
func ownedRegistrationRefusal(node *corev1.Node, request *v1alpha1.ValidationRequest) string {
	switch {
	case requestNodeIdentityConflicts(node, request):
		return "request owner reference names a different Node object"
	case !requestClaimMatches(node, request):
		return "request claim does not match the node's cordon claim"
	case !canRegisterOwnedHandoff(node, request):
		return "persisted handoff source is not on the node"
	case hasLegacyRequestSession(node, request):
		return "node session holds an entry for this request name without object identities"
	default:
		return ""
	}
}

// Consume the producer's exact source only in the same patch that stores this
// request in the session. The primary quarantine must already be cleared so a
// producer retry can still reconstruct the request until its handoff completes.
func consumeValidationHandoff(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	source, pending := node.Annotations[annotationQuarantineValidationHealthEvent]
	if !pending || request.UID == "" || node.UID == "" ||
		requestNodeIdentityConflicts(node, request) || !hasMatchingNodeOwner(node, request) ||
		!requestClaimMatches(node, request) {
		return false
	}

	if _, quarantined := node.Annotations[annotationQuarantineHealthEvent]; quarantined {
		return false
	}

	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
	if request.Annotations[annotationValidationSourceHash] != hash {
		return false
	}

	delete(node.Annotations, annotationQuarantineValidationHealthEvent)

	return true
}

// Name-only legacy entries cannot prove that either original API object still
// exists. Finish them before enabling strict ownership instead of upgrading IDs.
func hasLegacyRequestSession(node *corev1.Node, request *v1alpha1.ValidationRequest) bool {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return true
	}

	for _, entry := range entries {
		if entry.Name == request.Name && (entry.NodeUID == "" || entry.RequestUID == "") {
			return true
		}
	}

	return false
}
