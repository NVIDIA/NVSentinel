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

package managed

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	listersv1 "k8s.io/client-go/listers/core/v1"
)

// listerWith returns a NodeLister backed by an informer cache pre-populated
// with the given Nodes. Matches the production code path: monitors call
// IsNodeOptedOut(ctx, lister, name) against an informer-backed lister.
func listerWith(t *testing.T, nodes ...*corev1.Node) listersv1.NodeLister {
	t.Helper()

	factory := informers.NewSharedInformerFactory(nil, 0)
	informer := factory.Core().V1().Nodes().Informer()

	for _, n := range nodes {
		require.NoError(t, informer.GetStore().Add(n))
	}

	return factory.Core().V1().Nodes().Lister()
}

func node(name string, labels map[string]string) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: labels,
		},
	}
}

func TestIsNodeOptedOut(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		nodes    []*corev1.Node
		nodeName string
		want     bool
	}{
		{
			name:     "managed=false -> opted out",
			nodes:    []*corev1.Node{node("n1", map[string]string{ManagedLabelKey: "false"})},
			nodeName: "n1",
			want:     true,
		},
		{
			name:     "managed=true -> NOT opted out (managed normally)",
			nodes:    []*corev1.Node{node("n1", map[string]string{ManagedLabelKey: "true"})},
			nodeName: "n1",
			want:     false,
		},
		{
			name:     "managed label absent -> NOT opted out",
			nodes:    []*corev1.Node{node("n1", map[string]string{"other": "label"})},
			nodeName: "n1",
			want:     false,
		},
		{
			name:     "managed label nil map -> NOT opted out",
			nodes:    []*corev1.Node{node("n1", nil)},
			nodeName: "n1",
			want:     false,
		},
		{
			name:     "managed=<typo> -> NOT opted out (only canonical false matters)",
			nodes:    []*corev1.Node{node("n1", map[string]string{ManagedLabelKey: "False"})},
			nodeName: "n1",
			want:     false,
		},
		{
			name:     "managed=anything-else -> NOT opted out",
			nodes:    []*corev1.Node{node("n1", map[string]string{ManagedLabelKey: "maintenance"})},
			nodeName: "n1",
			want:     false,
		},
		{
			name:     "node not in cache -> NOT opted out (fail-open)",
			nodes:    []*corev1.Node{node("n1", map[string]string{ManagedLabelKey: "false"})},
			nodeName: "missing",
			want:     false,
		},
		{
			name:     "empty cache -> NOT opted out (fail-open)",
			nodes:    nil,
			nodeName: "n1",
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			lister := listerWith(t, tt.nodes...)
			got := IsNodeOptedOut(context.Background(), lister, tt.nodeName)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestIsNodeOptedOut_NilLister(t *testing.T) {
	t.Parallel()

	assert.False(t, IsNodeOptedOut(context.Background(), nil, "n1"),
		"nil lister must fail open, not panic")
}

func TestIsNodeOptedOut_EmptyNodeName(t *testing.T) {
	t.Parallel()

	lister := listerWith(t)
	assert.False(t, IsNodeOptedOut(context.Background(), lister, ""),
		"empty node name must fail open, not query the cache")
}
