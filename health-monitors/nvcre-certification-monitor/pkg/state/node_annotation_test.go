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

package state

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newFakeManager(objects ...runtime.Object) (*AnnotationManager, *corev1.Node) {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)

	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "gpu-01",
		},
	}

	allObjects := append([]runtime.Object{node}, objects...)
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(allObjects...).Build()

	return NewAnnotationManager(c), node
}

func getNode(t *testing.T, mgr *AnnotationManager, name string) *corev1.Node {
	t.Helper()
	node := &corev1.Node{}
	err := mgr.client.Get(context.Background(), types.NamespacedName{Name: name}, node)
	require.NoError(t, err)
	return node
}

func getAnnotationKeys(t *testing.T, mgr *AnnotationManager, nodeName string) []string {
	t.Helper()
	node := getNode(t, mgr, nodeName)
	raw, ok := node.Annotations[AnnotationKey]
	if !ok || raw == "" {
		return nil
	}
	var keys []string
	require.NoError(t, json.Unmarshal([]byte(raw), &keys))
	return keys
}

func TestAddTuple(t *testing.T) {
	mgr, _ := newFakeManager()
	ctx := context.Background()

	err := mgr.AddTuple(ctx, "gpu-01", "nccl-all-gather/WorkloadFailed")
	require.NoError(t, err)

	node := getNode(t, mgr, "gpu-01")
	keys := getAnnotationKeys(t, mgr, "gpu-01")
	assert.Equal(t, []string{"nccl-all-gather/WorkloadFailed"}, keys)
	assert.Equal(t, "true", node.Labels[LabelKey])
}

func TestRemoveTuple(t *testing.T) {
	mgr, _ := newFakeManager()
	ctx := context.Background()

	require.NoError(t, mgr.AddTuple(ctx, "gpu-01", "nccl-all-gather/WorkloadFailed"))
	require.NoError(t, mgr.AddTuple(ctx, "gpu-01", "nemotron5-8b/ThresholdViolation"))

	require.NoError(t, mgr.RemoveTuple(ctx, "gpu-01", "nccl-all-gather/WorkloadFailed"))

	keys := getAnnotationKeys(t, mgr, "gpu-01")
	assert.Equal(t, []string{"nemotron5-8b/ThresholdViolation"}, keys)

	node := getNode(t, mgr, "gpu-01")
	assert.Equal(t, "true", node.Labels[LabelKey], "label should persist while tuples remain")

	require.NoError(t, mgr.RemoveTuple(ctx, "gpu-01", "nemotron5-8b/ThresholdViolation"))

	node = getNode(t, mgr, "gpu-01")
	_, hasAnnotation := node.Annotations[AnnotationKey]
	_, hasLabel := node.Labels[LabelKey]
	assert.False(t, hasAnnotation, "annotation should be removed when last tuple is deleted")
	assert.False(t, hasLabel, "label should be removed when last tuple is deleted")
}

func TestParseAnnotation(t *testing.T) {
	mgr, _ := newFakeManager()

	tests := []struct {
		name        string
		annotations map[string]string
		wantKeys    []string
		wantErr     bool
	}{
		{
			name: "valid annotation",
			annotations: map[string]string{
				AnnotationKey: `["nccl-all-gather/WorkloadFailed","nemotron5-8b/ThresholdViolation"]`,
			},
			wantKeys: []string{"nccl-all-gather/WorkloadFailed", "nemotron5-8b/ThresholdViolation"},
		},
		{
			name:        "no annotations",
			annotations: nil,
			wantKeys:    nil,
		},
		{
			name:        "empty annotation value",
			annotations: map[string]string{AnnotationKey: ""},
			wantKeys:    nil,
		},
		{
			name:        "empty JSON array",
			annotations: map[string]string{AnnotationKey: "[]"},
			wantKeys:    nil,
		},
		{
			name:        "malformed JSON",
			annotations: map[string]string{AnnotationKey: "not-json"},
			wantKeys:    nil,
			wantErr:     true,
		},
		{
			name:        "missing quotes around tuples",
			annotations: map[string]string{AnnotationKey: `[nccl-all-gather/WorkloadFailed]`},
			wantKeys:    nil,
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			node := &corev1.Node{
				ObjectMeta: metav1.ObjectMeta{
					Name:        "gpu-01",
					Annotations: tt.annotations,
				},
			}

			keys, err := mgr.ParseAnnotation(node)

			if tt.wantErr {
				assert.ErrorIs(t, err, ErrMalformedAnnotation)
				assert.Nil(t, keys)
				return
			}

			assert.NoError(t, err)

			if tt.wantKeys == nil {
				assert.Nil(t, keys)
				return
			}

			assert.Len(t, keys, len(tt.wantKeys))
			for _, k := range tt.wantKeys {
				assert.Contains(t, keys, k)
			}
		})
	}
}

func TestAddTuple_MalformedAnnotationIsLeftUntouched(t *testing.T) {
	corrupt := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "gpu-02",
			Labels:      map[string]string{LabelKey: "true"},
			Annotations: map[string]string{AnnotationKey: "not-json"},
		},
	}
	mgr, _ := newFakeManager(corrupt)
	ctx := context.Background()

	err := mgr.AddTuple(ctx, "gpu-02", "nccl-all-gather/WorkloadFailed")
	require.ErrorIs(t, err, ErrMalformedAnnotation)

	err = mgr.RemoveTuple(ctx, "gpu-02", "nccl-all-gather/WorkloadFailed")
	require.ErrorIs(t, err, ErrMalformedAnnotation)

	node := getNode(t, mgr, "gpu-02")
	assert.Equal(t, "not-json", node.Annotations[AnnotationKey], "malformed annotation must not be overwritten")
	assert.Equal(t, "true", node.Labels[LabelKey])
}
