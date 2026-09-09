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

package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/commons/pkg/kubeclient"
	"github.com/nvidia/nvsentinel/lifecycle-manager/api/v1alpha1"
)

type sessionEntry struct {
	Name   string   `json:"name"`
	Tests  []string `json:"tests,omitempty"`
	Failed bool     `json:"failed,omitempty"`
}

func marshalSessionAnnotation(entries []sessionEntry) (string, error) {
	b, err := json.Marshal(entries)
	if err != nil {
		return "", fmt.Errorf("marshal %q annotation: %w", annotationValidationSession, err)
	}

	return string(b), nil
}

func parseSessionAnnotation(sessionAnnotation string) ([]sessionEntry, error) {
	if len(sessionAnnotation) == 0 {
		return nil, nil
	}

	var entries []sessionEntry

	if err := json.Unmarshal([]byte(sessionAnnotation), &entries); err != nil {
		return nil, fmt.Errorf("unmarshal %q annotation: %w", annotationValidationSession, err)
	}

	return entries, nil
}

func (r *ValidationRequestReconciler) addToSessionAndCheckEligibility(ctx context.Context, ns v1alpha1.NodeSpec,
	validationRequest *v1alpha1.ValidationRequest, criteria []v1alpha1.CriteriaSpec,
	resolvedTests []string) (exists bool, ready bool, err error) {
	var node corev1.Node
	if err := r.Get(ctx, client.ObjectKey{Name: ns.Name}, &node); err != nil {
		if apierrors.IsNotFound(err) {
			return false, false, nil
		}

		return false, false, fmt.Errorf("get node %q: %w", ns.Name, err)
	}

	if err := r.updateValidationSession(ctx, ns.Name, validationRequest.Name, resolvedTests); err != nil {
		return true, false, fmt.Errorf("ensure session on node %q: %w", ns.Name, err)
	}

	activeValidationRequest := node.Annotations[annotationActiveValidationRequest]
	if len(activeValidationRequest) != 0 && activeValidationRequest != validationRequest.Name {
		slog.InfoContext(ctx, "Node claimed by another validation request, waiting", "node", ns.Name,
			"activeValidationRequest", activeValidationRequest)

		return true, false, nil
	}

	failedCriteria, err := r.evaluateNodeReadinessCriteria(&node, criteria)
	if err != nil {
		slog.ErrorContext(ctx, "Failed to check node readiness", "node", ns.Name, "error", err)
		return true, false, fmt.Errorf("failed to check node readiness: %w", err)
	}

	if len(failedCriteria) != 0 {
		slog.InfoContext(ctx, "Node not ready, waiting", "node", ns.Name, "criterion", failedCriteria)
		return true, false, nil
	}

	return true, true, nil
}

func (r *ValidationRequestReconciler) updateValidationSession(ctx context.Context, nodeName,
	validationRequestName string, tests []string) error {
	return r.patchNodeWithRetry(ctx, nodeName, func(node *corev1.Node) (bool, error) {
		entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
		if err != nil {
			return false, err
		}

		for _, e := range entries {
			if e.Name == validationRequestName {
				return false, nil
			}
		}

		entries = append(entries, sessionEntry{Name: validationRequestName, Tests: tests})

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		annotation, err := marshalSessionAnnotation(entries)
		if err != nil {
			return false, err
		}

		node.Annotations[annotationValidationSession] = annotation

		return true, nil
	})
}

func (r *ValidationRequestReconciler) releaseNodesFromSuccessfulRequest(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest, supersedeTests []string) error {
	for _, ns := range validationRequest.Spec.Nodes {
		err := r.patchNodeWithRetry(ctx, ns.Name, func(node *corev1.Node) (bool, error) {
			return releaseNodeFromSession(node, validationRequest.Name, supersedeTests, r.Config.Validation.Spec.SchedulingGate)
		})
		if err != nil {
			return fmt.Errorf("release node %q from successful request: %w", ns.Name, err)
		}
	}

	return nil
}

func releaseNodeFromSession(node *corev1.Node, validationRequestName string, supersedeTests []string,
	schedulingGate *v1alpha1.SchedulingGateConfig) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	remaining, sessionBecameEmpty := removeSessionEntry(entries, validationRequestName, supersedeTests)

	changed := false

	if len(remaining) != len(entries) {
		if sessionBecameEmpty {
			delete(node.Annotations, annotationValidationSession)
		} else {
			annotation, err := marshalSessionAnnotation(remaining)
			if err != nil {
				return false, err
			}

			node.Annotations[annotationValidationSession] = annotation
		}

		changed = true
	}

	if node.Annotations[annotationActiveValidationRequest] == validationRequestName {
		delete(node.Annotations, annotationActiveValidationRequest)

		changed = true
	}

	if sessionBecameEmpty && applySchedulingGateRelease(node, schedulingGate) {
		changed = true
	}

	return changed, nil
}

func markNodeSessionEntryFailed(node *corev1.Node, validationRequestName string) (bool, error) {
	entries, err := parseSessionAnnotation(node.Annotations[annotationValidationSession])
	if err != nil {
		return false, err
	}

	changed := false

	for i := range entries {
		if entries[i].Name == validationRequestName && !entries[i].Failed {
			entries[i].Failed = true
			changed = true
		}
	}

	if changed {
		annotation, err := marshalSessionAnnotation(entries)
		if err != nil {
			return false, err
		}

		node.Annotations[annotationValidationSession] = annotation
	}

	if node.Annotations[annotationActiveValidationRequest] == validationRequestName {
		delete(node.Annotations, annotationActiveValidationRequest)

		changed = true
	}

	return changed, nil
}

func (r *ValidationRequestReconciler) releaseNodesFromFailedRequest(ctx context.Context,
	validationRequest *v1alpha1.ValidationRequest) error {
	for _, ns := range validationRequest.Spec.Nodes {
		err := r.patchNodeWithRetry(ctx, ns.Name, func(node *corev1.Node) (bool, error) {
			return markNodeSessionEntryFailed(node, validationRequest.Name)
		})
		if err != nil {
			return fmt.Errorf("mark node %q failed in session: %w", ns.Name, err)
		}
	}

	return nil
}

func (r *ValidationRequestReconciler) setActiveValidationRequest(ctx context.Context,
	nodeName, validationRequestName string) error {
	return r.patchNodeWithRetry(ctx, nodeName, func(node *corev1.Node) (bool, error) {
		if node.Annotations[annotationActiveValidationRequest] == validationRequestName {
			return false, nil
		}

		if node.Annotations == nil {
			node.Annotations = make(map[string]string)
		}

		node.Annotations[annotationActiveValidationRequest] = validationRequestName

		return true, nil
	})
}

func (r *ValidationRequestReconciler) fetchDeletedAndNotReadyNodes(ctx context.Context,
	g *v1alpha1.TestGroupStatus) ([]string, []string, error) {
	var deletedNodes, nodesFailingReadiness []string

	for _, nodeName := range g.Nodes {
		var node corev1.Node
		if err := r.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			if apierrors.IsNotFound(err) {
				deletedNodes = append(deletedNodes, nodeName)
				continue
			}

			return nil, nil, fmt.Errorf("get node %q: %w", nodeName, err)
		}

		failedCriteria, err := r.evaluateNodeReadinessCriteria(&node, r.Config.Validation.Spec.ReadinessCriteria)
		if err != nil {
			return nil, nil, fmt.Errorf("failed to check node readiness: %w", err)
		}

		if len(failedCriteria) != 0 {
			nodesFailingReadiness = append(nodesFailingReadiness, nodeName)
		}
	}

	return deletedNodes, nodesFailingReadiness, nil
}

func (r *ValidationRequestReconciler) patchNodeWithRetry(ctx context.Context, nodeName string,
	mutate func(*corev1.Node) (changed bool, err error)) error {
	return kubeclient.RetryNodePatch(func() error {
		var node corev1.Node
		if err := r.APIReader.Get(ctx, client.ObjectKey{Name: nodeName}, &node); err != nil {
			return client.IgnoreNotFound(err)
		}

		original := node.DeepCopy()

		changed, err := mutate(&node)
		if err != nil {
			return err
		}

		if !changed {
			return nil
		}

		patch, err := kubeclient.NodeMergePatch(original, &node)
		if err != nil {
			return fmt.Errorf("build merge patch for node %q: %w", nodeName, err)
		}

		if patch == nil {
			return nil
		}

		return r.Patch(ctx, &node, client.RawPatch(types.MergePatchType, patch))
	})
}

func removeSessionEntry(entries []sessionEntry, validationRequestName string,
	supersedeTests []string) (remaining []sessionEntry, sessionBecameEmpty bool) {
	for _, e := range entries {
		if e.Name == validationRequestName {
			continue
		}

		if supersedeTests != nil && e.Failed && sameTests(e.Tests, supersedeTests) {
			continue
		}

		remaining = append(remaining, e)
	}

	return remaining, len(entries) > 0 && len(remaining) == 0
}

func applySchedulingGateRelease(node *corev1.Node, schedulingGate *v1alpha1.SchedulingGateConfig) bool {
	if schedulingGate == nil {
		return false
	}

	changed := false

	if schedulingGate.Cordon.Remove && node.Spec.Unschedulable {
		node.Spec.Unschedulable = false
		changed = true
	}

	remainingTaints, taintsChanged := removeConfiguredTaints(node.Spec.Taints, schedulingGate.Taints)
	node.Spec.Taints = remainingTaints
	changed = changed || taintsChanged

	return changed
}

func removeConfiguredTaints(taints []corev1.Taint, configuredTaints []v1alpha1.TaintConfig) ([]corev1.Taint, bool) {
	remaining := slices.DeleteFunc(slices.Clone(taints), func(t corev1.Taint) bool {
		return slices.ContainsFunc(configuredTaints, func(cfg v1alpha1.TaintConfig) bool {
			return cfg.Remove && taintMatches(t, cfg)
		})
	})

	return remaining, len(remaining) != len(taints)
}

func taintMatches(nodeTaint corev1.Taint, taintCfg v1alpha1.TaintConfig) bool {
	return nodeTaint.Key == taintCfg.Key &&
		nodeTaint.Value == taintCfg.Value &&
		string(nodeTaint.Effect) == taintCfg.Effect
}

func sameTests(a, b []string) bool {
	sortedA := append([]string{}, a...)
	sortedB := append([]string{}, b...)

	sort.Strings(sortedA)
	sort.Strings(sortedB)

	return slices.Equal(sortedA, sortedB)
}
