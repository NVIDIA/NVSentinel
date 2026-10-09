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

// Package cacheconfig builds the controller-runtime cache used by Janitor.
package cacheconfig

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/selection"
	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/janitor/pkg/config"
	"github.com/nvidia/nvsentinel/janitor/pkg/gpuservices"
)

// Build returns cache options that retain only Node and Pod fields read by
// Janitor. When GPU reset is enabled, the Pod cache is also limited to the
// configured GPU service manager's namespace and the pods of its apps.
func Build(cfg *config.Config) (cache.Options, error) {
	byObject := map[client.Object]cache.ByObject{
		&corev1.Node{}: {
			Transform: transformNodeForCache,
		},
	}

	if cfg.GPUReset.Enabled {
		serviceManager, err := gpuservices.NewManager(
			cfg.GPUReset.ServiceManager.Name,
			cfg.GPUReset.ServiceManager.Spec,
		)
		if err != nil {
			return cache.Options{}, fmt.Errorf("resolve GPU service manager for Pod cache: %w", err)
		}

		podSelector, err := podCacheSelector(serviceManager.Spec)
		if err != nil {
			return cache.Options{}, err
		}

		byObject[&corev1.Pod{}] = cache.ByObject{
			Namespaces: map[string]cache.Config{
				serviceManager.Spec.Namespace: {
					LabelSelector: podSelector,
				},
			},
			Transform: transformPodForCache,
		}
	}

	return cache.Options{ByObject: byObject}, nil
}

// podCacheSelector returns the manager selector. For each label key that every
// app selector uses, the selector also requires that key to have one of the app
// values. The built-in gpu-operator manager has no manager selector, so this keeps
// other pods in its namespace out of the cache. Each app pod still matches,
// because it has one of the values for every such key.
func podCacheSelector(spec gpuservices.ManagerSpec) (labels.Selector, error) {
	selector := labels.SelectorFromSet(spec.ManagerSelector)

	for key, values := range sharedAppSelectorValues(spec.Apps) {
		requirement, err := labels.NewRequirement(key, selection.In, sets.List(values))
		if err != nil {
			return nil, fmt.Errorf("build Pod cache selector for label %s: %w", key, err)
		}

		selector = selector.Add(*requirement)
	}

	return selector, nil
}

// sharedAppSelectorValues returns, for each label key that every app selector
// uses, the values of that key across the app selectors.
func sharedAppSelectorValues(apps []gpuservices.AppSpec) map[string]sets.Set[string] {
	if len(apps) == 0 {
		return nil
	}

	shared := make(map[string]sets.Set[string], len(apps[0].AppSelector))
	for key := range apps[0].AppSelector {
		shared[key] = sets.New[string]()
	}

	for _, app := range apps {
		for key, values := range shared {
			value, ok := app.AppSelector[key]
			if !ok {
				delete(shared, key)

				continue
			}

			values.Insert(value)
		}
	}

	return shared
}

// transformNodeForCache keeps fields used by Node patches, taint handling, and
// readiness checks while dropping unused status and metadata payloads.
func transformNodeForCache(obj any) (any, error) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil, fmt.Errorf("node cache transform expected *v1.Node, got %T", obj)
	}

	readyConditions := nodeConditionsForCache(node.Status.Conditions)

	node.TypeMeta = metav1.TypeMeta{}
	node.ObjectMeta = metav1.ObjectMeta{
		Name:            node.Name,
		UID:             node.UID,
		ResourceVersion: node.ResourceVersion,
		Labels:          node.Labels,
	}
	node.Spec = corev1.NodeSpec{Taints: node.Spec.Taints}
	node.Status = corev1.NodeStatus{Conditions: readyConditions}

	return node, nil
}

// transformPodForCache keeps fields used by cache selectors, the node-name
// index, and GPU service readiness checks.
func transformPodForCache(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("pod cache transform expected *v1.Pod, got %T", obj)
	}

	readyConditions := podReadyConditionsForCache(pod.Status.Conditions)

	pod.TypeMeta = metav1.TypeMeta{}
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		UID:             pod.UID,
		ResourceVersion: pod.ResourceVersion,
		Labels:          pod.Labels,
	}
	pod.Spec = corev1.PodSpec{NodeName: pod.Spec.NodeName}
	pod.Status = corev1.PodStatus{
		Phase:      pod.Status.Phase,
		Conditions: readyConditions,
	}

	return pod, nil
}

// nodeConditionsForCache retains only the type and status used to determine
// whether a Node is ready.
func nodeConditionsForCache(conditions []corev1.NodeCondition) []corev1.NodeCondition {
	var cached []corev1.NodeCondition

	for _, condition := range conditions {
		if condition.Type != corev1.NodeReady {
			continue
		}

		cached = append(cached, corev1.NodeCondition{
			Type:   condition.Type,
			Status: condition.Status,
		})
	}

	return cached
}

// podReadyConditionsForCache retains only the PodReady condition fields used
// while restoring GPU services.
func podReadyConditionsForCache(conditions []corev1.PodCondition) []corev1.PodCondition {
	var cached []corev1.PodCondition

	for _, condition := range conditions {
		if condition.Type != corev1.PodReady {
			continue
		}

		cached = append(cached, corev1.PodCondition{
			Type:   condition.Type,
			Status: condition.Status,
		})
	}

	return cached
}
