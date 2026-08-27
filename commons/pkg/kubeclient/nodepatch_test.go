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

package kubeclient

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

func node(labels, annotations map[string]string) *v1.Node {
	return &v1.Node{
		Name:            "node-1",
		ResourceVersion: "1",
		Labels:          labels,
		Annotations:     annotations,
	}
}

func TestNodePatcher_CachedNode_UsesPatchAndSkipsNoOp(t *testing.T) {
	current := node(map[string]string{"a": "1"}, nil)
	clientset := fake.NewSimpleClientset(current.DeepCopy())
	var patcher NodePatcher
	mutate := func(node *v1.Node) error {
		node.Labels["b"] = "2"
		return nil
	}

	clientset.ClearActions()
	changed, err := patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		current.Name,
		current,
		mutate,
	)
	require.NoError(t, err)
	require.True(t, changed)
	require.Len(t, clientset.Actions(), 1)
	action, ok := clientset.Actions()[0].(k8stesting.PatchAction)
	require.True(t, ok)
	assert.JSONEq(t, `{"metadata":{"labels":{"b":"2"}}}`, string(action.GetPatch()))

	updated, err := clientset.CoreV1().Nodes().Get(t.Context(), current.Name, metav1.GetOptions{})
	require.NoError(t, err)
	clientset.ClearActions()
	changed, err = patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		current.Name,
		updated,
		mutate,
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Empty(t, clientset.Actions())
}

func TestNodePatcher_NoOpBetweenWrites_KeepsReadingLiveNode(t *testing.T) {
	current := node(nil, map[string]string{"events": "base"})
	clientset := fake.NewSimpleClientset(current.DeepCopy())
	var patcher NodePatcher

	_, err := patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		current.Name,
		current,
		func(node *v1.Node) error {
			node.Annotations["events"] += "|first"
			return nil
		},
	)
	require.NoError(t, err)

	stale := current.DeepCopy()
	stale.ResourceVersion = "stale"
	clientset.ClearActions()
	changed, err := patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		current.Name,
		stale,
		func(*v1.Node) error { return nil },
	)
	require.NoError(t, err)
	assert.False(t, changed)
	require.Len(t, clientset.Actions(), 1)
	assert.Equal(t, "get", clientset.Actions()[0].GetVerb())

	clientset.ClearActions()
	changed, err = patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		current.Name,
		stale,
		func(node *v1.Node) error {
			node.Annotations["events"] += "|second"
			return nil
		},
	)
	require.NoError(t, err)
	assert.True(t, changed)
	require.Len(t, clientset.Actions(), 2)
	assert.Equal(t, "get", clientset.Actions()[0].GetVerb())
	assert.Equal(t, "patch", clientset.Actions()[1].GetVerb())

	updated, err := clientset.CoreV1().Nodes().Get(t.Context(), current.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "base|first|second", updated.Annotations["events"])
}

func TestNodePatcher_LiveReadRetriesTransientFailure(t *testing.T) {
	current := node(map[string]string{"a": "1"}, nil)
	clientset := fake.NewSimpleClientset(current.DeepCopy())
	var patcher NodePatcher
	patcher.pendingVersions.Store(current.Name, "written")

	stale := current.DeepCopy()
	stale.ResourceVersion = "stale"
	getAttempts := 0
	clientset.PrependReactor("get", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		getAttempts++
		if getAttempts == 1 {
			return true, nil, apierrors.NewTooManyRequests("try again", 0)
		}

		return false, nil, nil
	})

	changed, err := patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		current.Name,
		stale,
		func(*v1.Node) error { return nil },
	)
	require.NoError(t, err)
	assert.False(t, changed)
	assert.Equal(t, 2, getAttempts)
}

func TestNodePatcher_Conflict_RefreshesLiveNodeBeforeRetry(t *testing.T) {
	cached := node(map[string]string{"cached": "true"}, nil)
	live := cached.DeepCopy()
	live.ResourceVersion = "2"
	live.Labels["concurrent"] = "preserved"
	clientset := fake.NewSimpleClientset(live)

	patchAttempts := 0
	clientset.PrependReactor("patch", "nodes", func(k8stesting.Action) (bool, runtime.Object, error) {
		patchAttempts++
		if patchAttempts == 1 {
			return true, nil, apierrors.NewConflict(
				schema.GroupResource{Resource: "nodes"},
				cached.Name,
				assert.AnError,
			)
		}

		return false, nil, nil
	})

	var patcher NodePatcher
	changed, err := patcher.Patch(
		context.Background(),
		clientset.CoreV1().Nodes(),
		cached.Name,
		cached,
		func(node *v1.Node) error {
			node.Labels["desired"] = "true"
			return nil
		},
	)
	require.NoError(t, err)
	assert.True(t, changed)
	assert.Equal(t, 2, patchAttempts)

	updated, err := clientset.CoreV1().Nodes().Get(t.Context(), cached.Name, metav1.GetOptions{})
	require.NoError(t, err)
	assert.Equal(t, "preserved", updated.Labels["concurrent"])
	assert.Equal(t, "true", updated.Labels["desired"])
}

func TestNodeMergePatch_ReturnsExpectedPatch(t *testing.T) {
	resourceVersionOriginal := node(nil, nil)
	resourceVersionOriginal.ResourceVersion = "42"
	resourceVersionModified := resourceVersionOriginal.DeepCopy()
	resourceVersionModified.Spec.Unschedulable = true

	projected := &v1.Node{
		Name:            "node-1",
		ResourceVersion: "1",
		Labels:          map[string]string{"gpu": "true"},
		Annotations:     map[string]string{"kept": "yes"},
	}
	projectedModified := projected.DeepCopy()
	projectedModified.Labels["driver.installed"] = "true"

	specOriginal := node(nil, nil)
	specModified := specOriginal.DeepCopy()
	specModified.Spec.Unschedulable = true
	specModified.Spec.Taints = []v1.Taint{{Key: "held", Effect: v1.TaintEffectNoSchedule}}

	tests := []struct {
		name     string
		original *v1.Node
		modified *v1.Node
		expected string
		excluded []string
	}{
		{
			name:     "no change produces no patch",
			original: node(map[string]string{"a": "1"}, map[string]string{"b": "2"}),
			modified: node(map[string]string{"a": "1"}, map[string]string{"b": "2"}),
			expected: "",
		},
		{
			name:     "adds a label",
			original: node(map[string]string{"a": "1"}, nil),
			modified: node(map[string]string{"a": "1", "b": "2"}, nil),
			expected: `{"metadata":{"labels":{"b":"2"}}}`,
		},
		{
			name:     "changes a label without mentioning the others",
			original: node(map[string]string{"a": "1", "b": "2"}, nil),
			modified: node(map[string]string{"a": "9", "b": "2"}, nil),
			expected: `{"metadata":{"labels":{"a":"9"}}}`,
		},
		{
			name:     "removes a label with an explicit null",
			original: node(map[string]string{"a": "1", "b": "2"}, nil),
			modified: node(map[string]string{"a": "1"}, nil),
			expected: `{"metadata":{"labels":{"b":null}}}`,
		},
		{
			name:     "adds an annotation",
			original: node(nil, nil),
			modified: node(nil, map[string]string{"bootstrap": "true"}),
			expected: `{"metadata":{"annotations":{"bootstrap":"true"}}}`,
		},
		{
			name:     "carries labels and annotations in a single patch",
			original: node(map[string]string{"a": "1"}, nil),
			modified: node(map[string]string{"a": "2"}, map[string]string{"bootstrap": "true"}),
			expected: `{"metadata":{"annotations":{"bootstrap":"true"},"labels":{"a":"2"}}}`,
		},
		{
			name:     "a nil map and an empty map are the same thing",
			original: node(nil, nil),
			modified: node(map[string]string{}, map[string]string{}),
			expected: "",
		},
		{
			name:     "sets a label onto a node that had none",
			original: node(nil, nil),
			modified: node(map[string]string{"a": "1"}, nil),
			expected: `{"metadata":{"labels":{"a":"1"}}}`,
		},
		{
			name:     "spec change includes original resource version",
			original: resourceVersionOriginal,
			modified: resourceVersionModified,
			expected: `{"metadata":{"resourceVersion":"42"},"spec":{"unschedulable":true}}`,
		},
		{
			name:     "projected fields remain absent",
			original: projected,
			modified: projectedModified,
			expected: `{"metadata":{"labels":{"driver.installed":"true"}}}`,
			excluded: []string{"annotations", "spec"},
		},
		{
			name:     "sets spec fields",
			original: specOriginal,
			modified: specModified,
			expected: `{"metadata":{"resourceVersion":"1"},"spec":{"taints":[{"key":"held","effect":"NoSchedule"}],"unschedulable":true}}`,
		},
		{
			name:     "clears spec fields",
			original: specModified,
			modified: specOriginal,
			expected: `{"metadata":{"resourceVersion":"1"},"spec":{"taints":null,"unschedulable":null}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, err := NodeMergePatch(tt.original, tt.modified)
			require.NoError(t, err)

			if tt.expected == "" {
				assert.Nil(t, patch, "equivalent nodes must not cost an API call")
				return
			}

			assert.JSONEq(t, tt.expected, string(patch))
			for _, field := range tt.excluded {
				assert.NotContains(t, string(patch), field)
			}
		})
	}
}
