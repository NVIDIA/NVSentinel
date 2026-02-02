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

// Package annotation provides functionality for managing node remediation state
// through Kubernetes node annotations. It enables tracking of ongoing remediation
// actions across equivalence groups.
package annotation

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// NodeAnnotationManager manages node annotations for tracking remediation state.
type NodeAnnotationManager struct {
	client client.Client
}

// NewNodeAnnotationManager creates a new NodeAnnotationManager.
func NewNodeAnnotationManager(client client.Client) *NodeAnnotationManager {
	return &NodeAnnotationManager{
		client: client,
	}
}

// GetRemediationState retrieves the current remediation state from node annotation
func (m *NodeAnnotationManager) GetRemediationState(
	ctx context.Context,
	nodeName string,
) (*RemediationStateAnnotation, *corev1.Node, error) {
	node := &corev1.Node{}

	err := m.client.Get(ctx, types.NamespacedName{
		Name: nodeName,
	}, node)
	if err != nil {
		return nil, node, fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}
	// TODO: maybe split this up so it's not returning both node and state

	annotationValue, exists := node.Annotations[AnnotationKey]
	if !exists {
		// No annotation means no active remediations
		return &RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]EquivalenceGroupState),
		}, node, nil
	}

	var state RemediationStateAnnotation
	if err = json.Unmarshal([]byte(annotationValue), &state); err != nil {
		slog.Error("Failed to unmarshal annotation", "node", nodeName, "error", err)
		// Return empty state if unmarshal fails
		return &RemediationStateAnnotation{
			EquivalenceGroups: make(map[string]EquivalenceGroupState),
		}, node, nil
	}

	if state.EquivalenceGroups == nil {
		state.EquivalenceGroups = make(map[string]EquivalenceGroupState)
	}

	return &state, node, nil
}

// UpdateRemediationState updates the node annotation with new remediation state
func (m *NodeAnnotationManager) UpdateRemediationState(ctx context.Context, nodeName string,
	group string, crName string, actionName string) error {
	// Get current state
	state, node, err := m.GetRemediationState(ctx, nodeName)
	if err != nil {
		slog.Warn("Failed to get current remediation state", "node", nodeName, "error", err)
		return fmt.Errorf("failed to get current remediation state: %w", err)
	}

	// Update state for the group
	state.EquivalenceGroups[group] = EquivalenceGroupState{
		MaintenanceCR: crName,
		CreatedAt:     time.Now().UTC(),
		ActionName:    actionName,
	}

	// Marshal to JSON
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return fmt.Errorf("failed to marshal remediation state: %w", err)
	}

	updatedNode := node.DeepCopy()
	if updatedNode.Annotations == nil {
		updatedNode.Annotations = map[string]string{}
	}

	updatedNode.Annotations[AnnotationKey] = string(stateJSON)

	if err = m.client.Update(ctx, updatedNode); err != nil {
		return fmt.Errorf("failed to update node annotation: %w", err)
	}

	slog.Info("Updated remediation state annotation for node",
		"node", nodeName,
		"group", group,
		"crName", crName)

	return nil
}

// ClearRemediationState removes the remediation state annotation from a node
func (m *NodeAnnotationManager) ClearRemediationState(ctx context.Context, nodeName string) error {
	node := &corev1.Node{}

	err := m.client.Get(ctx, types.NamespacedName{
		Name: nodeName,
	}, node)
	if err != nil {
		return fmt.Errorf("failed to get node %s: %w", nodeName, err)
	}

	if node.Annotations == nil {
		return nil
	}

	updatedNode := node.DeepCopy()
	delete(updatedNode.Annotations, AnnotationKey)

	if err = m.client.Update(ctx, updatedNode); err != nil {
		return fmt.Errorf("failed to update node annotation: %w", err)
	}

	slog.Info("Cleared remediation state annotation for node", "node", nodeName)

	return nil
}

// RemoveGroupFromState removes a specific group from the remediation state
func (m *NodeAnnotationManager) RemoveGroupFromState(ctx context.Context, nodeName string, group string) error {
	state, node, err := m.GetRemediationState(ctx, nodeName)
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

	updatedNode := node.DeepCopy()
	if updatedNode.Annotations == nil {
		updatedNode.Annotations = map[string]string{}
	}

	updatedNode.Annotations[AnnotationKey] = string(stateJSON)

	if err = m.client.Update(ctx, updatedNode); err != nil {
		return fmt.Errorf("failed to patch node annotation: %w", err)
	}

	slog.Info("Removed group from remediation state for node", "node", nodeName, "group", group)

	return nil
}
