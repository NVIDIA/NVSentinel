// Copyright (c) 2026, NVIDIA CORPORATION.  All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Removing the last entry of a map must delete that entry, not the map. A
// caller patches against a projection of the cached node, and the live node
// carries labels the projection never had: hostname, zone, instance type. A
// patch of "labels": null takes all of them with it.
func TestNodeMergePatch_RemovingLastEntry_DeletesEntryNotMap(t *testing.T) {
	node := func(labels, annotations map[string]string) *v1.Node {
		return &v1.Node{ObjectMeta: metav1.ObjectMeta{
			Name:        "node-a",
			Labels:      labels,
			Annotations: annotations,
		}}
	}

	tests := []struct {
		name               string
		original, modified *v1.Node
		want               string
	}{
		{
			name:     "one of two labels removed",
			original: node(map[string]string{"a": "1", "b": "2"}, nil),
			modified: node(map[string]string{"a": "1"}, nil),
			want:     `{"metadata":{"labels":{"b":null}}}`,
		},
		{
			name:     "every label removed",
			original: node(map[string]string{"a": "1", "b": "2"}, nil),
			modified: node(map[string]string{}, nil),
			want:     `{"metadata":{"labels":{"a":null,"b":null}}}`,
		},
		{
			name:     "sole label removed",
			original: node(map[string]string{"only": "1"}, nil),
			modified: node(map[string]string{}, nil),
			want:     `{"metadata":{"labels":{"only":null}}}`,
		},
		{
			name:     "every annotation removed",
			original: node(nil, map[string]string{"x": "1"}),
			modified: node(nil, map[string]string{}),
			want:     `{"metadata":{"annotations":{"x":null}}}`,
		},
		{
			name:     "label added to a node that had none",
			original: node(map[string]string{}, nil),
			modified: node(map[string]string{"a": "1"}, nil),
			want:     `{"metadata":{"labels":{"a":"1"}}}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patch, err := NodeMergePatch(tt.original, tt.modified)
			require.NoError(t, err)
			require.JSONEq(t, tt.want, string(patch))
			require.NotContains(t, string(patch), `"labels":null`,
				"a null map deletes every label on the live node")
			require.NotContains(t, string(patch), `"annotations":null`,
				"a null map deletes every annotation on the live node")
		})
	}
}

// A map the caller never touched must not be patched at all, so that a
// projection missing a map cannot write its gap over the live object.
func TestNodeMergePatch_UntouchedNilMaps_ProduceNoPatch(t *testing.T) {
	unchanged := &v1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node-a"}}

	patch, err := NodeMergePatch(unchanged, unchanged.DeepCopy())
	require.NoError(t, err)
	require.Nil(t, patch, "two nodes that agree need no patch")
}
