// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

// Package state persists the monitor's bookkeeping on Kubernetes objects: the
// per-node annotation and label that record which certification-failure tuples
// are held on a node, and the per-Certification cert-processed and
// error-recovered annotations that make sweep decisions restart-safe.
package state

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// AnnotationKey is the node annotation that holds the set of certification
	// failure tuples currently active on the node.
	AnnotationKey = "nvsentinel.dgxc.nvidia.com/nvcre-cert-failures-details"

	// LabelKey is set on nodes that carry at least one cert-failure tuple.
	LabelKey = "nvsentinel.dgxc.nvidia.com/nvcre-cert-failure"

	// FieldManager identifies this controller in server-side writes.
	FieldManager = "nvcre-certification-monitor"
)

// ErrMalformedAnnotation is returned when the node's cert-failures annotation
// is present but is not a JSON string array. The annotation is left untouched
// so an operator can inspect and repair it; callers skip the tuple (or, when
// reading, the node) and move on.
var ErrMalformedAnnotation = errors.New("malformed cert-failures annotation")

// AnnotationManager reads and writes the per-node cert-failures annotation.
type AnnotationManager struct {
	client client.Client
}

// NewAnnotationManager constructs an AnnotationManager.
func NewAnnotationManager(c client.Client) *AnnotationManager {
	return &AnnotationManager{client: c}
}

// AddTuple adds a "<variant>/<reason>" key to the node's annotation.
func (m *AnnotationManager) AddTuple(ctx context.Context, nodeName, key string) error {
	if err := m.updateAnnotation(ctx, nodeName, func(keys []string) ([]string, bool) {
		for _, k := range keys {
			if k == key {
				return keys, false
			}
		}

		return append(keys, key), true
	}); err != nil {
		if !errors.Is(err, ErrMalformedAnnotation) {
			slog.Error("failed to add tuple", "error", err, "key", key, "nodeName", nodeName)
		}

		return fmt.Errorf("failed to add tuple %q on node %s: %w", key, nodeName, err)
	}

	return nil
}

// RemoveTuple removes a "<variant>/<reason>" key from the node's annotation.
// If no keys remain, the annotation is deleted.
func (m *AnnotationManager) RemoveTuple(ctx context.Context, nodeName, key string) error {
	if err := m.updateAnnotation(ctx, nodeName, func(keys []string) ([]string, bool) {
		for i, k := range keys {
			if k == key {
				return append(keys[:i], keys[i+1:]...), true
			}
		}

		return keys, false
	}); err != nil {
		if !errors.Is(err, ErrMalformedAnnotation) {
			slog.Error("failed to remove tuple", "error", err, "key", key, "nodeName", nodeName)
		}

		return fmt.Errorf("failed to remove tuple %q from node %s: %w", key, nodeName, err)
	}

	return nil
}

func (m *AnnotationManager) updateAnnotation(
	ctx context.Context,
	nodeName string,
	updateFn func([]string) ([]string, bool),
) error {
	return retry.RetryOnConflict(retry.DefaultBackoff, func() error {
		node := &corev1.Node{}

		if err := m.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
			if apierrors.IsNotFound(err) {
				slog.Warn("Node does not exist, skipping annotation update", "node", nodeName)

				return nil
			}

			return fmt.Errorf("failed to get node: %w", err)
		}

		// Capture the unmodified node so the patch only carries the fields we
		// actually change.
		base := node.DeepCopy()

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		existing, err := parseExistingKeys(node.Annotations[AnnotationKey])
		if err != nil {
			return fmt.Errorf("node %s: %w", nodeName, err)
		}

		updated, changed := updateFn(existing)
		if !changed {
			return nil
		}

		if node.Labels == nil {
			node.Labels = make(map[string]string)
		}

		if len(updated) == 0 {
			delete(node.Annotations, AnnotationKey)
			delete(node.Labels, LabelKey)
		} else {
			b, err := json.Marshal(updated)
			if err != nil {
				slog.Error("failed to marshal annotation", "error", err, "nodeName", nodeName)

				return fmt.Errorf("failed to marshal annotation: %w", err)
			}

			node.Annotations[AnnotationKey] = string(b)
			node.Labels[LabelKey] = "true"
		}

		if err := m.client.Patch(ctx, node, client.MergeFrom(base), &client.PatchOptions{
			FieldManager: FieldManager,
		}); err != nil {
			slog.Error("failed to update node annotation", "error", err, "nodeName", nodeName)

			return fmt.Errorf("failed to update node %s annotation: %w", nodeName, err)
		}

		return nil
	})
}

func parseExistingKeys(raw string) ([]string, error) {
	if raw == "" {
		return nil, nil
	}

	var existing []string
	if err := json.Unmarshal([]byte(raw), &existing); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMalformedAnnotation, err)
	}

	return existing, nil
}

// ParseAnnotation extracts the set of keys from a node's cert-failures annotation.
//
// A nil error with an empty/nil set means the node carries no cert-failure
// annotation (or an empty list). A non-nil error means the annotation is
// present but malformed and wraps ErrMalformedAnnotation.
func (m *AnnotationManager) ParseAnnotation(node *corev1.Node) (map[string]struct{}, error) {
	if node.Annotations == nil {
		return nil, nil
	}

	raw, ok := node.Annotations[AnnotationKey]
	if !ok || raw == "" {
		return nil, nil
	}

	var keys []string

	if err := json.Unmarshal([]byte(raw), &keys); err != nil {
		return nil, fmt.Errorf("%w on node %s: %w", ErrMalformedAnnotation, node.Name, err)
	}

	if len(keys) == 0 {
		return nil, nil
	}

	set := make(map[string]struct{}, len(keys))

	for _, k := range keys {
		set[k] = struct{}{}
	}

	return set, nil
}
