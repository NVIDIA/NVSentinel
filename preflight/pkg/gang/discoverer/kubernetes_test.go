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
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func makePodWithWorkloadRef(ns, workload, podGroup string) *corev1.Pod {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns},
	}

	if workload != "" {
		pod.Spec.WorkloadRef = &corev1.WorkloadReference{
			Name:     workload,
			PodGroup: podGroup,
		}
	}

	return pod
}

func TestWorkloadRefDiscoverer_CanHandle(t *testing.T) {
	d := NewWorkloadRefDiscoverer(nil)

	tests := []struct {
		name     string
		workload string
		want     bool
	}{
		{"has workloadRef", "my-workload", true},
		{"no workloadRef", "", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePodWithWorkloadRef("ns", tt.workload, "")

			if got := d.CanHandle(pod); got != tt.want {
				t.Errorf("CanHandle() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestWorkloadRefDiscoverer_ExtractGangID(t *testing.T) {
	d := NewWorkloadRefDiscoverer(nil)

	tests := []struct {
		name     string
		ns       string
		workload string
		podGroup string
		want     string
	}{
		{"workload only", "ml", "train", "", "kubernetes-ml-train"},
		{"workload with podGroup", "ml", "train", "workers", "kubernetes-ml-train-workers"},
		{"no workloadRef", "ml", "", "", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			pod := makePodWithWorkloadRef(tt.ns, tt.workload, tt.podGroup)

			if got := d.ExtractGangID(pod); got != tt.want {
				t.Errorf("ExtractGangID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkloadRefDiscoverer_Name(t *testing.T) {
	d := NewWorkloadRefDiscoverer(nil)

	if got := d.Name(); got != "kubernetes" {
		t.Errorf("Name() = %q, want %q", got, "kubernetes")
	}
}

// --- DiscoverPeers tests ---

func makeWorkloadCRD(namespace, name string, podGroups []map[string]any) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(WorkloadGVK)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	if podGroups != nil {
		pgSlice := make([]any, len(podGroups))
		for i, pg := range podGroups {
			pgSlice[i] = pg
		}
		_ = unstructured.SetNestedSlice(obj.Object, pgSlice, "spec", "podGroups")
	}
	return obj
}

func makeWorkloadPod(name, namespace, workload, podGroup, ip string, phase corev1.PodPhase) *corev1.Pod {
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
	if workload != "" {
		pod.Spec.WorkloadRef = &corev1.WorkloadReference{
			Name:     workload,
			PodGroup: podGroup,
		}
	}
	return pod
}

func TestWorkloadRefDiscoverer_DiscoverPeers(t *testing.T) {
	t.Run("discovers peers by workloadRef", func(t *testing.T) {
		workload := makeWorkloadCRD("default", "train", []map[string]any{
			{"name": "workers", "policy": map[string]any{"gang": map[string]any{"minCount": int64(3)}}},
		})
		pods := []runtime.Object{
			makeWorkloadPod("w-0", "default", "train", "workers", "10.0.0.1", corev1.PodRunning),
			makeWorkloadPod("w-1", "default", "train", "workers", "10.0.0.2", corev1.PodRunning),
			makeWorkloadPod("w-2", "default", "train", "workers", "10.0.0.3", corev1.PodPending),
			makeWorkloadPod("other", "default", "other-workload", "", "10.0.0.4", corev1.PodRunning),
		}

		c := fake.NewClientBuilder().WithRuntimeObjects(append(pods, workload)...).Build()
		d := NewWorkloadRefDiscoverer(c)

		info, err := d.DiscoverPeers(context.Background(), makeWorkloadPod("w-0", "default", "train", "workers", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 3)
		assert.Equal(t, 3, info.ExpectedMinCount)
		assert.Equal(t, "kubernetes-default-train-workers", info.GangID)
	})

	t.Run("no matching pods returns nil", func(t *testing.T) {
		workload := makeWorkloadCRD("default", "train", nil)
		c := fake.NewClientBuilder().WithRuntimeObjects(workload).Build()
		d := NewWorkloadRefDiscoverer(c)

		info, err := d.DiscoverPeers(context.Background(), makeWorkloadPod("w-0", "default", "train", "workers", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		assert.Nil(t, info)
	})

	t.Run("workload not found falls back to discovered count", func(t *testing.T) {
		pods := []runtime.Object{
			makeWorkloadPod("w-0", "default", "missing", "workers", "10.0.0.1", corev1.PodRunning),
			makeWorkloadPod("w-1", "default", "missing", "workers", "10.0.0.2", corev1.PodRunning),
		}
		c := fake.NewClientBuilder().WithRuntimeObjects(pods...).Build()
		d := NewWorkloadRefDiscoverer(c)

		info, err := d.DiscoverPeers(context.Background(), makeWorkloadPod("w-0", "default", "missing", "workers", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2)
		assert.Equal(t, 2, info.ExpectedMinCount, "should fall back to discovered peer count")
	})

	t.Run("pod without workloadRef returns nil", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		d := NewWorkloadRefDiscoverer(c)

		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Namespace: "default"}}
		info, err := d.DiscoverPeers(context.Background(), pod)
		require.NoError(t, err)
		assert.Nil(t, info)
	})
}
