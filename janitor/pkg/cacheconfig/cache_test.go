// Copyright (c) 2025, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
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

	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/gpuservices"
)

func TestBuild_GPUResetEnabled_ScopesPodCache(t *testing.T) {
	options, err := Build(&config.Config{
		GPUReset: config.GPUResetControllerConfig{
			Enabled: true,
			ServiceManager: gpuservices.Manager{
				Name: "gpu-operator",
			},
		},
	})
	require.NoError(t, err)

	nodeCache := cacheForObject(t, options, &corev1.Node{})
	assert.NotNil(t, nodeCache.Transform)

	podCache := cacheForObject(t, options, &corev1.Pod{})
	require.Contains(t, podCache.Namespaces, "gpu-operator")
	assert.Equal(
		t,
		"app.kubernetes.io/managed-by=gpu-operator",
		podCache.Namespaces["gpu-operator"].LabelSelector.String(),
	)
	assert.NotNil(t, podCache.Transform)
}

func TestBuild_GPUResetDisabled_DoesNotConfigurePodCache(t *testing.T) {
	options, err := Build(&config.Config{})
	require.NoError(t, err)

	_ = cacheForObject(t, options, &corev1.Node{})
	assert.Len(t, options.ByObject, 1)
}

func TestBuild_UnknownServiceManager_ReturnsError(t *testing.T) {
	_, err := Build(&config.Config{
		GPUReset: config.GPUResetControllerConfig{
			Enabled: true,
			ServiceManager: gpuservices.Manager{
				Name: "unknown",
			},
		},
	})
	require.ErrorContains(t, err, "resolve GPU service manager for Pod cache")
}

func TestTransformNodeForCache_RetainsJanitorFieldsOnly(t *testing.T) {
	transitionTime := metav1.NewTime(time.Unix(123, 0))
	node := &corev1.Node{
		TypeMeta: metav1.TypeMeta{Kind: "Node", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "node-a",
			UID:             types.UID("node-uid"),
			ResourceVersion: "node-rv",
			Labels:          map[string]string{"retain": "label"},
			Annotations:     map[string]string{"drop": "annotation"},
		},
		Spec: corev1.NodeSpec{
			ProviderID: "drop-provider",
			Taints: []corev1.Taint{{
				Key:    "retain",
				Value:  "taint",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{
				{
					Type:               corev1.NodeReady,
					Status:             corev1.ConditionTrue,
					LastTransitionTime: transitionTime,
					Reason:             "drop-reason",
				},
				{
					Type:   corev1.NodeMemoryPressure,
					Status: corev1.ConditionFalse,
				},
			},
			Images: []corev1.ContainerImage{{Names: []string{"drop-image"}}},
		},
	}

	transformed, err := transformNodeForCache(node)
	require.NoError(t, err)
	assert.Same(t, node, transformed)
	assert.Equal(t, &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "node-a",
			UID:             types.UID("node-uid"),
			ResourceVersion: "node-rv",
			Labels:          map[string]string{"retain": "label"},
		},
		Spec: corev1.NodeSpec{
			Taints: []corev1.Taint{{
				Key:    "retain",
				Value:  "taint",
				Effect: corev1.TaintEffectNoSchedule,
			}},
		},
		Status: corev1.NodeStatus{
			Conditions: []corev1.NodeCondition{{
				Type:   corev1.NodeReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}, transformed)
}

func TestTransformPodForCache_RetainsJanitorFieldsOnly(t *testing.T) {
	pod := &corev1.Pod{
		TypeMeta: metav1.TypeMeta{Kind: "Pod", APIVersion: "v1"},
		ObjectMeta: metav1.ObjectMeta{
			Name:            "device-plugin",
			Namespace:       "gpu-operator",
			UID:             types.UID("pod-uid"),
			ResourceVersion: "pod-rv",
			Labels:          map[string]string{"app": "device-plugin"},
			Annotations:     map[string]string{"drop": "annotation"},
		},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Containers: []corev1.Container{{
				Name:  "drop-container",
				Image: "drop-image",
			}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "192.0.2.1",
			Conditions: []corev1.PodCondition{
				{
					Type:    corev1.PodReady,
					Status:  corev1.ConditionTrue,
					Reason:  "drop-reason",
					Message: "drop-message",
				},
				{
					Type:   corev1.PodInitialized,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	transformed, err := transformPodForCache(pod)
	require.NoError(t, err)
	assert.Same(t, pod, transformed)
	assert.Equal(t, &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "device-plugin",
			Namespace:       "gpu-operator",
			UID:             types.UID("pod-uid"),
			ResourceVersion: "pod-rv",
			Labels:          map[string]string{"app": "device-plugin"},
		},
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionTrue,
			}},
		},
	}, transformed)
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
