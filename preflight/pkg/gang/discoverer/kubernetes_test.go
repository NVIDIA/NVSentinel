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
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	podGroupV1alpha2 = PodGroupGroupKind.WithVersion("v1alpha2") // K8s 1.36
	podGroupV1beta1  = PodGroupGroupKind.WithVersion("v1beta1")  // K8s 1.37
)

//go:fix inline
func stringPtr(s string) *string {
	return new(s)
}

func makePodWithSchedulingGroup(ns, podGroup string) *corev1.Pod {
	pod := &corev1.Pod{
		Namespace: ns,
	}

	if podGroup != "" {
		pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{
			PodGroupName: new(podGroup),
		}
	}

	return pod
}

func TestKubernetesDiscoverer_CanHandle(t *testing.T) {
	d := NewKubernetesDiscoverer(nil, podGroupV1alpha2)

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
	d := NewKubernetesDiscoverer(nil, podGroupV1alpha2)

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
	d := NewKubernetesDiscoverer(nil, podGroupV1alpha2)

	if got := d.Name(); got != "kubernetes" {
		t.Errorf("Name() = %q, want %q", got, "kubernetes")
	}
}

// --- DiscoverPeers tests ---

func makeNativePodGroup(
	gvk schema.GroupVersionKind,
	namespace, name string,
	minCount int64,
) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
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
		Name:      name,
		Namespace: namespace,
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
			PodGroupName: new(podGroup),
		}
	}
	return pod
}

func TestKubernetesDiscoverer_DiscoverPeers(t *testing.T) {
	for _, gvk := range []schema.GroupVersionKind{podGroupV1alpha2, podGroupV1beta1} {
		t.Run("discovers peers by schedulingGroup with PodGroup "+gvk.Version, func(t *testing.T) {
			podGroup := makeNativePodGroup(gvk, "default", "train-workers", 3)
			pods := []runtime.Object{
				makeSchedulingGroupPod("w-0", "default", "train-workers", "10.0.0.1", corev1.PodRunning),
				makeSchedulingGroupPod("w-1", "default", "train-workers", "10.0.0.2", corev1.PodRunning),
				makeSchedulingGroupPod("w-2", "default", "train-workers", "10.0.0.3", corev1.PodPending),
				makeSchedulingGroupPod("other", "default", "other-podgroup", "10.0.0.4", corev1.PodRunning),
			}

			c := fake.NewClientBuilder().WithRuntimeObjects(append(pods, podGroup)...).Build()
			d := NewKubernetesDiscoverer(c, gvk)

			info, err := d.DiscoverPeers(context.Background(), makeSchedulingGroupPod("w-0", "default", "train-workers", "10.0.0.1", corev1.PodRunning))
			require.NoError(t, err)
			require.NotNil(t, info)
			assert.Len(t, info.Peers, 3)
			assert.Equal(t, 3, info.ExpectedMinCount)
			assert.Equal(t, "kubernetes-default-train-workers", info.GangID)
			require.NotNil(t, info.OwnerReference)
			assert.Equal(t, gvk.GroupVersion().String(), info.OwnerReference.APIVersion)
			assert.Equal(t, "PodGroup", info.OwnerReference.Kind)
			assert.Equal(t, "train-workers", info.OwnerReference.Name)
			assert.Equal(t, k8stypes.UID("train-workers-uid"), info.OwnerReference.UID)
		})
	}

	t.Run("no matching pods returns nil", func(t *testing.T) {
		podGroup := makeNativePodGroup(podGroupV1alpha2, "default", "train-workers", 0)
		c := fake.NewClientBuilder().WithRuntimeObjects(podGroup).Build()
		d := NewKubernetesDiscoverer(c, podGroupV1alpha2)

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
		d := NewKubernetesDiscoverer(c, podGroupV1alpha2)

		info, err := d.DiscoverPeers(context.Background(), makeSchedulingGroupPod("w-0", "default", "missing", "10.0.0.1", corev1.PodRunning))
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2)
		assert.Equal(t, 2, info.ExpectedMinCount, "should fall back to discovered peer count")
	})

	t.Run("pod without schedulingGroup returns nil", func(t *testing.T) {
		c := fake.NewClientBuilder().Build()
		d := NewKubernetesDiscoverer(c, podGroupV1alpha2)

		pod := &corev1.Pod{Namespace: "default"}
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

// newWorkloadRefTestClient keeps Pods unstructured, so spec.workloadRef
// survives in the fake client the way it does in the controller cache.
func newWorkloadRefTestClient(t *testing.T, objects ...runtime.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	for _, gvk := range []schema.GroupVersionKind{podGVK, WorkloadGVK} {
		scheme.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objects...).Build()
}

func makeWorkloadRefPod(t *testing.T, name, workloadName, replicaKey string) *unstructured.Unstructured {
	t.Helper()

	pod := &unstructured.Unstructured{}
	pod.SetGroupVersionKind(podGVK)
	pod.SetNamespace("default")
	pod.SetName(name)
	ref := map[string]any{"name": workloadName, "podGroup": "workers"}
	if replicaKey != "" {
		ref["podGroupReplicaKey"] = replicaKey
	}
	require.NoError(t, unstructured.SetNestedField(pod.Object, ref, "spec", "workloadRef"))
	require.NoError(t, unstructured.SetNestedField(pod.Object, "10.0.0.1", "status", "podIP"))
	require.NoError(t, unstructured.SetNestedField(pod.Object, string(corev1.PodRunning), "status", "phase"))

	return pod
}

// admittedPod returns the pod as the webhook sees it: no name yet, no status,
// and not stored in the API server.
func admittedPod(stored *unstructured.Unstructured) *unstructured.Unstructured {
	pod := stored.DeepCopy()
	pod.SetName("")
	pod.SetGenerateName("worker-")
	unstructured.RemoveNestedField(pod.Object, "status")

	return pod
}

// TestWorkloadRefDiscoverer_GangIdentity checks that the webhook and the gang
// controller compute the same gang ID. The webhook mounts the ConfigMap for
// the ID from AdmissionGang; the controller writes the one for the ID from
// ExtractGangID and DiscoverPeers. Each replica of a pod group is its own gang.
func TestWorkloadRefDiscoverer_GangIdentity(t *testing.T) {
	stored := map[string]*unstructured.Unstructured{
		"r0-a":       makeWorkloadRefPod(t, "r0-a", "train", "replica-0"),
		"r0-b":       makeWorkloadRefPod(t, "r0-b", "train", "replica-0"),
		"r1-a":       makeWorkloadRefPod(t, "r1-a", "train", "replica-1"),
		"r1-b":       makeWorkloadRefPod(t, "r1-b", "train", "replica-1"),
		"no-replica": makeWorkloadRefPod(t, "no-replica", "train", ""),
	}
	objects := []runtime.Object{makeNativeWorkload("default", "train")}
	for _, pod := range stored {
		objects = append(objects, pod)
	}
	d := NewWorkloadRefDiscoverer(newWorkloadRefTestClient(t, objects...))

	tests := []struct {
		pod        string
		wantGangID string
		wantPeers  []string
	}{
		{pod: "r0-a", wantGangID: "kubernetes-default-train-workers-replica-0", wantPeers: []string{"r0-a", "r0-b"}},
		{pod: "r1-b", wantGangID: "kubernetes-default-train-workers-replica-1", wantPeers: []string{"r1-a", "r1-b"}},
		{pod: "no-replica", wantGangID: "kubernetes-default-train-workers", wantPeers: []string{"no-replica"}},
	}

	for _, tt := range tests {
		t.Run(tt.pod, func(t *testing.T) {
			admissionGangID, ownerRef, err := d.AdmissionGang(context.Background(), admittedPod(stored[tt.pod]))
			require.NoError(t, err)
			assert.Equal(t, tt.wantGangID, admissionGangID)
			require.NotNil(t, ownerRef)
			assert.Equal(t, "Workload", ownerRef.Kind)
			assert.Equal(t, k8stypes.UID("train-uid"), ownerRef.UID)

			pod := &corev1.Pod{Name: tt.pod, Namespace: "default"}
			assert.Equal(t, tt.wantGangID, d.ExtractGangID(pod))

			info, err := d.DiscoverPeers(context.Background(), pod)
			require.NoError(t, err)
			require.NotNil(t, info)
			assert.Equal(t, tt.wantGangID, info.GangID)

			var peers []string
			for _, peer := range info.Peers {
				peers = append(peers, peer.PodName)
			}
			assert.ElementsMatch(t, tt.wantPeers, peers)
		})
	}
}

func TestWorkloadRefDiscoverer_AdmissionGang_Unresolved(t *testing.T) {
	d := NewWorkloadRefDiscoverer(newWorkloadRefTestClient(t))

	t.Run("missing Workload keeps the gang without an owner", func(t *testing.T) {
		gangID, ownerRef, err := d.AdmissionGang(context.Background(),
			admittedPod(makeWorkloadRefPod(t, "w-0", "missing", "")))
		require.Error(t, err)
		assert.Equal(t, "kubernetes-default-missing-workers", gangID)
		assert.Nil(t, ownerRef)
	})

	t.Run("pod without workloadRef is not a gang member", func(t *testing.T) {
		pod := admittedPod(makeWorkloadRefPod(t, "w-0", "train", ""))
		unstructured.RemoveNestedField(pod.Object, "spec", "workloadRef")

		gangID, ownerRef, err := d.AdmissionGang(context.Background(), pod)
		require.NoError(t, err)
		assert.Empty(t, gangID)
		assert.Nil(t, ownerRef)
	})
}
