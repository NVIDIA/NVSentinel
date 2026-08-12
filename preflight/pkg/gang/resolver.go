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
	"fmt"
	"sync"

	"github.com/nvidia/nvsentinel/preflight/pkg/config"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DiscovererResolver returns the gang discoverer that applies to a given
// namespace. It holds a cluster-wide default discoverer plus a set of
// namespace-scoped discoverers that are maintained dynamically (e.g. by the
// PreflightConfig controller) via Set and Remove. Lookups via For are
// safe for concurrent use by the admission webhook and the gang controller.
type DiscovererResolver struct {
	defaultDiscoverer GangDiscoverer

	mu          sync.RWMutex
	byNamespace map[string]GangDiscoverer
}

// NewResolver creates a DiscovererResolver from an already-constructed default
// discoverer and an optional map of namespace -> discoverer overrides. The
// override map is copied; later mutations should go through Set/Remove.
func NewResolver(defaultDiscoverer GangDiscoverer, overrides map[string]GangDiscoverer) *DiscovererResolver {
	byNamespace := make(map[string]GangDiscoverer, len(overrides))
	for ns, d := range overrides {
		byNamespace[ns] = d
	}

	return &DiscovererResolver{
		defaultDiscoverer: defaultDiscoverer,
		byNamespace:       byNamespace,
	}
}

// NewResolverFromConfig builds a DiscovererResolver whose default discoverer is
// constructed from the cluster-wide cfg.GangDiscovery. The default is validated
// against the cluster (via NewDiscovererFromConfig), so an invalid or
// unavailable scheduler CRD fails fast at startup. Per-namespace discoverers are
// added later by the PreflightConfig controller via Set/Remove.
func NewResolverFromConfig(
	cfg *config.Config,
	c client.Client,
	restMapper meta.RESTMapper,
) (*DiscovererResolver, error) {
	defaultDiscoverer, err := NewDiscovererFromConfig(cfg.GangDiscovery, c, restMapper)
	if err != nil {
		return nil, fmt.Errorf("failed to create default gang discoverer: %w", err)
	}

	return NewResolver(defaultDiscoverer, nil), nil
}

// For returns the gang discoverer for the given namespace, falling back to the
// cluster-wide default discoverer when the namespace has no override.
func (r *DiscovererResolver) For(namespace string) GangDiscoverer {
	r.mu.RLock()
	defer r.mu.RUnlock()

	if discoverer, ok := r.byNamespace[namespace]; ok {
		return discoverer
	}

	return r.defaultDiscoverer
}

// Set registers (or replaces) the discoverer for a namespace. Its only caller
// is the PreflightConfig controller, which always holds a non-nil resolver.
func (r *DiscovererResolver) Set(namespace string, discoverer GangDiscoverer) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.byNamespace[namespace] = discoverer
}

// Remove drops any namespace-scoped discoverer, so the namespace falls back to
// the cluster-wide default. Its only caller is the PreflightConfig controller,
// which always holds a non-nil resolver.
func (r *DiscovererResolver) Remove(namespace string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	delete(r.byNamespace, namespace)
}
