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

package annotation

import (
	"context"
	"time"

	corev1 "k8s.io/api/core/v1"
)

const (
	// AnnotationKey is the key for the node annotation that tracks remediation state
	AnnotationKey = "latestFaultRemediationState"
)

// NodeAnnotationManagerInterface defines the interface for managing node annotations
type NodeAnnotationManagerInterface interface {
	GetRemediationState(ctx context.Context, nodeName string) (*RemediationStateAnnotation, *corev1.Node, error)
	UpdateRemediationState(
		ctx context.Context,
		nodeName string,
		group string,
		crName string,
		actionName string,
		resourceRef MaintenanceResourceReference,
	) error
	EnsureRemediationStateGVK(
		ctx context.Context,
		nodeName string,
		resourceRefs map[string]MaintenanceResourceReference,
	) error
	ClearRemediationState(ctx context.Context, nodeName string) error
	RemoveGroupsFromState(ctx context.Context, nodeName string, groups []string) error
}

// RemediationStateAnnotation represents the structure of the node annotation
type RemediationStateAnnotation struct {
	EquivalenceGroups map[string]EquivalenceGroupState `json:"equivalenceGroups"`
}

// MaintenanceResourceReference is the concrete object identity needed to find
// a created maintenance CR without re-deriving its GVK from current config.
type MaintenanceResourceReference struct {
	Namespace string
	Version   string
	ApiGroup  string
	Kind      string
}

// EquivalenceGroupState represents the state of a single equivalence group
type EquivalenceGroupState struct {
	MaintenanceCR string    `json:"maintenanceCR"`
	CreatedAt     time.Time `json:"createdAt"`

	// Action that created the CR (e.g., "RESTART_BM"). Kept as provenance
	// and to resolve legacy annotations that predate the concrete resource fields.
	ActionName string `json:"actionName"`

	// Concrete resource identity for the CR that was created.
	Namespace string `json:"namespace,omitempty"`
	Version   string `json:"version,omitempty"`
	ApiGroup  string `json:"apiGroup,omitempty"`
	Kind      string `json:"kind,omitempty"`
}
