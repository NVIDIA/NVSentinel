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
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// conflictBackoff is a custom retry backoff for annotation read-modify-write
// operations under concurrent access. The default retry (5 steps, 10ms, no
// exponential increase) is insufficient when multiple goroutines update the
// same node's annotations concurrently.
var conflictBackoff = wait.Backoff{
	Steps:    10,
	Duration: 20 * time.Millisecond,
	Factor:   2.0,
	Jitter:   0.1,
}

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
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		// Get current state
		state, node, err := m.GetRemediationState(ctx, nodeName)
		if err != nil {
			slog.Warn("Failed to get current remediation state", "node", nodeName, "error", err)
			return err
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
			return err
		}

		updatedNode := node.DeepCopy()
		if updatedNode.Annotations == nil {
			updatedNode.Annotations = map[string]string{}
		}

		updatedNode.Annotations[AnnotationKey] = string(stateJSON)

		if err = m.client.Update(ctx, updatedNode); err != nil {
			return err
		}

		slog.Info("Updated remediation state annotation for node",
			"node", nodeName,
			"group", group,
			"crName", crName)

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to update remediation state for node %s: %w", nodeName, err)
	}

	return nil
}

// ClearRemediationState removes the remediation state annotation from a node
func (m *NodeAnnotationManager) ClearRemediationState(ctx context.Context, nodeName string) error {
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		node := &corev1.Node{}

		if err := m.client.Get(ctx, types.NamespacedName{
			Name: nodeName,
		}, node); err != nil {
			return err
		}

		if node.Annotations == nil {
			return nil
		}

		updatedNode := node.DeepCopy()
		delete(updatedNode.Annotations, AnnotationKey)

		if err := m.client.Update(ctx, updatedNode); err != nil {
			return err
		}

		slog.Info("Cleared remediation state annotation for node", "node", nodeName)

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to clear remediation state for node %s: %w", nodeName, err)
	}

	return nil
}

// RemoveGroupsFromState removes multiple groups from the remediation state in a single atomic read-modify-write
// operation. This avoids the race condition that occurs when removing groups one at a time in a loop.
func (m *NodeAnnotationManager) RemoveGroupsFromState(ctx context.Context, nodeName string, groups []string) error {
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		state, node, err := m.GetRemediationState(ctx, nodeName)
		if err != nil {
			return err
		}

		// Remove the groups
		for _, group := range groups {
			delete(state.EquivalenceGroups, group)
		}

		// If no groups remain, clear the entire annotation
		if len(state.EquivalenceGroups) == 0 {
			updatedNode := node.DeepCopy()
			if updatedNode.Annotations != nil {
				delete(updatedNode.Annotations, AnnotationKey)
			}

			if err = m.client.Update(ctx, updatedNode); err != nil {
				return err
			}

			slog.Info("Cleared remediation state annotation for node", "node", nodeName)

			return nil
		}

		stateJSON, err := json.Marshal(state)
		if err != nil {
			return err
		}

		updatedNode := node.DeepCopy()
		if updatedNode.Annotations == nil {
			updatedNode.Annotations = map[string]string{}
		}

		updatedNode.Annotations[AnnotationKey] = string(stateJSON)

		if err = m.client.Update(ctx, updatedNode); err != nil {
			return err
		}

		slog.Info("Removed groups from remediation state for node", "node", nodeName, "groups", groups)

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to remove groups from remediation state for node %s: %w", nodeName, err)
	}

	return nil
}
