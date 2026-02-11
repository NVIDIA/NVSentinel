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

// Package discoverer provides gang discovery implementations for different schedulers.
package discoverer

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/cel-go/cel"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodGroupConfig defines the configuration for a PodGroup-based gang discoverer.
type PodGroupConfig struct {
	// Name is the discoverer name (e.g., "volcano").
	Name string

	// AnnotationKeys are the annotation keys to check for pod group name (in order).
	AnnotationKeys []string

	// LabelKeys are optional label keys to check as fallback (in order).
	LabelKeys []string

	// PodGroupGVK is the GroupVersionKind for the PodGroup CRD (resolved from GVR via RESTMapper).
	PodGroupGVK schema.GroupVersionKind

	// MinCountExpr is a CEL expression to extract minCount from PodGroup.
	// Receives 'podGroup' as map[string]any.
	MinCountExpr string
}

// PodGroupDiscoverer discovers gang members using PodGroup CRDs.
// This is a generic implementation that works with Volcano and similar PodGroup-based schedulers.
type PodGroupDiscoverer struct {
	client          client.Client
	config          PodGroupConfig
	minCountProgram cel.Program
}

// NewPodGroupDiscoverer creates a new PodGroup-based gang discoverer.
func NewPodGroupDiscoverer(
	c client.Client,
	config PodGroupConfig,
) (*PodGroupDiscoverer, error) {
	// Compile CEL expression for minCount extraction
	env, err := cel.NewEnv(
		cel.Variable("podGroup", cel.MapType(cel.StringType, cel.DynType)),
	)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL environment: %w", err)
	}

	ast, issues := env.Compile(config.MinCountExpr)
	if issues != nil && issues.Err() != nil {
		return nil, fmt.Errorf("failed to compile minCountExpr %q: %w", config.MinCountExpr, issues.Err())
	}

	program, err := env.Program(ast)
	if err != nil {
		return nil, fmt.Errorf("failed to create CEL program: %w", err)
	}

	return &PodGroupDiscoverer{
		client:          c,
		config:          config,
		minCountProgram: program,
	}, nil
}

// Name returns the discoverer name.
func (d *PodGroupDiscoverer) Name() string {
	return d.config.Name
}

// CanHandle returns true if the pod belongs to a gang managed by this discoverer.
func (d *PodGroupDiscoverer) CanHandle(pod *corev1.Pod) bool {
	return d.getPodGroupName(pod) != ""
}

// ExtractGangID extracts the gang identifier from a pod.
func (d *PodGroupDiscoverer) ExtractGangID(pod *corev1.Pod) string {
	podGroupName := d.getPodGroupName(pod)
	if podGroupName == "" {
		return ""
	}

	return fmt.Sprintf("%s-%s-%s", d.config.Name, pod.Namespace, podGroupName)
}

// getPodGroupName extracts the pod group name from annotations or labels.
func (d *PodGroupDiscoverer) getPodGroupName(pod *corev1.Pod) string {
	// Check annotations first
	if pod.Annotations != nil {
		for _, key := range d.config.AnnotationKeys {
			if name := pod.Annotations[key]; name != "" {
				return name
			}
		}
	}

	// Check labels as fallback
	if pod.Labels != nil {
		for _, key := range d.config.LabelKeys {
			if name := pod.Labels[key]; name != "" {
				return name
			}
		}
	}

	return ""
}

// DiscoverPeers finds all pods in the same PodGroup.
func (d *PodGroupDiscoverer) DiscoverPeers(ctx context.Context, pod *corev1.Pod) (*types.GangInfo, error) {
	podGroupName := d.getPodGroupName(pod)
	if podGroupName == "" {
		slog.Debug("Pod not handled by discoverer",
			"discoverer", d.config.Name,
			"pod", pod.Name,
			"namespace", pod.Namespace)

		return nil, nil
	}

	gangID := d.ExtractGangID(pod)

	slog.Info("Discovering gang",
		"discoverer", d.config.Name,
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"podGroup", podGroupName,
		"gangID", gangID)

	// Get expected size from PodGroup CRD - required for correct gang coordination
	expectedCount, err := d.getPodGroupMinMember(ctx, pod.Namespace, podGroupName)
	if err != nil {
		return nil, fmt.Errorf("failed to get PodGroup %s/%s minMember (check RBAC): %w",
			pod.Namespace, podGroupName, err)
	}

	var podList corev1.PodList
	if err := d.client.List(ctx, &podList, client.InNamespace(pod.Namespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", pod.Namespace, err)
	}

	var peers []types.PeerInfo

	for i := range podList.Items {
		p := &podList.Items[i]

		// Check if this pod belongs to the same gang
		if d.getPodGroupName(p) != podGroupName {
			continue
		}

		// Skip pods that are not running or pending
		if p.Status.Phase != corev1.PodRunning && p.Status.Phase != corev1.PodPending {
			continue
		}

		peers = append(peers, types.PeerInfo{
			PodName:   p.Name,
			PodIP:     p.Status.PodIP,
			NodeName:  p.Spec.NodeName,
			Namespace: p.Namespace,
		})
	}

	if len(peers) == 0 {
		slog.Warn("No peers found for gang",
			"discoverer", d.config.Name,
			"pod", pod.Name,
			"podGroup", podGroupName,
			"gangID", gangID)

		return nil, nil
	}

	slog.Info("Discovered gang",
		"discoverer", d.config.Name,
		"gangID", gangID,
		"podGroup", podGroupName,
		"expectedCount", expectedCount,
		"discoveredPeers", len(peers))

	return &types.GangInfo{
		GangID:           gangID,
		ExpectedMinCount: expectedCount,
		Peers:            peers,
	}, nil
}

// getPodGroupMinMember retrieves the minMember field from a PodGroup CRD using CEL.
func (d *PodGroupDiscoverer) getPodGroupMinMember(
	ctx context.Context,
	namespace, name string,
) (int, error) {
	podGroup := &unstructured.Unstructured{}
	podGroup.SetGroupVersionKind(d.config.PodGroupGVK)

	if err := d.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, podGroup); err != nil {
		return 0, fmt.Errorf("failed to get PodGroup %s/%s: %w", namespace, name, err)
	}

	result, _, err := d.minCountProgram.Eval(map[string]any{
		"podGroup": podGroup.Object,
	})
	if err != nil {
		return 0, fmt.Errorf("failed to evaluate minCountExpr %q: %w", d.config.MinCountExpr, err)
	}

	// Convert result to int
	switch v := result.Value().(type) {
	case int64:
		return int(v), nil
	case float64:
		return int(v), nil
	case int:
		return v, nil
	default:
		return 0, fmt.Errorf("minCountExpr %q returned non-numeric type %T", d.config.MinCountExpr, v)
	}
}
