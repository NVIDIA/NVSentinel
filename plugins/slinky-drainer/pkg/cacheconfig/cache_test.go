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

	"github.com/nvidia/nvsentinel/plugins/slinky-drainer/pkg/nodemeta"
)

const (
	testStateLabel       = nodemeta.StateLabelKey
	testCordonAnnotation = nodemeta.CordonReasonAnnotationKey
)

func TestBuild_ScopesEachKindToWhatTheDrainerReads(t *testing.T) {
	options, err := Build("slinky")
	require.NoError(t, err)

	tests := []struct {
		name string
		// object identifies the ByObject entry under test.
		object client.Object
		// wantNamespaces is the exact set of namespaces the informer subscribes
		// to. Empty means cluster-wide.
		wantNamespaces []string
	}{
		{
			name:           "pods are limited to the slinky namespace",
			object:         &corev1.Pod{},
			wantNamespaces: []string{"slinky"},
		},
		{
			name:           "nodes stay cluster-wide because nodes are cluster-scoped",
			object:         &corev1.Node{},
			wantNamespaces: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			byObject := cacheForObject(t, options, tt.object)

			require.Len(t, byObject.Namespaces, len(tt.wantNamespaces))

			for _, namespace := range tt.wantNamespaces {
				require.Contains(t, byObject.Namespaces, namespace)
			}

			assert.NotNil(t, byObject.Transform, "every cached kind must be pruned")
		})
	}
}

// An empty namespace is cache.AllNamespaces, so accepting it would hand back a
// cluster-wide Pod informer instead of a scoped one.
func TestBuild_EmptyNamespace_ReturnsError(t *testing.T) {
	_, err := Build("")

	require.ErrorContains(t, err, "slinky namespace must not be empty")
}

func TestTransformNodeForCache_RetainsDrainerFieldsOnly(t *testing.T) {
	tests := []struct {
		name string
		node *corev1.Node
		want *corev1.Node
	}{
		{
			name: "drops spec, status, managed fields and unrelated metadata",
			node: &corev1.Node{
				Kind: "Node", APIVersion: "v1",
				Name:            "node-a",
				UID:             types.UID("node-uid"),
				ResourceVersion: "node-rv",
				Labels: map[string]string{
					testStateLabel: "draining",
					// Node Feature Discovery labels are numerous on GPU nodes
					// and churn independently of remediation.
					"feature.node.kubernetes.io/cpu-model.id": "drop",
					"nvidia.com/gpu.product":                  "drop",
				},
				Annotations: map[string]string{
					testCordonAnnotation: "[T] [NVSentinel] 79",
					"kubectl.kubernetes.io/last-applied-configuration": "drop-large-blob",
				},
				ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "drop-manager"}},
				Spec: corev1.NodeSpec{
					ProviderID:    "drop-provider",
					Unschedulable: true,
					Taints:        []corev1.Taint{{Key: "drop", Effect: corev1.TaintEffectNoSchedule}},
				},
				Status: corev1.NodeStatus{
					Conditions: []corev1.NodeCondition{{Type: corev1.NodeReady, Status: corev1.ConditionTrue}},
					Images:     []corev1.ContainerImage{{Names: []string{"drop-image"}}},
				},
			},
			want: &corev1.Node{
				Name:            "node-a",
				UID:             types.UID("node-uid"),
				ResourceVersion: "node-rv",
				Labels:          map[string]string{testStateLabel: "draining"},
				Annotations:     map[string]string{testCordonAnnotation: "[T] [NVSentinel] 79"},
			},
		},
		{
			// shouldRemoveAnnotation reads a missing label, so an unlabelled
			// node must survive the transform unchanged rather than gain maps.
			name: "keeps absent labels and annotations absent",
			node: &corev1.Node{
				Name: "node-b",
				Spec: corev1.NodeSpec{Unschedulable: true},
			},
			want: &corev1.Node{Name: "node-b"},
		},
		{
			// A node carrying only foreign metadata must come out with nil
			// maps, so it is indistinguishable from a node NVSentinel has
			// never touched.
			name: "drops metadata down to nil when no retained key is present",
			node: &corev1.Node{
				Name:        "node-c",
				Labels:      map[string]string{"nvidia.com/gpu.count": "8"},
				Annotations: map[string]string{"csi.volume.kubernetes.io/nodeid": "drop"},
			},
			want: &corev1.Node{Name: "node-c"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transformed, err := transformNodeForCache(tt.node)

			require.NoError(t, err)
			assert.Same(t, tt.node, transformed, "transform must prune in place")
			assert.Equal(t, tt.want, transformed)
		})
	}
}

func TestTransformPodForCache_RetainsDrainerFieldsOnly(t *testing.T) {
	tests := []struct {
		name string
		pod  *corev1.Pod
		want *corev1.Pod
	}{
		{
			name: "drops spec, metadata and condition detail",
			pod: &corev1.Pod{
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
					Conditions: []corev1.PodCondition{{
						Type:               corev1.PodReady,
						Status:             corev1.ConditionTrue,
						Reason:             "drop-reason",
						Message:            "drop-message",
						LastTransitionTime: metav1.NewTime(time.Unix(123, 0)),
					}},
				},
			},
			want: &corev1.Pod{
				Name:            "slurmd-0",
				Namespace:       "slinky",
				UID:             types.UID("pod-uid"),
				ResourceVersion: "pod-rv",
				Spec:            corev1.PodSpec{NodeName: "node-a"},
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{{
						Type:   corev1.PodReady,
						Status: corev1.ConditionTrue,
					}},
				},
			},
		},
		{
			// The reconciler matches six Slurm condition types today and the
			// Slinky operator can report more, so conditions must not be
			// filtered by type.
			name: "keeps unrecognised Slurm state conditions",
			pod: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: "SlurmNodeStateDrain", Status: corev1.ConditionTrue},
						{Type: "SlurmNodeStateSomeFutureState", Status: corev1.ConditionTrue, Message: "drop"},
					},
				},
			},
			want: &corev1.Pod{
				Status: corev1.PodStatus{
					Conditions: []corev1.PodCondition{
						{Type: "SlurmNodeStateDrain", Status: corev1.ConditionTrue},
						{Type: "SlurmNodeStateSomeFutureState", Status: corev1.ConditionTrue},
					},
				},
			},
		},
		{
			name: "leaves a pod with no conditions with no conditions",
			pod: &corev1.Pod{
				Name: "slurmd-1",
				Spec: corev1.PodSpec{NodeName: "node-b"},
			},
			want: &corev1.Pod{
				Name: "slurmd-1",
				Spec: corev1.PodSpec{NodeName: "node-b"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			transformed, err := transformPodForCache(tt.pod)

			require.NoError(t, err)
			assert.Same(t, tt.pod, transformed, "transform must prune in place")
			assert.Equal(t, tt.want, transformed)
		})
	}
}

func TestTransformForCache_WrongType_ReturnsError(t *testing.T) {
	tests := []struct {
		name      string
		transform func(any) (any, error)
		object    any
		wantErr   string
	}{
		{
			name:      "node transform given a pod",
			transform: transformNodeForCache,
			object:    &corev1.Pod{},
			wantErr:   "node cache transform expected *v1.Node, got *v1.Pod",
		},
		{
			name:      "pod transform given a node",
			transform: transformPodForCache,
			object:    &corev1.Node{},
			wantErr:   "pod cache transform expected *v1.Pod, got *v1.Node",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := tt.transform(tt.object)

			require.ErrorContains(t, err, tt.wantErr)
		})
	}
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
