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
		slog.ErrorContext(ctx, "Failed to unmarshal annotation", "node", nodeName, "error", err)
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
			slog.WarnContext(ctx, "Failed to get current remediation state", "node", nodeName, "error", err)
			return err
		}

		// Preserve the attempt count: it is owned by RecordRemediationAttempt, which runs
		// before the CR is created so that failed creations are counted too.
		// Update state for the group
		state.EquivalenceGroups[group] = EquivalenceGroupState{
			MaintenanceCR: crName,
			CreatedAt:     time.Now().UTC(),
			ActionName:    actionName,
			AttemptCount:  state.EquivalenceGroups[group].AttemptCount,
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

		slog.InfoContext(ctx, "Updated remediation state annotation for node",
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

// RecordRemediationAttempt increments the attempt counter for a group and returns the new
// value. It is called before the maintenance CR is created so that attempts which never
// produce a CR (missing CRD, RBAC denial, rejecting webhook) are still counted and cannot
// loop past the configured cap. The group entry is created if absent; MaintenanceCR and
// ActionName stay empty until UpdateRemediationState records the CR that was created.
func (m *NodeAnnotationManager) RecordRemediationAttempt(ctx context.Context, nodeName string,
	group string) (int, error) {
	attemptCount := 0

	err := retry.RetryOnConflict(conflictBackoff, func() error {
		state, node, err := m.GetRemediationState(ctx, nodeName)
		if err != nil {
			return err
		}

		// A missing key yields the zero struct, so this also covers the first attempt.
		groupState := state.EquivalenceGroups[group]
		groupState.AttemptCount++
		attemptCount = groupState.AttemptCount
		state.EquivalenceGroups[group] = groupState

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

		slog.InfoContext(ctx, "Recorded remediation attempt for node",
			"node", nodeName,
			"group", group,
			"attemptCount", attemptCount)

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("failed to record remediation attempt for node %s: %w", nodeName, err)
	}

	return attemptCount, nil
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

		slog.InfoContext(ctx, "Cleared remediation state annotation for node", "node", nodeName)

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to clear remediation state for node %s: %w", nodeName, err)
	}

	return nil
}

// endGroupSessions clears the CR reference of each named group while keeping its attempt
// budget, and drops groups that never recorded an attempt.
func endGroupSessions(state *RemediationStateAnnotation, groups []string) {
	for _, group := range groups {
		groupState, exists := state.EquivalenceGroups[group]
		if !exists {
			continue
		}

		if groupState.AttemptCount == 0 {
			delete(state.EquivalenceGroups, group)
			continue
		}

		state.EquivalenceGroups[group] = EquivalenceGroupState{AttemptCount: groupState.AttemptCount}
	}
}

// RemoveGroupsFromState ends the remediation session for multiple groups in a single atomic
// read-modify-write operation. This avoids the race condition that occurs when removing groups
// one at a time in a loop.
//
// The CR reference is dropped so a new remediation may start, but AttemptCount is carried over:
// groups are removed precisely when their CR failed or vanished, which is the case the attempt
// cap exists to stop. Deleting the count here would reset the budget on every failure and the
// cap could never be reached. The count is cleared by ClearRemediationState when the quarantine
// session actually ends.
func (m *NodeAnnotationManager) RemoveGroupsFromState(ctx context.Context, nodeName string, groups []string) error {
	err := retry.RetryOnConflict(conflictBackoff, func() error {
		state, node, err := m.GetRemediationState(ctx, nodeName)
		if err != nil {
			return err
		}

		// Drop the CR reference but keep the attempt budget for this session.
		endGroupSessions(state, groups)

		// If no groups remain, clear the entire annotation
		if len(state.EquivalenceGroups) == 0 {
			updatedNode := node.DeepCopy()
			if updatedNode.Annotations != nil {
				delete(updatedNode.Annotations, AnnotationKey)
			}

			if err = m.client.Update(ctx, updatedNode); err != nil {
				return err
			}

			slog.InfoContext(ctx, "Cleared remediation state annotation for node", "node", nodeName)

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

		slog.InfoContext(ctx, "Removed groups from remediation state for node", "node", nodeName, "groups", groups)

		return nil
	})
	if err != nil {
		return fmt.Errorf("failed to remove groups from remediation state for node %s: %w", nodeName, err)
	}

	return nil
}
