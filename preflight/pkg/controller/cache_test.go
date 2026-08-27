// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
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
	"testing"
	"time"

	"github.com/nvidia/nvsentinel/preflight/pkg/gang"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/discoverer"
	gangtypes "github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

func TestManagerCacheOptionsKeepsPodCacheClusterWide(t *testing.T) {
	options := ManagerCacheOptions(NewActiveNamespaces())
	podOptions := podCacheOptions(t, options)

	assert.NotNil(t, podOptions.Namespaces)
	assert.Empty(t, podOptions.Namespaces)
	require.NotNil(t, podOptions.Transform)
}

func TestManagerCacheOptionsSupportsDynamicPreflightConfigNamespaces(t *testing.T) {
	options := ManagerCacheOptions(NewActiveNamespaces())
	resolver := gang.NewResolver(&mockDiscoverer{}, nil)
	pfc := volcanoPFC("added-after-startup", "default")
	reconciler, _ := newReconcilerWith(t, resolver, pfc)

	reconcile(t, reconciler, pfc.Namespace, pfc.Name)

	assert.Equal(t, "volcano", resolver.For(pfc.Namespace).Name())
	assert.Empty(t, podCacheOptions(t, options).Namespaces,
		"the Pod cache must remain cluster-wide when namespace overrides change")
}

func TestTransformPodForCacheRetainsRequiredFields(t *testing.T) {
	deletionTime := metav1.NewTime(time.Now())
	podGroup := "training"
	optional := true
	original := &corev1.Pod{
		APIVersion: "v1", Kind: "Pod",
		Name:              "worker-0",
		Namespace:         "team-a",
		UID:               "pod-uid",
		ResourceVersion:   "42",
		DeletionTimestamp: &deletionTime,
		Annotations:       map[string]string{"scheduler.example/group": "training"},
		Labels:            map[string]string{"job": "training"},
		Finalizers:        []string{"unused"},
		ManagedFields:     []metav1.ManagedFieldsEntry{{Manager: "unused"}},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Volumes: []corev1.Volume{
				{
					Name: gangtypes.GangConfigVolumeName,
					ConfigMap: &corev1.ConfigMapVolumeSource{
						Name:     "gang-config",
						Optional: &optional,
					},
				},
				{Name: "unused"},
			},
			SchedulingGroup: &corev1.PodSchedulingGroup{PodGroupName: &podGroup},
			Containers:      []corev1.Container{{Name: "large", Image: "large-image"}},
			InitContainers:  []corev1.Container{{Name: "large-init", Image: "large-image"}},
		},
		Status: corev1.PodStatus{
			PodIP:      "10.0.0.1",
			Phase:      corev1.PodRunning,
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady}},
		},
	}

	gotObject, err := activeNamespacesFor("team-a").transform(original)
	require.NoError(t, err)
	got := gotObject.(*corev1.Pod)

	assert.Same(t, original, got)
	assert.Equal(t, &corev1.Pod{
		Name:              "worker-0",
		Namespace:         "team-a",
		UID:               "pod-uid",
		ResourceVersion:   "42",
		DeletionTimestamp: &deletionTime,
		Annotations:       map[string]string{"scheduler.example/group": "training"},
		Labels:            map[string]string{"job": "training"},
		Spec: corev1.PodSpec{
			NodeName: "node-a",
			Volumes: []corev1.Volume{{
				Name: gangtypes.GangConfigVolumeName,
				ConfigMap: &corev1.ConfigMapVolumeSource{
					Name: "gang-config",
				},
			}},
			SchedulingGroup: &corev1.PodSchedulingGroup{PodGroupName: &podGroup},
		},
		Status: corev1.PodStatus{
			PodIP: "10.0.0.1",
			Phase: corev1.PodRunning,
		},
	}, got)
}

func TestTransformPodForCache_UnstructuredPod_RetainsRequiredFields(t *testing.T) {
	pod := newUnstructuredObject(
		schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
		"team-a",
		"worker-0",
	)
	pod.SetResourceVersion("42")
	pod.SetAnnotations(map[string]string{"scheduler.example/group": "training"})
	pod.SetLabels(map[string]string{"job": "training"})
	require.NoError(t, unstructured.SetNestedField(
		pod.Object, "node-a", "spec", "nodeName"))
	require.NoError(t, unstructured.SetNestedSlice(pod.Object, []any{
		map[string]any{
			"name": gangtypes.GangConfigVolumeName,
			"configMap": map[string]any{
				"name":     "gang-config",
				"optional": true,
			},
		},
		map[string]any{"name": "unused"},
	}, "spec", "volumes"))
	require.NoError(t, unstructured.SetNestedField(
		pod.Object, "training", "spec", "schedulingGroup", "podGroupName"))
	require.NoError(t, unstructured.SetNestedField(
		pod.Object, "workload", "spec", "workloadRef", "name"))
	require.NoError(t, unstructured.SetNestedField(
		pod.Object, "workers", "spec", "workloadRef", "podGroup"))
	require.NoError(t, unstructured.SetNestedSlice(
		pod.Object, []any{map[string]any{"name": "large"}}, "spec", "containers"))
	require.NoError(t, unstructured.SetNestedField(
		pod.Object, "10.0.0.1", "status", "podIP"))
	require.NoError(t, unstructured.SetNestedField(
		pod.Object, string(corev1.PodRunning), "status", "phase"))

	transformed, err := activeNamespacesFor("team-a").transform(pod)
	require.NoError(t, err)
	got := transformed.(*unstructured.Unstructured)
	assert.Same(t, pod, got)

	volumes, found, err := unstructured.NestedSlice(got.Object, "spec", "volumes")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []any{map[string]any{
		"name": gangtypes.GangConfigVolumeName,
		"configMap": map[string]any{
			"name": "gang-config",
		},
	}}, volumes)

	assert.Equal(t, "training", mustNestedString(t, got, "spec", "schedulingGroup", "podGroupName"))
	assert.Equal(t, "workload", mustNestedString(t, got, "spec", "workloadRef", "name"))
	assert.Equal(t, "workers", mustNestedString(t, got, "spec", "workloadRef", "podGroup"))
	assert.Equal(t, "node-a", mustNestedString(t, got, "spec", "nodeName"))
	assert.Equal(t, "10.0.0.1", mustNestedString(t, got, "status", "podIP"))
	assert.Equal(t, string(corev1.PodRunning), mustNestedString(t, got, "status", "phase"))

	_, found, err = unstructured.NestedSlice(got.Object, "spec", "containers")
	require.NoError(t, err)
	assert.False(t, found)
}

func TestTransformPodForCachePreservesPredicateBehavior(t *testing.T) {
	oldPod := gangPodForCacheTest("worker-0", "team-a", "")
	newPod := gangPodForCacheTest("worker-0", "team-a", "10.0.0.1")
	oldTransformed := mustTransformTypedPod(t, oldPod)
	newTransformed := mustTransformTypedPod(t, newPod)

	predicate := (&GangController{}).podIPChangedPredicate()
	assert.True(t, predicate.Create(event.CreateEvent{Object: newTransformed}))
	assert.True(t, predicate.Update(event.UpdateEvent{ObjectOld: oldTransformed, ObjectNew: newTransformed}))

	unchangedIP := newPod.DeepCopy()
	unchangedIP.Spec.Containers = []corev1.Container{{Name: "changed-heavy-field"}}
	assert.False(t, predicate.Update(event.UpdateEvent{
		ObjectOld: newTransformed,
		ObjectNew: mustTransformTypedPod(t, unchangedIP),
	}))
}

func TestTransformedPodsSupportAllGangDiscoverers(t *testing.T) {
	t.Run("configured PodGroup annotations and labels", func(t *testing.T) {
		podGroupGVK := schema.GroupVersionKind{
			Group: "scheduling.example.io", Version: "v1", Kind: "PodGroup",
		}
		podGroup := newUnstructuredObject(podGroupGVK, "team-a", "training")
		podGroup.SetUID("training-uid")
		require.NoError(t, unstructured.SetNestedField(podGroup.Object, int64(2), "spec", "minMember"))

		annotationPod := discovererPod("worker-0", "team-a", "10.0.0.1")
		annotationPod.Annotations = map[string]string{"scheduler.example/group": "training"}
		labelPod := discovererPod("worker-1", "team-a", "10.0.0.2")
		labelPod.Labels = map[string]string{"scheduler.example/job": "training"}

		c := fake.NewClientBuilder().WithRuntimeObjects(
			mustTransformTypedPod(t, annotationPod),
			mustTransformTypedPod(t, labelPod),
			podGroup,
		).Build()
		d, err := discoverer.NewPodGroupDiscoverer(c, discoverer.PodGroupConfig{
			Name:           "configured",
			AnnotationKeys: []string{"scheduler.example/group"},
			LabelKeys:      []string{"scheduler.example/job"},
			PodGroupGVK:    podGroupGVK,
			MinCountExpr:   "podGroup.spec.minMember",
		})
		require.NoError(t, err)

		info, err := d.DiscoverPeers(context.Background(), annotationPod)
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2)
		assert.Equal(t, 2, info.ExpectedMinCount)
	})

	t.Run("native schedulingGroup", func(t *testing.T) {
		podGroup := newUnstructuredObject(discoverer.PodGroupGVK, "team-a", "training")
		require.NoError(t, unstructured.SetNestedField(
			podGroup.Object, int64(2), "spec", "schedulingPolicy", "gang", "minCount"))
		pods := []runtime.Object{podGroup}
		for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
			pod := discovererPod("worker-"+string(rune('0'+i)), "team-a", ip)
			group := "training"
			pod.Spec.SchedulingGroup = &corev1.PodSchedulingGroup{PodGroupName: &group}
			pods = append(pods, mustTransformTypedPod(t, pod))
		}

		d := discoverer.NewKubernetesDiscoverer(fake.NewClientBuilder().WithRuntimeObjects(pods...).Build())
		request := pods[1].(*corev1.Pod)
		info, err := d.DiscoverPeers(context.Background(), request)
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2)
		assert.Equal(t, 2, info.ExpectedMinCount)
	})

	t.Run("Kubernetes 1.35 workloadRef", func(t *testing.T) {
		workload := newUnstructuredObject(discoverer.WorkloadGVK, "team-a", "training")
		pods := []runtime.Object{workload}
		for i, ip := range []string{"10.0.0.1", "10.0.0.2"} {
			pod := newUnstructuredObject(schema.GroupVersionKind{Version: "v1", Kind: "Pod"},
				"team-a", "worker-"+string(rune('0'+i)))
			require.NoError(t, unstructured.SetNestedField(
				pod.Object, map[string]any{"name": "training", "podGroup": "workers"},
				"spec", "workloadRef"))
			require.NoError(t, unstructured.SetNestedField(pod.Object, "node-a", "spec", "nodeName"))
			require.NoError(t, unstructured.SetNestedField(pod.Object, ip, "status", "podIP"))
			require.NoError(t, unstructured.SetNestedField(
				pod.Object, string(corev1.PodRunning), "status", "phase"))
			transformed, err := activeNamespacesFor("team-a").transform(pod)
			require.NoError(t, err)
			pods = append(pods, transformed.(runtime.Object))
		}

		scheme := runtime.NewScheme()
		podGVK := schema.GroupVersionKind{Version: "v1", Kind: "Pod"}
		scheme.AddKnownTypeWithName(podGVK, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(
			podGVK.GroupVersion().WithKind("PodList"),
			&unstructured.UnstructuredList{},
		)
		scheme.AddKnownTypeWithName(discoverer.WorkloadGVK, &unstructured.Unstructured{})
		scheme.AddKnownTypeWithName(
			discoverer.WorkloadGVK.GroupVersion().WithKind("WorkloadList"),
			&unstructured.UnstructuredList{},
		)

		c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(pods...).Build()
		cachedPod := &unstructured.Unstructured{}
		cachedPod.SetGroupVersionKind(podGVK)
		require.NoError(t, c.Get(
			context.Background(),
			client.ObjectKey{Namespace: "team-a", Name: "worker-0"},
			cachedPod,
		))
		assert.Equal(t, "training", mustNestedString(t, cachedPod, "spec", "workloadRef", "name"))

		d := discoverer.NewWorkloadRefDiscoverer(c)
		request := &corev1.Pod{
			Name: "worker-0", Namespace: "team-a"}
		info, err := d.DiscoverPeers(context.Background(), request)
		require.NoError(t, err)
		require.NotNil(t, info)
		assert.Len(t, info.Peers, 2)
		assert.Equal(t, 2, info.ExpectedMinCount)
	})
}

func TestTransformPodForCache_InactiveNamespace_ReturnsStub(t *testing.T) {
	pod := &corev1.Pod{
		Name: "worker-0", Namespace: "other-ns",
		UID: "pod-uid", ResourceVersion: "99",
		Annotations: map[string]string{"big": "annotation"},
		Labels:      map[string]string{"big": "label"},
		Spec:        corev1.PodSpec{NodeName: "node-a", Containers: []corev1.Container{{Name: "large"}}},
		Status:      corev1.PodStatus{PodIP: "10.0.0.1", Phase: corev1.PodRunning},
	}

	// "other-ns" is not in the active set.
	result, err := activeNamespacesFor("team-a").transform(pod)
	require.NoError(t, err)
	got := result.(*corev1.Pod)

	assert.Same(t, pod, got)
	assert.Equal(t, "worker-0", got.Name)
	assert.Equal(t, "other-ns", got.Namespace)
	assert.Equal(t, k8stypes.UID("pod-uid"), got.UID)
	assert.Equal(t, "99", got.ResourceVersion)
	assert.Empty(t, got.Annotations)
	assert.Empty(t, got.Labels)
	assert.Empty(t, got.Spec.NodeName)
	assert.Empty(t, got.Spec.Containers)
	assert.Empty(t, got.Status.PodIP)
}

func TestTransformPodForCache_NamespaceBecomesActive_FullTransformApplied(t *testing.T) {
	active := NewActiveNamespaces()
	pod := gangPodForCacheTest("worker-0", "team-b", "10.0.0.1")

	// Before activation: stub
	stub, err := active.transform(pod.DeepCopy())
	require.NoError(t, err)
	assert.Empty(t, stub.(*corev1.Pod).Status.PodIP)

	// After activation: full transform
	active.Add("team-b")
	full, err := active.transform(pod.DeepCopy())
	require.NoError(t, err)
	assert.Equal(t, "10.0.0.1", full.(*corev1.Pod).Status.PodIP)
}

func podCacheOptions(t *testing.T, options cache.Options) cache.ByObject {
	t.Helper()

	for object, objectOptions := range options.ByObject {
		if _, ok := object.(*corev1.Pod); ok {
			return objectOptions
		}
	}

	t.Fatal("Pod cache options not found")

	return cache.ByObject{}
}

// activeNamespacesFor returns an ActiveNamespaces pre-populated with the given
// namespace names, for use in tests.
func activeNamespacesFor(namespaces ...string) *ActiveNamespaces {
	a := NewActiveNamespaces()
	for _, ns := range namespaces {
		a.Add(ns)
	}
	return a
}

// transform is a test convenience that calls the pod transform for this active set.
func (a *ActiveNamespaces) transform(obj any) (any, error) {
	return podTransformForCache(a)(obj)
}

func mustTransformTypedPod(t *testing.T, pod *corev1.Pod) *corev1.Pod {
	t.Helper()

	transformed, err := activeNamespacesFor(pod.Namespace).transform(pod)
	require.NoError(t, err)

	return transformed.(*corev1.Pod)
}

func gangPodForCacheTest(name, namespace, ip string) *corev1.Pod {
	pod := discovererPod(name, namespace, ip)
	pod.Spec.Volumes = []corev1.Volume{{
		Name: gangtypes.GangConfigVolumeName,
		ConfigMap: &corev1.ConfigMapVolumeSource{
			Name: "gang-config",
		},
	}}

	return pod
}

func discovererPod(name, namespace, ip string) *corev1.Pod {
	return &corev1.Pod{
		APIVersion: "v1", Kind: "Pod",
		Name: name, Namespace: namespace, UID: k8stypes.UID(name + "-uid"),
		Spec: corev1.PodSpec{NodeName: "node-a"},
		Status: corev1.PodStatus{
			PodIP: ip,
			Phase: corev1.PodRunning,
		},
	}
}

func newUnstructuredObject(
	gvk schema.GroupVersionKind,
	namespace, name string,
) *unstructured.Unstructured {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	obj.SetNamespace(namespace)
	obj.SetName(name)
	obj.SetUID(k8stypes.UID(name + "-uid"))

	return obj
}

func mustNestedString(t *testing.T, obj *unstructured.Unstructured, fields ...string) string {
	t.Helper()

	value, found, err := unstructured.NestedString(obj.Object, fields...)
	require.NoError(t, err)
	require.True(t, found)

	return value
}

var _ client.Object = &corev1.Pod{}
