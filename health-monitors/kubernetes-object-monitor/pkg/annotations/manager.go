// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package annotations

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"maps"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	AnnotationKey = "nvsentinel.nvidia.com/k8s-object-monitor-policy-matches"

	// loadPageSize is the number of nodes in each page of the startup load.
	loadPageSize = 500
)

type PolicyMatchState map[string]string

type Manager struct {
	client client.Client
}

func NewManager(c client.Client) *Manager {
	return &Manager{client: c}
}

func (m *Manager) AddMatch(ctx context.Context, nodeName, stateKey, targetNode string) error {
	slog.Debug("Adding match state to annotation", "node", nodeName, "stateKey", stateKey, "targetNode", targetNode)

	return m.updateAnnotation(ctx, nodeName, func(state PolicyMatchState) (PolicyMatchState, bool) {
		if _, exists := state[stateKey]; exists {
			return state, false
		}

		state[stateKey] = targetNode

		return state, true
	})
}

func (m *Manager) RemoveMatch(ctx context.Context, nodeName, stateKey string) error {
	slog.Debug("Removing match state from annotation", "node", nodeName, "stateKey", stateKey)

	err := m.updateAnnotation(ctx, nodeName, func(state PolicyMatchState) (PolicyMatchState, bool) {
		if _, exists := state[stateKey]; !exists {
			return state, false
		}

		delete(state, stateKey)

		return state, true
	})
	if apierrors.IsNotFound(err) {
		return nil
	}

	return err
}

func (m *Manager) GetMatches(ctx context.Context, nodeName string) (PolicyMatchState, error) {
	node := &v1.Node{}
	if err := m.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		if apierrors.IsNotFound(err) {
			return make(PolicyMatchState), nil
		}

		return make(PolicyMatchState), fmt.Errorf("failed to get node: %w", err)
	}

	return parseMatches(node.Annotations[AnnotationKey])
}

// LoadAllMatches reads the match state from the annotations of every node. It runs at
// startup, before the manager cache starts, so reader must not be the cached client.
// It lists only node metadata, one page at a time, so that the load stays small on
// large clusters.
func (m *Manager) LoadAllMatches(ctx context.Context, reader client.Reader) (map[string]string, error) {
	allMatches := make(map[string]string)
	continueToken := ""

	for {
		nodeList := &metav1.PartialObjectMetadataList{}
		nodeList.SetGroupVersionKind(v1.SchemeGroupVersion.WithKind("NodeList"))

		if err := reader.List(ctx, nodeList, client.Limit(loadPageSize), client.Continue(continueToken)); err != nil {
			return nil, fmt.Errorf("failed to list nodes: %w", err)
		}

		for i := range nodeList.Items {
			node := &nodeList.Items[i]

			matches, err := parseMatches(node.Annotations[AnnotationKey])
			if err != nil {
				slog.Warn("Failed to load matches from node", "node", node.Name, "error", err)
				continue
			}

			maps.Copy(allMatches, matches)
		}

		continueToken = nodeList.GetContinue()
		if continueToken == "" {
			return allMatches, nil
		}
	}
}

func parseMatches(annotationStr string) (PolicyMatchState, error) {
	if annotationStr == "" {
		return make(PolicyMatchState), nil
	}

	var state PolicyMatchState
	if err := json.Unmarshal([]byte(annotationStr), &state); err != nil {
		return nil, fmt.Errorf("failed to unmarshal annotation: %w", err)
	}

	return state, nil
}

func (m *Manager) updateAnnotation(ctx context.Context,
	nodeName string,
	updateFn func(PolicyMatchState) (PolicyMatchState, bool),
) error {
	slog.Debug("Updating annotation", "node", nodeName)

	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node := &v1.Node{}
		if err := m.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			if apierrors.IsNotFound(err) {
				slog.Warn("Node not found, assuming deleted", "node", nodeName)
				return nil
			}

			return fmt.Errorf("failed to get node: %w", err)
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		state := make(PolicyMatchState)
		if existingAnnotation := node.Annotations[AnnotationKey]; existingAnnotation != "" {
			if err := json.Unmarshal([]byte(existingAnnotation), &state); err != nil {
				slog.Warn("Failed to parse existing annotation, starting fresh", "node", nodeName, "error", err)
			}
		}

		updatedState, changed := updateFn(state)
		if !changed {
			return nil
		}

		if len(updatedState) == 0 {
			delete(node.Annotations, AnnotationKey)
		} else {
			annotationBytes, err := json.Marshal(updatedState)
			if err != nil {
				return fmt.Errorf("failed to marshal state: %w", err)
			}

			node.Annotations[AnnotationKey] = string(annotationBytes)
		}

		return m.client.Update(ctx, node, &client.UpdateOptions{
			FieldManager: "kubernetes-object-monitor",
		})
	})
}
