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

package gang

import (
	"context"
	"testing"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"
	"github.com/nvidia/nvsentinel/preflight/pkg/gang/types"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// mockResolverDiscoverer is a minimal GangDiscoverer used to assert which
// discoverer the resolver returns for a namespace.
type mockResolverDiscoverer struct {
	name string
}

func (m *mockResolverDiscoverer) Name() string                       { return m.name }
func (m *mockResolverDiscoverer) CanHandle(_ *corev1.Pod) bool       { return true }
func (m *mockResolverDiscoverer) ExtractGangID(_ *corev1.Pod) string { return "" }
func (m *mockResolverDiscoverer) DiscoverPeers(_ context.Context, _ *corev1.Pod) (*types.GangInfo, error) {
	return nil, nil
}

func TestDiscovererResolver_For(t *testing.T) {
	def := &mockResolverDiscoverer{name: "default"}
	volcano := &mockResolverDiscoverer{name: "volcano"}
	kai := &mockResolverDiscoverer{name: "kai"}

	resolver := NewResolver(def, map[string]GangDiscoverer{
		"team-a":         volcano,
		"team-a-staging": volcano,
		"team-d":         kai,
	})

	tests := []struct {
		name      string
		namespace string
		want      string
	}{
		{name: "override namespace returns its discoverer", namespace: "team-a", want: "volcano"},
		{name: "second namespace sharing an override", namespace: "team-a-staging", want: "volcano"},
		{name: "distinct override namespace", namespace: "team-d", want: "kai"},
		{name: "unlisted namespace falls back to default", namespace: "team-z", want: "default"},
		{name: "empty namespace falls back to default", namespace: "", want: "default"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolver.For(tt.namespace)
			require.NotNil(t, got)
			assert.Equal(t, tt.want, got.Name())
		})
	}
}

func TestDiscovererResolver_SetRemove(t *testing.T) {
	def := &mockResolverDiscoverer{name: "default"}
	volcano := &mockResolverDiscoverer{name: "volcano"}

	resolver := NewResolver(def, nil)

	// Unknown namespace falls back to default.
	assert.Equal(t, "default", resolver.For("team-a").Name())

	// After Set, the namespace resolves to the registered discoverer.
	resolver.Set("team-a", volcano)
	assert.Equal(t, "volcano", resolver.For("team-a").Name())

	// After Remove, it falls back to the default again.
	resolver.Remove("team-a")
	assert.Equal(t, "default", resolver.For("team-a").Name())
}

func TestNewResolverFromConfig(t *testing.T) {
	fakeClient := fake.NewClientBuilder().Build()

	restMapper := meta.NewDefaultRESTMapper([]schema.GroupVersion{
		{Group: "scheduling.k8s.io", Version: "v1alpha2"},
		{Group: "scheduling.volcano.sh", Version: "v1beta1"},
	})
	restMapper.Add(schema.GroupVersionKind{
		Group: "scheduling.k8s.io", Version: "v1alpha2", Kind: "PodGroup",
	}, meta.RESTScopeNamespace)
	restMapper.Add(schema.GroupVersionKind{
		Group: "scheduling.volcano.sh", Version: "v1beta1", Kind: "PodGroup",
	}, meta.RESTScopeNamespace)

	t.Run("builds the cluster-wide default discoverer", func(t *testing.T) {
		cfg := &config.Config{
			FileConfig: config.FileConfig{
				GangDiscovery: config.GangDiscoveryConfig{
					Name:           "volcano",
					AnnotationKeys: []string{"scheduling.k8s.io/group-name"},
					PodGroupGVR: config.GVRConfig{
						Group:    "scheduling.volcano.sh",
						Version:  "v1beta1",
						Resource: "podgroups",
					},
					MinCountExpr: "podGroup.spec.minMember",
				},
			},
		}

		resolver, err := NewResolverFromConfig(cfg, fakeClient, restMapper)
		require.NoError(t, err)

		// Every namespace uses the default until overrides are registered.
		assert.Equal(t, "volcano", resolver.For("team-a").Name())
		assert.Equal(t, "volcano", resolver.For("team-other").Name())
	})

	t.Run("invalid default config fails fast", func(t *testing.T) {
		cfg := &config.Config{
			FileConfig: config.FileConfig{
				GangDiscovery: config.GangDiscoveryConfig{
					// name set but missing GVR/keys/expr => invalid.
					Name: "broken",
				},
			},
		}

		_, err := NewResolverFromConfig(cfg, fakeClient, restMapper)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "default gang discoverer")
	})
}
