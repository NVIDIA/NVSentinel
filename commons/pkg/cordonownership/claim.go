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

// Package cordonownership defines the persisted claim shared by quarantine and validation.
package cordonownership

import (
	"encoding/json"
	"fmt"
	"slices"

	"github.com/google/uuid"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
)

const (
	AnnotationKey                  = "nvsentinel.nvidia.com/cordon-claim"
	RequestClaimIDAnnotationKey    = "nvsentinel.nvidia.com/cordon-claim-id"
	ValidationSessionAnnotationKey = "nvsentinel.nvidia.com/validation-session"
	QuarantineAnnotationKey        = "quarantineHealthEvent"
	PendingHandoffAnnotationKey    = "quarantineValidationHealthEvent"
)

// Claim identifies one continuous cordon acquired by NVSentinel on a specific Node object.
type Claim struct {
	ID             string    `json:"id"`
	NodeUID        types.UID `json:"nodeUID"`
	PreserveCordon bool      `json:"preserveCordon,omitempty"`
}

// New creates a fresh identity. The caller must prove acquisition before storing it.
func New(node *corev1.Node) Claim {
	return Claim{ID: uuid.NewString(), NodeUID: node.UID}
}

// Read validates identity, but not active associations or permission to release.
func Read(node *corev1.Node) (Claim, bool) {
	var claim Claim
	if err := json.Unmarshal([]byte(node.Annotations[AnnotationKey]), &claim); err != nil {
		return Claim{}, false
	}

	return claim, claim.ID != "" && claim.NodeUID != "" && claim.NodeUID == node.UID
}

// Store changes only the in-memory Node. The caller persists it with a version precondition.
func Store(node *corev1.Node, claim Claim) (bool, error) {
	if claim.ID == "" || claim.NodeUID == "" || claim.NodeUID != node.UID {
		return false, fmt.Errorf("cordon claim has no matching identity for node %s", node.Name)
	}

	encoded, err := json.Marshal(claim)
	if err != nil {
		return false, fmt.Errorf("encode cordon claim: %w", err)
	}

	if node.Annotations[AnnotationKey] == string(encoded) {
		return false, nil
	}

	if node.Annotations == nil {
		node.Annotations = make(map[string]string)
	}

	node.Annotations[AnnotationKey] = string(encoded)

	return true, nil
}

// Delete retires the in-memory claim without changing scheduling state.
func Delete(node *corev1.Node) bool {
	_, present := node.Annotations[AnnotationKey]
	delete(node.Annotations, AnnotationKey)

	return present
}

// HasAssociations is conservative: even a malformed session prevents early retirement.
func HasAssociations(node *corev1.Node) bool {
	_, quarantined := node.Annotations[QuarantineAnnotationKey]
	_, pending := node.Annotations[PendingHandoffAnnotationKey]

	return quarantined || pending ||
		node.Annotations[ValidationSessionAnnotationKey] != ""
}

// Inheritable requires active evidence as well as a matching claim. A detached historical claim is insufficient.
func Inheritable(node *corev1.Node) (Claim, bool) {
	claim, valid := Read(node)
	if !valid || !node.Spec.Unschedulable || claim.PreserveCordon || !HasAssociations(node) {
		return Claim{}, false
	}

	if !sessionAllowsInheritance(node.Annotations[ValidationSessionAnnotationKey], claim) {
		return Claim{}, false
	}

	active := hasHealthEvents(node.Annotations[QuarantineAnnotationKey]) ||
		hasHealthEvents(node.Annotations[PendingHandoffAnnotationKey]) ||
		node.Annotations[ValidationSessionAnnotationKey] != ""

	return claim, active
}

// Both quarantine and pending handoff annotations contain arrays of health events.
func hasHealthEvents(raw string) bool {
	var events []json.RawMessage

	return json.Unmarshal([]byte(raw), &events) == nil && len(events) > 0
}

// sessionReference reads only the ownership fields from the validation session's additive schema.
type sessionReference struct {
	NodeUID         types.UID `json:"nodeUID"`
	RequestUID      types.UID `json:"requestUID"`
	ClaimID         string    `json:"claimID"`
	CordonOwnership string    `json:"cordonOwnership"`
	PreserveCordon  bool      `json:"preserveCordon"`
}

func sessionAllowsInheritance(raw string, claim Claim) bool {
	if raw == "" {
		return true
	}

	var entries []sessionReference
	if err := json.Unmarshal([]byte(raw), &entries); err != nil || len(entries) == 0 {
		return false
	}

	return !slices.ContainsFunc(entries, func(entry sessionReference) bool {
		return entry.NodeUID != claim.NodeUID || entry.RequestUID == "" || entry.ClaimID != claim.ID ||
			entry.CordonOwnership != "owned" || entry.PreserveCordon
	})
}
