// Copyright (c) 2026, NVIDIA CORPORATION. All rights reserved.
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
	"testing"

	"github.com/stretchr/testify/require"
	v1 "k8s.io/api/core/v1"
)

func TestNodeMergePatch_OptimisticLock_UsesOriginalIdentity(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*v1.Node)
		expected string
	}{
		{
			name: "annotation change",
			mutate: func(node *v1.Node) {
				node.Annotations["session"] = "next"
			},
			expected: `{"metadata":{"annotations":{"session":"next"},"uid":"original-node","resourceVersion":"42"}}`,
		},
		{
			name: "last annotation removed",
			mutate: func(node *v1.Node) {
				delete(node.Annotations, "session")
			},
			expected: `{"metadata":{"annotations":{"session":null},"uid":"original-node","resourceVersion":"42"}}`,
		},
		{
			name: "spec change",
			mutate: func(node *v1.Node) {
				node.Spec.Unschedulable = true
			},
			expected: `{"metadata":{"uid":"original-node","resourceVersion":"42"},"spec":{"unschedulable":true}}`,
		},
		{
			name:   "no supported change",
			mutate: func(*v1.Node) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := node(nil, map[string]string{"session": "current"})
			original.UID = "original-node"
			original.ResourceVersion = "42"
			modified := original.DeepCopy()
			// Mutators cannot change the identity or version used for preconditions.
			modified.UID = "different-node"
			modified.ResourceVersion = "99"
			tt.mutate(modified)

			patch, err := NodeMergePatch(original, modified, WithOptimisticLock())
			require.NoError(t, err)
			if tt.expected == "" {
				require.Nil(t, patch, "preconditions alone must not produce a write")
				return
			}

			require.JSONEq(t, tt.expected, string(patch))
		})
	}
}

func TestNodeMergePatch_OptimisticLock_EquivalentEmptyMapsProduceNoPatch(t *testing.T) {
	original := node(nil, nil)
	original.UID = "original-node"
	modified := original.DeepCopy()
	modified.Labels = map[string]string{}
	modified.Annotations = map[string]string{}

	patch, err := NodeMergePatch(original, modified, WithOptimisticLock())
	require.NoError(t, err)
	require.Nil(t, patch, "nil and empty maps remain equivalent with preconditions")
}
