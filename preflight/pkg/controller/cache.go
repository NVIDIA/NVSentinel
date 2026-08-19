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
	"fmt"
	"sync"

	gangtypes "github.com/nvidia/nvsentinel/preflight/pkg/gang/types"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ActiveNamespaces is the thread-safe set of namespaces where preflight is
// enabled. The NamespaceReconciler keeps it current; the pod cache transform
// reads it to decide whether to retain full gang fields or return a minimal
// stub for pods that the gang controller will never process.
type ActiveNamespaces struct {
	mu  sync.RWMutex
	set map[string]struct{}
}

func NewActiveNamespaces() *ActiveNamespaces {
	return &ActiveNamespaces{set: make(map[string]struct{})}
}

func (a *ActiveNamespaces) Add(ns string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.set[ns] = struct{}{}
}

func (a *ActiveNamespaces) Remove(ns string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	delete(a.set, ns)
}

func (a *ActiveNamespaces) Contains(ns string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()

	_, ok := a.set[ns]

	return ok
}

// ManagerCacheOptions reduces the memory used by the manager's cluster-wide Pod
// informer. Pods in preflight-enabled namespaces are stripped to only the fields
// used by the gang controller; pods in all other namespaces are reduced to a
// minimal identity stub.
func ManagerCacheOptions(active *ActiveNamespaces) cache.Options {
	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {
				// An empty map explicitly keeps this cache cluster-wide instead
				// of inheriting any future DefaultNamespaces configuration.
				Namespaces: map[string]cache.Config{},
				Transform:  podTransformForCache(active),
			},
		},
	}
}

func podTransformForCache(active *ActiveNamespaces) func(any) (any, error) {
	return func(obj any) (any, error) {
		switch pod := obj.(type) {
		case *corev1.Pod:
			if active.Contains(pod.Namespace) {
				return transformTypedPod(pod), nil
			}

			return stubTypedPod(pod), nil
		case *unstructured.Unstructured:
			// Kubernetes 1.35 Pods are read as unstructured objects so their
			// spec.workloadRef field survives decoding by the Kubernetes 1.36 client.
			if active.Contains(pod.GetNamespace()) {
				return transformUnstructuredPod(pod), nil
			}

			return stubUnstructuredPod(pod), nil
		default:
			return nil, fmt.Errorf("expected Pod cache object, got %T", obj)
		}
	}
}

func transformTypedPod(pod *corev1.Pod) *corev1.Pod {
	objectMeta := pod.ObjectMeta
	spec := pod.Spec
	status := pod.Status

	pod.TypeMeta = metav1.TypeMeta{}
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:              objectMeta.Name,
		Namespace:         objectMeta.Namespace,
		UID:               objectMeta.UID,
		ResourceVersion:   objectMeta.ResourceVersion,
		DeletionTimestamp: objectMeta.DeletionTimestamp,
		Annotations:       objectMeta.Annotations,
		Labels:            objectMeta.Labels,
	}
	pod.Spec = corev1.PodSpec{
		NodeName:        spec.NodeName,
		Volumes:         gangConfigVolumesForCache(spec.Volumes),
		SchedulingGroup: spec.SchedulingGroup,
	}
	pod.Status = corev1.PodStatus{
		Phase: status.Phase,
		PodIP: status.PodIP,
	}

	return pod
}

// gangConfigVolumesForCache keeps the injected gang volume identity and the
// ConfigMap name used by reconciliation.
func gangConfigVolumesForCache(volumes []corev1.Volume) []corev1.Volume {
	for _, volume := range volumes {
		if volume.Name != gangtypes.GangConfigVolumeName {
			continue
		}

		cached := corev1.Volume{Name: volume.Name}
		if volume.ConfigMap != nil {
			cached.ConfigMap = &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: volume.ConfigMap.Name},
			}
		}

		return []corev1.Volume{cached}
	}

	return nil
}

// stubTypedPod reduces a pod outside any preflight-enabled namespace to the
// minimum identity fields needed by the informer to track the object.
func stubTypedPod(pod *corev1.Pod) *corev1.Pod {
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		UID:             pod.UID,
		ResourceVersion: pod.ResourceVersion,
	}
	pod.TypeMeta = metav1.TypeMeta{}
	pod.Spec = corev1.PodSpec{}
	pod.Status = corev1.PodStatus{}

	return pod
}

func stubUnstructuredPod(pod *unstructured.Unstructured) *unstructured.Unstructured {
	stub := &unstructured.Unstructured{}
	stub.SetGroupVersionKind(pod.GroupVersionKind())
	stub.SetName(pod.GetName())
	stub.SetNamespace(pod.GetNamespace())
	stub.SetUID(pod.GetUID())
	stub.SetResourceVersion(pod.GetResourceVersion())
	pod.Object = stub.Object

	return pod
}

func transformUnstructuredPod(pod *unstructured.Unstructured) *unstructured.Unstructured {
	transformed := &unstructured.Unstructured{}
	transformed.SetGroupVersionKind(pod.GroupVersionKind())
	transformed.SetName(pod.GetName())
	transformed.SetNamespace(pod.GetNamespace())
	transformed.SetUID(pod.GetUID())
	transformed.SetResourceVersion(pod.GetResourceVersion())
	transformed.SetDeletionTimestamp(pod.GetDeletionTimestamp())
	transformed.SetAnnotations(pod.GetAnnotations())
	transformed.SetLabels(pod.GetLabels())

	copyNestedField(pod, transformed, "spec", "nodeName")
	copyUnstructuredGangConfigVolume(pod, transformed)
	copyNestedField(pod, transformed, "spec", "schedulingGroup")
	// Kubernetes 1.35 workloadRef is unstructured because the field was
	// replaced by schedulingGroup in the Kubernetes 1.36 Go API.
	copyNestedField(pod, transformed, "spec", "workloadRef")
	copyNestedField(pod, transformed, "status", "podIP")
	copyNestedField(pod, transformed, "status", "phase")

	pod.Object = transformed.Object

	return pod
}

func copyUnstructuredGangConfigVolume(from, to *unstructured.Unstructured) {
	volumes, found, err := unstructured.NestedSlice(from.Object, "spec", "volumes")
	if err != nil || !found {
		return
	}

	for _, value := range volumes {
		volume, ok := value.(map[string]any)
		if !ok || volume["name"] != gangtypes.GangConfigVolumeName {
			continue
		}

		cached := map[string]any{"name": gangtypes.GangConfigVolumeName}
		if name, exists, _ := unstructured.NestedString(volume, "configMap", "name"); exists {
			cached["configMap"] = map[string]any{"name": name}
		}

		_ = unstructured.SetNestedSlice(to.Object, []any{cached}, "spec", "volumes")

		return
	}
}

// copyNestedField copies one field without retaining its surrounding object.
func copyNestedField(from, to *unstructured.Unstructured, fields ...string) {
	value, found, err := unstructured.NestedFieldCopy(from.Object, fields...)
	if err != nil || !found {
		return
	}

	_ = unstructured.SetNestedField(to.Object, value, fields...)
}
