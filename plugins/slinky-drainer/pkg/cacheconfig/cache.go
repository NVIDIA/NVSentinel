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

// Package cacheconfig builds the controller-runtime cache used by Slinky Drainer.
package cacheconfig

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/nvidia/nvsentinel/plugins/slinky-drainer/pkg/nodemeta"
)

// Build returns cache options that limit the Pod cache to slinkyNamespace and
// retain only the Pod and Node fields read by the reconciler. Without this the
// manager caches every Pod and every full Node in the cluster, which on a large
// GPU fleet is orders of magnitude more memory than the drainer needs.
//
// Pruned objects must never be written back with Update, because the dropped
// fields would be sent as empty. Node writes use a merge patch for this reason.
//
// slinkyNamespace must not be empty. The cache reads an empty namespace as
// cache.AllNamespaces, so an empty value would silently restore the
// cluster-wide Pod informer this function exists to avoid.
func Build(slinkyNamespace string) (cache.Options, error) {
	if slinkyNamespace == "" {
		return cache.Options{}, errors.New("slinky namespace must not be empty")
	}

	return cache.Options{
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Pod{}: {
				Namespaces: map[string]cache.Config{slinkyNamespace: {}},
				Transform:  transformPodForCache,
			},
			&corev1.Node{}: {
				Transform: transformNodeForCache,
			},
		},
	}, nil
}

// transformNodeForCache keeps node identity, the one label that gates
// annotation removal, and the one annotation the reconciler writes. Node spec
// and status go, as does every other label and annotation.
//
// Dropping the rest of the labels also quietens the controller. The Node watch
// filters on predicate.LabelChangedPredicate, which compares the cached label
// maps, so retaining the full set would wake the reconciler every time an
// unrelated label changed. GPU nodes carry large Node Feature Discovery and GPU
// Feature Discovery label sets that churn independently of remediation.
func transformNodeForCache(obj any) (any, error) {
	node, ok := obj.(*corev1.Node)
	if !ok {
		return nil, fmt.Errorf("node cache transform expected *v1.Node, got %T", obj)
	}

	// Identity is kept for the client machinery rather than the reconciler:
	// Name addresses the object, and UID and ResourceVersion keep a cached Node
	// usable as the base of a patch.
	node.TypeMeta = metav1.TypeMeta{}
	node.ObjectMeta = metav1.ObjectMeta{
		Name:            node.Name,
		UID:             node.UID,
		ResourceVersion: node.ResourceVersion,
		Labels:          retainKey(node.Labels, nodemeta.StateLabelKey),
		Annotations:     retainKey(node.Annotations, nodemeta.CordonReasonAnnotationKey),
	}
	node.Spec = corev1.NodeSpec{}
	node.Status = corev1.NodeStatus{}

	return node, nil
}

// retainKey returns a map holding key alone, or nil when source does not have
// it. Nil rather than an empty map matters: the reconciler treats a missing
// state label as the signal that remediation finished, and a merge patch
// computed from an empty map would still be correct but noisier.
func retainKey(source map[string]string, key string) map[string]string {
	value, found := source[key]
	if !found {
		return nil
	}

	return map[string]string{key: value}
}

// transformPodForCache keeps the node-name index key and the conditions that
// report Slurm drain state. Everything else is dropped.
func transformPodForCache(obj any) (any, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil, fmt.Errorf("pod cache transform expected *v1.Pod, got %T", obj)
	}

	conditions := podConditionsForCache(pod.Status.Conditions)

	pod.TypeMeta = metav1.TypeMeta{}
	pod.ObjectMeta = metav1.ObjectMeta{
		Name:            pod.Name,
		Namespace:       pod.Namespace,
		UID:             pod.UID,
		ResourceVersion: pod.ResourceVersion,
	}
	pod.Spec = corev1.PodSpec{NodeName: pod.Spec.NodeName}
	pod.Status = corev1.PodStatus{Conditions: conditions}

	return pod, nil
}

// podConditionsForCache retains the type and status of every condition. The
// reconciler matches several Slurm state condition types, so keeping all of
// them stays correct when the Slinky operator reports a new state.
func podConditionsForCache(conditions []corev1.PodCondition) []corev1.PodCondition {
	if len(conditions) == 0 {
		return nil
	}

	cached := make([]corev1.PodCondition, 0, len(conditions))

	for _, condition := range conditions {
		cached = append(cached, corev1.PodCondition{
			Type:   condition.Type,
			Status: condition.Status,
		})
	}

	return cached
}
