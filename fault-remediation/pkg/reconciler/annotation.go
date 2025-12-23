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

package reconciler

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/util/retry"
)

const (
	// AnnotationKey is the key for the node annotation that tracks remediation state
	AnnotationKey = "latestFaultRemediationState"
)

// NodeAnnotationManagerInterface defines the interface for managing node annotations
type NodeAnnotationManagerInterface interface {
	GetRemediationState(ctx context.Context, nodeName string) (*RemediationStateAnnotation, error)
	UpdateRemediationState(ctx context.Context, nodeName string, group string, crName string, actionName string) error
	ClearRemediationState(ctx context.Context, nodeName string) error
	RemoveGroupFromState(ctx context.Context, nodeName string, group string) error
}

// RemediationStateAnnotation represents the structure of the node annotation
type RemediationStateAnnotation struct {
	EquivalenceGroups map[string]EquivalenceGroupState `json:"equivalenceGroups"`
}

// EquivalenceGroupState represents the state of a single equivalence group
type EquivalenceGroupState struct {
	MaintenanceCR string    `json:"maintenanceCR"`
	CreatedAt     time.Time `json:"createdAt"`

	// Action that created the CR (e.g., "RESTART_BM")
	ActionName string `json:"actionName"`
}

// NodeAnnotationManager manages node annotations for tracking remediation state
type NodeAnnotationManager struct {
	kubeClient kubernetes.Interface
}

// NewNodeAnnotationManager creates a new NodeAnnotationManager
func NewNodeAnnotationManager(kubeClient kubernetes.Interface) *NodeAnnotationManager {
	return &NodeAnnotationManager{
		kubeClient: kubeClient,
	}
}

// patchNodeWithRetry applies a patch to a node with exponential backoff retry
func (m *NodeAnnotationManager) patchNodeWithRetry(ctx context.Context, nodeName string, patch []byte) error {
	return retry.OnError(retry.DefaultRetry, isRetryableError, func() error {
		_, err := m.kubeClient.CoreV1().Nodes().Patch(ctx, nodeName, types.MergePatchType, patch, metav1.PatchOptions{})
		if err != nil && isRetryableError(err) {
			slog.Warn("Retryable error patching node annotation. Retrying...",
				"node", nodeName,
				"error", err)
		}

		if err != nil {
			return fmt.Errorf("failed to patch node %s: %w", nodeName, err)
		}

		return nil
	})
}

// isRetryableError checks if an error is retryable
func isRetryableError(err error) bool {
	// Conflict errors (resource version conflicts)
	if errors.IsConflict(err) {
		return true
	}

	// Server timeout or rate limiting
	if errors.IsServerTimeout(err) || errors.IsTooManyRequests(err) {
		return true
	}

	// Temporary network errors
	if errors.IsTimeout(err) || errors.IsServiceUnavailable(err) {
		return true
	}

	return false
}

// GetRemediationState retrieves the current remediation state from node annotation
func (m *NodeAnnotationManager) GetRemediationState(
	ctx context.Context,
	nodeName string,
) (*RemediationStateAnnotation, error) {
	var node *corev1.Node

	err := retry.OnError(retry.DefaultRetry, isRetryableError, func() error {
		n, err := m.kubeClient.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		if err != nil {
			if isRetryableError(err) {
				slog.Warn("Retryable error getting node", "node", nodeName, "error", err)
			}

			return err
		}

		node = n

		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	annotationValue, exists := node.Annotations[AnnotationKey]
	if !exists {
		// No annotation means no active remediations
		return &RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]EquivalenceGroupState),
		}, nil
	}

	var state RemediationStateAnnotation
	if err := json.Unmarshal([]byte(annotationValue), &state); err != nil {
		slog.Error("Failed to unmarshal annotation", "node", nodeName, "error", err)
		// Return empty state if unmarshal fails
		return &RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]EquivalenceGroupState),
		}, nil
	}

	return &state, nil
}

// UpdateRemediationState updates the node annotation with new remediation state
func (m *NodeAnnotationManager) UpdateRemediationState(ctx context.Context, nodeName string,
	group string, crName string, actionName string) error {
	// Get current state
	state, err := m.GetRemediationState(ctx, nodeName)
	if err != nil {
		// Log but continue with empty state
		slog.Warn("Failed to get current remediation state", "node", nodeName, "error", err)

		state = &RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]EquivalenceGroupState),
		}
	}

	// Update state for the group
	state.EquivalenceGroups[group] = EquivalenceGroupState{
		MaintenanceCR: crName,
		ActionName:    actionName,
		CreatedAt:     time.Now().UTC(),
	}

	// Marshal to JSON
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal remediation state: %w", err)
	}

	// Prepare patch
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationKey, string(stateJSON)))

	// Apply patch with retry
	if err := m.patchNodeWithRetry(ctx, nodeName, patch); err != nil {
		return fmt.Errorf("failed to update remediation state annotation for %s: %w", nodeName, err)
	}

	slog.Info("Updated remediation state annotation for node",
		"node", nodeName,
		"group", group,
		"crName", crName)

	return nil
}

// ClearRemediationState removes the remediation state annotation from a node
func (m *NodeAnnotationManager) ClearRemediationState(ctx context.Context, nodeName string) error {
	// Prepare patch to remove the annotation
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:null}}}`, AnnotationKey))

	// Apply patch with retry
	if err := m.patchNodeWithRetry(ctx, nodeName, patch); err != nil {
		return fmt.Errorf("failed to clear remediation state annotation for node %s: %w", nodeName, err)
	}

	slog.Info("Cleared remediation state annotation for node", "node", nodeName)

	return nil
}

// RemoveGroupFromState removes a specific group from the remediation state
func (m *NodeAnnotationManager) RemoveGroupFromState(ctx context.Context, nodeName string, group string) error {
	state, err := m.GetRemediationState(ctx, nodeName)
	if err != nil {
		return fmt.Errorf("failed to get current remediation state: %w", err)
	}

	// Remove the group
	delete(state.EquivalenceGroups, group)

	// If no groups remain, clear the entire annotation
	if len(state.EquivalenceGroups) == 0 {
		return m.ClearRemediationState(ctx, nodeName)
	}

	// Marshal to JSON
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal remediation state: %w", err)
	}

	// Prepare patch
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, AnnotationKey, string(stateJSON)))

	// Apply patch with retry
	if err := m.patchNodeWithRetry(ctx, nodeName, patch); err != nil {
		return fmt.Errorf("failed to remove group from node annotation for %s: %w", nodeName, err)
	}

	slog.Info("Removed group from remediation state for node", "node", nodeName, "group", group)

	return nil
}
