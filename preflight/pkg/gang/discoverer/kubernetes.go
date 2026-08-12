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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodGroupGVK is the GroupVersionKind for K8s native PodGroup resources.
var PodGroupGVK = schema.GroupVersionKind{
	Group:   "scheduling.k8s.io",
	Version: "v1alpha2",
	Kind:    "PodGroup",
}

// WorkloadGVK is the GroupVersionKind for K8s 1.35 Workload resources.
var WorkloadGVK = schema.GroupVersionKind{
	Group:   "scheduling.k8s.io",
	Version: "v1alpha1",
	Kind:    "Workload",
}

var podGVK = schema.GroupVersionKind{
	Group:   "",
	Version: "v1",
	Kind:    "Pod",
}

var podListGVK = schema.GroupVersionKind{
	Group:   "",
	Version: "v1",
	Kind:    "PodList",
}

// WorkloadRefDiscoverer discovers gang members using K8s 1.35 native workloadRef.
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
	workloadName, _ := w.getPodWorkloadRef(context.Background(), pod.Namespace, pod.Name)
	return workloadName != ""
}

// ExtractGangID extracts the gang identifier from a pod's workloadRef.
func (w *WorkloadRefDiscoverer) ExtractGangID(pod *corev1.Pod) string {
	workloadName, podGroup := w.getPodWorkloadRef(context.Background(), pod.Namespace, pod.Name)

	if workloadName == "" {
		return ""
	}

	if podGroup != "" {
		return fmt.Sprintf("kubernetes-%s-%s-%s", pod.Namespace, workloadName, podGroup)
	}

	return fmt.Sprintf("kubernetes-%s-%s", pod.Namespace, workloadName)
}

// OwnerReference returns the native Workload owner reference for the pod's gang.
func (w *WorkloadRefDiscoverer) OwnerReference(ctx context.Context, pod *corev1.Pod) (*metav1.OwnerReference, error) {
	workloadName, _ := w.getPodWorkloadRef(ctx, pod.Namespace, pod.Name)
	if workloadName == "" {
		return nil, nil
	}

	return w.workloadOwnerReference(ctx, pod.Namespace, workloadName)
}

func (w *WorkloadRefDiscoverer) workloadOwnerReference(
	ctx context.Context,
	namespace, workloadName string,
) (*metav1.OwnerReference, error) {
	workload, err := w.getWorkload(ctx, namespace, workloadName)
	if err != nil {
		return nil, err
	}

	return ownerReferenceForUnstructured(workload, WorkloadGVK), nil
}

// DiscoverPeers finds all pods with the same workloadRef.
func (w *WorkloadRefDiscoverer) DiscoverPeers(
	ctx context.Context,
	pod *corev1.Pod,
) (*types.GangInfo, error) {
	workloadName, podGroup := w.getPodWorkloadRef(ctx, pod.Namespace, pod.Name)
	if workloadName == "" {
		return nil, nil
	}

	gangID := formatWorkloadRefGangID(pod.Namespace, workloadName, podGroup)

	slog.Info("Discovering workloadRef gang",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"workload", workloadName,
		"podGroup", podGroup,
		"gangID", gangID)

	workload, err := w.getWorkload(ctx, pod.Namespace, workloadName)
	if err != nil {
		slog.Warn("Failed to get Workload, will use discovered pod count and skip owner reference",
			"workload", workloadName,
			"namespace", pod.Namespace,
			"error", err)
	}

	ownerReference := ownerReferenceForUnstructured(workload, WorkloadGVK)
	expectedMinCount := w.fetchExpectedMinCount(pod.Namespace, workloadName, podGroup, workload)

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
		OwnerReference:   ownerReference,
	}, nil
}

// fetchExpectedMinCount retrieves expected count, logging any errors.
func (w *WorkloadRefDiscoverer) fetchExpectedMinCount(
	namespace, workloadName, podGroup string,
	workload *unstructured.Unstructured,
) int {
	if workload == nil {
		return 0
	}

	count, err := w.getWorkloadMinCount(namespace, workloadName, podGroup, workload)
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
	podList := &unstructured.UnstructuredList{}
	podList.SetGroupVersionKind(podListGVK)

	if err := w.client.List(ctx, podList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	var peers []types.PeerInfo

	for i := range podList.Items {
		p := &podList.Items[i]

		if !w.isPeerMatch(p, workloadName, podGroup) {
			continue
		}

		podIP, _, _ := unstructured.NestedString(p.Object, "status", "podIP")
		nodeName, _, _ := unstructured.NestedString(p.Object, "spec", "nodeName")

		peers = append(peers, types.PeerInfo{
			PodName:   p.GetName(),
			PodIP:     podIP,
			NodeName:  nodeName,
			Namespace: p.GetNamespace(),
		})
	}

	return peers, nil
}

// isPeerMatch checks if a pod matches the workloadRef criteria.
func (w *WorkloadRefDiscoverer) isPeerMatch(p *unstructured.Unstructured, workloadName, podGroup string) bool {
	pWorkloadName, pPodGroup := getUnstructuredWorkloadRef(p)
	if pWorkloadName != workloadName {
		return false
	}

	if podGroup != "" && pPodGroup != podGroup {
		return false
	}

	phase, _, _ := unstructured.NestedString(p.Object, "status", "phase")

	return phase == string(corev1.PodRunning) || phase == string(corev1.PodPending)
}

// getWorkloadMinCount retrieves the minCount from a Workload's podGroup gang policy.
func (w *WorkloadRefDiscoverer) getWorkloadMinCount(
	namespace, name, podGroup string,
	workload *unstructured.Unstructured,
) (int, error) {
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

		// If podGroup specified, match it; otherwise take first one.
		pgName, _, _ := unstructured.NestedString(pg, "name")
		if podGroup != "" && pgName != podGroup {
			continue
		}

		minCount, found, _ := nestedInt(pg, "policy", "gang", "minCount")
		if found {
			return minCount, nil
		}
	}

	return 0, nil
}

func (w *WorkloadRefDiscoverer) getWorkload(
	ctx context.Context,
	namespace, name string,
) (*unstructured.Unstructured, error) {
	workload := &unstructured.Unstructured{}
	workload.SetGroupVersionKind(WorkloadGVK)

	if err := w.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, workload); err != nil {
		return nil, fmt.Errorf("failed to get Workload %s/%s: %w", namespace, name, err)
	}

	return workload, nil
}

func (w *WorkloadRefDiscoverer) getPodWorkloadRef(ctx context.Context, namespace, name string) (string, string) {
	if namespace == "" || name == "" {
		return "", ""
	}

	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(podGVK)

	if err := w.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, pod); err != nil {
		slog.Debug("Failed to get Pod while checking workloadRef",
			"namespace", namespace,
			"name", name,
			"error", err)

		return "", ""
	}

	return getUnstructuredWorkloadRef(pod)
}

func formatWorkloadRefGangID(namespace, workloadName, podGroup string) string {
	if podGroup != "" {
		return fmt.Sprintf("kubernetes-%s-%s-%s", namespace, workloadName, podGroup)
	}

	return fmt.Sprintf("kubernetes-%s-%s", namespace, workloadName)
}

func getUnstructuredWorkloadRef(pod *unstructured.Unstructured) (string, string) {
	workloadName, _, _ := unstructured.NestedString(pod.Object, "spec", "workloadRef", "name")
	podGroup, _, _ := unstructured.NestedString(pod.Object, "spec", "workloadRef", "podGroup")

	return workloadName, podGroup
}

// KubernetesDiscoverer discovers gang members using K8s native schedulingGroup.
// Pods are linked to PodGroups via spec.schedulingGroup:
//
//	spec:
//	  schedulingGroup:
//	    podGroupName: training-workers
type KubernetesDiscoverer struct {
	client client.Client
}

// NewKubernetesDiscoverer creates a new native Kubernetes gang discoverer.
func NewKubernetesDiscoverer(c client.Client) *KubernetesDiscoverer {
	return &KubernetesDiscoverer{
		client: c,
	}
}

func (w *KubernetesDiscoverer) Name() string {
	return "kubernetes"
}

// CanHandle returns true if the pod has a schedulingGroup.
func (w *KubernetesDiscoverer) CanHandle(pod *corev1.Pod) bool {
	return getSchedulingPodGroupName(pod) != ""
}

// ExtractGangID extracts the gang identifier from a pod's schedulingGroup.
func (w *KubernetesDiscoverer) ExtractGangID(pod *corev1.Pod) string {
	podGroup := getSchedulingPodGroupName(pod)

	if podGroup == "" {
		return ""
	}

	return fmt.Sprintf("kubernetes-%s-%s", pod.Namespace, podGroup)
}

// OwnerReference returns the native PodGroup owner reference for the pod's gang.
func (w *KubernetesDiscoverer) OwnerReference(ctx context.Context, pod *corev1.Pod) (*metav1.OwnerReference, error) {
	podGroupName := getSchedulingPodGroupName(pod)
	if podGroupName == "" {
		return nil, nil
	}

	podGroup, err := w.getPodGroup(ctx, pod.Namespace, podGroupName)
	if err != nil {
		return nil, err
	}

	return ownerReferenceForUnstructured(podGroup, PodGroupGVK), nil
}

// DiscoverPeers finds all pods with the same schedulingGroup.
func (w *KubernetesDiscoverer) DiscoverPeers(
	ctx context.Context,
	pod *corev1.Pod,
) (*types.GangInfo, error) {
	if !w.CanHandle(pod) {
		return nil, nil
	}

	podGroup := getSchedulingPodGroupName(pod)
	gangID := w.ExtractGangID(pod)

	slog.Info("Discovering schedulingGroup gang",
		"pod", pod.Name,
		"namespace", pod.Namespace,
		"podGroup", podGroup,
		"gangID", gangID)

	ownerReference, err := w.OwnerReference(ctx, pod)
	if err != nil {
		slog.Warn("Failed to get PodGroup owner reference, ConfigMap GC will rely on later backfill",
			"podGroup", podGroup,
			"namespace", pod.Namespace,
			"error", err)
	}

	expectedMinCount := w.fetchExpectedMinCount(ctx, pod.Namespace, podGroup)

	peers, err := w.findPeers(ctx, pod.Namespace, podGroup)
	if err != nil {
		return nil, err
	}

	if len(peers) == 0 {
		return nil, nil
	}

	if expectedMinCount == 0 {
		expectedMinCount = len(peers)
	}

	slog.Info("Discovered schedulingGroup gang",
		"gangID", gangID,
		"podGroup", podGroup,
		"expectedMinCount", expectedMinCount,
		"discoveredPeers", len(peers))

	return &types.GangInfo{
		GangID:           gangID,
		ExpectedMinCount: expectedMinCount,
		Peers:            peers,
		OwnerReference:   ownerReference,
	}, nil
}

// fetchExpectedMinCount retrieves expected count, logging any errors.
func (w *KubernetesDiscoverer) fetchExpectedMinCount(
	ctx context.Context,
	namespace, podGroup string,
) int {
	count, err := w.getPodGroupMinCount(ctx, namespace, podGroup)
	if err != nil {
		slog.Warn("Failed to get PodGroup minCount, will use discovered pod count",
			"error", err)
	}

	return count
}

// findPeers lists pods matching the schedulingGroup.
func (w *KubernetesDiscoverer) findPeers(
	ctx context.Context,
	namespace, podGroup string,
) ([]types.PeerInfo, error) {
	var podList corev1.PodList
	if err := w.client.List(ctx, &podList, client.InNamespace(namespace)); err != nil {
		return nil, fmt.Errorf("failed to list pods in namespace %s: %w", namespace, err)
	}

	var peers []types.PeerInfo

	for i := range podList.Items {
		p := &podList.Items[i]

		if !w.isPeerMatch(p, podGroup) {
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

// isPeerMatch checks if a pod matches the schedulingGroup criteria.
func (w *KubernetesDiscoverer) isPeerMatch(p *corev1.Pod, podGroup string) bool {
	if getSchedulingPodGroupName(p) != podGroup {
		return false
	}

	return p.Status.Phase == corev1.PodRunning || p.Status.Phase == corev1.PodPending
}

// getPodGroupMinCount retrieves the minCount from a PodGroup's gang policy.
func (w *KubernetesDiscoverer) getPodGroupMinCount(
	ctx context.Context,
	namespace, name string,
) (int, error) {
	podGroup, err := w.getPodGroup(ctx, namespace, name)
	if err != nil {
		return 0, err
	}

	minCount, found, err := nestedInt(podGroup.Object, "spec", "schedulingPolicy", "gang", "minCount")
	if err != nil {
		return 0, fmt.Errorf("failed to get gang minCount from PodGroup %s/%s: %w", namespace, name, err)
	}

	if found {
		return minCount, nil
	}

	return 0, nil
}

func (w *KubernetesDiscoverer) getPodGroup(
	ctx context.Context,
	namespace, name string,
) (*unstructured.Unstructured, error) {
	podGroup := &unstructured.Unstructured{}
	podGroup.SetGroupVersionKind(PodGroupGVK)

	if err := w.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: name}, podGroup); err != nil {
		return nil, fmt.Errorf("failed to get PodGroup %s/%s: %w", namespace, name, err)
	}

	return podGroup, nil
}

func nestedInt(obj map[string]any, fields ...string) (int, bool, error) {
	value, found, err := unstructured.NestedFieldNoCopy(obj, fields...)
	if err != nil || !found {
		return 0, found, err
	}

	switch v := value.(type) {
	case int:
		return v, true, nil
	case int32:
		return int(v), true, nil
	case int64:
		return int(v), true, nil
	case float32:
		return int(v), true, nil
	case float64:
		return int(v), true, nil
	default:
		return 0, true, fmt.Errorf("field %q is non-numeric type %T", fields, value)
	}
}

// getSchedulingPodGroupName extracts schedulingGroup.podGroupName from a pod.
// Returns empty string if not present.
func getSchedulingPodGroupName(pod *corev1.Pod) string {
	if pod.Spec.SchedulingGroup != nil && pod.Spec.SchedulingGroup.PodGroupName != nil {
		return *pod.Spec.SchedulingGroup.PodGroupName
	}

	return ""
}
