// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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

package informers

import (
	"context"
	"regexp"
	"testing"
	"time"

	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes/fake"
	"k8s.io/client-go/tools/cache"
	"k8s.io/utils/ptr"

	"github.com/nvidia/nvsentinel/data-models/pkg/model"
	"github.com/nvidia/nvsentinel/fault-quarantine/pkg/common"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestExcludedPodTransformUsesRuntimeNamespaceRegexSemantics(t *testing.T) {
	t.Parallel()

	excludeRegex := regexp.MustCompile(`^(kube-.*|nvsentinel)$`)
	transform := excludedPodTransform(excludeRegex)
	informers := &Informers{}

	tests := []struct {
		namespace string
		excluded  bool
	}{
		{namespace: "kube-system", excluded: true},
		{namespace: "kube-public", excluded: true},
		{namespace: "nvsentinel", excluded: true},
		{namespace: "user-kube-system", excluded: false},
		{namespace: "workload", excluded: false},
	}

	for _, tt := range tests {
		t.Run(tt.namespace, func(t *testing.T) {
			pod := richDrainEligiblePod(tt.namespace, "pod", "node-a")

			transformed, err := transform(pod)
			require.NoError(t, err)

			included, err := informers.shouldIncludeNamespace(tt.namespace, "*", excludeRegex)
			require.NoError(t, err)
			assert.Equal(t, tt.excluded, !included)

			transformedPod := transformed.(*v1.Pod)
			if tt.excluded {
				assert.Empty(t, transformedPod.Spec.NodeName)
				assert.Empty(t, transformedPod.Spec.Containers)
			} else {
				assert.NotSame(t, pod, transformedPod)
				assert.Equal(t, pod.Spec.NodeName, transformedPod.Spec.NodeName)
				assert.Equal(t, pod.Status.Phase, transformedPod.Status.Phase)
			}
		})
	}
}

func TestExcludedPodTransformDetectsDaemonSetOwners(t *testing.T) {
	t.Parallel()

	transform := excludedPodTransform(regexp.MustCompile(`^kube-system$`))
	pod := richDrainEligiblePod("workload", "daemon", "node-a")
	pod.OwnerReferences = []metav1.OwnerReference{
		{Kind: "ReplicaSet", Name: "replica"},
		{Kind: "DaemonSet", Name: "daemon"},
	}

	transformed, err := transform(pod)
	require.NoError(t, err)

	transformedPod := transformed.(*v1.Pod)
	assert.Equal(t, pod.Name, transformedPod.Name)
	assert.Equal(t, pod.Namespace, transformedPod.Namespace)
	assert.Equal(t, pod.UID, transformedPod.UID)
	assert.Equal(t, pod.ResourceVersion, transformedPod.ResourceVersion)
	assert.Empty(t, transformedPod.Spec.NodeName)
	assert.Empty(t, transformedPod.OwnerReferences)
	assert.True(t, isDaemonSetOwned(pod.OwnerReferences))
}

func TestExcludedPodTransformRetainsDrainFieldsOnly(t *testing.T) {
	t.Parallel()

	pod := richDrainEligiblePod("workload", "eligible", "node-a")
	transform := excludedPodTransform(regexp.MustCompile(`^kube-system$`))

	transformed, err := transform(pod)
	require.NoError(t, err)

	cachedPod := transformed.(*v1.Pod)
	expected := &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "eligible",
			Namespace:         "workload",
			UID:               types.UID("workload-eligible"),
			ResourceVersion:   "11",
			DeletionTimestamp: pod.DeletionTimestamp.DeepCopy(),
			Annotations: map[string]string{
				model.PodDeviceAnnotationName: `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
			},
			OwnerReferences: []metav1.OwnerReference{{Kind: "ReplicaSet"}},
		},
		Spec: v1.PodSpec{
			NodeName:                      "node-a",
			TerminationGracePeriodSeconds: ptr.To(int64(60)),
			Containers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
				},
			}},
			InitContainers: []v1.Container{{
				Resources: v1.ResourceRequirements{
					Limits: v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
				},
			}},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{{
				Type:               v1.PodReady,
				Status:             v1.ConditionTrue,
				LastTransitionTime: pod.Status.Conditions[0].LastTransitionTime,
			}},
		},
	}

	assert.NotSame(t, pod, cachedPod)
	assert.Equal(t, expected, cachedPod)
}

func TestInformerTransformsPassThroughUnknownObjects(t *testing.T) {
	t.Parallel()

	tombstone := cache.DeletedFinalStateUnknown{Key: "workload/pod"}
	podTransform := excludedPodTransform(regexp.MustCompile(`^kube-system$`))

	transformedPodObject, err := podTransform(tombstone)
	require.NoError(t, err)
	assert.Equal(t, tombstone, transformedPodObject)

	transformedNodeObject, err := nodeTransform(tombstone)
	require.NoError(t, err)
	assert.Equal(t, tombstone, transformedNodeObject)
}

func TestNodeTransformRetainsEventAndEvaluatorFields(t *testing.T) {
	t.Parallel()

	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "node-a",
			UID:             types.UID("node-uid"),
			ResourceVersion: "42",
			Labels:          map[string]string{"large": "metadata"},
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
				"unrelated": "discard",
			},
		},
		Spec:   v1.NodeSpec{Unschedulable: true},
		Status: v1.NodeStatus{Phase: v1.NodeRunning},
	}

	transformed, err := nodeTransform(node)
	require.NoError(t, err)

	transformedNode := transformed.(*v1.Node)
	assert.Equal(t, node.Name, transformedNode.Name)
	assert.Equal(t, node.UID, transformedNode.UID)
	assert.Equal(t, node.ResourceVersion, transformedNode.ResourceVersion)
	assert.Equal(t, map[string]string{
		common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
	}, transformedNode.Annotations)
	assert.Empty(t, transformedNode.Labels)
	assert.Empty(t, transformedNode.Spec)
	assert.Empty(t, transformedNode.Status)
}

func TestInformerTransformsIntegrateWithIndexesAndNodeEvents(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	systemPod := richDrainEligiblePod("kube-system", "system", "node-a")
	daemonPod := richDrainEligiblePod("workload", "daemon", "node-a")
	daemonPod.OwnerReferences = []metav1.OwnerReference{{Kind: "DaemonSet", Name: "daemon"}}
	eligiblePod := richDrainEligiblePod("workload", "eligible", "node-a")
	node := &v1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "node-a",
			UID:             types.UID("node-uid"),
			ResourceVersion: "7",
			Annotations: map[string]string{
				common.QuarantineHealthEventAnnotationKey: `{"events":[]}`,
			},
		},
	}

	client := fake.NewSimpleClientset(systemPod, daemonPod, eligiblePod, node)
	informers, err := NewInformers(client, 0, ptr.To(5), false, false, `^kube-system$`)
	require.NoError(t, err)
	require.NoError(t, informers.Run(ctx))

	nodeIndexedPods, err := informers.podInformer.GetIndexer().ByIndex(NodeIndex, node.Name)
	require.NoError(t, err)
	require.Len(t, nodeIndexedPods, 1)
	assert.Equal(t, eligiblePod.Name, nodeIndexedPods[0].(*v1.Pod).Name)
	assert.Equal(t, eligiblePod.Spec.NodeName, nodeIndexedPods[0].(*v1.Pod).Spec.NodeName)
	assert.Empty(t, nodeIndexedPods[0].(*v1.Pod).Spec.Containers[0].Image)

	systemCached, exists, err := informers.podInformer.GetIndexer().GetByKey("kube-system/system")
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, systemCached.(*v1.Pod).Spec.NodeName)

	daemonCached, exists, err := informers.podInformer.GetIndexer().GetByKey("workload/daemon")
	require.NoError(t, err)
	require.True(t, exists)
	assert.Empty(t, daemonCached.(*v1.Pod).Spec.NodeName)

	cachedNode, err := informers.GetNode(node.Name)
	require.NoError(t, err)
	assert.Equal(t, node.UID, cachedNode.UID)
	assert.Equal(t, node.ResourceVersion, cachedNode.ResourceVersion)
	assert.Equal(t, node.Annotations, cachedNode.Annotations)

	require.NoError(t, informers.UpdateNodeEvent(ctx, node.Name, "AwaitingPodCompletion", "waiting"))
	events, err := client.CoreV1().Events(metav1.NamespaceDefault).List(ctx, metav1.ListOptions{})
	require.NoError(t, err)
	require.Len(t, events.Items, 1)
	assert.Equal(t, node.UID, events.Items[0].InvolvedObject.UID)
}

func richDrainEligiblePod(namespace, name, nodeName string) *v1.Pod {
	deletionTimestamp := metav1.NewTime(time.Now().Add(-time.Minute))

	return &v1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Namespace:       namespace,
			UID:             types.UID(namespace + "-" + name),
			ResourceVersion: "11",
			Labels:          map[string]string{"app": name},
			Annotations: map[string]string{
				model.PodDeviceAnnotationName: `{"devices":{"nvidia.com/gpu":["GPU-1"]}}`,
				"unrelated":                   "discard",
			},
			OwnerReferences:   []metav1.OwnerReference{{Kind: "ReplicaSet", Name: "owner"}},
			Finalizers:        []string{"example.com/finalizer"},
			DeletionTimestamp: &deletionTimestamp,
		},
		Spec: v1.PodSpec{
			NodeName:                      nodeName,
			TerminationGracePeriodSeconds: ptr.To(int64(60)),
			InitContainers: []v1.Container{
				{
					Name:  "init",
					Image: "init:latest",
					Resources: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
			Containers: []v1.Container{
				{
					Name:  "workload",
					Image: "workload:latest",
					Resources: v1.ResourceRequirements{
						Limits:   v1.ResourceList{v1.ResourceName("nvidia.com/gpu"): resource.MustParse("1")},
						Requests: v1.ResourceList{v1.ResourceCPU: resource.MustParse("100m")},
					},
				},
			},
		},
		Status: v1.PodStatus{
			Phase: v1.PodRunning,
			Conditions: []v1.PodCondition{
				{
					Type:               v1.PodReady,
					Status:             v1.ConditionTrue,
					LastTransitionTime: metav1.NewTime(time.Now().Add(-time.Minute)),
				},
				{Type: v1.PodScheduled, Status: v1.ConditionTrue},
			},
			ContainerStatuses: []v1.ContainerStatus{{Name: "workload", Ready: true}},
		},
	}
}
