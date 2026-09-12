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

package cacheconfig

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestBuild_PodCache_IsLimitedToSlinkyNamespace(t *testing.T) {
	options := Build("slinky")

	podCache := cacheForObject(t, options, &corev1.Pod{})

	// A single namespace entry is what keeps the informer off every other
	// namespace in the cluster.
	require.Len(t, podCache.Namespaces, 1)
	require.Contains(t, podCache.Namespaces, "slinky")
	assert.NotNil(t, podCache.Transform)
}

func TestBuild_NodeCache_IsClusterWideAndPruned(t *testing.T) {
	options := Build("slinky")

	nodeCache := cacheForObject(t, options, &corev1.Node{})

	// Nodes are cluster-scoped, so the cache must stay cluster-wide. Only the
	// retained field set shrinks.
	assert.Empty(t, nodeCache.Namespaces)
	assert.NotNil(t, nodeCache.Transform)
}

func TestTransformNodeForCache_RetainsDrainerFieldsOnly(t *testing.T) {
	node := &corev1.Node{
		Kind: "Node", APIVersion: "v1",
		Name:            "node-a",
		UID:             types.UID("node-uid"),
		ResourceVersion: "node-rv",
		Labels:          map[string]string{"dgxc.nvidia.com/nvsentinel-state": "draining"},
		Annotations:     map[string]string{"nodeset.slinky.slurm.net/node-cordon-reason": "[T] [NVSentinel] 79"},
		ManagedFields:   []metav1.ManagedFieldsEntry{{Manager: "drop-manager"}},
		Spec: corev1.NodeSpec{
			ProviderID:    "drop-provider",
			Unschedulable: true,
			Taints:        []corev1.Taint{{Key: "drop", Effect: corev1.TaintEffectNoSchedule}},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
			Images:     []corev1.ContainerImage{{Names: []string{"drop-image"}}},
		},
	}

	transformed, err := transformNodeForCache(node)
	require.NoError(t, err)
	assert.Same(t, node, transformed)
	assert.Equal(t, &corev1.Node{
		Name:            "node-a",
		UID:             types.UID("node-uid"),
		ResourceVersion: "node-rv",
		Labels:          map[string]string{"dgxc.nvidia.com/nvsentinel-state": "draining"},
		Annotations:     map[string]string{"nodeset.slinky.slurm.net/node-cordon-reason": "[T] [NVSentinel] 79"},
	}, transformed)
}

func TestTransformNodeForCache_WrongType_ReturnsError(t *testing.T) {
	_, err := transformNodeForCache(&corev1.Pod{})

	require.ErrorContains(t, err, "node cache transform expected *v1.Node")
}

func TestTransformPodForCache_RetainsDrainerFieldsOnly(t *testing.T) {
	pod := &corev1.Pod{
		Kind: "Pod", APIVersion: "v1",
		Name:            "slurmd-0",
		Namespace:       "slinky",
		UID:             types.UID("pod-uid"),
		ResourceVersion: "pod-rv",
		Labels:          map[string]string{"drop": "label"},
		Annotations:     map[string]string{"drop": "annotation"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "drop-container", Image: "drop-image"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "192.0.2.1",
			Conditions: []corev1.PodCondition{
				{
					Type:               corev1.PodReady,
					Status:             corev1.ConditionTrue,
					Reason:             "drop-reason",
					Message:            "drop-message",
					LastTransitionTime: metav1.NewTime(time.Unix(123, 0)),
				},
				{Type: "SlurmNodeStateDrain", Status: corev1.ConditionTrue},
			},
		},
	}

	transformed, err := transformPodForCache(pod)
	require.NoError(t, err)
	assert.Same(t, pod, transformed)
	assert.Equal(t, &corev1.Pod{
		Name:            "slurmd-0",
		Namespace:       "slinky",
		UID:             types.UID("pod-uid"),
		ResourceVersion: "pod-rv",
		Spec:            corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
				{Type: "SlurmNodeStateDrain", Status: corev1.ConditionTrue},
			},
		},
	}, transformed)
}

// The reconciler matches six condition types today and the Slinky operator can
// report more, so the transform must not filter conditions by type.
func TestTransformPodForCache_RetainsUnknownSlurmConditions(t *testing.T) {
	pod := &corev1.Pod{
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{
				{Type: "SlurmNodeStateSomeFutureState", Status: corev1.ConditionTrue, Message: "drop-message"},
			},
		},
	}

	transformed, err := transformPodForCache(pod)
	require.NoError(t, err)
	assert.Equal(t, []corev1.PodCondition{
		{Type: "SlurmNodeStateSomeFutureState", Status: corev1.ConditionTrue},
	}, transformed.(*corev1.Pod).Status.Conditions)
}

func TestTransformPodForCache_WrongType_ReturnsError(t *testing.T) {
	_, err := transformPodForCache(&corev1.Node{})

	require.ErrorContains(t, err, "pod cache transform expected *v1.Pod")
}

func cacheForObject(
	t *testing.T,
	options cache.Options,
	object client.Object,
) cache.ByObject {
	t.Helper()

	objectType := reflect.TypeOf(object)

	for configuredObject, byObject := range options.ByObject {
		if reflect.TypeOf(configuredObject) == objectType {
			return byObject
		}
	}

	t.Fatalf("cache options do not include %T", object)

	return cache.ByObject{}
}
