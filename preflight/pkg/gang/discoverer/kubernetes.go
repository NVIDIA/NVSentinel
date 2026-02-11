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

	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkloadGVK is the GroupVersionKind for K8s 1.35+ Workload resources.
var WorkloadGVK = schema.GroupVersionKind{
	Group:   "scheduling.k8s.io",
	Version: "v1alpha1",
	Kind:    "Workload",
}

// WorkloadRefDiscoverer discovers gang members using K8s 1.35+ native workloadRef.
// Pods are linked to Workloads via spec.workloadRef:
//
//	spec:
//	  workloadRef:
//	    name: training-job-workload
//	    podGroup: workers
type WorkloadRefDiscoverer struct {
	client client.Client
}

// NewWorkloadRefDiscoverer creates a new workloadRef gang discoverer.
func NewWorkloadRefDiscoverer(c client.Client) *WorkloadRefDiscoverer {
	return &WorkloadRefDiscoverer{
		client: c,
	}
}

func (w *WorkloadRefDiscoverer) Name() string {
	return "kubernetes"
}

// CanHandle returns true if the pod has a workloadRef.
func (w *WorkloadRefDiscoverer) CanHandle(pod *corev1.Pod) bool {
	// Check if pod has workloadRef in spec
	// Note: As of K8s 1.35, workloadRef is a new field in PodSpec
	return getWorkloadRefName(pod) != ""
}

// ExtractGangID extracts the gang identifier from a pod's workloadRef.
func (w *WorkloadRefDiscoverer) ExtractGangID(pod *corev1.Pod) string {
	workloadName := getWorkloadRefName(pod)
	podGroup := getWorkloadRefPodGroup(pod)

	if workloadName == "" {
		return ""
	}

	if podGroup != "" {
		return fmt.Sprintf("kubernetes-%s-%s-%s", pod.Namespace, workloadName, podGroup)
	}

	return fmt.Sprintf("kubernetes-%s-%s", pod.Namespace, workloadName)
}

// DiscoverPeers finds all pods with the same workloadRef.
func (w *WorkloadRefDiscoverer) DiscoverPeers(
	ctx context.Context,
	pod *corev1.Pod,
) (*types.GangInfo, error) {
	if !w.CanHandle(pod) {
		return nil, nil
	}

	workloadName := getWorkloadRefName(pod)
	podGroup := getWorkloadRefPodGroup(pod)
	gangID := w.ExtractGangID(pod)

	slog.Info("Discovering workloadRef gang",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"workload", workloadName,
		"podGroup", podGroup,
		"gangID", gangID)

	expectedMinCount := w.fetchExpectedMinCount(ctx, pod.Namespace, workloadName, podGroup)

	peers, err := w.findPeers(ctx, pod.Namespace, workloadName, podGroup)
	if err != nil {
		return nil, err
	}

	if len(peers) == 0 {
		return nil, nil
	}

	if expectedMinCount == 0 {
		expectedMinCount = len(peers)
	}

	slog.Info("Discovered workloadRef gang",
		"gangID", gangID,
		"workload", workloadName,
		"podGroup", podGroup,
		"expectedMinCount", expectedMinCount,
		"discoveredPeers", len(peers))

	return &types.GangInfo{
		GangID:           gangID,
		ExpectedMinCount: expectedMinCount,
		Peers:            peers,
	}, nil
}

// fetchExpectedMinCount retrieves expected count, logging any errors.
func (w *WorkloadRefDiscoverer) fetchExpectedMinCount(
	ctx context.Context,
	namespace, workloadName, podGroup string,
) int {
	count, err := w.getWorkloadMinCount(ctx, namespace, workloadName, podGroup)
	if err != nil {
		slog.Warn("Failed to get Workload minCount, will use discovered pod count",
			"workload", workloadName,
			"error", err)
	}

	return count
}

// findPeers lists pods matching the workloadRef.
func (w *WorkloadRefDiscoverer) findPeers(
	ctx context.Context,
	namespace, workloadName, podGroup string,
) ([]types.PeerInfo, error) {
	var podList corev1.PodList
	if err := w.client.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	var peers []types.PeerInfo

	for i := range podList.Items {
		p := &podList.Items[i]

		if !w.isPeerMatch(p, workloadName, podGroup) {
			continue
		}

		peers = append(peers, types.PeerInfo{
			PodName:   p.Name,
			PodIP:     p.Status.PodIP,
			NodeName:  p.Spec.NodeName,
			Namespace: p.Namespace,
		})
	}

	return peers, nil
}

// isPeerMatch checks if a pod matches the workloadRef criteria.
func (w *WorkloadRefDiscoverer) isPeerMatch(p *corev1.Pod, workloadName, podGroup string) bool {
	pWorkloadName := getWorkloadRefName(p)
	if pWorkloadName != workloadName {
		return false
	}

	if podGroup != "" && getWorkloadRefPodGroup(p) != podGroup {
		return false
	}

	return p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending
}

// getWorkloadMinCount retrieves the minCount from a Workload's podGroup gang policy.
func (w *WorkloadRefDiscoverer) getWorkloadMinCount(
	ctx context.Context,
	namespace, name, podGroup string,
) (int, error) {
	workload := &unstructured.Unstructured{}
	workload.SetGroupVersionKind(WorkloadGVK)

	if err := w.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, workload); err != nil {
		return 0, fmt.Errorf("failed to get Workload %s/%s: %w", namespace, name, err)
	}

	podGroups, found, err := unstructured.NestedSlice(workload.Object, "spec", "podGroups")
	if err != nil {
		return 0, fmt.Errorf("failed to get podGroups from Workload %s/%s: %w", namespace, name, err)
	}

	if !found {
		return 0, nil
	}

	for _, pgRaw := range podGroups {
		pg, ok := pgRaw.(map[string]any)
		if !ok {
			continue
		}

		// If podGroup specified, match it; otherwise take first one
		pgName, _, _ := unstructured.NestedString(pg, "name")
		if podGroup != "" && pgName != podGroup {
			continue
		}

		minCount, found, _ := unstructured.NestedInt64(pg, "policy", "gang", "minCount")
		if found {
			return int(minCount), nil
		}
	}

	return 0, nil
}

// getWorkloadRefName extracts workloadRef.name from a pod.
// Returns empty string if not present.
func getWorkloadRefName(pod *corev1.Pod) string {
	if pod.Spec.WorkloadRef != nil {
		return pod.Spec.WorkloadRef.Name
	}

	return ""
}

// getWorkloadRefPodGroup extracts workloadRef.podGroup from a pod.
// Returns empty string if not present.
func getWorkloadRefPodGroup(pod *corev1.Pod) string {
	if pod.Spec.WorkloadRef != nil {
		return pod.Spec.WorkloadRef.PodGroup
	}

	return ""
}
