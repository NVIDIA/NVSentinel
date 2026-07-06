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

package discoverer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func stringPtr(s string) *string {
	return &s
}

func makePodWithSchedulingGroup(ns, podGroup string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
	}

	if podGroup != "" {
		pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{
			PodGroupName: stringPtr(podGroup),
		}
	}

	return pod
}

func TestKubernetesDiscoverer_CanHandle(t *testing.T) {
	d := NewKubernetesDiscoverer(nil)

	tests := []struct {
		name     string
		podGroup string
		want     bool
	}{
		{"has schedulingGroup", "my-podgroup", true},
		{"no schedulingGroup", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePodWithSchedulingGroup("ns", tt.podGroup)

			if got := d.CanHandle(pod); got != tt.want {
				t.Errorf("CanHandle() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestKubernetesDiscoverer_ExtractGangID(t *testing.T) {
	d := NewKubernetesDiscoverer(nil)

	tests := []struct {
		name     string
		ns       string
		podGroup string
		want     string
	}{
		{"has podGroup", "ml", "train-workers", "kubernetes-ml-train-workers"},
		{"no schedulingGroup", "ml", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePodWithSchedulingGroup(tt.ns, tt.podGroup)

			if got := d.ExtractGangID(pod); got != tt.want {
				t.Errorf("ExtractGangID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestKubernetesDiscoverer_Name(t *testing.T) {
	d := NewKubernetesDiscoverer(nil)

	if got := d.Name(); got != "kubernetes" {
		t.Errorf("Name() = %q, want %q", got, "kubernetes")
	}
}

// --- DiscoverPeers tests ---

func makeNativePodGroup(namespace, name string, minCount int64) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(PodGroupGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetUID(k8stypes.UID(name + "-uid"))
	if minCount > 0 {
		_ = unstructured.SetNestedField(obj.Object, minCount, "spec", "schedulingPolicy", "gang", "minCount")
	}
	return obj
}

func makeNativeWorkload(namespace, name string) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(WorkloadGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetUID(k8stypes.UID(name + "-uid"))
	return obj
}

func makeSchedulingGroupPod(name, namespace, podGroup, ip string, phase corev1.PodPhase) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{Name: "c", Image: "img"}},
		},
		Status: corev1.PodStatus{
			Phase: phase,
			PodIP: ip,
		},
	}
	if podGroup != "" {
		pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{
			PodGroupName: stringPtr(podGroup),
		}
	}
	return pod
}

func TestKubernetesDiscoverer_DiscoverPeers(t *testing.T) {
	t.Run("discovers peers by schedulingGroup", func(t *testing.T) {
		podGroup := makeNativePodGroup("default", "train-workers", 3)
		pods := []runtime.Object{
			makeSchedulingGroupPod("w-0", "default", "train-workers", "10.0.0.1", corev1.PodRunning),
			makeSchedulingGroupPod("w-1", "default", "train-workers", "10.0.0.2", corev1.PodRunning),
			makeSchedulingGroupPod("w-2", "default", "train-workers", "10.0.0.3", corev1.PodPending),
			makeSchedulingGroupPod("other", "default", "other-podgroup", "10.0.0.4", corev1.PodRunning),
		}

		c := fake.NewClientBuilder().WithRuntimeObjects(append(pods, podGroup)...).Build()
		d := NewKubernetesDiscoverer(c)

		info, err := d.DiscoverPeers(context.Background(), makeSchedulingGroupPod("w-0", "default", "train-workers", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 3)
		assert.Equal(t, 3, info.ExpectedMinCount)
		assert.Equal(t, "kubernetes-default-train-workers", info.GangID)
		require.NotNil(t, info.OwnerReference)
		assert.Equal(t, "scheduling.k8s.io/v1alpha2", info.OwnerReference.APIVersion)
		assert.Equal(t, "PodGroup", info.OwnerReference.Kind)
		assert.Equal(t, "train-workers", info.OwnerReference.Name)
		assert.Equal(t, k8stypes.UID("train-workers-uid"), info.OwnerReference.UID)
	})

	t.Run("no matching pods returns nil", func(t *testing.T) {
		podGroup := makeNativePodGroup("default", "train-workers", 0)
		c := fake.NewClientBuilder().WithRuntimeObjects(podGroup).Build()
		d := NewKubernetesDiscoverer(c)

		info, err := d.DiscoverPeers(context.Background(), makeSchedulingGroupPod("w-0", "default", "train-workers", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		assert.Nil(t, info)
	})

	t.Run("podGroup not found falls back to discovered count", func(t *testing.T) {
		pods := []runtime.Object{
			makeSchedulingGroupPod("w-0", "default", "missing", "10.0.0.1", corev1.PodRunning),
			makeSchedulingGroupPod("w-1", "default", "missing", "10.0.0.2", corev1.PodRunning),
		}
		c := fake.NewClientBuilder().WithRuntimeObjects(pods...).Build()
		d := NewKubernetesDiscoverer(c)

		info, err := d.DiscoverPeers(context.Background(), makeSchedulingGroupPod("w-0", "default", "missing", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2)
		assert.Equal(t, 2, info.ExpectedMinCount, "should fall back to discovered peer count")
	})

	t.Run("pod without schedulingGroup returns nil", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		d := NewKubernetesDiscoverer(c)

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
		info, err := d.DiscoverPeers(context.Background(), pod)
		require.NoError(t, err)
		assert.Nil(t, info)
	})
}

func TestWorkloadRefDiscoverer_OwnerReference(t *testing.T) {
	workload := makeNativeWorkload("default", "train-workload")
	c := fake.NewClientBuilder().WithObjects(workload).Build()
	d := NewWorkloadRefDiscoverer(c)

	ownerRef, err := d.workloadOwnerReference(context.Background(), "default", "train-workload")
	require.NoError(t, err)
	require.NotNil(t, ownerRef)
	assert.Equal(t, "scheduling.k8s.io/v1alpha1", ownerRef.APIVersion)
	assert.Equal(t, "Workload", ownerRef.Kind)
	assert.Equal(t, "train-workload", ownerRef.Name)
	assert.Equal(t, k8stypes.UID("train-workload-uid"), ownerRef.UID)
}
